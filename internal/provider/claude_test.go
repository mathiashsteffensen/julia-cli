package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

// claudeFixture stands in for a Claude Code process: it records what JULIA
// writes, answers control requests as the CLI would, and plays a scripted
// turn once the user message arrives. Tool calls it issues as MCP requests are
// answered through the same stream, and the answers are kept.
type claudeFixture struct {
	t           *testing.T
	incoming    chan claudeRead
	args        []string
	written     []map[string]json.RawMessage
	users       []map[string]json.RawMessage
	mcpReplies  []map[string]any
	turn        func(*claudeFixture)
	onUser      func(*claudeFixture, int)
	apiKey      string
	tools       []string
	noInit      bool
	initCount   int
	sessionID   string
	models      any
	usage       any
	controlFail map[string]string
}

func newClaudeFixture(t *testing.T) *claudeFixture {
	return &claudeFixture{t: t, incoming: make(chan claudeRead, 256), apiKey: "oauth"}
}

func (f *claudeFixture) push(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	message, err := parseClaudeMessage(data)
	if err != nil {
		f.t.Fatal(err)
	}
	f.incoming <- claudeRead{Message: message}
}

// request relays one MCP JSON-RPC message as Claude Code does.
func (f *claudeFixture) request(id string, message map[string]any) {
	f.push(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "mcp_message", "server_name": "julia", "message": message}})
}

func (f *claudeFixture) init() {
	f.initCount++
	tools := f.tools
	if tools == nil {
		tools = []string{"mcp__julia__Read"}
	}
	sessionID := f.sessionID
	if sessionID == "" {
		sessionID = f.argValue("--session-id")
		if sessionID == "" {
			sessionID = f.argValue("--resume")
		}
	}
	f.push(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID, "tools": tools, "mcp_servers": []any{map[string]any{"name": "julia", "status": "connected", "source": "sdk"}}, "model": "claude-test-5", "apiKeySource": f.apiKey, "capabilities": []string{"interrupt_receipt_v1"}})
}

func (f *claudeFixture) argValue(flag string) string {
	for i, arg := range f.args {
		if arg == flag && i+1 < len(f.args) {
			return f.args[i+1]
		}
	}
	return ""
}

func (f *claudeFixture) text(id, text string) {
	f.push(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{"type": "message_start", "message": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 100, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 10, "output_tokens": 1}}}})
	f.push(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}})
	f.push(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 7}}})
	f.push(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": id, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}, "usage": map[string]any{"input_tokens": 100, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 10, "output_tokens": 7}}})
}

func (f *claudeFixture) finish(isError bool, text string, extra map[string]any) {
	result := map[string]any{"type": "result", "subtype": "success", "is_error": isError, "result": text, "num_turns": 1, "usage": map[string]any{"input_tokens": 150, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 9}}
	for key, value := range extra {
		result[key] = value
	}
	f.push(result)
}

func (f *claudeFixture) start(_ context.Context, _ string, args []string, _ string) (*claudeClient, error) {
	f.args = args
	return &claudeClient{incoming: f.incoming, input: codexTestWriter(func(p []byte) (int, error) {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(p, &message); err != nil {
			return 0, err
		}
		f.written = append(f.written, message)
		var kind string
		_ = json.Unmarshal(message["type"], &kind)
		switch kind {
		case "control_request":
			var request struct {
				Subtype string   `json:"subtype"`
				Servers []string `json:"sdkMcpServers"`
			}
			_ = json.Unmarshal(message["request"], &request)
			var id string
			_ = json.Unmarshal(message["request_id"], &id)
			if reason, failed := f.controlFail[request.Subtype]; failed {
				f.push(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": reason}})
				return len(p), nil
			}
			switch request.Subtype {
			case "initialize":
				if slices.Contains(request.Servers, "julia") {
					f.request("mcp-init", map[string]any{"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code"}}})
					f.request("mcp-ready", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
					f.request("mcp-list", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
				}
				f.push(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{"commands": []any{}}}})
			case "list_models":
				f.push(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": f.models}})
			case "get_usage":
				f.push(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": f.usage}})
			default:
				f.push(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "unexpected " + request.Subtype}})
			}
		case "control_response":
			var response struct {
				Response struct {
					MCP map[string]any `json:"mcp_response"`
				} `json:"response"`
			}
			_ = json.Unmarshal(message["response"], &response)
			if response.Response.MCP != nil {
				f.mcpReplies = append(f.mcpReplies, response.Response.MCP)
			}
		case "user":
			f.users = append(f.users, message)
			if len(f.users) == 1 {
				if !f.noInit {
					f.init()
				}
				if f.turn != nil {
					f.turn(f)
				}
			} else if f.onUser != nil {
				f.onUser(f, len(f.users))
			}
		}
		return len(p), nil
	})}, nil
}

type claudeTestExecutor struct {
	calls []core.ToolCall
	image bool
}

func (e *claudeTestExecutor) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{{Name: "Read", Description: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}
}

