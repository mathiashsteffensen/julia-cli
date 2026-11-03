package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"julia/internal/core"
	"julia/internal/miner"
)

// A synthetic report by default. Set JULIA_MINING_BENCH_REPORT to measure a real
// one: the Save these benchmarks use does nothing, so it stays unchanged.
func benchmarkMiningReport(b *testing.B) *miner.Report {
	b.Helper()
	if path := os.Getenv("JULIA_MINING_BENCH_REPORT"); path != "" {
		report, err := miner.Load(path)
		if err != nil {
			b.Fatal(err)
		}
		return report
	}

	output := strings.Repeat("func return package import error context session report candidate\n", 30)
	report := &miner.Report{}
	for s := range 50 {
		var earlier []core.Message
		for t := range 30 {
			callID := fmt.Sprintf("call-%d-%d", s, t)
			messages := []core.Message{
				{Role: "user", Content: "Inspect the file and explain what it does."},
				{Role: "assistant", ToolCalls: []core.ToolCall{{ID: callID, Name: "Read", Arguments: json.RawMessage(`{"path":"store.go"}`)}}},
				{Role: "tool", ToolCallID: callID, Content: output},
				{Role: "assistant", Content: "It saves the report atomically."},
			}
			report.Candidates = append(report.Candidates, miner.Candidate{
				ID: fmt.Sprintf("%d-%d", s, t), Context: earlier, Messages: messages,
				Assessment: &miner.Assessment{Recommendation: "keep", Initiative: 4, Completion: 4, Restraint: 4},
			})
			earlier = append(earlier[:len(earlier):len(earlier)], messages...)
		}
	}
	return report
}

func benchmarkMiningReview(b *testing.B) *miningReview {
	b.Helper()
	m := newMiningReview(MiningReviewConfig{Report: benchmarkMiningReport(b), Save: func() error { return nil }})
	m.Update(tea.WindowSizeMsg{Width: 142, Height: 44})
	return m
}

// BenchmarkMiningReviewNext moves to the next card: the same rebuild a decision
// does after its save.
func BenchmarkMiningReviewNext(b *testing.B) {
	m := benchmarkMiningReview(b)
	for b.Loop() {
		m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
}

func BenchmarkMiningReviewScrollAndView(b *testing.B) {
	m := benchmarkMiningReview(b)
	for b.Loop() {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		_ = m.View()
	}
}
