package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/miner"
	"julia/internal/ui"
)

type commandMiningJudge struct{ calls int }

func (j *commandMiningJudge) Complete(_ context.Context, instructions, prompt string, schema any) (string, core.TokenUsage, error) {
	j.calls++
	assessment := miner.Assessment{
		Summary: "A direct, complete answer.", Category: "direct_answer", Recommendation: "keep",
		Reason: "No tools are needed.", Evidence: []string{"Message 1 answers the question in message 0."},
		Initiative: 5, Completion: 5, Restraint: 5,
	}
	data, _ := json.Marshal(assessment)
	return string(data), core.TokenUsage{Input: 100, Output: 20}, nil
}

func miningCommandFixture(t *testing.T) (miningCommand, *bytes.Buffer, string) {
	t.Helper()
	state := t.TempDir()
	sessions := filepath.Join(state, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	session := core.Session{
		ID: strings.Repeat("a", 24), Title: "A simple calculation", Directory: "/project",
		Provider: "codex", Model: "source-model", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Messages: []core.Message{{Role: "user", Content: "What is 2 + 2?"}, {Role: "assistant", Content: "4."}},
	}
	data, _ := json.Marshal(session)
	source := filepath.Join(sessions, session.ID+".json")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	command := miningCommand{
		config:   config.Config{Provider: "codex", Model: "test-judge"},
		sessions: sessions, workspace: "/project", output: &out,
		makeJudge: func(context.Context, config.Config) (core.Completer, error) {
			t.Fatal("offline command called a model")
			return nil, nil
		},
	}
	return command, &out, source
}

// run is julia mine with args, on c.
func (c miningCommand) run(ctx context.Context, args []string) error {
	command := mineCommand(func(*cobra.Command) (miningCommand, error) { return c, nil })
	// Never nil: for nil, cobra reads os.Args, the test binary's.
	command.SetArgs(append([]string{}, args...))
	command.SetOut(c.output)
	command.SilenceErrors, command.SilenceUsage = true, true
	return command.ExecuteContext(ctx)
}

func TestMiningCommandFullWorkflowAndResume(t *testing.T) {
	command, output, source := miningCommandFixture(t)
	before, _ := os.ReadFile(source)
	directory := filepath.Join(t.TempDir(), "report")
	ctx := context.Background()
	if err := command.run(ctx, []string{"scan", "--output", directory, "--limit", "10"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "report.json")
	if !strings.Contains(output.String(), "selected 1 candidates") {
		t.Fatalf("scan output: %s", output)
	}
	judge := &commandMiningJudge{}
	command.makeJudge = func(context.Context, config.Config) (core.Completer, error) { return judge, nil }
	if err := command.run(ctx, []string{"judge", path}); err != nil {
		t.Fatal(err)
	}
	if err := command.run(ctx, []string{"judge", directory}); err != nil || judge.calls != 1 {
		t.Fatalf("resume repeated judge call: %v calls=%d", err, judge.calls)
	}
	command.review = func(ctx context.Context, cfg ui.MiningReviewConfig) error {
		if cfg.Report.Candidates[0].Assessment == nil {
			t.Fatal("review did not receive assessment")
		}
		cfg.Report.Candidates[0].Review = miner.Review{Decision: "keep", Rank: 5}
		return cfg.Save()
	}
	if err := command.run(ctx, []string{"review", path}); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(t.TempDir(), "dataset")
	if err := command.run(ctx, []string{"export", "--output", export, "--validation-percent", "0", path}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(export, "training.jsonl"))
	if err != nil || !strings.Contains(string(data), "What is 2 + 2?") {
		t.Fatalf("missing training data: %s %v", data, err)
	}
	report, err := miner.Load(path)
	if err != nil || report.Candidates[0].Review.Rank != 5 || report.Judge != "codex/test-judge" || report.Usage.Output != 20 {
		t.Fatalf("report lost state: %+v %v", report, err)
	}
	summary, _ := os.ReadFile(filepath.Join(directory, "report.md"))
	if !strings.Contains(string(summary), "Rank: 5") {
		t.Fatalf("summary stale: %s", summary)
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(before, after) {
		t.Fatal("source session changed")
	}
}

func TestMiningRejectBeforeJudgeStaysLocal(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "report")
	if err := command.run(context.Background(), []string{"scan", "--output", directory}); err != nil {
		t.Fatal(err)
	}
	command.review = func(ctx context.Context, cfg ui.MiningReviewConfig) error {
		cfg.Report.Candidates[0].Review.Decision = "reject"
		return cfg.Save()
	}
	if err := command.run(context.Background(), []string{"review", directory}); err != nil {
		t.Fatal(err)
	}
	if err := command.run(context.Background(), []string{"judge", directory}); err != nil {
		t.Fatal(err)
	}
}

// A review key must not rescan every transcript, so the review's Save skips them.
// The next command's Load still checks them under the lock.
func TestMiningReviewSaveSkipsTranscriptsAndNextLoadChecksThem(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "report")
	ctx := context.Background()
	if err := command.run(ctx, []string{"scan", "--output", directory}); err != nil {
		t.Fatal(err)
	}

	command.review = func(ctx context.Context, cfg ui.MiningReviewConfig) error {
		cfg.Report.Candidates[0].Messages[0].Content = "password=abc12345"
		return cfg.Save()
	}
	if err := command.run(ctx, []string{"review", directory}); err != nil {
		t.Fatalf("review save scanned the transcripts: %v", err)
	}

	command.review = func(context.Context, ui.MiningReviewConfig) error {
		t.Fatal("review opened a report with a secret in its transcript")
		return nil
	}
	if err := command.run(ctx, []string{"review", directory}); err == nil {
		t.Fatal("review accepted a report with a secret in its transcript")
	}
}

func TestMiningLeavesNoLockBesideOtherFiles(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	path := filepath.Join(t.TempDir(), "notes.json")
	if err := os.WriteFile(path, []byte(`{"name":"package"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command.run(context.Background(), []string{"export", path}); err == nil {
		t.Fatal("export accepted a file that is not a report")
	}
	if _, err := os.Lstat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created a lock beside another file: %v", err)
	}
}

func TestMiningArgumentsAndOverwriteGuards(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	ctx := context.Background()
	for _, args := range [][]string{
		{"scan", "--since", "yesterday"}, {"scan", "--limit", "0"},
		{"scan", "--max-bytes", "-1"}, {"scan", "--focus", ""},
		{"judge"}, {"review", "one", "two"}, {"unknown"},
	} {
		if err := command.run(ctx, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	existing := t.TempDir()
	if err := command.run(ctx, []string{"scan", "--output", existing}); err == nil {
		t.Fatal("scan used an existing output directory")
	}
	for _, args := range [][]string{nil, {"--help"}, {"scan", "--help"}, {"judge", "--help"}, {"export", "--help"}} {
		if err := command.run(ctx, args); err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
	}
}

func TestMiningJudgeCannotMixModelsAndLockIsExclusive(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "report")
	if err := command.run(context.Background(), []string{"scan", "--output", directory}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "report.json")
	report, _ := miner.Load(path)
	report.Judge = "another/model"
	if err := miner.Save(path, report); err != nil {
		t.Fatal(err)
	}
	if err := command.run(context.Background(), []string{"judge", path}); err == nil || !strings.Contains(err.Error(), "select that model") {
		t.Fatalf("model mismatch: %v", err)
	}
	unlock, err := lockMiningReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockMiningReport(path); err == nil {
		t.Fatal("second report writer acquired lock")
	}
	unlock()
	unlock, err = lockMiningReport(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestMiningScanUsesCurrentWorkspaceUnlessAll(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	command.workspace = "/different"
	directory := filepath.Join(t.TempDir(), "report")
	if err := command.run(context.Background(), []string{"scan", "--output", directory}); err != nil {
		t.Fatal(err)
	}
	report, _ := miner.Load(filepath.Join(directory, "report.json"))
	if len(report.Candidates) != 0 {
		t.Fatal("scan crossed workspace boundary")
	}
	directory = filepath.Join(t.TempDir(), "all")
	if err := command.run(context.Background(), []string{"scan", "--all", "--output", directory}); err != nil {
		t.Fatal(err)
	}
	report, _ = miner.Load(filepath.Join(directory, "report.json"))
	if len(report.Candidates) != 1 {
		t.Fatal("all did not include other workspace")
	}
}

func TestMiningWorkflowWithCurrentToolDefinitions(t *testing.T) {
	state := t.TempDir()
	// As --dir resolves it, so the session is in the scanned workspace.
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	sessions := filepath.Join(state, "julia", "sessions")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	// The Skill tool comes from the user's skills folder, not the workspace.
	home := t.TempDir()
	t.Setenv("HOME", home)
	skill := filepath.Join(home, ".agents", "skills", "demo")
	if err := os.MkdirAll(skill, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: demo\ndescription: A demo skill.\n---\nUse it.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	call := func(id, name, arguments string) core.Message {
		return core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(arguments)}}}
	}
	session := core.Session{
		ID: strings.Repeat("c", 24), Title: "Tool use", Directory: workspace,
		Provider: "codex", Model: "source-model", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Messages: []core.Message{
			{Role: "user", Content: "List the files, fix a.txt and map the package."},
			call("c1", "Bash", `{"command":"ls","timeout":120}`), {Role: "tool", ToolCallID: "c1", Content: "a.txt"},
			call("c2", "MultiEdit", `{"path":"a.txt","edits":[{"old_string":"x","new_string":"y"}]}`), {Role: "tool", ToolCallID: "c2", Content: "Edited a.txt"},
			call("c3", "Subagent", `{"tasks":[{"role":"scout","task":"Map the package."}]}`), {Role: "tool", ToolCallID: "c3", Content: "The map."},
			call("c4", "Skill", `{"name":"demo"}`), {Role: "tool", ToolCallID: "c4", Content: "Use it."},
			{Role: "assistant", Content: "Done: a.txt fixed, and the package is mapped."},
		},
	}
	data, _ := json.Marshal(session)
	if err := os.WriteFile(filepath.Join(sessions, session.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	// Only julia mine scan builds the real definitions; their descriptions
	// must pass the report's secret checks, and these calls their schemas.
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"model":"codex/test-judge"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	directory := filepath.Join(t.TempDir(), "report")
	ctx := context.Background()
	root := newCommand(configPath)
	root.SetArgs([]string{"mine", "scan", "--dir", workspace, "--output", directory})
	root.SetOut(&out)
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "selected 1 candidates") {
		t.Fatalf("tool turn was not selected: %s", out.String())
	}

	command, _, _ := miningCommandFixture(t)
	command.makeJudge = func(context.Context, config.Config) (core.Completer, error) { return &commandMiningJudge{}, nil }
	if err := command.run(ctx, []string{"judge", directory}); err != nil {
		t.Fatal(err)
	}
	command.review = func(_ context.Context, cfg ui.MiningReviewConfig) error {
		cfg.Report.Candidates[0].Review = miner.Review{Decision: "keep", Rank: 5}
		return cfg.Save()
	}
	if err := command.run(ctx, []string{"review", directory}); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "dataset")
	if err := command.run(ctx, []string{"export", "--output", dataset, "--validation-percent", "0", directory}); err != nil {
		t.Fatal(err)
	}
	row, err := os.ReadFile(filepath.Join(dataset, "training.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"arguments":"{\"command\":\"ls\",\"timeout\":120}"`, `"tool_call_id":"call_4"`, `"name":"Subagent"`, `"arguments":"{\"name\":\"demo\"}"`} {
		if !strings.Contains(string(row), want) {
			t.Errorf("training row lacks %s: %s", want, row)
		}
	}
}

func TestMiningScanRejectsSecretFocusBeforeScanning(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "report")
	ctx := context.Background()
	err := command.run(ctx, []string{"scan", "--output", directory, "--focus", "Never ask for the password: confirmations"})
	if err == nil || !strings.Contains(err.Error(), "focus") {
		t.Fatalf("secret-like focus: %v", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed scan left its output directory: %v", err)
	}
	if err := command.run(ctx, []string{"scan", "--output", directory}); err != nil {
		t.Fatal("the same --output was not reusable:", err)
	}
}

func TestMiningReviewExportsWithItsOwnFlags(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "report")
	ctx := context.Background()
	if err := command.run(ctx, []string{"scan", "--output", directory}); err != nil {
		t.Fatal(err)
	}
	command.makeJudge = func(context.Context, config.Config) (core.Completer, error) { return &commandMiningJudge{}, nil }
	if err := command.run(ctx, []string{"judge", directory}); err != nil {
		t.Fatal(err)
	}
	dataset := filepath.Join(t.TempDir(), "rank5")
	command.review = func(_ context.Context, cfg ui.MiningReviewConfig) error {
		cfg.Report.Candidates[0].Review = miner.Review{Decision: "keep", Rank: 5}
		if err := cfg.Save(); err != nil {
			return err
		}
		if result, err := cfg.Export(); err != nil || !strings.HasPrefix(result, dataset) {
			t.Errorf("review export: %q %v", result, err)
		}
		return nil
	}
	if err := command.run(ctx, []string{"review", "--output", dataset, "--min-rank", "5", "--validation-percent", "0", directory}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dataset, "training.jsonl")); err != nil {
		t.Fatal("review did not export to its --output:", err)
	}
}

