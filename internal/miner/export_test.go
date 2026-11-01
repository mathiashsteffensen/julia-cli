package miner

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"julia/internal/core"
)

func exportTestReport() *Report {
	c := judgedCandidate("accepted")
	c.Assessment = judgedAssessment()
	c.Review = Review{Decision: "keep", Rank: 4}
	c.Feedback = "This following feedback must not become training input."
	return &Report{Version: Version, Focus: DefaultFocus, Candidates: []Candidate{c}}
}

func TestExportRequiresHumanAcceptanceAndKeepsProvenanceSeparate(t *testing.T) {
	report := exportTestReport()
	pending := judgedCandidate("pending")
	pending.Assessment = judgedAssessment()
	report.Candidates = append(report.Candidates, pending)
	before, _ := json.Marshal(report)
	out := filepath.Join(t.TempDir(), "dataset")
	result, err := Export(report, out, ExportOptions{MinRank: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Training != 1 || result.Validation != 0 {
		t.Fatalf("unexpected export counts %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(out, "training.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 || strings.Contains(string(data), "following feedback") || strings.Contains(string(data), "assessment") || strings.Contains(string(data), "source") {
		t.Fatalf("training row contains report data: %s", data)
	}
	var row trainingExample
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	if len(row.Messages) != 3 || row.Messages[0].Role != "system" || row.Messages[2].Content != "4." || row.Tools == nil {
		t.Fatalf("wrong training shape: %+v", row)
	}
	manifestData, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	var manifest exportManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil || len(manifest.Entries) != 1 || manifest.Entries[0].Source.SessionID == "" || manifest.Entries[0].Rank != 4 {
		t.Fatalf("missing provenance: %s, %v", manifestData, err)
	}
	for _, path := range []string{out, filepath.Join(out, "training.jsonl"), filepath.Join(out, "validation.jsonl"), filepath.Join(out, "manifest.json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("not private: %s %v", path, err)
		}
	}
	after, _ := json.Marshal(report)
	if string(before) != string(after) {
		t.Fatal("export changed source report")
	}
	if _, err := Export(report, out, ExportOptions{MinRank: 1}); err == nil {
		t.Fatal("export overwrote earlier dataset")
	}
}

func TestExportRejectsUnsafeOrUnreviewedTargetsWithoutCreatingOutput(t *testing.T) {
	for name, mutate := range map[string]func(*Candidate){
		"unjudged":      func(c *Candidate) { c.Assessment = nil },
		"repair":        func(c *Candidate) { c.Assessment.Recommendation = "needs_correction" },
		"context":       func(c *Candidate) { c.Assessment.MissingContext = true },
		"sensitive":     func(c *Candidate) { c.Assessment.Sensitive = true },
		"invalid rank":  func(c *Candidate) { c.Review.Rank = 6 },
		"invalid score": func(c *Candidate) { c.Assessment.Completion = 0 },
		"orphan tool": func(c *Candidate) {
			c.Messages[1] = core.Message{Role: "tool", ToolCallID: "absent", Content: "result"}
		},
		"pending":  func(c *Candidate) { c.Review = Review{} },
		"deferred": func(c *Candidate) { c.Review = Review{Decision: "defer"} },
	} {
		t.Run(name, func(t *testing.T) {
			report := exportTestReport()
			mutate(&report.Candidates[0])
			out := filepath.Join(t.TempDir(), "dataset")
			if _, err := Export(report, out, ExportOptions{MinRank: 1}); err == nil {
				t.Fatal("invalid export succeeded")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("failed export left an output directory")
			}
		})
	}
}

func TestExportToolCallsAndDeduplication(t *testing.T) {
	report := exportTestReport()
	report.Tools = []core.ToolDefinition{{Name: "Read", Description: "Read a file", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}, "additionalProperties": false,
	}}}
	c := &report.Candidates[0]
	c.Messages = []core.Message{
		{Role: "user", Content: "Read greeting.txt and tell me what it says."},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "provider-a", Name: "Read", Arguments: json.RawMessage("{ \"path\": \"greeting.txt\" }")}}},
		{Role: "tool", ToolCallID: "provider-a", Content: "Hello."},
		{Role: "assistant", Content: "It says Hello."},
	}
	other := *c
	other.ID = "duplicate"
	other.Messages = append([]core.Message(nil), c.Messages...)
	other.Messages[1].ToolCalls = []core.ToolCall{{ID: "provider-b", Name: "Read", Arguments: json.RawMessage("{\"path\":\"greeting.txt\"}")}}
	other.Messages[2].ToolCallID = "provider-b"
	report.Candidates = append(report.Candidates, other)
	out := filepath.Join(t.TempDir(), "dataset")
	result, err := Export(report, out, ExportOptions{MinRank: 1})
	if err != nil || result.Training != 1 {
		t.Fatalf("dedup failed: %+v %v", result, err)
	}
	data, _ := os.ReadFile(filepath.Join(out, "training.jsonl"))
	var row trainingExample
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	call := row.Messages[2].ToolCalls[0]
	if call.Type != "function" || call.ID != "call_1" || call.Function.Arguments != "{\"path\":\"greeting.txt\"}" || row.Messages[3].ToolCallID != call.ID || len(row.Tools) != 1 || row.Tools[0].Function.Name != "Read" {
		t.Fatalf("invalid tool encoding: %s", data)
	}
}

func TestExportSplitsWholeConversationGroups(t *testing.T) {
	group := func(validation bool) string {
		for i := 0; ; i++ {
			id := fmt.Sprintf("%024x", i)
			sum := sha256.Sum256([]byte(id))
			if (binary.BigEndian.Uint64(sum[:8])%100 < 20) == validation {
				return id
			}
		}
	}
	report := exportTestReport()
	report.Candidates = nil
	for i, root := range []string{group(false), group(true), group(true)} {
		c := judgedCandidate(fmt.Sprintf("candidate-%d", i))
		c.Source.GroupID = root
		c.Source.SessionID = fmt.Sprintf("%024x", i+100)
		c.Messages[0].Content = fmt.Sprintf("What is %d + 1?", i)
		c.Messages[1].Content = fmt.Sprint(i + 1)
		c.Assessment, c.Review = judgedAssessment(), Review{Decision: "keep", Rank: 5}
		report.Candidates = append(report.Candidates, c)
	}
	result, err := Export(report, filepath.Join(t.TempDir(), "dataset"), ExportOptions{MinRank: 1, ValidationPercent: 20})
	if err != nil || result.Training != 1 || result.Validation != 2 {
		t.Fatalf("group split %+v, %v", result, err)
	}
}

func TestExportAcceptsDirectoryWithTrailingSeparator(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "dataset")
	result, err := Export(exportTestReport(), directory+string(os.PathSeparator), ExportOptions{MinRank: 1})
	if err != nil || result.Directory != directory || result.Training != 1 {
		t.Fatalf("trailing separator: %+v %v", result, err)
	}
}
