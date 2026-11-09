package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/memory"
	"julia/internal/session"
	"julia/internal/ui"
)

// seedMemory points the default store at a fresh state directory and fills
// it with what a workspace sees: two of its own notes, one read and judged
// and one untouched, a global note, and a file that is not a note.
func seedMemory(t *testing.T) (*memory.Store, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := memory.Default()
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	dir := t.TempDir()
	workspace := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: dir}
	for _, note := range []*memory.Note{
		{Name: "deploy", Description: "Deploy with make release", Type: memory.TypeProcedure, Scope: workspace},
		{Name: "make-check", Description: "Run make check before reporting done", Type: memory.TypeFeedback, Body: "**Why:** go test alone misses vet.", Scope: workspace, Reads: 3, Helped: 1, Misled: 1},
		{Name: "editor", Description: "Prefers vim", Type: memory.TypeUser, Scope: memory.Scope{Kind: memory.ScopeGlobal}},
	} {
		if err := store.Save(note); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(store.Directory, "global", "broken.md"), []byte("just text"), 0600); err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func runMemory(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	// TestMemoryReview gives the card review its own stand-in.
	command := memoryCommand(func(*cobra.Command) (string, error) { return dir, nil }, nil)
	// Never nil: for nil, cobra reads os.Args, the test binary's.
	command.SetArgs(append([]string{}, args...))
	command.SetOut(&out)
	command.SilenceErrors, command.SilenceUsage = true, true
	err := command.Execute()
	return out.String(), err
}

