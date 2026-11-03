package miner

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"julia/internal/core"
)

// These benchmarks measure what a report costs a reviewer. By default they use a
// synthetic report the size of a long mining run. To measure a real one, set
// JULIA_MINING_BENCH_REPORT to its path: it is only read, and the benchmarks that
// write a report skip, so its transcripts stay in place.
const benchReportVariable = "JULIA_MINING_BENCH_REPORT"

var benchWords = strings.Fields(`func return package import error context session
	report candidate review decision message assistant result struct string range
	path file directory config handler request response value buffer index length
	the a that which when then because after before with without should must`)

func benchText(random *rand.Rand, size int) string {
	var text strings.Builder
	for text.Len() < size {
		text.WriteString(benchWords[random.IntN(len(benchWords))])
		if random.IntN(12) == 0 {
			text.WriteByte('\n')
		} else {
			text.WriteByte(' ')
		}
	}
	return text.String()
}

// benchTurn returns one turn of about size bytes: a request, several Read calls
// with their output, and a final answer.
func benchTurn(random *rand.Rand, callPrefix string, size int) []core.Message {
	const calls = 6
	messages := []core.Message{{Role: "user", Content: "Inspect the files and explain what they do."}}
	for i := range calls {
		callID := fmt.Sprintf("%s-%d", callPrefix, i)
		messages = append(messages,
			core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: callID, Name: "Read", Arguments: json.RawMessage(`{"path":"internal/miner/store.go"}`)}}},
			core.Message{Role: "tool", ToolCallID: callID, Content: benchText(random, size/calls)},
		)
	}
	return append(messages, core.Message{Role: "assistant", Content: "They save the report atomically."})
}

// syntheticReport has the shape a scan produces: each candidate is one turn and
// carries every earlier turn of its session, so one session's text repeats across
// its candidates.
func syntheticReport(sessions, turnsPerSession, turnBytes int) *Report {
	random := rand.New(rand.NewPCG(1, 2))
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	report := &Report{
		Version: Version, CreatedAt: created, SourceDirectory: "/state/julia/sessions",
		Focus: DefaultFocus, Tools: testTools, Stats: ScanStats{Skipped: map[string]int{}},
	}
	for s := range sessions {
		sessionID := fmt.Sprintf("%024x", s+1)
		var earlier []core.Message
		for t := range turnsPerSession {
			messages := benchTurn(random, fmt.Sprintf("call-%d-%d", s, t), turnBytes)
			report.Candidates = append(report.Candidates, Candidate{
				ID: fmt.Sprintf("%s-%d", sessionID, t),
				Source: Source{
					SessionID: sessionID, GroupID: sessionID, Title: "Benchmark session", Directory: "/work",
					Provider: "claude", Model: "claude-opus", Start: len(earlier), End: len(earlier) + len(messages), UpdatedAt: created,
				},
				Context:  earlier,
				Messages: messages,
				Signals:  []string{"tool_use"},
				Assessment: &Assessment{
					Summary: "Read the files and answered.", Category: "completion", Recommendation: "keep",
					Reason: "It answered from evidence.", Evidence: []string{"message 2"},
					Initiative: 4, Completion: 4, Restraint: 4,
				},
			})
			earlier = append(earlier[:len(earlier):len(earlier)], messages...)
		}
	}
	return report
}

// benchmarkReport returns the named report, or a synthetic one of about 45 MB.
func benchmarkReport(b *testing.B) *Report {
	b.Helper()
	if path := os.Getenv(benchReportVariable); path != "" {
		report, err := Load(path)
		if err != nil {
			b.Fatal(err)
		}
		return report
	}
	return syntheticReport(283, 2, 50_000)
}

func skipWhenReal(b *testing.B) {
	b.Helper()
	if os.Getenv(benchReportVariable) != "" {
		b.Skip("this benchmark writes a report; it runs only on synthetic data")
	}
}

func reportBytes(b *testing.B, report *Report) int64 {
	b.Helper()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	return int64(len(data))
}

// benchmarkReportFile returns a report on disk: the named one, or a synthetic one
// in a temporary directory.
func benchmarkReportFile(b *testing.B) string {
	b.Helper()
	if path := os.Getenv(benchReportVariable); path != "" {
		return path
	}
	path := filepath.Join(b.TempDir(), "report.json")
	if err := Save(path, benchmarkReport(b)); err != nil {
		b.Fatal(err)
	}
	return path
}

func BenchmarkReportValidate(b *testing.B) {
	report := benchmarkReport(b)
	b.SetBytes(reportBytes(b, report))
	for b.Loop() {
		if err := validateReport(report); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReportValidateProgress(b *testing.B) {
	report := benchmarkReport(b)
	for b.Loop() {
		if err := validateProgress(report); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReportMarshal(b *testing.B) {
	report := benchmarkReport(b)
	b.SetBytes(reportBytes(b, report))
	for b.Loop() {
		if _, err := json.MarshalIndent(report, "", "  "); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReportLoad(b *testing.B) {
	path := benchmarkReportFile(b)
	for b.Loop() {
		if _, err := Load(path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReportCheckFile(b *testing.B) {
	path := benchmarkReportFile(b)
	for b.Loop() {
		if err := CheckFile(path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReportWrite is the disk's share of a checkpoint: encode, write, flush
// and replace, with no validation.
func BenchmarkReportWrite(b *testing.B) {
	skipWhenReal(b)
	report := benchmarkReport(b)
	path := filepath.Join(b.TempDir(), "report.json")
	b.SetBytes(reportBytes(b, report))
	for b.Loop() {
		if err := writeReport(path, report, flushDirectory); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReviewDecision is the wait after each review key: one decision on the
// loaded report, then its checkpoint.
func BenchmarkReviewDecision(b *testing.B) {
	skipWhenReal(b)
	report := benchmarkReport(b)
	path := filepath.Join(b.TempDir(), "report.json")
	b.SetBytes(reportBytes(b, report))
	i := 0
	for b.Loop() {
		report.Candidates[i%len(report.Candidates)].Review = Review{Decision: "reject"}
		i++
		if err := Checkpoint(path, report); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReportShape logs counts and no content, so a run shows which shape its
// timings belong to.
func BenchmarkReportShape(b *testing.B) {
	report := benchmarkReport(b)
	contextBytes, messageBytes, largest, assessed, decided := 0, 0, 0, 0, 0
	for _, c := range report.Candidates {
		context, _ := json.Marshal(c.Context)
		messages, _ := json.Marshal(c.Messages)
		contextBytes += len(context)
		messageBytes += len(messages)
		largest = max(largest, len(context)+len(messages))
		if c.Assessment != nil {
			assessed++
		}
		if c.Review.Decision != "" {
			decided++
		}
	}
	b.Logf("candidates=%d assessed=%d decided=%d file=%dB context=%dB turns=%dB largest_card=%dB",
		len(report.Candidates), assessed, decided, reportBytes(b, report), contextBytes, messageBytes, largest)
	for b.Loop() {
	}
}
