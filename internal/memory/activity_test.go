package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

func TestActivityLogAppendsReadsAndTrims(t *testing.T) {
	s := &Store{Directory: t.TempDir()}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return at }
	if _, _, err := s.Activity(time.Time{}); err != nil {
		t.Fatalf("an empty store has no log, which is no error: %v", err)
	}
	if err := s.Log(Activity{Kind: ActivityCue, Note: "a", At: at.Add(-48 * time.Hour)}, Activity{Kind: ActivityRead, Note: "b", Error: strings.Repeat("e", 1000)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Directory, activityFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not json\n")
	f.Close()
	entries, skipped, err := s.Activity(at.Add(-time.Hour))
	if err != nil || skipped != 1 || len(entries) != 1 {
		t.Fatalf("entries %+v skipped %d: %v", entries, skipped, err)
	}
	if e := entries[0]; e.Note != "b" || !e.At.Equal(at) || len(e.Error) > maxActivityError+64 {
		t.Fatalf("entry %+v", e)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log mode %v %v", info.Mode(), err)
	}

	// Past its bound the log keeps its newer half, cut at a line.
	line := Activity{Kind: ActivityCue, Note: strings.Repeat("n", 60)}
	batch := make([]Activity, 1000)
	for i := range batch {
		batch[i] = line
	}
	for written := int64(0); written < maxActivityBytes; written += 100 * 1000 {
		if err := s.Log(batch...); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Log(Activity{Kind: ActivityTurn, Notes: 7}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Size() > maxActivityBytes {
		t.Fatalf("log grew to %d", info.Size())
	}
	entries, skipped, err = s.Activity(time.Time{})
	if err != nil || skipped != 0 || entries[len(entries)-1].Notes != 7 {
		t.Fatalf("after trim: %d entries, %d skipped, %v", len(entries), skipped, err)
	}
}

func TestSummarize(t *testing.T) {
	ws, other := "/w", "/o"
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	live := []*Note{
		{Name: "noisy", Scope: Scope{Kind: ScopeWorkspace, Workspace: ws}, Created: at},
		{Name: "idle", Scope: Scope{Kind: ScopeWorkspace, Workspace: ws}, Created: at},
		{Name: "editor", Scope: Scope{Kind: ScopeGlobal}, Created: at},
	}
	var entries []Activity
	for i, session := range []string{"s1", "s2", "s3", "s4"} {
		entries = append(entries,
			Activity{Kind: ActivityTurn, Session: session, Workspace: ws, Notes: 3},
			Activity{Kind: ActivityCue, Session: session, Workspace: ws, Note: "noisy", Scope: ScopeWorkspace, Tool: "Read"},
		)
		if i == 0 {
			entries = append(entries, Activity{Kind: ActivityTurn, Session: session, Workspace: ws, Notes: 3, Relevant: []string{"editor"}})
		}
	}
	tokens := core.TokenUsage{Input: 900, Output: 100}
	entries = append(entries,
		Activity{Kind: ActivityTurn, Session: "s5", Workspace: other, Notes: 1},
		Activity{Kind: ActivityRead, Session: "s5", Workspace: other, Note: "editor", Scope: ScopeGlobal},
		Activity{Kind: ActivityRead, Workspace: ws, Note: "idle", Scope: ScopeWorkspace, Source: SourceConsolidation},
		Activity{Kind: ActivitySave, Workspace: ws, Note: "fresh", Scope: ScopeWorkspace, Source: SourceAgent},
		Activity{Kind: ActivitySave, Workspace: ws, Scope: ScopeWorkspace, Source: SourceAgent, Error: "name"},
		Activity{Kind: ActivityForget, Workspace: ws, Note: "old", Scope: ScopeWorkspace, Source: SourceReflection},
		Activity{Kind: ActivityReflection, Workspace: ws, Outcome: ReflectionUnchanged, Tokens: &tokens, Helped: []string{"noisy"}, Ignored: []string{"editor", "noisy"}},
		Activity{Kind: ActivityReflection, Workspace: ws, Outcome: ReflectionSkipped},
	)
	st := Summarize(entries, live)
	if st.Turns != 6 || st.Sessions != 5 || st.IndexNotes != 16 || st.Relevant != 1 {
		t.Fatalf("turns %+v", st)
	}
	if st.Cues != 4 || st.CueSessions != 4 || st.Reads != 1 {
		t.Fatalf("cues %d in %d, reads %d", st.Cues, st.CueSessions, st.Reads)
	}
	if st.Saves[SourceAgent] != 1 || st.Forgets[SourceReflection] != 1 || st.Failures[ActivitySave] != 1 {
		t.Fatalf("changes %v %v %v", st.Saves, st.Forgets, st.Failures)
	}
	if st.Reflections[ReflectionUnchanged] != 1 || st.Reflections[ReflectionSkipped] != 1 || st.ReflectionTokens != tokens || st.Helped != 1 || st.Ignored != 2 {
		t.Fatalf("reflection %+v", st)
	}
	byName := map[string]*NoteStats{}
	for _, n := range st.Notes {
		byName[n.Name] = n
	}
	noisy, idle, editor := byName["noisy"], byName["idle"], byName["editor"]
	if st.Notes[0] != noisy || noisy.Cues != 4 || noisy.Sessions != 4 || noisy.Of != 4 || !noisy.Noisy() || noisy.Helped != 1 || noisy.Ignored != 1 {
		t.Fatalf("noisy %+v", noisy)
	}
	if idle.Used() != 0 || !idle.Live || idle.Noisy() {
		t.Fatalf("a consolidation read is no use: %+v", idle)
	}
	// A global note is shown, read and judged wherever it is seen.
	if editor.Workspace != "" || editor.Shown != 1 || editor.Reads != 1 || editor.Sessions != 2 || editor.Of != 5 || editor.Ignored != 1 {
		t.Fatalf("editor %+v", editor)
	}
	if fresh := byName["fresh"]; fresh == nil || fresh.Live || fresh.Saves != 1 {
		t.Fatalf("fresh %+v", fresh)
	}
}
