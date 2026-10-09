package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"jarvis/internal/config"
	"jarvis/internal/core"
	"jarvis/internal/provider"
)

// Any accidental route back to the executing API model fails locally, before
// a provider request can consume tokens or contact an external service.
type forbiddenReviewerModel struct {
	fantasy.LanguageModel
	calls int
}

func (m *forbiddenReviewerModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	return nil, errors.New("the executing API model must never review actions")
}

func TestBackendFactoryUsesFixedCodexReviewerForEveryProvider(t *testing.T) {
	for _, name := range []string{"codex", "openai", "anthropic", "google", "compatible"} {
		t.Run(name, func(t *testing.T) {
			binary, capture := reviewerProtocolFixture(t)
			cfg := reviewerFactoryConfig(t, name, binary)
			checkpointCalls := 0
			backend, review, err := newBackend(context.Background(), cfg, func(*core.Session) error {
				checkpointCalls++
				return errors.New("reviewer must not checkpoint the executing session")
			})
			if err != nil || backend == nil || review == nil {
				t.Fatal("construct backend and reviewer", err)
			}
			if backend.Name() != name {
				t.Fatalf("executing provider changed: %s", backend.Name())
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("backend construction started reviewer", err)
			}
			apiModel := &forbiddenReviewerModel{}
			if api, ok := backend.(*provider.API); ok {
				api.Model = apiModel
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			decision, err := review(ctx, core.Approval{Task: "Inspect the fixture", Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}})
			if err != nil || !decision.Allow || decision.Reason != "fixture approved" {
				t.Fatal("fixed reviewer was not used", decision, err)
			}
			if checkpointCalls != 0 || apiModel.calls != 0 {
				t.Fatal("review reached executing provider or session checkpoint", checkpointCalls, apiModel.calls)
			}
			if codex, ok := backend.(*provider.Codex); ok && (codex.Model != cfg.Model || codex.Reasoning != cfg.Reasoning) {
				t.Fatal("review overwrote executing model settings")
			}
			assertReviewerProtocol(t, capture)
		})
	}
}

func TestAPIFactoryMissingCodexIsLazyAndNeverFallsBackToExecutingModel(t *testing.T) {
	for _, name := range []string{"openai", "anthropic", "google", "compatible"} {
		for _, mode := range []string{"auto", "ask", "read-only"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				cfg := reviewerFactoryConfig(t, name, filepath.Join(t.TempDir(), "missing-codex"))
				cfg.Mode = mode
				backend, review, err := newBackend(context.Background(), cfg, nil)
				if err != nil || review == nil {
					t.Fatal("missing reviewer prevented API startup", err)
				}
				apiModel := &forbiddenReviewerModel{}
				backend.(*provider.API).Model = apiModel
				decision, err := review(context.Background(), core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}})
				if err == nil || !strings.Contains(err.Error(), "Codex CLI was not found") || decision.Allow || apiModel.calls != 0 {
					t.Fatal("missing reviewer did not fail closed independently", decision, err, apiModel.calls)
				}
			})
		}
	}
}

func reviewerFactoryConfig(t *testing.T, name, binary string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Provider, cfg.Model, cfg.CodexBinary = name, "main-model-must-stay-independent", binary
	if name == "codex" {
		cfg.Reasoning = "high"
	} else {
		cfg.BaseURL = "https://unused.invalid"
		t.Setenv(cfg.KeyEnv(), "fixture-key-never-sent")
	}
	return cfg
}

func reviewerProtocolFixture(t *testing.T) (binary, capture string) {
	t.Helper()
	directory := t.TempDir()
	binary, capture = filepath.Join(directory, "fake-codex"), filepath.Join(directory, "requests.jsonl")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Re-exec only the local test helper. Codex's app-server arguments are ignored.
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\nexec " + quoted + " -test.run=^TestReviewerFactoryProtocolProcess$\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JARVIS_TEST_REVIEWER_PROTOCOL", "1")
	t.Setenv("JARVIS_TEST_REVIEWER_CAPTURE", capture)
	return binary, capture
}

func assertReviewerProtocol(t *testing.T, capture string) {
	t.Helper()
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	threads, turns := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var request struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatal(err)
		}
		params := request.Params
		switch request.Method {
		case "thread/start":
			threads++
			config, _ := params["config"].(map[string]any)
			if params["model"] != provider.CodexReviewerModel || config["model_reasoning_effort"] != provider.CodexReviewerReasoning || params["serviceTier"] != "priority" || config["service_tier"] != "priority" {
				t.Fatal("reviewer thread inherited agent settings", params)
			}
			if params["ephemeral"] != true || params["sandbox"] != "read-only" || params["approvalPolicy"] != "never" {
				t.Fatal("reviewer lost isolation", params)
			}
		case "turn/start":
			turns++
			if params["effort"] != provider.CodexReviewerReasoning || params["serviceTier"] != "priority" || params["serviceTierForTurn"] != "priority" {
				t.Fatal("reviewer turn inherited agent settings", params)
			}
		case "model/list", "thread/resume":
			t.Fatal("fixed reviewer used inherited catalog or session", request.Method)
		}
	}
	if threads != 1 || turns != 1 {
		t.Fatal("expected exactly one isolated review", threads, turns)
	}
}

// TestReviewerFactoryProtocolProcess is a local, deterministic JSON-RPC process;
// it performs no inference or tools and never connects to a provider.
func TestReviewerFactoryProtocolProcess(t *testing.T) {
	if os.Getenv("JARVIS_TEST_REVIEWER_PROTOCOL") != "1" {
		return
	}
	os.Exit(runReviewerProtocolProcess())
}

func runReviewerProtocolProcess() int {
	capture, err := os.OpenFile(os.Getenv("JARVIS_TEST_REVIEWER_CAPTURE"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 1
	}
	defer capture.Close()
	output := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if _, err := capture.Write(append(scanner.Bytes(), '\n')); err != nil {
			return 2
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return 3
		}
		var result any
		switch request.Method {
		case "initialized":
			continue
		case "initialize":
			result = map[string]any{}
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "chatgpt"}, "requiresOpenaiAuth": true}
		case "config/read":
			result = map[string]any{"config": map[string]any{"model": "inherited-agent-model", "model_reasoning_effort": "xhigh", "service_tier": "flex"}}
		case "thread/start":
			result = map[string]any{"thread": map[string]any{"id": "review-thread"}, "model": request.Params["model"]}
		case "turn/start":
			result = map[string]any{"turn": map[string]any{"id": "review-turn"}}
		default:
			return 4
		}
		if err := output.Encode(map[string]any{"id": request.ID, "result": result}); err != nil {
			return 5
		}
		if request.Method == "turn/start" {
			if err := output.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "review-thread", "turnId": "review-turn", "item": map[string]any{"id": "review-message", "type": "agentMessage", "text": `{"allow":true,"reason":"fixture approved"}`}}}); err != nil {
				return 6
			}
			if err := output.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "review-thread", "turn": map[string]any{"id": "review-turn", "status": "completed"}}}); err != nil {
				return 7
			}
			return 0
		}
	}
	return 8
}
