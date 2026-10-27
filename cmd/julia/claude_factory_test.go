package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/provider"
)

func TestClaudeFactoryRunsClaudeCodeUnderTheConfiguredReviewer(t *testing.T) {
	t.Run("default Codex reviewer", func(t *testing.T) {
		codexBinary, codexCapture := reviewerProtocolFixture(t)
		claudeBinary, claudeCapture := claudeProtocolFixture(t)
		cfg := reviewerFactoryConfig(t, "codex", codexBinary)
		cfg.Provider, cfg.Model, cfg.Reasoning, cfg.ClaudeBinary = "claude", "opus", "high", claudeBinary
		backend, review, err := newBackend(context.Background(), cfg, nil)
		if err != nil || backend.Name() != "claude" {
			t.Fatal("construct Claude Code backend", backend, err)
		}
		claude, ok := backend.(*provider.Claude)
		if !ok || claude.Binary != claudeBinary || claude.Model != "opus" || claude.Reasoning != "high" {
			t.Fatalf("backend settings %+v", backend)
		}
		decision, err := review(context.Background(), core.Approval{Task: "Inspect the fixture", Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}})
		if err != nil || !decision.Allow || decision.Reason != "fixture approved" {
			t.Fatal("fixed Codex reviewer was not used", decision, err)
		}
		assertReviewerProtocol(t, codexCapture)
		if _, err := os.Stat(claudeCapture); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("review reached Claude Code", err)
		}
	})
	t.Run("Claude Code reviewer", func(t *testing.T) {
		for _, executing := range []string{"claude", "codex", "openai"} {
			t.Run(executing, func(t *testing.T) {
				codexBinary, codexCapture := reviewerProtocolFixture(t)
				claudeBinary, claudeCapture := claudeProtocolFixture(t)
				cfg := reviewerFactoryConfig(t, executing, codexBinary)
				cfg.ClaudeBinary, cfg.ReviewerModel = claudeBinary, "claude/opus"
				t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-reach-claude-code")
				backend, review, err := newBackend(context.Background(), cfg, nil)
				if err != nil || backend.Name() != executing {
					t.Fatal("construct backend", backend, err)
				}
				decision, err := review(context.Background(), core.Approval{Task: "Inspect the fixture", Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}})
				if err != nil || !decision.Allow || decision.Reason != "fixture approved" {
					t.Fatal("Claude Code reviewer was not used", decision, err)
				}
				record := readClaudeCapture(t, claudeCapture)
				if record.APIKeySet {
					t.Fatal("the API key reached Claude Code")
				}
				args := strings.Join(record.Args, " ")
				for _, want := range []string{"--model opus", "--effort " + provider.ClaudeReviewerReasoning, "--no-session-persistence", "--json-schema", "--tools  --strict-mcp-config", "--setting-sources  --disable-slash-commands", "--permission-prompt-tool stdio"} {
					if !strings.Contains(args, want) {
						t.Fatalf("reviewer args lack %q: %s", want, args)
					}
				}
				if strings.Contains(args, "--mcp-config") || strings.Contains(args, "--resume") || slices.Contains(record.Args, "--effort high") {
					t.Fatalf("reviewer inherited the executing settings: %s", args)
				}
				if executing == "codex" {
					if _, err := os.Stat(codexCapture); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("review reached Codex", err)
					}
				}
				for _, line := range record.Lines {
					var request struct {
						Request struct {
							Servers []string `json:"sdkMcpServers"`
						} `json:"request"`
					}
					if json.Unmarshal([]byte(line), &request) == nil && len(request.Request.Servers) > 0 {
						t.Fatal("review registered JULIA's tools")
					}
				}
			})
		}
	})
	t.Run("missing Claude Code fails closed", func(t *testing.T) {
		cfg := reviewerFactoryConfig(t, "openai", filepath.Join(t.TempDir(), "missing-codex"))
		cfg.ClaudeBinary, cfg.ReviewerModel = filepath.Join(t.TempDir(), "missing-claude"), "claude/sonnet"
		backend, review, err := newBackend(context.Background(), cfg, nil)
		if err != nil || review == nil {
			t.Fatal("missing reviewer prevented API startup", err)
		}
		apiModel := &forbiddenReviewerModel{}
		backend.(*provider.API).Model = apiModel
		decision, err := review(context.Background(), core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}})
		if err == nil || !strings.Contains(err.Error(), "Claude Code CLI was not found") || decision.Allow || apiModel.calls != 0 {
			t.Fatal("missing reviewer did not fail closed independently", decision, err, apiModel.calls)
		}
		cfg.Provider, cfg.Model = "claude", "opus"
		if _, _, err := newBackend(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "Claude Code CLI was not found") {
			t.Fatal("missing Claude Code did not stop the claude provider", err)
		}
	})
}

