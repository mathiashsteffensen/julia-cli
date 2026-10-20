package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"julia/agents"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/provider"
	"julia/internal/session"
)

// childSpend scripts what one delegated task's model calls cost and how it ends.
type childSpend struct {
	usage   core.TokenUsage
	context int
	err     error
}

// usageBackend records usage the way a real provider does: into the session it
// was given and onto the event stream. The parent makes one model call, then runs
// the Subagent tool, then notes what a provider checkpoint after the tool result
// would have persisted.
type usageBackend struct {
	parent        core.TokenUsage
	parentContext int
	children      map[string]childSpend
	persisted     core.SessionUsage
}

func (b *usageBackend) Name() string { return "fake" }
func (b *usageBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, emit core.Emit) error {
	call := func(u core.TokenUsage, contextTokens int) {
		event := core.UsageEvent(u, contextTokens)
		s.RecordUsage(event)
		emit(event)
	}
	if s.ParentID != "" {
		spend := b.children[prompt.Text]
		call(spend.usage, spend.context)
		s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt.Text}, core.Message{Role: "assistant", Content: "report for " + prompt.Text})
		return spend.err
	}
	call(b.parent, b.parentContext)
	if len(b.children) == 0 {
		b.persisted = s.Usage
		return nil
	}
	var tasks []map[string]string
	for task := range b.children {
		tasks = append(tasks, map[string]string{"role": "delegate", "task": task})
	}
	arguments, _ := json.Marshal(map[string]any{"tasks": tasks})
	result := executor.Execute(ctx, core.ToolCall{ID: "delegate", Name: "Subagent", Arguments: arguments})
	if result.IsError {
		return errors.New(result.Content)
	}
	b.persisted = s.Usage
	return nil
}

func sumUsage(usages ...core.TokenUsage) core.TokenUsage {
	var total core.TokenUsage
	for _, u := range usages {
		total.Add(u)
	}
	return total
}

func TestSubagentSpendRollsUpIntoParentSessionOnce(t *testing.T) {
	parent := core.TokenUsage{Input: 1_000, CacheRead: 9_000, CacheWrite: 500, Output: 120}
	one := core.TokenUsage{Input: 10, CacheRead: 20, CacheWrite: 5, Output: 7}
	two := core.TokenUsage{Input: 4_000, CacheRead: 12_000, Output: 900}
	three := core.TokenUsage{Input: 3, Output: 1}
	failed := core.TokenUsage{Input: 700, CacheRead: 300, Output: 50}
	for _, tt := range []struct {
		name     string
		children map[string]childSpend
		rollups  int // usage events beyond the parent's own call
	}{
		{"every child succeeds", map[string]childSpend{"task one": {usage: one, context: 300}, "task two": {usage: two, context: 17_000}, "task three": {usage: three, context: 4}}, 3},
		{"a failed child was still billed", map[string]childSpend{"task one": {usage: one, context: 300}, "task two": {usage: failed, context: 900, err: errors.New("child gave up")}}, 2},
		{"a child that made no model call adds nothing", map[string]childSpend{"task one": {usage: one, context: 300}, "task two": {}}, 1},
		{"only idle children", map[string]childSpend{"task one": {}}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := &usageBackend{parent: parent, parentContext: 1_600, children: tt.children}
			cfg := config.Default()
			cfg.SkillPaths = []string{}
			var mu sync.Mutex
			saved := map[string]core.Session{}
			runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: 10 * time.Second, parent: context.Background(),
				makeBackend: func(context.Context, config.Config) (core.Backend, core.Approver, error) { return backend, nil, nil },
				checkpoint:  func(s *core.Session) error { mu.Lock(); defer mu.Unlock(); saved[s.ID] = *s; return nil },
				roles:       agents.Builtin(),
			}
			s := session.New(t.TempDir(), "fake", "model")
			// A child's own usage events are marked as the child's; a UI
			// that sums the parent's events must skip them, since the
			// roll-up the parent emits when the child finishes carries them.
			var usage []core.Event
			if err := runner.Run(context.Background(), s, core.Prompt{Text: "delegate this"}, nil, func(event core.Event) {
				if event.Kind == "usage" && event.Subagent == nil {
					usage = append(usage, event)
				}
			}); err != nil {
				t.Fatal(err)
			}
			var childTotals []core.TokenUsage
			for _, child := range saved {
				childTotals = append(childTotals, child.Usage.Total)
			}
			if len(childTotals) != len(tt.children) {
				t.Fatalf("%d children saved, want %d", len(childTotals), len(tt.children))
			}
			want := sumUsage(append(childTotals, parent)...)
			if s.Usage.Total != want {
				t.Fatalf("parent total = %+v, want its own call plus every child: %+v", s.Usage.Total, want)
			}
			if s.Usage.Context != 1_600 {
				t.Fatalf("parent context = %d, want the parent's own 1600: a child's conversation is not part of it", s.Usage.Context)
			}
			if backend.persisted != s.Usage {
				t.Fatalf("usage at the tool-result checkpoint %+v differs from the final %+v: the roll-up would not be saved", backend.persisted, s.Usage)
			}
			// What the stream carried must add up to what the session holds, so a UI summing events agrees with the saved totals.
			if len(usage) != 1+tt.rollups {
				t.Fatalf("%d usage events reached the parent stream, want %d", len(usage), 1+tt.rollups)
			}
			var streamed core.Session
			for _, event := range usage {
				streamed.RecordUsage(event)
			}
			if streamed.Usage.Total != want || streamed.Usage.Context != 1_600 {
				t.Fatalf("stream carried %+v, session holds %+v", streamed.Usage, s.Usage)
			}
			rollups := 0
			for _, event := range usage {
				if event.ContextTokens != 0 {
					if event.Aggregate {
						t.Fatalf("a single model call was marked as a roll-up: %+v", event)
					}
					continue
				}
				rollups++
				if event.Usage().IsZero() {
					t.Fatalf("empty roll-up event emitted: %+v", event)
				}
				if !event.Aggregate {
					t.Fatalf("a roll-up is not one model call and must say so: %+v", event)
				}
			}
			if rollups != tt.rollups {
				t.Fatalf("%d roll-up events, want %d", rollups, tt.rollups)
			}
		})
	}
}

