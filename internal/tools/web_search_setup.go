package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"jarvis/internal/core"
)

// Firecrawl's browser authorization for coding agents: the human signs in (or
// signs up) and authorizes at authURL, and the key is released to whoever holds
// the PKCE verifier. session_id travels in the URL fragment, never to a server log.
const (
	webSearchAuthURL     = "https://www.firecrawl.dev/cli-auth"
	webSearchAuthPollURL = "https://www.firecrawl.dev/api/auth/cli/status"
	webSearchAuthTimeout = 5 * time.Minute
	maxWebSearchKeyFile  = 4 << 10
)

// Polling is a variable so tests need not wait; the server allows 30 polls a minute.
var webSearchAuthInterval = 3 * time.Second

// LoadSearchKey reads a Firecrawl key saved by first-use setup. A missing file
// is not an error: setup runs on the first WebSearch call instead.
func LoadSearchKey(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	var stored struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxWebSearchKeyFile)).Decode(&stored); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if !validSearchKey(stored.APIKey) {
		return "", fmt.Errorf("%s does not contain a valid Firecrawl API key", path)
	}
	return stored.APIKey, nil
}

func saveSearchKey(path, key string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"api_key": key})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".firecrawl-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func validSearchKey(key string) bool {
	if len(key) < 8 || len(key) > 256 || key[:3] != "fc-" {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// searchKey returns the configured key, picking up one saved by another session
// and otherwise running first-use setup with the human.
func (e *Executor) searchKey(ctx context.Context, call core.ToolCall) (string, error) {
	if e.config.SearchKey != "" {
		return e.config.SearchKey, nil
	}
	if key, err := LoadSearchKey(e.config.SearchKeyFile); err != nil {
		return "", err
	} else if key != "" {
		e.config.SearchKey = key
		return key, nil
	}
	if e.config.SearchKeyFile == "" {
		return "", errors.New("WebSearch is not configured: set " + WebSearchKeyEnv + " to a Firecrawl API key and restart JARVIS")
	}
	key, err := e.setupWebSearch(ctx, call)
	if err != nil {
		return "", err
	}
	e.config.SearchKey = key
	return key, nil
}

func (e *Executor) setupWebSearch(ctx context.Context, call core.ToolCall) (string, error) {
	approval := core.Approval{
		Call: call, Task: e.config.Task, Directory: e.directory,
		Network: true, ExecutionMode: "firecrawl-setup",
		Reason: "WebSearch uses Firecrawl and needs an API key. Allow JARVIS to open Firecrawl in your browser, where you sign in or create a free account and click Authorize? The key is then saved to " + e.config.SearchKeyFile + " (owner-only) and reused by later sessions. Set " + WebSearchKeyEnv + " instead to skip this.",
	}
	// Setup always needs a person at the browser, so it skips the automatic reviewer.
	if err := e.authorizeRequest(ctx, approval, false); err != nil {
		return "", fmt.Errorf("WebSearch setup not completed: %w. Alternatively set %s", err, WebSearchKeyEnv)
	}
	sessionID, verifier, err := webSearchAuthSecrets()
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	link := webSearchAuthURL + "?" + url.Values{"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "source": {"coding-agent"}}.Encode() + "#session_id=" + sessionID
	e.emit("warning", "Authorize Firecrawl for WebSearch in your browser. If it did not open, visit:\n"+link+"\nWaiting up to 5 minutes; Esc cancels.")
	if openBrowser != nil {
		_ = openBrowser(link)
	}
	if e.webClient == nil {
		e.webClient = newWebSearchClient()
	}
	key, err := e.pollWebSearchAuth(ctx, sessionID, verifier)
	if err != nil {
		return "", err
	}
	if err := saveSearchKey(e.config.SearchKeyFile, key); err != nil {
		// The key still works for this session; only persistence failed.
		e.emit("warning", "Firecrawl authorized, but saving the key failed: "+err.Error())
	} else {
		e.emit("status", "Firecrawl authorized; key saved to "+e.config.SearchKeyFile)
	}
	return key, nil
}

func webSearchAuthSecrets() (sessionID, verifier string, err error) {
	var session, secret [32]byte
	if _, err = rand.Read(session[:]); err != nil {
		return "", "", err
	}
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(session[:]), base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

// openBrowser is a variable so tests never launch a real browser.
var openBrowser = func(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "linux":
		cmd = exec.Command("xdg-open", link)
	default:
		return errors.New("no browser launcher for " + runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// pollWebSearchAuth waits for the human to authorize. Pending keeps polling until
// the deadline; repeated transport or server failures stop early, because more
// polling cannot fix them, and a rejection stops at once.
func (e *Executor) pollWebSearchAuth(ctx context.Context, sessionID, verifier string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, webSearchAuthTimeout)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"session_id": sessionID, "code_verifier": verifier})
	if err != nil {
		return "", err
	}
	transportFailures, serverFailures := 0, 0
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", errors.New("Firecrawl authorization was not completed within 5 minutes; try WebSearch again or set " + WebSearchKeyEnv)
			}
			return "", ctx.Err()
		case <-time.After(webSearchAuthInterval):
		}
		key, pending, err := e.pollWebSearchAuthOnce(ctx, payload)
		switch {
		case key != "":
			return key, nil
		case pending:
			transportFailures, serverFailures = 0, 0
		case errors.Is(err, errWebSearchAuthTransport):
			if ctx.Err() == nil {
				if transportFailures++; transportFailures >= 3 {
					return "", fmt.Errorf("Firecrawl authorization polling failed: %w", err)
				}
			}
		case errors.Is(err, errWebSearchAuthBusy):
			if serverFailures++; serverFailures >= 5 {
				return "", fmt.Errorf("Firecrawl authorization polling failed: %w", err)
			}
		default:
			return "", fmt.Errorf("Firecrawl authorization polling failed: %w", err)
		}
	}
}

var (
	errWebSearchAuthTransport = errors.New("could not reach www.firecrawl.dev")
	errWebSearchAuthBusy      = errors.New("www.firecrawl.dev is busy")
)

func (e *Executor) pollWebSearchAuthOnce(ctx context.Context, payload []byte) (key string, pending bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webSearchAuthPollURL, bytes.NewReader(payload))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "JARVIS/1.0 (web search setup)")
	resp, err := e.webClient.Do(req)
	if err != nil {
		return "", false, errWebSearchAuthTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", false, fmt.Errorf("%w (HTTP %d)", errWebSearchAuthBusy, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("www.firecrawl.dev rejected the authorization request (HTTP %d)", resp.StatusCode)
	}
	var status struct {
		Status string `json:"status"`
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxWebSearchKeyFile)).Decode(&status); err != nil {
		return "", false, fmt.Errorf("%w (unreadable response)", errWebSearchAuthBusy)
	}
	if status.Status == "complete" {
		if !validSearchKey(status.APIKey) {
			return "", false, errors.New("Firecrawl returned a malformed API key")
		}
		return status.APIKey, false, nil
	}
	return "", true, nil
}