func TestClaudeCatalogLoginAndDoctorLines(t *testing.T) {
	claudeBinary, _ := claudeProtocolFixture(t)
	codexBinary, _ := reviewerProtocolFixture(t)
	cfg := config.Default()
	cfg.CodexBinary, cfg.ClaudeBinary = codexBinary, claudeBinary
	models, err := cliCatalog(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.Provider+"/"+m.ID)
	}
	if !slices.Contains(ids, "claude/opus") || !slices.Contains(ids, "claude/claude-opus-5-5") {
		t.Fatalf("catalog %v", ids)
	}
	if status := claudeLoginStatus(context.Background(), claudeBinary); status != "signed in · claude.ai" {
		t.Fatalf("login status %q", status)
	}
	if status := claudeLoginStatus(context.Background(), filepath.Join(t.TempDir(), "missing")); !strings.HasPrefix(status, "unknown") {
		t.Fatalf("login status without a binary %q", status)
	}
	cfg.Provider, cfg.Model = "claude", ""
	if model, reasoning := modelSettings(cfg); model != "inherited from Claude Code" || reasoning != "inherited from Claude Code" {
		t.Fatal(model, reasoning)
	}
	if lines := contextSummary(cfg); !strings.Contains(lines[0], "Claude Code compacts its own sessions") {
		t.Fatal(lines)
	}
	cfg.ReviewerModel = "claude/sonnet"
	if got := reviewerSummary(cfg); got != "Reviewer  Claude Code sonnet · reasoning low" {
		t.Fatal(got)
	}
	if err := login(context.Background(), cfg, []string{"openai"}); err == nil || !strings.Contains(err.Error(), "usage: julia login") {
		t.Fatal("login accepted an API provider", err)
	}
}

type claudeCaptureRecord struct {
	Args      []string
	APIKeySet bool
	Lines     []string
}