func (e *claudeTestExecutor) Execute(_ context.Context, call core.ToolCall) core.ToolResult {
	e.calls = append(e.calls, call)
	if e.image {
		return core.ToolResult{Content: "shot", Images: []core.Image{{MediaType: "image/png", Data: []byte("png-bytes"), Width: 2, Height: 2}}}
	}
	return core.ToolResult{Content: "file content"}
}

func (f *claudeFixture) lastMCPReply() map[string]any {
	if len(f.mcpReplies) == 0 {
		f.t.Fatal("no MCP reply")
	}
	return f.mcpReplies[len(f.mcpReplies)-1]
}

func TestClaudeRunsToolThroughMCPAndCheckpoints(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.text("msg_1", "Checking. ")
		f.request("mcp-call", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "Read", "arguments": map[string]any{"path": "README.md"}, "_meta": map[string]any{"claudecode/toolUseId": "toolu_01", "progressToken": 2}}})
		f.text("msg_2", "Done.")
		f.finish(false, "Done.", nil)
	}
	checkpoints := 0
	backend := &Claude{start: f.start, Model: "sonnet", Reasoning: "high", Checkpoint: func(s *core.Session) error {
		checkpoints++
		if s.RemoteID == "" {
			t.Error("checkpoint before remote ID")
		}
		return nil
	}}
	session := &core.Session{Directory: t.TempDir()}
	executor := &claudeTestExecutor{}
	var events []core.Event
	if err := backend.Run(context.Background(), session, core.Prompt{Text: "Read the readme"}, executor, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 || executor.calls[0].Name != "Read" || string(executor.calls[0].Arguments) != `{"path":"README.md"}` {
		t.Fatalf("executed %+v", executor.calls)
	}
	// The prompt, its acceptance, the tool's intent and result, and the turn.
	if checkpoints != 5 {
		t.Fatalf("checkpoint count %d", checkpoints)
	}
	if session.RemoteID == "" || session.RemoteID != f.argValue("--session-id") || session.Model != "sonnet" {
		t.Fatalf("session metadata %+v", session)
	}
	if len(session.Messages) != 5 || session.Messages[0].Role != "user" || session.Messages[1].Content != "Checking. " || session.Messages[2].ToolCalls[0].ID != "toolu_01" || session.Messages[3].Content != "file content" || session.Messages[4].Content != "Done." {
		t.Fatalf("messages %+v", session.Messages)
	}
	// The MCP handshake and the call were answered on the stream.
	var methods []string
	for _, reply := range f.mcpReplies {
		if result, ok := reply["result"].(map[string]any); ok {
			switch {
			case result["protocolVersion"] != nil:
				methods = append(methods, "initialize")
				if result["protocolVersion"] != "2025-11-25" {
					t.Fatal("protocol version not echoed", result)
				}
			case result["tools"] != nil:
				methods = append(methods, "tools/list")
				tools := result["tools"].([]any)
				if len(tools) != 1 || tools[0].(map[string]any)["name"] != "Read" || tools[0].(map[string]any)["inputSchema"] == nil {
					t.Fatalf("tool list %+v", tools)
				}
			case result["content"] != nil:
				methods = append(methods, "tools/call")
				content := result["content"].([]any)
				if result["isError"] != false || len(content) != 1 || content[0].(map[string]any)["text"] != "file content" {
					t.Fatalf("call result %+v", result)
				}
			default:
				methods = append(methods, "ack")
			}
		}
	}
	if want := []string{"initialize", "ack", "tools/list", "tools/call"}; !slices.Equal(methods, want) {
		t.Fatalf("MCP replies %v", methods)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if want := []string{"status", "status", "text", "usage", "tool_start", "tool_end", "text", "usage"}; !slices.Equal(kinds, want) {
		t.Fatalf("events %v", kinds)
	}
	if usage := session.Usage; usage.Total.Input != 200 || usage.Total.CacheRead != 80 || usage.Total.CacheWrite != 20 || usage.Total.Output != 14 || usage.Context != 157 {
		t.Fatalf("usage %+v", usage)
	}
	if !strings.Contains(strings.Join(f.args, " "), "--tools  --strict-mcp-config --setting-sources  --disable-slash-commands") || f.argValue("--model") != "sonnet" || f.argValue("--effort") != "high" || f.argValue("--allowedTools") != "mcp__julia" || f.argValue("--permission-prompt-tool") != "stdio" || !strings.Contains(f.argValue("--mcp-config"), `"type":"sdk"`) || slices.Contains(f.args, "--no-session-persistence") {
		t.Fatalf("args %q", f.args)
	}
	if !strings.Contains(f.argValue("--system-prompt"), core.SystemPrompt) || !strings.Contains(f.argValue("--system-prompt"), "mcp__julia__Bash") {
		t.Fatal("system prompt lacks the instructions or the tool note")
	}
	var state claudeJournal
	if err := json.Unmarshal(session.State, &state); err != nil || len(state.Calls) != 0 || state.PendingTurn != "" || state.NextCall != 1 {
		t.Fatalf("journal %+v %v", state, err)
	}
}

func TestClaudeSendsImagesBothWays(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.request("mcp-call", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "mcp__julia__Read", "arguments": map[string]any{}}})
		f.finish(false, "", nil)
	}
	executor := &claudeTestExecutor{image: true}
	session := &core.Session{Directory: t.TempDir()}
	image := core.Image{MediaType: "image/jpeg", Data: []byte("jpeg"), Width: 1, Height: 1}
	if err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "look", Images: []core.Image{image}}, executor, nil); err != nil {
		t.Fatal(err)
	}
	var user struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(f.users[0]["message"], &user.Message); err != nil {
		t.Fatal(err)
	}
	if len(user.Message.Content) != 2 || user.Message.Content[0]["text"] != "look" || user.Message.Content[1]["source"].(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("jpeg")) {
		t.Fatalf("user content %+v", user.Message.Content)
	}
	content := f.lastMCPReply()["result"].(map[string]any)["content"].([]any)
	if len(content) != 2 || content[0].(map[string]any)["text"] != "shot" || content[1].(map[string]any)["type"] != "image" || content[1].(map[string]any)["mimeType"] != "image/png" || content[1].(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("png-bytes")) {
		t.Fatalf("MCP image result %+v", content)
	}
	if executor.calls[0].Name != "Read" || string(executor.calls[0].Arguments) != "{}" {
		t.Fatalf("prefix not stripped or arguments missing: %+v", executor.calls)
	}
	if session.Messages[2].Content != "shot\n[image: PNG · 2×2 · 9 B]" || len(session.Messages[0].Images) != 1 {
		t.Fatalf("transcript %+v", session.Messages)
	}
}