// emitOnlyBackend reports usage on the stream but never records it, as a backend
// that forgot to would. The runtime's emit wrapper must not paper over that.
type emitOnlyBackend struct{ events []core.Event }

func (b *emitOnlyBackend) Name() string { return "fake" }
func (b *emitOnlyBackend) Run(_ context.Context, _ *core.Session, _ core.Prompt, _ core.ToolExecutor, emit core.Emit) error {
	for _, event := range b.events {
		emit(event)
	}
	return nil
}

func TestRuntimeEmitWrapperForwardsUsageWithoutRecordingIt(t *testing.T) {
	events := []core.Event{core.UsageEvent(core.TokenUsage{Input: 100, CacheRead: 900, Output: 30}, 1_030), {Kind: "status", Text: "working"}}
	cfg := config.Default()
	cfg.SkillPaths = []string{}
	runner := &runtimeBackend{backend: &emitOnlyBackend{events: events}, cfg: cfg, timeout: 10 * time.Second, parent: context.Background()}
	s := session.New(t.TempDir(), "fake", "model")
	var seen []core.Event
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "task"}, nil, func(event core.Event) { seen = append(seen, event) }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != events[0] {
		t.Fatalf("usage event not forwarded unchanged: %+v", seen)
	}
	if s.Usage != (core.SessionUsage{}) {
		t.Fatalf("the wrapper recorded usage the provider owns: %+v", s.Usage)
	}
	for _, entry := range s.Audit {
		if entry.Event.Kind == "usage" {
			t.Fatal("usage events do not belong in the audit trail")
		}
	}
}

func TestRuntimeProviderRecordedUsageIsCountedOnce(t *testing.T) {
	u := core.TokenUsage{Input: 100, CacheRead: 900, Output: 30}
	child := core.TokenUsage{Input: 40, CacheRead: 160, Output: 8}
	for _, withSubagents := range []bool{false, true} {
		backend := &usageBackend{parent: u, parentContext: 1_030}
		want := u
		if withSubagents {
			// The Subagent tool really runs: the parent's call is still counted once, plus the child's.
			backend.children = map[string]childSpend{"inspect": {usage: child, context: 200}}
			want = sumUsage(u, child)
		}
		cfg := config.Default()
		cfg.SkillPaths = []string{}
		runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: 10 * time.Second, parent: context.Background()}
		if withSubagents {
			runner.makeBackend = func(context.Context, config.Config) (core.Backend, core.Approver, error) { return backend, nil, nil }
			runner.roles = agents.Builtin()
		}
		s := session.New(t.TempDir(), "fake", "model")
		if err := runner.Run(context.Background(), s, core.Prompt{Text: "task"}, nil, func(core.Event) {}); err != nil {
			t.Fatal(err)
		}
		if s.Usage.Total != want || s.Usage.Context != 1_030 {
			t.Fatalf("subagents=%v: usage %+v, want total %+v and the parent's own context", withSubagents, s.Usage, want)
		}
	}
}