func readClaudeCapture(t *testing.T, path string) claudeCaptureRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var header struct {
		Args      []string `json:"args"`
		APIKeySet bool     `json:"api_key_set"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	return claudeCaptureRecord{Args: header.Args, APIKeySet: header.APIKeySet, Lines: lines[1:]}
}

// claudeProtocolFixture is a local stand-in for the Claude Code CLI: it
// speaks the stream-json protocol, records its arguments, environment and
// stdin to capture, and never contacts a provider.
func claudeProtocolFixture(t *testing.T) (binary, capture string) {
	t.Helper()
	directory := t.TempDir()
	binary, capture = filepath.Join(directory, "fake-claude"), filepath.Join(directory, "claude.jsonl")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	script := "#!/bin/sh\nexec " + quoted + " -test.run=^TestClaudeProtocolProcess$ -- \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JULIA_TEST_CLAUDE_PROTOCOL", "1")
	t.Setenv("JULIA_TEST_CLAUDE_CAPTURE", capture)
	return binary, capture
}

// TestClaudeProtocolProcess is the fixture's process: a deterministic
// stream-json speaker that answers the control protocol, the MCP handshake
// and one turn, with no inference and no tools of its own.
func TestClaudeProtocolProcess(t *testing.T) {
	if os.Getenv("JULIA_TEST_CLAUDE_PROTOCOL") != "1" {
		return
	}
	os.Exit(runClaudeProtocolProcess())
}

func runClaudeProtocolProcess() int {
	args := os.Args
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[i+1:]
	}
	if slices.Equal(args, []string{"auth", "status"}) {
		os.Stdout.WriteString(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}` + "\n")
		return 0
	}
	capture, err := os.OpenFile(os.Getenv("JULIA_TEST_CLAUDE_CAPTURE"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 1
	}
	defer capture.Close()
	_, keySet := os.LookupEnv("ANTHROPIC_API_KEY")
	header, _ := json.Marshal(map[string]any{"args": args, "api_key_set": keySet})
	capture.Write(append(header, '\n'))
	output := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	tools := []string{}
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	sessionID := flag("--session-id")
	if sessionID == "" {
		sessionID = flag("--resume")
	}
	for scanner.Scan() {
		capture.Write(append(scanner.Bytes(), '\n'))
		var message struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string   `json:"subtype"`
				Servers []string `json:"sdkMcpServers"`
			} `json:"request"`
			Response struct {
				RequestID string `json:"request_id"`
				Response  struct {
					MCP struct {
						Result struct {
							Tools []struct {
								Name string `json:"name"`
							} `json:"tools"`
						} `json:"result"`
					} `json:"mcp_response"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			return 3
		}
		switch message.Type {
		case "control_request":
			respond := func(response any) error {
				return output.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": message.RequestID, "response": response}})
			}
			switch message.Request.Subtype {
			case "initialize":
				if slices.Contains(message.Request.Servers, "julia") {
					// The handshake: the host answers each before the
					// initialize response is sent.
					for _, request := range []map[string]any{{"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25"}}, {"jsonrpc": "2.0", "method": "notifications/initialized"}, {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}} {
						if err := output.Encode(map[string]any{"type": "control_request", "request_id": "mcp-" + request["method"].(string), "request": map[string]any{"subtype": "mcp_message", "server_name": "julia", "message": request}}); err != nil {
							return 5
						}
						if !scanner.Scan() {
							return 6
						}
						capture.Write(append(scanner.Bytes(), '\n'))
						var reply struct {
							Response struct {
								Response struct {
									MCP struct {
										Result struct {
											Tools []struct {
												Name string `json:"name"`
											} `json:"tools"`
										} `json:"result"`
									} `json:"mcp_response"`
								} `json:"response"`
							} `json:"response"`
						}
						if json.Unmarshal(scanner.Bytes(), &reply) != nil {
							return 7
						}
						for _, tool := range reply.Response.Response.MCP.Result.Tools {
							tools = append(tools, "mcp__julia__"+tool.Name)
						}
					}
				}
				if err := respond(map[string]any{"commands": []any{}}); err != nil {
					return 5
				}
			case "list_models":
				if err := respond(map[string]any{"models": []any{map[string]any{"value": "default", "resolvedModel": "claude-opus-5-5", "displayName": "Default", "supportedEffortLevels": []string{"low", "high"}}, map[string]any{"value": "opus", "resolvedModel": "claude-opus-5-5", "displayName": "Opus", "supportedEffortLevels": []string{"low", "high"}}}}); err != nil {
					return 5
				}
				return 0
			case "get_usage":
				if err := respond(map[string]any{"rate_limits_available": true, "rate_limits": map[string]any{"seven_day": map[string]any{"utilization": 25, "resets_at": "2026-11-01T00:00:00Z"}}}); err != nil {
					return 5
				}
				return 0
			default:
				return 4
			}
		case "user":
			if err := output.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID, "tools": tools, "mcp_servers": []any{map[string]any{"name": "julia", "status": "connected", "source": "sdk"}}, "model": "claude-fixture", "apiKeySource": "oauth"}); err != nil {
				return 6
			}
			result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "fixture answer", "num_turns": 1, "usage": map[string]any{"input_tokens": 12, "output_tokens": 3}}
			if flag("--json-schema") != "" {
				result["structured_output"] = map[string]any{"allow": true, "reason": "fixture approved"}
			}
			if err := output.Encode(result); err != nil {
				return 7
			}
			time.Sleep(10 * time.Millisecond)
			return 0
		}
	}
	return 8
}
