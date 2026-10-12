package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"julia/internal/core"
)

const (
	webSearchEndpoint = "https://api.firecrawl.dev/v2/search"
	// WebSearchKeyEnv names the environment variable holding the Firecrawl API key.
	WebSearchKeyEnv  = "FIRECRAWL_API_KEY"
	maxWebSearchBody = 1 << 20
	maxWebSearchText = 4 << 10
	// Firecrawl rejects queries over 500 characters; bytes are the stricter bound.
	maxWebSearchQuery = 500
)

func webSearchDefinition() core.ToolDefinition {
	return core.ToolDefinition{
		Name:        "WebSearch",
		Description: "Search the public web through Firecrawl's search API. Sends only the query, after automatic/human review; unavailable in read-only mode or without " + WebSearchKeyEnv + ". Returns at most 4 KiB of titles, source URLs, and short unverified snippets (default 3 results, maximum 5), never page contents. Do not send secrets or private project content. Authentication, credit, rate-limit, and service failures return errors, not empty results. No destination pages are fetched.",
		Parameters: schema(map[string]any{
			"query": map[string]any{"type": "string", "description": "Public search query, nonempty and at most 500 UTF-8 bytes; no control characters. Operators such as site: are supported", "minLength": 1, "maxLength": maxWebSearchQuery},
			"limit": map[string]any{"type": "integer", "description": "Number of results, default 3; increase only when useful", "minimum": 1, "maximum": 5},
		}, "query"),
	}
}

// This client has one fixed search destination, no cookie jar or inherited proxy,
// and never follows redirects. Search result URLs are returned, never requested.
// HTTP runs in the harness, not in Bash's OS sandbox, so the API key never
// reaches a shell environment.
func newWebSearchClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 25 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConns:          2,
			MaxConnsPerHost:       2,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (e *Executor) webSearch(ctx context.Context, call core.ToolCall) (string, error) {
	if e.config.Mode == "read-only" {
		return "", errors.New("read-only mode prohibits WebSearch's outbound queries")
	}
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	args.Limit = 3
	if err := decode(call.Arguments, &args); err != nil {
		return "", err
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" || len(args.Query) > maxWebSearchQuery || strings.IndexFunc(args.Query, webControl) >= 0 {
		return "", errors.New("query must be nonempty, at most 500 UTF-8 bytes, and contain no control characters")
	}
	if args.Limit < 1 || args.Limit > 5 {
		return "", errors.New("limit must be between 1 and 5")
	}
	key, err := e.searchKey(ctx, call)
	if err != nil {
		return "", err
	}
	approval := core.Approval{
		Call: call, Task: e.config.Task, Directory: e.directory,
		Network: true, ExecutionMode: "web-search",
		Reason: "Send this query to Firecrawl's search API? Only the query is sent, authenticated with the configured Firecrawl key; no workspace files are uploaded and no destination pages are fetched. Search runs through a fixed HTTPS client, not the Bash sandbox.",
	}
	if err := e.authorizeRequest(ctx, approval, true); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.webClient == nil {
		e.webClient = newWebSearchClient()
	}
	// Ask Firecrawl to give up before the client does so slow searches report
	// a clean timeout. No scrapeOptions: only url/title/description come back.
	payload, err := json.Marshal(map[string]any{"query": args.Query, "limit": args.Limit, "sources": []string{"web"}, "timeout": 20000})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webSearchEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "JULIA/1.0 (web search)")
	resp, err := e.webClient.Do(req)
	if err != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return "", fmt.Errorf("Firecrawl search request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := e.firecrawlStatus(resp.StatusCode, key, "search"); err != nil {
		return "", err
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return "", errors.New("Firecrawl returned an unexpected content type, not search JSON")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebSearchBody+1))
	if err != nil {
		return "", fmt.Errorf("read Firecrawl search response: %w", err)
	}
	if len(body) > maxWebSearchBody {
		return "", errors.New("Firecrawl search response exceeded 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	results, err := parseWebSearch(body)
	if err != nil {
		return "", err
	}
	return formatWebSearch(results, args.Limit), nil
}

// firecrawlStatus turns a Firecrawl response status into the error the agent
// sees. Error bodies are never echoed: they are remote content and may quote
// the request. what names the operation, search or fetch.
func (e *Executor) firecrawlStatus(status int, key, what string) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		if e.config.SearchKeyFile != "" && key != e.envKey {
			// A revoked saved key would otherwise fail forever; set up again next time.
			e.config.SearchKey = ""
			if err := os.Remove(e.config.SearchKeyFile); err == nil || errors.Is(err, os.ErrNotExist) {
				return errors.New("Firecrawl rejected the saved API key (HTTP 401); it was removed, so the next web call will set up Firecrawl again")
			}
		}
		return fmt.Errorf("Firecrawl rejected the API key (HTTP 401); check %s", WebSearchKeyEnv)
	case http.StatusForbidden:
		return fmt.Errorf("Firecrawl refused the %s (HTTP 403); the key may lack access", what)
	case http.StatusPaymentRequired:
		return fmt.Errorf("Firecrawl %s unavailable: the account is out of credits (HTTP 402)", what)
	case http.StatusTooManyRequests:
		return fmt.Errorf("Firecrawl %s rate limit reached (HTTP 429). No retry was attempted", what)
	case http.StatusRequestTimeout:
		return fmt.Errorf("Firecrawl %s timed out (HTTP 408). No retry was attempted", what)
	}
	return fmt.Errorf("Firecrawl %s returned HTTP %d; redirects are not followed", what, status)
}