func TestClaudeRefusesUnknownToolsAndNativeCapabilities(t *testing.T) {
	t.Run("unadvertised tool", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.turn = func(f *claudeFixture) {
			f.request("mcp-call", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "Bash", "arguments": map[string]any{"command": "rm -rf /"}}})
			f.finish(false, "", nil)
		}
		executor := &claudeTestExecutor{}
		if err := (&Claude{start: f.start}).Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "go"}, executor, nil); err != nil {
			t.Fatal(err)
		}
		if len(executor.calls) != 0 || f.lastMCPReply()["result"].(map[string]any)["isError"] != true {
			t.Fatalf("unknown tool executed or allowed: %+v", f.lastMCPReply())
		}
	})
	t.Run("native tool present", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.tools = []string{"mcp__julia__Read", "Bash"}
		err := (&Claude{start: f.start}).Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "go"}, &claudeTestExecutor{}, nil)
		if err == nil || !strings.Contains(err.Error(), "unexpected native capability (Bash)") {
			t.Fatalf("native tool not rejected: %v", err)
		}
	})
	t.Run("permission prompt denied", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.turn = func(f *claudeFixture) {
			f.push(map[string]any{"type": "control_request", "request_id": "perm", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}}})
			f.finish(false, "", nil)
		}
		if err := (&Claude{start: f.start}).Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "go"}, &claudeTestExecutor{}, nil); err != nil {
			t.Fatal(err)
		}
		denied := false
		for _, message := range f.written {
			var response struct {
				RequestID string `json:"request_id"`
				Response  struct {
					Behavior string `json:"behavior"`
				} `json:"response"`
			}
			if json.Unmarshal(message["response"], &response) == nil && response.RequestID == "perm" {
				denied = response.Response.Behavior == "deny"
			}
		}
		if !denied {
			t.Fatal("permission prompt was not denied")
		}
	})
}

func TestClaudeSignedOut(t *testing.T) {
	// A Claude.ai sign-in holds no API key, so the session's apiKeySource is
	// "none" signed in or out: only the first model call tells. Claude Code
	// holds the prompt by then, so the transcript keeps it too.
	for name, finish := range map[string]func(*claudeFixture){
		"authentication error": func(f *claudeFixture) {
			f.push(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "error": "authentication_failed", "is_api_error_message": true, "message": map[string]any{"id": "x", "content": []any{map[string]any{"type": "text", "text": "Not logged in · Please run /login"}}}})
			f.finish(true, "Not logged in · Please run /login", nil)
		},
		"result alone": func(f *claudeFixture) { f.finish(true, "Not logged in · Please run /login", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newClaudeFixture(t)
			f.apiKey = "none"
			f.turn = finish
			session := &core.Session{Directory: t.TempDir()}
			err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "hello"}, &claudeTestExecutor{}, nil)
			if !errors.Is(err, errClaudeSignedOut) {
				t.Fatalf("error %v", err)
			}
			if len(session.Messages) != 1 || session.RemoteID == "" {
				t.Fatalf("session %+v", session)
			}
		})
	}
	// Signed in, the same "none" opens the session and the turn runs.
	f := newClaudeFixture(t)
	f.apiKey = "none"
	f.turn = func(f *claudeFixture) {
		f.text("msg_1", "Hello.")
		f.finish(false, "Hello.", nil)
	}
	session := &core.Session{Directory: t.TempDir()}
	if err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "hello"}, &claudeTestExecutor{}, nil); err != nil || len(session.Messages) != 2 {
		t.Fatal(err, session.Messages)
	}
}

