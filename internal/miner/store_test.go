package miner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"julia/internal/core"
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
		{"context past the turn", func(r *Report) {
			r.Candidates[0].Context = []core.Message{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "reply"}}
		}},
		{"context image bytes", func(r *Report) {
			r.Candidates[0].Source.Start, r.Candidates[0].Source.End = 2, 6
			r.Candidates[0].Context = []core.Message{{Role: "user", Content: "earlier", Images: []core.Image{{MediaType: "image/png", Data: []byte("bytes")}}}, {Role: "assistant", Content: "reply"}}
		}},
		{"context tool schema", func(r *Report) {
			r.Candidates[0].Source.Start, r.Candidates[0].Source.End = 4, 8
			r.Candidates[0].Context = []core.Message{
				{Role: "user", Content: "earlier"},
				{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "e", Name: "Read", Arguments: json.RawMessage(`{"path":9}`)}}},
				{Role: "tool", ToolCallID: "e", Content: "output"},
				{Role: "assistant", Content: "Done."},
			}
		}},
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

func TestReportDiagnosticsRoundTripAndLegacy(t *testing.T) {
	r, _ := testReport(t)
	r.Scan = &ScanSettings{Limit: 100, MaxBytes: 49152, SourceProvider: "provider", SourceModel: "model", Since: "2026-01-02T03:04:05.123Z"}
	r.Rejections = []Rejection{
		{SessionID: id("a"), Start: 4, End: 8, Stage: "too_large", Reason: "size_limit", ContextBytes: 20, TurnBytes: 30, FeedbackBytes: 4},
		{Stage: "invalid_session", Reason: "invalid_session_id"},
	}
	r.Stats.RejectionReasons = map[string]int{"too_large/size_limit": 3, "invalid_session/invalid_session_id": 1}
	r.Stats.RejectionsOmitted = 2
	path := filepath.Join(t.TempDir(), "report.json")
	if err := Save(path, r); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Scan, r.Scan) || !reflect.DeepEqual(loaded.Rejections, r.Rejections) || !reflect.DeepEqual(loaded.Stats.RejectionReasons, r.Stats.RejectionReasons) || loaded.Stats.RejectionsOmitted != 2 {
		t.Fatalf("diagnostics did not round-trip: %+v", loaded)
	}

	// Simulate a historical report that predates all diagnostics fields.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "scan")
	delete(legacy, "rejections")
	stats := legacy["stats"].(map[string]any)
	delete(stats, "rejection_reasons")
	delete(stats, "rejections_omitted")
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(path)
	if err != nil || loaded.Scan != nil || len(loaded.Rejections) != 0 || len(loaded.Stats.RejectionReasons) != 0 {
		t.Fatalf("legacy report: %+v, %v", loaded, err)
	}
}

