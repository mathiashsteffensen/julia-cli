package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestScanRejectionDiagnostics(t *testing.T) {
	unknown := turn("Read the file.", "read", `{"path":"file"}`)
	unknown[1].ToolCalls[0].Name = "RetiredTool"
	secret := turn("Read the file.", "read", `{"path":"file"}`)
	secret[2].Content = "password=private-value"
	emptyFinal := turn("Read the file.", "read", `{"path":"file"}`)
	emptyFinal[3].Content = ""

	cases := []struct {
		name, reason string
		messages     []core.Message
	}{
		{"unknown tool", "unknown_tool", unknown},
		{"secret", "secret_text", secret},
		{"empty final", "empty_assistant", emptyFinal},
		{"missing argument", "missing_argument", turn("Read.", "read", `{}`)},
		{"unexpected argument", "unexpected_argument", turn("Read.", "read", `{"path":"file","extra":"value"}`)},
		{"argument type", "argument_type", turn("Read.", "read", `{"path":123}`)},
		{"stable first failure", "unexpected_argument", turn("Read.", "read", `{"path":123,"extra":"value"}`)},
		{"argument enum", "argument_enum", turn("Read.", "read", `{"path":"file","options":{"mode":"three"}}`)},
		{"argument object", "arguments_not_object", turn("Read.", "read", `[]`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			messages := append(append([]core.Message(nil), tc.messages...),
				core.Message{Role: "user", Content: "Explain that."},
				core.Message{Role: "assistant", Content: "An explanation."})
			writeSession(t, dir, sessionFor(id("a"), messages))
			report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Candidates) != 0 || len(report.Rejections) != 2 {
				t.Fatalf("unexpected result: %+v", report)
			}
			for i, stage := range []string{"invalid_turn", "invalid_context"} {
				rejection := report.Rejections[i]
				if rejection.SessionID != id("a") || rejection.Stage != stage || rejection.Reason != tc.reason {
					t.Fatalf("rejection %d: %+v", i, rejection)
				}
				if report.Stats.Skipped[stage] != 1 || report.Stats.RejectionReasons[stage+"/"+tc.reason] != 1 {
					t.Fatalf("counts: %+v", report.Stats)
				}
			}
			if report.Rejections[0].Start != 0 || report.Rejections[0].End != 4 ||
				report.Rejections[1].Start != 4 || report.Rejections[1].End != 6 {
				t.Fatalf("incorrect source ranges: %+v", report.Rejections)
			}
		})
	}
}

func TestScanSizeComparisonPreservesPrivacy(t *testing.T) {
	dir := t.TempDir()
	first := turn("Inspect parser.go.", "first", `{"path":"parser.go"}`)
	first[2].Content = strings.Repeat("x", 100*1024)
	second := turn("Inspect parser_test.go.", "second", `{"path":"parser_test.go"}`)
	second[2].Content = strings.Repeat("y", 80*1024)
	writeSession(t, dir, sessionFor(id("a"), append(first, second...)))

	private := sessionFor(id("b"), turn("Read the issue.", "issue", `{"path":"unused"}`))
	private.Messages[1].ToolCalls[0].Name = "Linear"
	writeSession(t, dir, private)
	child := sessionFor(id("c"), turn("Fix the code.", "read", `{"path":"code.go"}`))
	child.ParentID = private.ID
	writeSession(t, dir, child)
	secret := sessionFor(id("d"), turn("Read config.", "config", `{"path":"config"}`))
	secret.Messages[2].Content = "api_key=PRIVATE_SENTINEL"
	writeSession(t, dir, secret)

	for i, size := range []int{48 * 1024, 128 * 1024, 256 * 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			report, err := Scan(context.Background(), dir, ScanOptions{MaxBytes: size}, testTools)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Candidates) != i || report.Stats.Skipped["too_large"] != 2-i {
				t.Fatalf("size comparison: %+v, candidates=%d", report.Stats, len(report.Candidates))
			}
			if report.Scan.MaxBytes != size || report.Scan.Limit != defaultLimit {
				t.Fatalf("effective settings: %+v", report.Scan)
			}
			if report.Stats.Skipped["private_session"] != 2 || report.Stats.Skipped["invalid_turn"] != 1 {
				t.Fatalf("size limit weakened privacy: %+v", report.Stats)
			}
			for _, rejection := range report.Rejections {
				if rejection.Stage == "too_large" {
					if rejection.Reason != "size_limit" || rejection.ContextBytes+rejection.TurnBytes+rejection.FeedbackBytes <= size {
						t.Fatalf("missing size evidence: %+v", rejection)
					}
					if rejection.Start == 4 && rejection.ContextBytes < 100*1024 {
						t.Fatalf("earlier context was not counted: %+v", rejection)
					}
				}
			}
			for _, candidate := range report.Candidates {
				if candidate.Source.SessionID != id("a") {
					t.Fatalf("private candidate survived: %s", candidate.Source.SessionID)
				}
			}
		})
	}
}

func TestScanDiagnosticsNeverCopyRejectedText(t *testing.T) {
	dir := t.TempDir()
	const sentinel = "PRIVATE_REJECTION_SENTINEL"
	unknown := turn("Read.", "read", `{"path":"file"}`)
	unknown[1].ToolCalls[0].Name = sentinel
	extra := turn("Read.", "read", `{"path":"file","PRIVATE_REJECTION_SENTINEL":"value"}`)
	secret := turn("Read.", "read", `{"path":"file"}`)
	secret[2].Content = "password=" + sentinel
	for i, messages := range [][]core.Message{unknown, extra, secret} {
		session := sessionFor(id(fmt.Sprint(i+1)), messages)
		session.Title = sentinel
		writeSession(t, dir, session)
	}
	if err := os.WriteFile(filepath.Join(dir, sentinel+".json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Scan(context.Background(), dir, ScanOptions{}, testTools)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Candidates) != 0 || len(report.Rejections) != 4 || strings.Contains(string(data), sentinel) || strings.Contains(Markdown(report), sentinel) {
		t.Fatalf("diagnostics leaked rejected text or lost rejections: %s", data)
	}
}

func TestRejectionReferencesAreBoundedButCountsAreComplete(t *testing.T) {
	report := &Report{Stats: ScanStats{Skipped: map[string]int{}}}
	rejection := Rejection{SessionID: id("a"), Start: 0, End: 2, Stage: "invalid_turn", Reason: "image"}
	for range maxRejections + 3 {
		report.reject(rejection)
	}
	if len(report.Rejections) != maxRejections || report.Stats.RejectionsOmitted != 3 ||
		report.Stats.Skipped["invalid_turn"] != maxRejections+3 ||
		report.Stats.RejectionReasons["invalid_turn/image"] != maxRejections+3 {
		t.Fatalf("incorrect bounded diagnostics: %+v", report.Stats)
	}
}

func TestScanRecordsEffectiveFilters(t *testing.T) {
	options := ScanOptions{Workspace: "/work", Provider: "test", Model: "m", Limit: 12, MaxBytes: 12345, Since: sessionFor(id("a"), nil).UpdatedAt}
	report, err := Scan(context.Background(), t.TempDir(), options, testTools)
	if err != nil {
		t.Fatal(err)
	}
	want := &ScanSettings{Limit: 12, MaxBytes: 12345, SourceProvider: "test", SourceModel: "m", Since: "2026-01-01T00:00:00Z"}
	if !reflect.DeepEqual(report.Scan, want) {
		t.Fatalf("settings = %+v, want %+v", report.Scan, want)
	}
}