func TestClaudePromptSavedBeforeSendAndRecovered(t *testing.T) {
	f := newClaudeFixture(t)
	f.noInit = true // the process dies before it opens the session
	f.turn = func(f *claudeFixture) { f.incoming <- claudeRead{Err: io.EOF} }
	session := &core.Session{Directory: t.TempDir(), RemoteID: "11111111-2222-4333-8444-555555555555", State: json.RawMessage(`{"kind":"claude","calls":{}}`)}
	var saved []int
	backend := &Claude{start: f.start, Checkpoint: func(s *core.Session) error { saved = append(saved, len(s.Messages)); return nil }}
	err := backend.Run(context.Background(), session, core.Prompt{Text: "first"}, &claudeTestExecutor{}, nil)
	if err == nil || !strings.Contains(err.Error(), "Claude Code stream") {
		t.Fatalf("error %v", err)
	}
	if len(saved) < 1 || saved[0] != 1 {
		t.Fatalf("prompt not saved before sending: %v", saved)
	}
	if len(session.Messages) != 0 {
		t.Fatal("prompt kept after a refused turn")
	}
	// A crash after Claude Code accepted the prompt keeps it, marked pending,
	// and the next turn repeats it.
	session.Messages = append(session.Messages, core.Message{Role: "user", Content: "first"})
	session.State = json.RawMessage(`{"kind":"claude","calls":{},"pending_turn":"11111111-2222-4333-8444-555555555555"}`)
	g := newClaudeFixture(t)
	g.turn = func(g *claudeFixture) { g.finish(false, "", nil) }
	if err := (&Claude{start: g.start}).Run(context.Background(), session, core.Prompt{Text: "second"}, &claudeTestExecutor{}, nil); err != nil {
		t.Fatal(err)
	}
	var user struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(g.users[0]["message"], &user); err != nil {
		t.Fatal(err)
	}
	if text := user.Content[0]["text"].(string); !strings.Contains(text, "JULIA stopped while sending the previous user request") || !strings.Contains(text, "first") || !strings.HasSuffix(text, "second") {
		t.Fatalf("pending prompt not repeated: %q", text)
	}
	if g.argValue("--resume") != "11111111-2222-4333-8444-555555555555" || g.argValue("--session-id") != "" {
		t.Fatalf("session not resumed: %q", g.args)
	}
	if session.Messages[len(session.Messages)-1].Content != "second" || strings.Contains(string(session.State), "pending_turn") {
		t.Fatalf("session after recovery %+v %s", session.Messages, session.State)
	}
}

func TestClaudeInterruptedToolBlocksTheSession(t *testing.T) {
	session := &core.Session{Directory: t.TempDir(), RemoteID: "s", State: json.RawMessage(`{"kind":"claude","calls":{"call-3":{"call":{"id":"call-3","name":"Bash","arguments":{}}}}}`)}
	err := (&Claude{start: newClaudeFixture(t).start}).Run(context.Background(), session, core.Prompt{Text: "go"}, &claudeTestExecutor{}, nil)
	if err == nil || !strings.Contains(err.Error(), "tool Bash was interrupted") {
		t.Fatalf("error %v", err)
	}
	if _, err := loadClaudeJournal(json.RawMessage(`{"kind":"codex","calls":{}}`)); err == nil || !strings.Contains(err.Error(), "another provider") {
		t.Fatal("accepted another provider's state", err)
	}
}

func TestClaudeSteersMidTurnAndAnswersQueuedMessages(t *testing.T) {
	f := newClaudeFixture(t)
	inbox := core.NewInbox()
	f.turn = func(f *claudeFixture) { f.text("msg_1", "Starting. ") }
	f.onUser = func(f *claudeFixture, count int) {
		// The steer arrived after the model answered: Claude Code queues it
		// and reports one more turn to come with the first result.
		f.finish(false, "Starting.", map[string]any{"queued_turn_count": 1})
		f.text("msg_2", "Tests pass.")
		f.finish(false, "Tests pass.", map[string]any{"queued_turn_count": 0})
	}
	session := &core.Session{Directory: t.TempDir()}
	var events []core.Event
	// The user writes once the first words have shown, as they would.
	emit := func(e core.Event) {
		events = append(events, e)
		if e.Kind == "text" && e.Text == "Starting. " {
			inbox.Send(core.Prompt{Text: "also check the tests"})
		}
	}
	if err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "start", Steering: inbox}, &claudeTestExecutor{}, emit); err != nil {
		t.Fatal(err)
	}
	if len(f.users) != 2 {
		t.Fatalf("user messages %d", len(f.users))
	}
	var priority string
	_ = json.Unmarshal(f.users[1]["priority"], &priority)
	if priority != "now" {
		t.Fatalf("steer priority %q", priority)
	}
	var roles []string
	for _, m := range session.Messages {
		roles = append(roles, m.Role+":"+m.Content)
	}
	if want := []string{"user:start", "assistant:Starting. ", "user:also check the tests", "assistant:Tests pass."}; !slices.Equal(roles, want) {
		t.Fatalf("transcript %v", roles)
	}
	steered := false
	for _, e := range events {
		steered = steered || (e.Kind == "steer" && e.Text == "also check the tests")
	}
	if !steered || len(inbox.Take()) != 0 {
		t.Fatal("steer not reported or left waiting")
	}
}