// steeringBackend saves a steer as the providers do: an ordinary user message,
// told apart from a new turn only by the steer event.
type steeringBackend struct{}

func (steeringBackend) Name() string { return "fake" }

func (steeringBackend) Run(_ context.Context, s *core.Session, prompt core.Prompt, _ core.ToolExecutor, emit core.Emit) error {
	s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt.Text}, core.Message{Role: "user", Content: "Also the changelog."})
	emit(core.Event{Kind: "steer", Text: "Also the changelog."})
	s.Messages = append(s.Messages, core.Message{Role: "assistant", Content: "Done."})
	return nil
}

func TestSteersAreAuditedForTheMiner(t *testing.T) {
	runner, s := newQuestionRunner(steeringBackend{}, nil, false, t.TempDir())
	if err := runner.Run(context.Background(), s, core.Prompt{Text: "Fix the parser."}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range s.Audit {
		if entry.Event.Kind == "steer" && entry.Event.Text == "Also the changelog." {
			return
		}
	}
	t.Fatalf("steer missing from the audit: %+v", s.Audit)
}

func TestMiningSummaryFailureDoesNotLoseSavedVote(t *testing.T) {
	command, _, _ := miningCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "report")
	if err := command.run(context.Background(), []string{"scan", "--output", directory}); err != nil {
		t.Fatal(err)
	}
	command.review = func(ctx context.Context, cfg ui.MiningReviewConfig) error {
		cfg.Report.Candidates[0].Review.Decision = "reject"
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		return errors.New("test review exited with error")
	}
	if err := command.run(context.Background(), []string{"review", directory}); err == nil {
		t.Fatal("lost review error")
	}
	report, _ := miner.Load(filepath.Join(directory, "report.json"))
	if report.Candidates[0].Review.Decision != "reject" {
		t.Fatal("lost persisted vote")
	}
}