func TestAPIBackendReceivesConfiguredContextWindow(t *testing.T) {
	for _, tokens := range []int{core.DefaultContextWindow, 123_456} {
		cfg := reviewerFactoryConfig(t, "openai", "")
		cfg.ContextTokens = tokens
		backend, _, err := newBackend(context.Background(), cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		api, ok := backend.(*provider.API)
		if !ok || api.ContextTokens != tokens {
			t.Fatalf("API backend context window = %+v, want %d", backend, tokens)
		}
	}
}

func TestTokenSummary(t *testing.T) {
	for _, tt := range []struct {
		name  string
		usage core.TokenUsage
		want  string
	}{
		{"nothing yet", core.TokenUsage{}, "0 in · 0 out"},
		{"no cache", core.TokenUsage{Input: 500, Output: 20}, "500 in · 20 out"},
		{"in is the whole prompt, cached is its cache-read share", core.TokenUsage{Input: 100_000, CacheRead: 1_300_000, Output: 48_000}, "1.4M in (1.3M cached) · 48k out"},
		{"cache writes count as input but not as cached", core.TokenUsage{Input: 100, CacheWrite: 900, Output: 5}, "1k in · 5 out"},
		{"writes and reads together", core.TokenUsage{Input: 1_200, CacheRead: 8_000, CacheWrite: 800, Output: 410}, "10k in (8k cached) · 410 out"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenSummary(tt.usage); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDoctorContextSummary(t *testing.T) {
	for _, tt := range []struct {
		name           string
		provider       string
		tokens, legacy int
		want           []string
	}{
		{name: "API provider shows the compaction point", provider: "anthropic", tokens: 256_000, want: []string{"Context   256k tokens · compacts at 217.6k"}},
		{name: "custom window", provider: "openrouter", tokens: 100_000, want: []string{"Context   100k tokens · compacts at 85k"}},
		{name: "Codex compacts itself", provider: "codex", tokens: 256_000, want: []string{"Context   256k tokens · Codex compacts its own threads"}},
		{name: "deprecated key is called out", provider: "openai", tokens: 256_000, legacy: 160_000, want: []string{
			"Context   256k tokens · compacts at 217.6k",
			"Context   context_bytes (160000) is deprecated and ignored; context_tokens (256000) is used instead",
		}},
		{name: "deprecated key with Codex", provider: "codex", tokens: 300_000, legacy: 160_000, want: []string{
			"Context   300k tokens · Codex compacts its own threads",
			"Context   context_bytes (160000) is deprecated and ignored; context_tokens (300000) is used instead",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Provider, cfg.ContextTokens, cfg.ContextBytes = tt.provider, tt.tokens, tt.legacy
			got := contextSummary(cfg)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	if lines := contextSummary(config.Default()); len(lines) != 1 || strings.Contains(strings.Join(lines, ""), "deprecated") {
		t.Fatalf("a default config must not mention the deprecated key: %q", lines)
	}
}

// Every config written by an earlier 'julia config init' carries context_bytes,
// and configs reject unknown keys, so the CLI must keep starting with it.
func TestCLIStartsWithLegacyContextBytesAndValidatesContextTokens(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Stdout = output
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tt := range []struct {
		name, config, wantError string
	}{
		{"legacy config from config init", `{"model":"codex","mode":"auto","codex_binary":"codex","max_steps":256,"context_bytes":160000,"timeout":"4h","skill_paths":[]}`, ""},
		{"legacy value is not validated", `{"context_bytes":5,"skill_paths":[]}`, ""},
		{"new key", `{"context_tokens":128000,"skill_paths":[]}`, ""},
		{"window too small", `{"context_tokens":1000,"skill_paths":[]}`, "context_tokens must be at least"},
		{"unknown keys still rejected", `{"context_words":1,"skill_paths":[]}`, "context_words"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(tt.config), 0600); err != nil {
				t.Fatal(err)
			}
			os.Args = []string{"julia", "--config", path, "--dir", dir, "--snapshot", "40"}
			err := run()
			if tt.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Fatalf("got %v; want %q", err, tt.wantError)
			}
		})
	}
}

func TestUIConfigCarriesTheConfiguredContextWindow(t *testing.T) {
	for _, tokens := range []int{core.DefaultContextWindow, 100_000} {
		cfg := config.Default()
		cfg.ContextTokens, cfg.Mode = tokens, "ask"
		s := session.New(t.TempDir(), "fake", "model")
		got := baseUIConfig(s, cfg, nil)
		if got.ContextWindow != tokens || got.Session != s || got.Mode != "ask" {
			t.Fatalf("UI config %+v does not follow the configuration", got)
		}
	}
}