func TestClaudeFailedTurnAndCheckpointFailure(t *testing.T) {
	t.Run("failed turn", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.turn = func(f *claudeFixture) {
			f.push(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []string{"model_not_found: no such model"}})
		}
		session := &core.Session{Directory: t.TempDir()}
		err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "hello"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "model_not_found") {
			t.Fatalf("error %v", err)
		}
		if len(session.Messages) != 1 {
			t.Fatal("an accepted prompt must stay in the transcript")
		}
	})
	t.Run("checkpoint failure", func(t *testing.T) {
		f := newClaudeFixture(t)
		err := (&Claude{start: f.start, Checkpoint: func(*core.Session) error { return errors.New("disk full") }}).Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "hello"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("error %v", err)
		}
		if len(f.users) != 0 {
			t.Fatal("sent the prompt after persistence failed")
		}
	})
	t.Run("initialize refused", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.controlFail = map[string]string{"initialize": "no"}
		err := (&Claude{start: f.start}).Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "hello"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "initialize Claude Code") {
			t.Fatalf("error %v", err)
		}
	})
}

func TestClaudeUsageFallsBackToTheResult(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.push(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "m", "content": []any{map[string]any{"type": "text", "text": "Plain."}}}})
		f.finish(false, "Plain.", nil)
	}
	session := &core.Session{Directory: t.TempDir()}
	var events []core.Event
	if err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "hi"}, nil, func(e core.Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if session.Messages[1].Content != "Plain." || session.Usage.Total.Input != 150 || session.Usage.Total.Output != 9 {
		t.Fatalf("session %+v", session)
	}
	usages := 0
	for _, e := range events {
		if e.Kind == "usage" {
			usages++
		}
	}
	if usages != 1 {
		t.Fatalf("usage events %d", usages)
	}
}