func TestMiningJudgeUsesConfiguredEndpointWithoutTools(t *testing.T) {
	command, output, _ := miningCommandFixture(t)
	directory := filepath.Join(t.TempDir(), "report")
	if err := command.run(context.Background(), []string{"scan", "--output", directory}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "" {
			t.Errorf("wrong endpoint/auth: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			Model    string            `json:"model"`
			Tools    []json.RawMessage `json:"tools"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Model != "judge-model" || len(request.Tools) != 0 || len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" {
			t.Errorf("judge request not isolated: %+v", request)
		}
		answer, _, _ := (&commandMiningJudge{}).Complete(context.Background(), "", "", nil)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "mining-response", "object": "chat.completion", "created": 1, "model": "judge-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		})
	}))
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "not-for-this-endpoint")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{"model":"mining-local/judge-model","providers":[{"id":"mining-local","base_url":%q}]}`, server.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	root := newCommand(configPath)
	root.SetArgs([]string{"mine", "judge", directory})
	root.SetOut(output)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one call, got %d", calls.Load())
	}
	report, err := miner.Load(filepath.Join(directory, "report.json"))
	if err != nil || report.Judge != "mining-local/judge-model" || report.Candidates[0].Assessment == nil || report.Usage.Output != 20 {
		t.Fatalf("judge integration: %+v %v", report, err)
	}
}