// The listing shows each scope's notes as the index orders them, with the
// counts that are nonzero, then where the store is and what it could not read.
func TestMemoryCommandLists(t *testing.T) {
	store, dir := seedMemory(t)
	got, err := runMemory(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "This workspace (" + dir + ")\n" +
		"  make-check  feedback   2026-10-16  Run make check before reporting done  · read 3 · helped 1 · misled 1\n" +
		"  deploy      procedure  2026-10-16  Deploy with make release\n" +
		"Every workspace\n" +
		"  editor      user       2026-10-16  Prefers vim\n" +
		"Store: " + store.Directory + "\n" +
		"Memory note not loaded: " + filepath.Join(store.Directory, "global", "broken.md") + ": a note starts with a --- line, then its frontmatter\n"
	if got != want {
		t.Fatalf("listing:\n%s\nwant:\n%s", got, want)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	empty, err := memory.Default()
	if err != nil {
		t.Fatal(err)
	}
	got, err = runMemory(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	want = "This workspace (" + dir + ")\n  No notes yet.\nEvery workspace\n  No notes yet.\nStore: " + empty.Directory + "\n"
	if got != want {
		t.Fatalf("empty listing:\n%s\nwant:\n%s", got, want)
	}
}

// A bare name finds the workspace's note before a global one; a scope prefix
// picks one; a missing note is an error that says where it was looked for.
func TestMemoryCommandShowAndForgetResolveScopes(t *testing.T) {
	store, dir := seedMemory(t)
	workspace := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: dir}
	if err := store.Save(&memory.Note{Name: "editor", Description: "Prefers emacs here", Type: memory.TypeUser, Scope: workspace}); err != nil {
		t.Fatal(err)
	}
	local, err := store.Load(workspace, "editor")
	if err != nil {
		t.Fatal(err)
	}
	global, err := store.Load(memory.Scope{Kind: memory.ScopeGlobal}, "editor")
	if err != nil {
		t.Fatal(err)
	}
	for arg, note := range map[string]*memory.Note{"editor": local, "workspace/editor": local, "global/editor": global} {
		got, err := runMemory(t, dir, "show", arg)
		if err != nil {
			t.Fatalf("show %s: %v", arg, err)
		}
		data, err := os.ReadFile(note.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got != note.Path+"\n"+string(data) || !strings.Contains(got, "\ndescription: "+note.Description+"\n") {
			t.Fatalf("show %s:\n%s", arg, got)
		}
	}
	got, err := runMemory(t, dir, "forget", "editor")
	if err != nil || got != "Forgot workspace note editor: Prefers emacs here\n" {
		t.Fatalf("forget: %q %v", got, err)
	}
	if _, err := os.Stat(local.Path); !os.IsNotExist(err) {
		t.Fatalf("workspace note still there: %v", err)
	}
	if got, err = runMemory(t, dir, "show", "editor"); err != nil || !strings.HasPrefix(got, global.Path+"\n") {
		t.Fatalf("show after forget: %q %v", got, err)
	}
	if got, err = runMemory(t, dir, "forget", "global/editor"); err != nil || got != "Forgot global note editor: Prefers vim\n" {
		t.Fatalf("forget global: %q %v", got, err)
	}
	for arg, want := range map[string]string{
		"editor":           "no workspace or global note is named editor",
		"global/editor":    "no global note is named editor",
		"workspace/editor": "no workspace note is named editor",
		"team/editor":      `scope "team" must be workspace or global`,
		"Bad Name":         `name "Bad Name" must be ` + memory.NameRule,
	} {
		for _, verb := range []string{"show", "forget"} {
			got, err := runMemory(t, dir, verb, arg)
			if err == nil || err.Error() != want || got != "" {
				t.Fatalf("%s %s: %q %v", verb, arg, got, err)
			}
		}
	}
	if _, err := runMemory(t, dir, "show", "broken"); err == nil || !strings.Contains(err.Error(), "broken.md") {
		t.Fatalf("show broken: %v", err)
	}
}

// The remaining forms name the store and its workspaces; anything else is
// an error that prints nothing.
func TestMemoryCommandPathWorkspacesAndUsage(t *testing.T) {
	store, dir := seedMemory(t)
	if got, err := runMemory(t, dir, "path"); err != nil || got != store.Directory+"\n" {
		t.Fatalf("path: %q %v", got, err)
	}
	if got, err := runMemory(t, dir, "workspaces"); err != nil || got != dir+"\n" {
		t.Fatalf("workspaces: %q %v", got, err)
	}
	for _, args := range [][]string{{"show"}, {"forget"}, {"show", "a", "b"}, {"confirm"}, {"confirm", "a", "b"}, {"path", "extra"}, {"workspaces", "extra"}, {"prune"}} {
		got, err := runMemory(t, dir, args...)
		if err == nil || got != "" {
			t.Fatalf("%v: %q %v", args, got, err)
		}
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got, err := runMemory(t, dir, "workspaces"); err != nil || got != "No workspace has notes yet.\n" {
		t.Fatalf("workspaces of an empty store: %q %v", got, err)
	}
}

// The doctor's memory line names the store, counts what this directory sees
// and says whether reflection runs and on what, then repeats each warning.
func TestMemorySummary(t *testing.T) {
	store, dir := seedMemory(t)
	cfg := config.Default()
	got := memorySummary(cfg, dir)
	want := []string{
		"Memory    " + store.Directory + " · 2 notes for this workspace · 1 global · reflection on · reasoning low",
		"Memory    Memory note not loaded: " + filepath.Join(store.Directory, "global", "broken.md") + ": a note starts with a --- line, then its frontmatter",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	cfg.Memory.Model = &config.ModelEntry{ID: "codex/gpt-6-luna", Reasoning: "high"}
	if got = memorySummary(cfg, dir); got[0] != strings.TrimSuffix(want[0], " · reasoning low")+" codex/gpt-6-luna · reasoning high" {
		t.Fatalf("configured model: %q", got[0])
	}
	cfg.Memory.Model = &config.ModelEntry{ID: "anthropic/claude-opus-5"}
	// Anthropic takes no effort from JULIA, so none is shown.
	if got = memorySummary(cfg, dir); got[0] != strings.TrimSuffix(want[0], " · reasoning low")+" anthropic/claude-opus-5" {
		t.Fatalf("model without effort: %q", got[0])
	}
	cfg.Memory.Reflection = "off"
	if got = memorySummary(cfg, dir); len(got) != 2 || got[0] != "Memory    "+store.Directory+" · 2 notes for this workspace · 1 global · reflection off" {
		t.Fatalf("reflection off: %q", got)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	empty, err := memory.Default()
	if err != nil {
		t.Fatal(err)
	}
	if got = memorySummary(config.Default(), dir); len(got) != 1 || got[0] != "Memory    "+empty.Directory+" · 0 notes for this workspace · 0 global · reflection on · reasoning low" {
		t.Fatalf("empty store: %q", got)
	}
}

func TestMemoryMarkdownForTheInterface(t *testing.T) {
	store, dir := seedMemory(t)
	list, err := memoryMarkdown(store, dir, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**This workspace**", "**Every workspace**", "- **", "\n> Memory note not loaded: "} {
		if !strings.Contains(list, want) {
			t.Fatalf("list lacks %q:\n%s", want, list)
		}
	}
	recall, err := store.Recall(dir)
	if err != nil || recall.Count() == 0 {
		t.Fatal("seed left no notes", err)
	}
	first := recall.Workspace[0]
	shown, err := memoryMarkdown(store, dir, "show", first.Name, true)
	if err != nil || !strings.HasPrefix(shown, "**"+first.Name+"** · "+first.Type+" · workspace · updated ") || !strings.Contains(shown, first.Description) || !strings.Contains(shown, "_"+first.Path+"_") {
		t.Fatalf("show: %q %v", shown, err)
	}
	if _, err := memoryMarkdown(store, dir, "forget", first.Name, true); err == nil || !strings.Contains(err.Error(), "read-only in this run") {
		t.Fatalf("read-only forget: %v", err)
	}
	forgot, err := memoryMarkdown(store, dir, "forget", first.Name, false)
	if err != nil || !strings.HasPrefix(forgot, "Forgot workspace note **"+first.Name+"**: ") {
		t.Fatalf("forget: %q %v", forgot, err)
	}
	if _, err := memoryMarkdown(store, dir, "show", first.Name, false); err == nil {
		t.Fatal("shown after forgetting")
	}
	if _, err := memoryMarkdown(store, dir, "prune", "", false); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestMemoryFlagIsValidatedBeforeAnyProviderCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"model":"codex/gpt-6-astra"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	output, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Stdout = output
	for _, tc := range []struct {
		flags     []string
		wantError string
	}{
		{[]string{"--memory", "off"}, ""},
		{[]string{"--memory", "read"}, ""},
		{[]string{"--memory", "write"}, ""},
		{[]string{"--memory", "sometimes"}, "--memory must be off, read or write"},
	} {
		os.Args = append([]string{"julia", "--config", path, "--dir", dir, "--snapshot", "40"}, tc.flags...)
		err := run()
		if tc.wantError == "" && err != nil {
			t.Fatal(tc.flags, err)
		}
		if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
			t.Fatalf("%v: got %v", tc.flags, err)
		}
	}
}

func TestMemoryCommandConfirmClearsTheUnverifiedMark(t *testing.T) {
	store, dir := seedMemory(t)
	scope := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: dir}
	if err := store.Save(&memory.Note{Name: "from-page", Description: "Something a page said", Type: memory.TypeReference, Scope: scope, Tainted: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := runMemory(t, dir); err != nil || !strings.Contains(got, "unverified") {
		t.Fatalf("list hides the mark: %q %v", got, err)
	}
	if got, err := runMemory(t, dir, "confirm", "from-page"); err != nil || got != "Confirmed workspace note from-page: Something a page said\n" {
		t.Fatalf("confirm: %q %v", got, err)
	}
	if note, _ := store.Load(scope, "from-page"); note.Tainted {
		t.Fatal("still marked")
	}
	if got, err := runMemory(t, dir, "confirm", "from-page"); err != nil || got != "The workspace note from-page is not marked unverified.\n" {
		t.Fatalf("second confirm: %q %v", got, err)
	}
	if _, err := runMemory(t, dir, "confirm", "nothing"); err == nil {
		t.Fatal("confirmed a missing note")
	}
}

func TestConsolidationOffersTheMemoryToolAlone(t *testing.T) {
	backend := &toolListingBackend{}
	cfg := config.Default()
	cfg.Mode = "read-only"
	runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: time.Minute, parent: context.Background(), memory: &memory.Store{Directory: t.TempDir()}, consolidating: true, memoryMode: "yolo", headless: true}
	s := session.New(t.TempDir(), "fake", "m")
	if err := runner.Run(context.Background(), s, core.Prompt{Text: memory.ConsolidationPrompt}, nil, func(core.Event) {}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(backend.names, ",") != "Memory" || !backend.readRefused {
		t.Fatalf("tools %v, read refused %v", backend.names, backend.readRefused)
	}
}

// toolListingBackend records the tools a turn is offered and tries a Read.
type toolListingBackend struct {
	names       []string
	readRefused bool
}

func (b *toolListingBackend) Name() string { return "fake" }
func (b *toolListingBackend) Run(ctx context.Context, s *core.Session, prompt core.Prompt, executor core.ToolExecutor, _ core.Emit) error {
	for _, definition := range executor.Definitions() {
		b.names = append(b.names, definition.Name)
	}
	result := executor.Execute(ctx, core.ToolCall{Name: "Read", Arguments: json.RawMessage(`{"path":"AGENTS.md"}`)})
	b.readRefused = result.IsError && strings.Contains(result.Content, "Memory tool alone")
	s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt.Text})
	return nil
}

func TestContinueSkipsTheConsolidationSession(t *testing.T) {
	store := &session.Store{Directory: t.TempDir()}
	dir := t.TempDir()
	work := session.New(dir, "codex", "m")
	work.UpdatedAt = time.Now().Add(-time.Hour)
	consolidation := session.New(dir, "codex", "m")
	consolidation.Role = "consolidation"
	for _, s := range []*core.Session{work, consolidation} {
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	defer store.Close()
	latest, err := store.Latest(dir)
	if err != nil || latest.ID != work.ID {
		t.Fatalf("latest %+v %v", latest, err)
	}
}

func TestMemoryMarkdownShowsLessonsAsWritten(t *testing.T) {
	store, dir := seedMemory(t)
	if err := store.Save(&memory.Note{Name: "init", Description: "Keep __init__.py and internal/**/*.go in step", Type: memory.TypeProject, Scope: memory.Scope{Kind: memory.ScopeWorkspace, Workspace: dir}}); err != nil {
		t.Fatal(err)
	}
	list, err := memoryMarkdown(store, dir, "", "", false)
	if err != nil || !strings.Contains(list, `Keep \_\_init\_\_.py and internal/\*\*/\*.go in step`) {
		t.Fatalf("list:\n%s %v", list, err)
	}
}

// Stats sums the activity log over the notes the workspace sees: what
// reached turns, what was used, what reflection did, and which notes are
// noisy, unused or gone.
func TestMemoryStats(t *testing.T) {
	store, dir := seedMemory(t)
	if got, err := runMemory(t, dir, "stats"); err != nil || !strings.Contains(got, "Nothing recorded yet") {
		t.Fatalf("empty log: %q %v", got, err)
	}
	// Cues on two of the notes, and a pinned one beside them.
	workspace := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: dir}
	for _, note := range []*memory.Note{
		{Name: "make-check", Description: "Run make check before reporting done", Type: memory.TypeFeedback, Scope: workspace, Triggers: memory.Triggers{Commands: []string{"^go test"}}},
		{Name: "deploy", Description: "Deploy with make release", Type: memory.TypeProcedure, Scope: workspace, Triggers: memory.Triggers{Paths: []string{"deploy/**"}}},
		{Name: "style", Description: "Write for the next reader", Type: memory.TypeFeedback, Scope: memory.Scope{Kind: memory.ScopeGlobal}},
	} {
		if err := store.Save(note); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Pin(memory.Scope{Kind: memory.ScopeGlobal}, "style", true); err != nil {
		t.Fatal(err)
	}
	tokens := core.TokenUsage{Input: 12_000, Output: 800}
	var entries []memory.Activity
	for _, session := range []string{"a1", "a2", "a3"} {
		entries = append(entries,
			memory.Activity{Kind: memory.ActivityTurn, At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Session: session, Workspace: dir, Notes: 4, Omitted: []string{"deploy"}},
			memory.Activity{Kind: memory.ActivityCue, Session: session, Workspace: dir, Note: "make-check", Scope: memory.ScopeWorkspace, Tool: "Bash"},
		)
	}
	entries = append(entries,
		memory.Activity{Kind: memory.ActivityTurn, Session: "elsewhere", Workspace: "/other", Notes: 1},
		memory.Activity{Kind: memory.ActivitySave, Session: "a1", Workspace: dir, Note: "gone-now", Scope: memory.ScopeWorkspace, Source: memory.SourceAgent},
		memory.Activity{Kind: memory.ActivitySave, Session: "a1", Workspace: dir, Scope: memory.ScopeWorkspace, Source: memory.SourceAgent, Error: "save needs name"},
		memory.Activity{Kind: memory.ActivityReflection, Session: "a2", Workspace: dir, Outcome: memory.ReflectionUnchanged, Tokens: &tokens, Ignored: []string{"editor", "style"}},
		memory.Activity{Kind: memory.ActivityReflection, Session: "a3", Workspace: dir, Outcome: memory.ReflectionSkipped},
	)
	if err := store.Log(entries...); err != nil {
		t.Fatal(err)
	}
	// The notes were saved on 2026-10-16; a month on, the untouched ones are unused.
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := memoryStats(store, dir, 0, false, now, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Memory over the whole log · this workspace (" + dir + ")",
		"Turns       3 in 3 sessions · 4.0 notes in the index on average · 0 brought a note in full",
		"Cues        3 in 3 of 3 sessions",
		"Saves       1 by the agent",
		"Failed      1 save",
		"Reflection  1 ran · 1 changed nothing · 1 turn skipped · 12k tokens in, 800 out",
		"Verdicts    0 helped · 0 misled · 2 ignored",
		// The table counts the window, not a note's lifetime counters; a
		// note the index had no room for was not in it.
		"make-check         3      3      0      0       0       0        0  3 of 3  · noisy: its cues fire in most sessions; narrow them, or pin it if it bears on most work",
		"global/editor      3      0      0      0       0       0        1  0 of 3  · index only · often ignored: pin it if it bears on most work",
		"global/style       3      0      0      0       0       0        1  0 of 3  · pinned · often ignored though pinned",
		"deploy             0      0      0      0       0       0        0  0 of 3  · cues never fired",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats lack %q:\n%s", want, got)
		}
	}
	// A note forgotten before it was ever used is not listed.
	if strings.Contains(got, "gone-now") {
		t.Errorf("an unused note that is gone is listed:\n%s", got)
	}
	// Within the window only, and every workspace with -all.
	out.Reset()
	if err := memoryStats(store, dir, 7, true, now, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Memory over the last 7 days · every workspace") || !strings.Contains(got, "Nothing recorded yet") {
		t.Fatalf("windowed:\n%s", got)
	}
	if _, err := runMemory(t, dir, "stats", "extra"); err == nil {
		t.Fatal("accepted a stray argument")
	}
}

// pin and unpin mark a note for every turn, which the listing shows.
func TestMemoryCommandPins(t *testing.T) {
	_, dir := seedMemory(t)
	if got, err := runMemory(t, dir, "pin", "editor"); err != nil || !strings.HasPrefix(got, "Pinned global note editor; every turn shows it in full: Prefers vim") {
		t.Fatalf("pin: %q %v", got, err)
	}
	if got, _ := runMemory(t, dir); !strings.Contains(got, "Prefers vim  · pinned") {
		t.Fatalf("listing:\n%s", got)
	}
	if got, _ := runMemory(t, dir, "pin", "editor"); !strings.Contains(got, "already pinned") {
		t.Fatalf("pin again: %q", got)
	}
	if got, err := runMemory(t, dir, "unpin", "global/editor"); err != nil || !strings.HasPrefix(got, "Unpinned global note editor") {
		t.Fatalf("unpin: %q %v", got, err)
	}
}

// review goes through the notes this workspace sees, in index order, after
// the warnings; confirming, forgetting, pinning and editing reach the store,
// except for a note rewritten since the review read it. An edit saves the
// note as the user's, and its text cannot set what the store keeps.
func TestMemoryReview(t *testing.T) {
	store, dir := seedMemory(t)
	workspace := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: dir}
	if err := store.Save(&memory.Note{Name: "from-page", Description: "Something a page said", Type: memory.TypeReference, Scope: workspace, Tainted: true}); err != nil {
		t.Fatal(err)
	}
	review := func(_ context.Context, cfg ui.MemoryReviewConfig) error {
		var names []string
		for _, note := range cfg.Notes {
			names = append(names, note.Name)
		}
		if got := strings.Join(names, " "); got != "make-check deploy from-page editor" {
			t.Errorf("reviewed %s, want the index order", got)
		}
		if err := cfg.Confirm(cfg.Notes[2]); err != nil {
			t.Error(err)
		}
		if err := cfg.Forget(cfg.Notes[1]); err != nil {
			t.Error(err)
		}
		if err := cfg.Pin(cfg.Notes[3], true); err != nil {
			t.Error(err)
		}

		draft := string(cfg.Notes[2].Draft())
		renamed := strings.Replace(draft, "name: from-page", "name: from-me", 1)
		if _, err := cfg.Edit(cfg.Notes[2], []byte(renamed)); err == nil || !strings.Contains(err.Error(), "must match the file name") {
			t.Errorf("renamed a note: %v", err)
		}
		forged := strings.Replace(draft, "description: Something a page said", "description: What I checked myself\nreads: 99\npinned: true\ntainted: true\nsource: agent", 1)
		edited, err := cfg.Edit(cfg.Notes[2], []byte(forged))
		if err != nil {
			t.Fatal(err)
		}
		if edited.Description != "What I checked myself" || edited.Source != memory.SourceUser || edited.Tainted || edited.Pinned || edited.Reads != 0 {
			t.Errorf("edit saved %+v", edited)
		}
		// The card now holds the edited note, which takes further decisions.
		if err := cfg.Pin(edited, true); err != nil {
			t.Errorf("pin after an edit: %v", err)
		}

		if err := store.Save(&memory.Note{Name: "make-check", Description: "Rewritten meanwhile", Type: memory.TypeFeedback, Scope: workspace, Tainted: true}); err != nil {
			t.Fatal(err)
		}
		stale := cfg.Notes[0]
		for _, decide := range []func() error{
			func() error { return cfg.Confirm(stale) },
			func() error { return cfg.Forget(stale) },
			func() error { return cfg.Pin(stale, true) },
			func() error { _, err := cfg.Edit(stale, stale.Draft()); return err },
		} {
			if err := decide(); err == nil || !strings.Contains(err.Error(), "make-check changed after the review opened") {
				t.Errorf("decided on a rewritten note: %v", err)
			}
		}
		return nil
	}
	runReview := func(review func(context.Context, ui.MemoryReviewConfig) error) string {
		var out bytes.Buffer
		command := memoryCommand(func(*cobra.Command) (string, error) { return dir, nil }, review)
		command.SetArgs([]string{"review"})
		command.SetOut(&out)
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if got := runReview(review); !strings.HasPrefix(got, "Memory note not loaded: ") {
		t.Fatalf("warnings not shown: %q", got)
	}
	if note, err := store.Load(workspace, "from-page"); err != nil || note.Tainted || note.Source != memory.SourceUser || note.Description != "What I checked myself" {
		t.Fatalf("confirm and edit did not reach the store: %+v %v", note, err)
	}
	if note, err := store.Load(memory.Scope{Kind: memory.ScopeGlobal}, "editor"); err != nil || !note.Pinned {
		t.Fatalf("pin did not reach the store: %+v %v", note, err)
	}
	if _, err := store.Load(workspace, "deploy"); !os.IsNotExist(err) {
		t.Fatalf("forget did not reach the store: %v", err)
	}
	if note, err := store.Load(workspace, "make-check"); err != nil || !note.Tainted || note.Description != "Rewritten meanwhile" {
		t.Fatalf("the rewritten note changed: %+v %v", note, err)
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := runReview(func(context.Context, ui.MemoryReviewConfig) error {
		t.Error("the review opened on an empty store")
		return nil
	}); got != "No notes to review yet.\n" {
		t.Fatalf("empty store: %q", got)
	}
}

// A note written by hand has no times, so the store reads its file's, to
// the nanosecond; a confirm writes them down to the second. The card must
// still take the next decision.
func TestMemoryReviewDecidesTwiceOnAHandWrittenNote(t *testing.T) {
	store, dir := seedMemory(t)
	path := filepath.Join(store.Directory, "global", "by-hand.md")
	if err := os.WriteFile(path, []byte("---\nname: by-hand\ndescription: Written by hand\ntype: project\ntainted: true\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	written := time.Date(2026, 10, 16, 12, 0, 0, 500_000_000, time.UTC)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}

	review := func(_ context.Context, cfg ui.MemoryReviewConfig) error {
		for _, note := range cfg.Notes {
			if note.Name != "by-hand" {
				continue
			}
			if err := cfg.Confirm(note); err != nil {
				t.Errorf("confirm: %v", err)
			}
			if err := cfg.Pin(note, true); err != nil {
				t.Errorf("pin after a confirm: %v", err)
			}
		}
		return nil
	}
	command := memoryCommand(func(*cobra.Command) (string, error) { return dir, nil }, review)
	command.SetArgs([]string{"review"})
	command.SetOut(io.Discard)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	if note, err := store.Load(memory.Scope{Kind: memory.ScopeGlobal}, "by-hand"); err != nil || note.Tainted || !note.Pinned {
		t.Fatalf("confirm and pin did not reach the store: %+v %v", note, err)
	}
}