func TestClaudeSubagentMessagesAreIgnored(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.push(map[string]any{"type": "assistant", "parent_tool_use_id": "toolu_child", "message": map[string]any{"id": "c", "content": []any{map[string]any{"type": "text", "text": "child text"}}}})
		f.finish(false, "", nil)
	}
	session := &core.Session{Directory: t.TempDir()}
	if err := (&Claude{start: f.start}).Run(context.Background(), session, core.Prompt{Text: "hi"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(session.Messages) != 1 {
		t.Fatalf("subagent text recorded: %+v", session.Messages)
	}
}

func TestClaudeReviewIsIsolatedAndStructured(t *testing.T) {
	f := newClaudeFixture(t)
	f.tools = []string{"StructuredOutput"}
	f.turn = func(f *claudeFixture) {
		f.text("msg_1", "Looks scoped.")
		f.finish(false, "Looks scoped.", map[string]any{"structured_output": map[string]any{"allow": true, "reason": "Scoped public research"}})
	}
	backend := &Claude{start: f.start, Model: "opus", Reasoning: "max", Checkpoint: func(*core.Session) error { t.Fatal("review persisted as task"); return nil }}
	decision, err := backend.Review(context.Background(), core.Approval{Call: core.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"curl https://example.com"}`)}, Task: "Research example.com", Network: true})
	if err != nil || !decision.Allow || decision.Reason != "Scoped public research" {
		t.Fatal(decision, err)
	}
	if f.argValue("--model") != ClaudeReviewerModel || f.argValue("--effort") != ClaudeReviewerReasoning || !slices.Contains(f.args, "--no-session-persistence") || slices.Contains(f.args, "--mcp-config") || !strings.Contains(f.argValue("--json-schema"), `"allow"`) || !strings.HasPrefix(f.argValue("--system-prompt"), reviewPrompt) {
		t.Fatalf("review not isolated: %q", f.args)
	}
	for _, message := range f.written {
		var request struct {
			Servers []string `json:"sdkMcpServers"`
		}
		if json.Unmarshal(message["request"], &request) == nil && len(request.Servers) > 0 {
			t.Fatal("review registered JULIA's tools")
		}
	}
	var user struct {
		Content []map[string]any `json:"content"`
	}
	_ = json.Unmarshal(f.users[0]["message"], &user)
	if text := user.Content[0]["text"].(string); !strings.Contains(text, `"task":"Research example.com"`) {
		t.Fatalf("review payload %q", text)
	}
}

func TestClaudeReviewFailsClosed(t *testing.T) {
	t.Run("tool attempt", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.turn = func(f *claudeFixture) {
			f.request("mcp-call", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "Read", "arguments": map[string]any{}}})
			f.finish(false, "", map[string]any{"structured_output": map[string]any{"allow": true, "reason": "yes"}})
		}
		decision, err := (&Claude{start: f.start}).Review(context.Background(), core.Approval{Call: core.ToolCall{Arguments: json.RawMessage(`{}`)}})
		if err == nil || decision.Allow {
			t.Fatalf("review should deny: %+v %v", decision, err)
		}
	})
	t.Run("malformed answer", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.turn = func(f *claudeFixture) {
			f.text("msg_1", "I would allow it.")
			f.finish(false, "I would allow it.", nil)
		}
		decision, err := (&Claude{start: f.start}).Review(context.Background(), core.Approval{Call: core.ToolCall{Arguments: json.RawMessage(`{}`)}})
		if err == nil || decision.Allow {
			t.Fatalf("review should deny: %+v %v", decision, err)
		}
	})
	t.Run("configured reviewer model", func(t *testing.T) {
		f := newClaudeFixture(t)
		f.turn = func(f *claudeFixture) {
			f.finish(false, "", map[string]any{"structured_output": map[string]any{"allow": false, "reason": "no"}})
		}
		if _, err := (&Claude{start: f.start, ReviewModel: "opus"}).Review(context.Background(), core.Approval{Call: core.ToolCall{Arguments: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		if f.argValue("--model") != "opus" {
			t.Fatalf("reviewer model %q", f.args)
		}
	})
}

func TestClaudeCompleteReturnsStructuredOutputAndSpend(t *testing.T) {
	f := newClaudeFixture(t)
	f.tools = []string{"StructuredOutput"}
	f.turn = func(f *claudeFixture) {
		f.text("msg_1", "thinking out loud")
		f.finish(false, "thinking out loud", map[string]any{"structured_output": map[string]any{"notes": []any{"remember this"}}})
	}
	answer, spent, err := (&Claude{start: f.start, Model: "sonnet"}).Complete(context.Background(), "Reflect.", "turn transcript", map[string]any{"type": "object"})
	if err != nil || answer != `{"notes":["remember this"]}` {
		t.Fatal(answer, err)
	}
	if spent.Output != 7 || spent.CacheRead != 40 {
		t.Fatalf("spent %+v", spent)
	}
	if f.argValue("--effort") != "low" || !strings.HasPrefix(f.argValue("--system-prompt"), "Reflect.") || !strings.Contains(f.argValue("--system-prompt"), "isolated step") {
		t.Fatalf("complete args %q", f.args)
	}
	g := newClaudeFixture(t)
	g.turn = func(g *claudeFixture) {
		g.text("msg_1", "plain words")
		g.finish(false, "plain words", nil)
	}
	if answer, _, err := (&Claude{start: g.start}).Complete(context.Background(), "Reflect.", "turn", nil); err != nil || answer != "plain words" {
		t.Fatal(answer, err)
	}
}

func TestClaudeStructuredOutputCapabilityIsScoped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tools  []string
		schema any
		run    bool
		want   string
	}{
		{name: "conversation", tools: []string{"StructuredOutput"}, run: true, want: "StructuredOutput"},
		{name: "plain side call", tools: []string{"StructuredOutput"}, want: "StructuredOutput"},
		{name: "native tool alongside helper", tools: []string{"StructuredOutput", "Bash"}, schema: map[string]any{"type": "object"}, want: "Bash"},
		{name: "similar name", tools: []string{"StructuredOutputExtra"}, schema: map[string]any{"type": "object"}, want: "StructuredOutputExtra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaudeFixture(t)
			f.tools = tc.tools
			backend := &Claude{start: f.start}
			var err error
			if tc.run {
				err = backend.Run(context.Background(), &core.Session{Directory: t.TempDir()}, core.Prompt{Text: "go"}, &claudeTestExecutor{}, nil)
			} else {
				_, _, err = backend.Complete(context.Background(), "Reflect.", "turn", tc.schema)
			}
			if err == nil || !strings.Contains(err.Error(), "unexpected native capability ("+tc.want+")") {
				t.Fatalf("unexpected capability not rejected: %v", err)
			}
		})
	}
}

func TestClaudeModelsAndWeeklyUsage(t *testing.T) {
	f := newClaudeFixture(t)
	f.models = map[string]any{"models": []any{
		map[string]any{"value": "default", "resolvedModel": "claude-opus-5-5", "displayName": "Default (recommended)", "description": "Use the default model", "supportedEffortLevels": []string{"low", "medium", "high", "xhigh", "max"}},
		map[string]any{"value": "opus", "resolvedModel": "claude-opus-5-5", "displayName": "Opus", "description": "Opus 5.5", "supportedEffortLevels": []string{"low", "high", "high"}},
		map[string]any{"value": "haiku", "resolvedModel": "claude-haiku-5-5", "displayName": "Haiku"},
	}}
	f.usage = map[string]any{"rate_limits_available": true, "subscription_type": "max", "rate_limits": map[string]any{"five_hour": map[string]any{"utilization": 12, "resets_at": "2026-10-27T20:00:00Z"}, "seven_day": map[string]any{"utilization": 39.5, "resets_at": "2026-10-30T07:00:00Z"}}}
	backend := &Claude{start: f.start}
	models, err := backend.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.Provider+"/"+m.ID)
	}
	if want := []string{"claude/claude-opus-5-5", "claude/opus", "claude/haiku"}; !slices.Equal(ids, want) || !models[0].IsDefault || models[1].IsDefault || !slices.Equal(models[1].ReasoningEfforts, []string{"low", "high"}) || models[2].Name != "Haiku" {
		t.Fatalf("models %+v", models)
	}
	if !slices.Contains(f.args, "--no-session-persistence") || len(f.users) != 0 {
		t.Fatal("catalog lookup started a conversation", f.args)
	}
	usage, err := backend.WeeklyUsage(context.Background())
	if err != nil || usage == nil || usage.RemainingPercent != 60.5 || usage.ResetsAt.UTC().Format(time.RFC3339) != "2026-10-30T07:00:00Z" {
		t.Fatalf("usage %+v %v", usage, err)
	}
	if usage, err := parseClaudeWeeklyUsage([]byte(`{"rate_limits_available":false,"rate_limits":null}`)); err != nil || usage != nil {
		t.Fatal("unavailable usage must be nil", usage, err)
	}
	if _, err := parseClaudeModels([]byte(`{"models":[]}`)); err == nil {
		t.Fatal("empty catalog accepted")
	}
}

func TestClaudeEnvironmentKeepsAPIKeysOut(t *testing.T) {
	env := claudeEnvironment([]string{"HOME=/h", "ANTHROPIC_API_KEY=sk-live", "ANTHROPIC_AUTH_TOKEN=tok", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "ANTHROPIC_BASE_URL=https://proxy.example"})
	if want := []string{"HOME=/h", "ANTHROPIC_BASE_URL=https://proxy.example"}; !slices.Equal(env, want) {
		t.Fatalf("environment %v", env)
	}
	id, err := newUUID()
	if err != nil || len(id) != 36 || id[14] != '4' {
		t.Fatal(id, err)
	}
}

func TestClaudeClientQueuesTurnMessagesDuringCalls(t *testing.T) {
	f := newClaudeFixture(t)
	client, _ := f.start(context.Background(), "", nil, "")
	// A turn message that arrives while a control request is in flight waits
	// in order for the turn's loop; the response is matched by request ID.
	f.push(map[string]any{"type": "system", "subtype": "status", "status": "requesting"})
	f.models = map[string]any{"models": []any{}}
	if _, err := client.call(context.Background(), map[string]any{"subtype": "list_models"}, nil); err != nil {
		t.Fatal(err)
	}
	message, _, err := client.nextOr(context.Background(), nil)
	if err != nil || message.Type != "system" || message.Subtype != "status" {
		t.Fatalf("queued message lost: %+v %v", message, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.nextOr(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not honored", err)
	}
}

func TestClaudeContextWindowReporting(t *testing.T) {
	for _, tc := range []struct {
		name           string
		streamModel    string
		assistantModel string
		models         map[string]any
		initialWindow  int
		wantWindow     int
		wantEvent      bool
	}{
		{
			name:        "stream model resolves alias and ignores background",
			streamModel: "claude-opus-actual", initialWindow: 100_000, wantWindow: 200_000, wantEvent: true,
			models: map[string]any{"claude-opus-actual": map[string]any{"contextWindow": 200_000}, "claude-haiku-background": map[string]any{"contextWindow": 1_000_000}},
		},
		{
			name:        "assistant model overrides init and stream",
			streamModel: "claude-old", assistantModel: "claude-opus-actual", wantWindow: 200_000, wantEvent: true,
			models: map[string]any{"claude-opus-actual": map[string]any{"contextWindow": 200_000}, "claude-old": map[string]any{"contextWindow": 50_000}},
		},
		{
			name: "init model fallback", wantWindow: 180_000, wantEvent: true,
			models: map[string]any{"claude-test-5": map[string]any{"contextWindow": 180_000}, "claude-haiku-background": map[string]any{"contextWindow": 1_000_000}},
		},
		{
			name: "unknown model does not select another", streamModel: "unknown", initialWindow: 90_000, wantWindow: 90_000,
			models: map[string]any{"claude-test-5": map[string]any{"contextWindow": 180_000}, "claude-haiku-background": map[string]any{"contextWindow": 1_000_000}},
		},
		{
			name: "missing window", streamModel: "claude-opus-actual", initialWindow: 90_000, wantWindow: 90_000,
			models: map[string]any{"claude-opus-actual": map[string]any{}},
		},
		{
			name: "negative window", streamModel: "claude-opus-actual", initialWindow: 90_000, wantWindow: 90_000,
			models: map[string]any{"claude-opus-actual": map[string]any{"contextWindow": -1}},
		},
		{
			name: "zero window", streamModel: "claude-opus-actual", initialWindow: 90_000, wantWindow: 90_000,
			models: map[string]any{"claude-opus-actual": map[string]any{"contextWindow": 0}},
		},
		{
			name: "invalid window", streamModel: "claude-opus-actual", initialWindow: 90_000, wantWindow: 90_000,
			models: map[string]any{"claude-opus-actual": map[string]any{"contextWindow": "unknown"}},
		},
		{
			name: "missing model usage", streamModel: "claude-opus-actual", initialWindow: 90_000, wantWindow: 90_000,
		},
		{
			name: "unchanged window", streamModel: "claude-opus-actual", initialWindow: 200_000, wantWindow: 200_000,
			models: map[string]any{"claude-opus-actual": map[string]any{"contextWindow": 200_000}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaudeFixture(t)
			f.turn = func(f *claudeFixture) {
				f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": tc.streamModel, "usage": map[string]any{"input_tokens": 100}}}})
				f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "hello"}}})
				f.push(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 7}}})
				f.push(map[string]any{"type": "assistant", "message": map[string]any{"id": "m", "model": tc.assistantModel}})
				f.push(map[string]any{"type": "assistant", "parent_tool_use_id": "child", "message": map[string]any{"id": "child", "model": "claude-haiku-background"}})
				f.finish(false, "", map[string]any{"modelUsage": tc.models})
			}
			session := &core.Session{Directory: t.TempDir(), Model: "opus"}
			session.Usage.ContextWindow = tc.initialWindow
			var lastCheckpoint core.SessionUsage
			var events []core.Event
			backend := &Claude{start: f.start, Checkpoint: func(s *core.Session) error { lastCheckpoint = s.Usage; return nil }}
			if err := backend.Run(context.Background(), session, core.Prompt{Text: "hi"}, nil, func(e core.Event) { events = append(events, e) }); err != nil {
				t.Fatal(err)
			}
			if session.Usage.ContextWindow != tc.wantWindow || lastCheckpoint.ContextWindow != tc.wantWindow {
				t.Errorf("window session=%d checkpoint=%d, want %d", session.Usage.ContextWindow, lastCheckpoint.ContextWindow, tc.wantWindow)
			}
			if session.Usage.Total.Input != 100 || session.Usage.Total.Output != 7 || session.Usage.Context != 107 {
				t.Errorf("stream usage changed: %+v", session.Usage)
			}
			metadata := 0
			for _, event := range events {
				if event.Kind == "usage" && event.ContextWindow != 0 {
					metadata++
					if event.ContextWindow != tc.wantWindow || event.Usage() != (core.TokenUsage{}) || event.ContextTokens != 0 || event.Elapsed != 0 || event.Aggregate {
						t.Errorf("metadata event carries other usage: %+v", event)
					}
				}
			}
			if (metadata == 1) != tc.wantEvent || metadata > 1 {
				t.Errorf("metadata events=%d want event=%v", metadata, tc.wantEvent)
			}
		})
	}
}

func TestClaudeContextWindowResultFallbackIsCheckpointed(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.push(map[string]any{"type": "assistant", "message": map[string]any{"id": "m", "model": "claude-resolved"}})
		f.finish(false, "", map[string]any{"modelUsage": map[string]any{"claude-resolved": map[string]any{"contextWindow": 200_000}}})
	}
	var checkpoint core.SessionUsage
	session := &core.Session{Directory: t.TempDir(), Model: "sonnet"}
	backend := &Claude{start: f.start, Checkpoint: func(s *core.Session) error { checkpoint = s.Usage; return nil }}
	if err := backend.Run(context.Background(), session, core.Prompt{Text: "hi"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if checkpoint != session.Usage || checkpoint.ContextWindow != 200_000 || checkpoint.Total.Input != 150 || checkpoint.Total.Output != 9 || checkpoint.Context != 159 {
		t.Fatalf("checkpoint=%+v session=%+v", checkpoint, session.Usage)
	}
}

func TestClaudeContextWindowSideCallsAreIsolated(t *testing.T) {
	f := newClaudeFixture(t)
	f.turn = func(f *claudeFixture) {
		f.push(map[string]any{"type": "assistant", "message": map[string]any{"id": "m", "model": "claude-resolved"}})
		f.finish(false, "", map[string]any{"modelUsage": map[string]any{"claude-resolved": map[string]any{"contextWindow": 200_000}}})
	}
	session := &core.Session{Directory: t.TempDir()}
	session.Usage.ContextWindow = 90_000
	// Review and Complete both run through this isolated side-call path.
	side := &claudeSideCall{model: "sonnet"}
	var events []core.Event
	backend := &Claude{start: f.start, Checkpoint: func(*core.Session) error {
		t.Fatal("side call checkpointed")
		return nil
	}}
	if err := backend.run(context.Background(), session, core.Prompt{Text: "hi"}, nil, func(e core.Event) { events = append(events, e) }, side, nil); err != nil {
		t.Fatal(err)
	}
	if session.Usage.ContextWindow != 90_000 || session.Usage.Total != (core.TokenUsage{}) || side.spent.Input != 150 || side.spent.Output != 9 {
		t.Fatalf("session usage=%+v side spend=%+v", session.Usage, side.spent)
	}
	for _, event := range events {
		if event.Kind == "usage" {
			t.Fatalf("side call emitted usage: %+v", event)
		}
	}
}