func TestReportRejectsInvalidDiagnostics(t *testing.T) {
	r, _ := testReport(t)
	r.Scan = &ScanSettings{Limit: 100, MaxBytes: 49152}
	r.Rejections = []Rejection{{SessionID: id("a"), Start: 1, End: 2, Stage: "too_large", Reason: "size_limit"}}
	r.Stats.RejectionReasons = map[string]int{"too_large/size_limit": 1}
	original, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*Report)
	}{
		{"zero limit", func(r *Report) { r.Scan.Limit = 0 }},
		{"excess limit", func(r *Report) { r.Scan.Limit = maxCandidates + 1 }},
		{"zero bytes", func(r *Report) { r.Scan.MaxBytes = 0 }},
		{"excess bytes", func(r *Report) { r.Scan.MaxBytes = maxTurnBytes + 1 }},
		{"bad date", func(r *Report) { r.Scan.Since = "2026-01-02" }},
		{"secret provider", func(r *Report) { r.Scan.SourceProvider = "password=abc12345" }},
		{"secret model", func(r *Report) { r.Scan.SourceModel = "password=abc12345" }},
		{"bad id", func(r *Report) { r.Rejections[0].SessionID = "filename.json" }},
		{"missing id", func(r *Report) { r.Rejections[0].SessionID = "" }},
		{"bad stage", func(r *Report) { r.Rejections[0].Stage = "raw error" }},
		{"bad reason", func(r *Report) { r.Rejections[0].Reason = "raw error" }},
		{"bad range", func(r *Report) { r.Rejections[0].End = 0 }},
		{"empty turn range", func(r *Report) { r.Rejections[0].End = 1 }},
		{"negative context", func(r *Report) { r.Rejections[0].ContextBytes = -1 }},
		{"negative turn", func(r *Report) { r.Rejections[0].TurnBytes = -1 }},
		{"negative feedback", func(r *Report) { r.Rejections[0].FeedbackBytes = -1 }},
		{"bad reason key", func(r *Report) { r.Stats.RejectionReasons["too_large/raw error"] = 1 }},
		{"negative reason count", func(r *Report) { r.Stats.RejectionReasons["too_large/size_limit"] = -1 }},
		{"negative omitted", func(r *Report) { r.Stats.RejectionsOmitted = -1 }},
		{"excess references", func(r *Report) { r.Rejections = make([]Rejection, maxRejections+1) }},
	}
	path := filepath.Join(t.TempDir(), "report.json")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var clone Report
			if err := json.Unmarshal(original, &clone); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&clone)
			data, err := json.Marshal(clone)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("loaded invalid diagnostics")
			}
			if err := Save(path, &clone); err == nil {
				t.Fatal("saved invalid diagnostics")
			}
		})
	}
	for _, field := range []string{`"scan":{"limit":100,"max_bytes":49152,"injected":"secret"}`, `"rejections":[{"session_id":"aaaaaaaaaaaaaaaaaaaaaaaa","start":0,"end":1,"stage":"too_large","reason":"size_limit","raw_error":"secret"}]`} {
		data := strings.Replace(string(original), "\"scan\":{\"limit\":100,\"max_bytes\":49152}", field, 1)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted injected field: %s", field)
		}
	}
}

func TestMarkdownDiagnosticsAreBoundedAndEscaped(t *testing.T) {
	r, _ := testReport(t)
	r.Scan = &ScanSettings{Limit: 100, MaxBytes: 49152, SourceProvider: "provider\n# injected", SourceModel: "model[link]"}
	r.Candidates[0].Messages[0].Content = "PRIVATE_REJECTED_TRANSCRIPT"
	r.Stats.RejectionReasons = map[string]int{"too_large/size_limit": 21, "invalid_turn/image": 2}
	r.Stats.RejectionsOmitted = 3
	for i := 0; i < 21; i++ {
		r.Rejections = append(r.Rejections, Rejection{SessionID: id("a"), Start: i, End: i + 1, Stage: "too_large", Reason: "size_limit", ContextBytes: 10, TurnBytes: 20, FeedbackBytes: 30})
	}
	text := Markdown(r)
	for _, want := range []string{"provider \\# injected", `model\[link\]`, "bytes: context 10, turn 20, feedback 30", "Stored rejection references not shown: 1; references not stored: 3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "PRIVATE_REJECTED_TRANSCRIPT") || strings.Contains(text, "\n# injected") || strings.Contains(text, "messages [20, 21)") {
		t.Fatalf("unsafe or unbounded markdown: %s", text)
	}
	first, second := strings.Index(text, "invalid\\_turn/image: 2"), strings.Index(text, "too\\_large/size\\_limit: 21")
	if first < 0 || second < 0 || first > second {
		t.Fatal("reason counts are missing or not sorted")
	}

	// Even an unvalidated in-memory report must not render raw rejection data.
	r.Rejections[0].SessionID = "PRIVATE_FILENAME"
	r.Rejections[0].Stage = "PRIVATE_ERROR"
	r.Rejections[0].Reason = "PRIVATE_ARGUMENT"
	r.Stats.RejectionReasons["PRIVATE_ERROR/PRIVATE_ARGUMENT"] = 1
	text = Markdown(r)
	for _, private := range []string{"PRIVATE_FILENAME", "PRIVATE_ERROR", "PRIVATE_ARGUMENT"} {
		if strings.Contains(text, private) {
			t.Fatalf("markdown included %s", private)
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
