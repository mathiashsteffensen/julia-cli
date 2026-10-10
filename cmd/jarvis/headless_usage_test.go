package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jarvis/internal/core"
	"jarvis/internal/session"
)

// tokenAPI is a scripted OpenAI-compatible endpoint. The conversation it serves
// is three requests: a streamed tool call whose prompt is large enough to cross
// the compaction threshold, the non-streamed summary that compaction asks for,
// and a streamed final answer.
type tokenAPI struct {
	mu       sync.Mutex
	requests []bool // whether each request asked for a stream
}

func (a *tokenAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	a.mu.Lock()
	a.requests = append(a.requests, request.Stream)
	n := len(a.requests)
	a.mu.Unlock()
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"summary","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"Summary: listed the workspace."},"finish_reason":"stop"}],"usage":{"prompt_tokens":31000,"completion_tokens":200,"total_tokens":31200,"prompt_tokens_details":{"cached_tokens":0}}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(data string) { fmt.Fprintf(w, "data: %s\n\n", data) }
	const head = `{"id":"c","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,`
	if n == 1 {
		chunk(head + `"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"List","arguments":"{\"path\":\".\"}"}}]},"finish_reason":null}]}`)
		chunk(head + `"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30000,"completion_tokens":20,"total_tokens":30020,"prompt_tokens_details":{"cached_tokens":24000}}}`)
	} else {
		chunk(head + `"delta":{"role":"assistant","content":"All done."},"finish_reason":null}]}`)
		chunk(head + `"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":900,"completion_tokens":10,"total_tokens":910,"prompt_tokens_details":{"cached_tokens":0}}}`)
	}
	chunk("[DONE]")
}

// What tokenAPI reports, normalized: the first call's 30000-token prompt is
// 24000 cached and 6000 new; the summary and the answer are uncached.
var tokenAPITotal = core.TokenUsage{Input: 6_000 + 31_000 + 900, CacheRead: 24_000, Output: 20 + 200 + 10}

// runHeadless runs one --prompt task against the scripted API with a 32k window,
// whose compaction point (27.2k) the first reply's prompt already exceeds. It
// returns what the process wrote to stdout and stderr and the saved session.
func runHeadless(t *testing.T, extra ...string) (stdout, stderr string, saved *core.Session, api *tokenAPI) {
	t.Helper()
	api = &tokenAPI{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	dir, state := t.TempDir(), t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("JARVIS_TEST_KEY", "fixture-key")
	path := filepath.Join(t.TempDir(), "config.json")
	config := fmt.Sprintf(`{"provider":"compatible","model":"test-model","base_url":%q,"api_key_env":"JARVIS_TEST_KEY","mode":"read-only","max_steps":8,"context_tokens":32000,"skill_paths":[]}`, server.URL+"/v1")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	errs, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	defer errs.Close()
	oldArgs, oldStdout, oldStderr := os.Args, os.Stdout, os.Stderr
	defer func() { os.Args, os.Stdout, os.Stderr = oldArgs, oldStdout, oldStderr }()
	os.Stdout, os.Stderr = out, errs
	os.Args = append([]string{"jarvis", "--config", path, "--dir", dir}, append(extra, "--prompt", "list the workspace")...)
	runErr := run()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	read := func(f *os.File) string {
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	stdout, stderr = read(out), read(errs)
	if runErr != nil {
		t.Fatalf("headless run failed: %v\nstderr:\n%s", runErr, stderr)
	}
	store := &session.Store{Directory: filepath.Join(state, "jarvis", "sessions")}
	defer store.Close()
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("saved sessions: %v %v", list, err)
	}
	saved, err = store.Load(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return stdout, stderr, saved, api
}

func requireCompactedConversation(t *testing.T, api *tokenAPI) {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	if fmt.Sprint(api.requests) != "[true false true]" {
		t.Fatalf("requests (stream flags) = %v, want the tool call, then the compaction summary, then the answer: context_tokens must reach the compaction check", api.requests)
	}
}

func TestHeadlessRunSummarizesSessionTokens(t *testing.T) {
	stdout, stderr, saved, api := runHeadless(t)
	requireCompactedConversation(t, api)
	if !strings.Contains(stdout, "All done.") {
		t.Fatalf("stdout = %q", stdout)
	}
	// 6000+31000+900 new, 24000 cached: 61.9k sent in all.
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	last2 := lines[len(lines)-2:]
	if last2[0] != "Session: "+saved.ID || last2[1] != "Tokens: 61.9k in (24k cached) · 230 out" {
		t.Fatalf("stderr summary = %q", last2)
	}
	if saved.Usage.Total != tokenAPITotal {
		t.Fatalf("saved total = %+v, want %+v (compaction call included)", saved.Usage.Total, tokenAPITotal)
	}
	// Compaction shrank the context, then the answer's own prompt and reply re-measured it.
	if saved.Usage.Context != 910 {
		t.Fatalf("saved context = %d, want 910", saved.Usage.Context)
	}
}

func TestHeadlessJSONUsageEventsMatchSavedSession(t *testing.T) {
	stdout, stderr, saved, api := runHeadless(t, "--json")
	requireCompactedConversation(t, api)
	if strings.Contains(stderr, "Tokens:") {
		t.Fatalf("--json must keep the stderr summary out of the way: %q", stderr)
	}
	var streamed core.Session
	var usage int
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var event core.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("bad event line %q: %v", scanner.Text(), err)
		}
		if event.Kind == "usage" {
			usage++
			streamed.RecordUsage(event)
		}
	}
	// One event per model call: the tool-call step, the compaction summary and the answer.
	if usage != 3 {
		t.Fatalf("%d usage events, want 3:\n%s", usage, stdout)
	}
	if streamed.Usage != saved.Usage || streamed.Usage.Total != tokenAPITotal {
		t.Fatalf("streamed %+v, saved %+v, want total %+v", streamed.Usage, saved.Usage, tokenAPITotal)
	}
}