type webSearchResult struct {
	title, url, snippet string
}

func parseWebSearch(body []byte) ([]webSearchResult, error) {
	var response struct {
		Success *bool `json:"success"`
		Data    *struct {
			Web []struct {
				URL         string `json:"url"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"web"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.New("could not parse Firecrawl search JSON")
	}
	if response.Success == nil || !*response.Success {
		return nil, errors.New("Firecrawl reported an unsuccessful search")
	}
	if response.Data == nil {
		return nil, errors.New("Firecrawl returned no recognizable search results; its response format may have changed")
	}
	var results []webSearchResult
	seen := make(map[string]bool)
	for _, item := range response.Data.Web {
		if len(results) == 5 {
			break
		}
		target := webResultURL(item.URL)
		title := compactWebText(item.Title, 160)
		if target == "" || seen[target] {
			continue
		}
		if title == "" {
			title = compactWebText(target, 160)
		}
		seen[target] = true
		results = append(results, webSearchResult{title, target, compactWebText(item.Description, 240)})
	}
	if len(results) == 0 && len(response.Data.Web) > 0 {
		return nil, errors.New("Firecrawl returned no usable result URLs")
	}
	return results, nil
}

func webResultURL(raw string) string {
	if len(raw) > 8192 || strings.IndexFunc(raw, webControl) >= 0 {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	result := u.String()
	if len(result) > 1024 || strings.IndexFunc(result, webControl) >= 0 {
		return ""
	}
	return result
}

func webControl(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

func compactWebText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if webControl(r) {
			return -1
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	end := limit - len("…")
	for !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

func formatWebSearch(results []webSearchResult, limit int) string {
	if len(results) == 0 {
		return "Firecrawl found no results for this query."
	}
	var out strings.Builder
	out.WriteString("Firecrawl search — unverified web snippets, not page contents:\n")
	const omitted = "\nAdditional results omitted to keep this response within 4 KiB.\n"
	for i, result := range results[:min(len(results), limit)] {
		entry := "\n" + strconv.Itoa(i+1) + ". " + result.title + "\n" + result.url + "\n"
		if result.snippet != "" {
			entry += result.snippet + "\n"
		}
		if out.Len()+len(entry)+len(omitted) > maxWebSearchText {
			out.WriteString(omitted)
			break
		}
		out.WriteString(entry)
	}
	return out.String()
}
