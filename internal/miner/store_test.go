package miner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testReport(t *testing.T) (*Report, string) {
	t.Helper()
	dir := t.TempDir()
	writeSession(t, dir, sessionFor(id("a"), turn("question", "c", `{"path":"file"}`)))
	r, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Candidates) != 1 {
		t.Fatal("no candidate")
	}
	return r, dir
}

func TestReportPrivateAtomicRoundTrip(t *testing.T) {
	r, sessions := testReport(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, r); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Candidates[0].ID != r.Candidates[0].ID {
		t.Fatalf("load: %+v %v", loaded, err)
	}
	if err := Save(filepath.Join(sessions, id("a")+".json"), r); err == nil {
		t.Fatal("overwrote session")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := Save(link, r); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := Save(dir, r); err == nil {
		t.Fatal("accepted directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".miner-report-") {
			t.Fatalf("left temporary file %s", e.Name())
		}
	}
}

func TestLoadRejectsInvalidDecisionsAndSchemas(t *testing.T) {
	r, _ := testReport(t)
	path := filepath.Join(t.TempDir(), "report.json")
	cases := []struct {
		name   string
		mutate func(*Report)
	}{
		{"version", func(r *Report) { r.Version++ }},
		{"unknown decision", func(r *Report) { r.Candidates[0].Review.Decision = "ship" }},
		{"rank without keep", func(r *Report) { r.Candidates[0].Review.Rank = 1 }},
		{"keep without assessment", func(r *Report) { r.Candidates[0].Review = Review{Decision: "keep", Rank: 3} }},
		{"unsafe keep", func(r *Report) {
			r.Candidates[0].Assessment = &Assessment{Recommendation: "keep", Initiative: 1, Completion: 1, Restraint: 1, Sensitive: true}
			r.Candidates[0].Review = Review{Decision: "keep", Rank: 3}
		}},
		{"bad assessment", func(r *Report) {
			r.Candidates[0].Assessment = &Assessment{Recommendation: "yes", Initiative: 1, Completion: 1, Restraint: 1}
		}},
		{"tool schema", func(r *Report) { r.Candidates[0].Messages[1].ToolCalls[0].Arguments = json.RawMessage(`{"path":9}`) }},
		{"secret feedback", func(r *Report) { r.Candidates[0].Feedback = "password=abc12345" }},
	}
	original, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var clone Report
			if err := json.Unmarshal(original, &clone); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&clone)
			b, err := json.Marshal(clone)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("accepted invalid report")
			}
			if err := Save(path, &clone); err == nil {
				t.Fatal("saved invalid report")
			}
		})
	}
	if err := os.WriteFile(path, append(original, []byte(` {}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestMarkdownNeverIncludesTranscriptAndEscapesControls(t *testing.T) {
	r, _ := testReport(t)
	r.Candidates[0].Source.Title = "title\x1b]52;0;data\x07\n# injected"
	r.Candidates[0].Messages[0].Content = "UNIQUE_PRIVATE_TRANSCRIPT"
	text := Markdown(r)
	if strings.Contains(text, "UNIQUE_PRIVATE_TRANSCRIPT") || strings.ContainsRune(text, '\x1b') || strings.Contains(text, "\n# injected") {
		t.Fatalf("unsafe markdown: %q", text)
	}
	for _, want := range []string{"Sessions:", "Cost: not recorded", "messages [0, 4)", "Signals:", "Rank:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
}

func TestSaveDistinguishesCommittedDurabilityFailure(t *testing.T) {
	report, _ := testReport(t)
	path := filepath.Join(t.TempDir(), "report.json")
	if err := Save(path, report); err != nil {
		t.Fatal(err)
	}
	report.Candidates[0].Review = Review{Decision: "reject"}
	err := saveReport(path, report, func(*os.File) error { return errors.New("directory sync failed") })
	var durability *DurabilityError
	if !errors.As(err, &durability) {
		t.Fatalf("committed failure not distinguished: %v", err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Candidates[0].Review.Decision != "reject" {
		t.Fatalf("new report was not committed: %+v %v", loaded, err)
	}
}
