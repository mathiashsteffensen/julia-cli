package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"julia/internal/core"
	"julia/internal/miner"
)

func miningTestReport() *miner.Report {
	return &miner.Report{Candidates: []miner.Candidate{
		{ID: "one", Assessment: &miner.Assessment{Recommendation: "keep"}},
		{ID: "two", Assessment: &miner.Assessment{Recommendation: "reject"}},
		{ID: "three", Assessment: &miner.Assessment{Recommendation: "keep"}},
	}}
}

func miningKey(m *miningReview, code rune) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return cmd
}

func TestMiningDecisionsPersistAndAdvance(t *testing.T) {
	report := miningTestReport()
	var saves [][]miner.Review
	m := newMiningReview(MiningReviewConfig{Report: report, Save: func() error {
		var reviews []miner.Review
		for _, c := range report.Candidates {
			reviews = append(reviews, c.Review)
		}
		saves = append(saves, reviews)
		return nil
	}})
	miningKey(m, '1')
	miningKey(m, '5')
	miningKey(m, 'r')
	if len(saves) != 3 || saves[0][0] != (miner.Review{Decision: "keep", Rank: 1}) ||
		saves[1][1] != (miner.Review{Decision: "keep", Rank: 5}) ||
		saves[2][2] != (miner.Review{Decision: "reject"}) {
		t.Fatalf("persisted reviews: %+v", saves)
	}
	if m.selected != 2 {
		t.Fatalf("all decided: selection = %d, want last card", m.selected)
	}
	miningKey(m, 'u')
	if m.selected != 2 || report.Candidates[2].Review != (miner.Review{}) {
		t.Fatalf("clear moved or retained review: %d %+v", m.selected, report.Candidates[2].Review)
	}
	miningKey(m, tea.KeyLeft)
	miningKey(m, 'd')
	if m.selected != 2 || report.Candidates[1].Review != (miner.Review{Decision: "defer"}) {
		t.Fatalf("defer did not advance to undecided: %d %+v", m.selected, report.Candidates[1].Review)
	}
	miningKey(m, tea.KeyRight)
	if m.selected != 0 {
		t.Fatalf("next did not wrap: %d", m.selected)
	}
	miningKey(m, 'r')
	if m.selected != 2 {
		t.Fatalf("did not wrap to next undecided: %d", m.selected)
	}
}

func TestMiningSaveFailureRollsBack(t *testing.T) {
	report := miningTestReport()
	report.Candidates[0].Review = miner.Review{Decision: "keep", Rank: 4}
	calls := 0
	m := newMiningReview(MiningReviewConfig{Report: report, Save: func() error {
		calls++
		if report.Candidates[0].Review.Decision != "reject" {
			t.Fatal("save did not receive working decision")
		}
		return errors.New("disk full")
	}})
	m.selected = 0 // Revisit a previously accepted card.
	miningKey(m, 'r')
	if calls != 1 || m.selected != 0 || report.Candidates[0].Review != (miner.Review{Decision: "keep", Rank: 4}) {
		t.Fatalf("failed save changed selection or review: calls %d, selected %d, review %+v", calls, m.selected, report.Candidates[0].Review)
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "disk full") {
		t.Fatal("save failure not visible")
	}
}

func TestMiningBlockedKeep(t *testing.T) {
	tests := []struct {
		name       string
		assessment *miner.Assessment
	}{
		{"unassessed", nil},
		{"sensitive", &miner.Assessment{Sensitive: true}},
		{"missing context", &miner.Assessment{MissingContext: true}},
		{"correction", &miner.Assessment{Recommendation: "needs_correction"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := miningTestReport()
			report.Candidates[0].Assessment = test.assessment
			calls := 0
			m := newMiningReview(MiningReviewConfig{Report: report, Save: func() error { calls++; return nil }})
			miningKey(m, '5')
			if calls != 0 || m.selected != 0 || report.Candidates[0].Review.Decision != "" || !strings.Contains(m.notice, "Cannot accept") {
				t.Fatalf("blocked keep was applied: calls %d, candidate %+v, notice %q", calls, report.Candidates[0], m.notice)
			}
		})
	}
}

func TestMiningExportRequiresConfirmation(t *testing.T) {
	report := miningTestReport()
	report.Candidates[0].Review = miner.Review{Decision: "keep", Rank: 5}
	exports := 0
	m := newMiningReview(MiningReviewConfig{Report: report, Export: func() (string, error) {
		exports++
		return "", errors.New("export failed on disk")
	}})
	miningKey(m, 'e')
	if !m.confirming || exports != 0 || !strings.Contains(ansi.Strip(m.viewport.View()), "Accepted: 1") {
		t.Fatalf("missing confirmation or counts: confirming %v exports %d view %s", m.confirming, exports, m.viewport.View())
	}
	miningKey(m, 'n')
	if exports != 0 || m.confirming {
		t.Fatal("cancel exported or left confirmation open")
	}
	miningKey(m, 'e')
	miningKey(m, 'y')
	if exports != 1 || m.confirming || !strings.Contains(ansi.Strip(m.View().Content), "export failed on disk") {
		t.Fatalf("export failure not visible: %d %q", exports, m.notice)
	}
	miningKey(m, 'e')
	if cmd := miningKey(m, 'q'); cmd == nil || exports != 1 {
		t.Fatal("quit exported or did not quit")
	}
}

func TestMiningTranscriptScrollsWithoutClipping(t *testing.T) {
	report := miningTestReport()
	var lines []string
	for i := range 70 {
		lines = append(lines, fmt.Sprintf("result-line-%03d", i))
	}
	report.Candidates[0].Messages = []core.Message{
		{Role: "assistant", Content: "before", ToolCalls: []core.ToolCall{{ID: "call-1", Name: "Bash", Arguments: json.RawMessage(`{"command":"echo sentinel-argument"}`)}}},
		{Role: "tool", ToolCallID: "call-1", Content: strings.Join(lines, "\n")},
	}
	report.Candidates[0].Feedback = "feedback-sentinel"
	m := newMiningReview(MiningReviewConfig{Report: report})
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 12})
	var seen strings.Builder
	for i := 0; i < 300; i++ {
		seen.WriteString(ansi.Strip(m.viewport.View()))
		seen.WriteByte('\n')
		if m.viewport.AtBottom() {
			break
		}
		miningKey(m, tea.KeyDown)
	}
	for _, want := range append([]string{"sentinel-argument", "Tool call: Bash", "NOT EXPORTED", "feedback-sentinel"}, lines...) {
		if !strings.Contains(seen.String(), want) {
			t.Errorf("scrolling missed %q", want)
		}
	}
	if !m.viewport.AtBottom() {
		t.Fatal("did not reach bottom")
	}
	miningKey(m, tea.KeyPgUp)
	if m.viewport.AtBottom() {
		t.Fatal("page up did not scroll")
	}
}

func TestMiningNarrowAndSanitized(t *testing.T) {
	report := miningTestReport()
	report.Candidates[0].Source.Title = "title\x1b]52;c;bad\x07safe"
	report.Candidates[0].Messages = []core.Message{{Role: "user", Content: strings.Repeat("x", 100) + " tail"}}
	m := newMiningReview(MiningReviewConfig{Report: report})
	for _, size := range [][2]int{{1, 1}, {3, 3}, {10, 8}, {20, 12}, {40, 20}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View().Content
		lines := strings.Split(view, "\n")
		if len(lines) > size[1] {
			t.Errorf("%v: %d lines", size, len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Errorf("%v: line width %d: %q", size, ansi.StringWidth(line), line)
			}
		}
		if strings.Contains(view, "\x1b]52") {
			t.Fatalf("unsafe control sequence in view: %q", view)
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 15})
	var seen strings.Builder
	for i := 0; i < 100; i++ {
		seen.WriteString(m.viewport.View())
		if m.viewport.AtBottom() {
			break
		}
		miningKey(m, tea.KeyDown)
	}
	if strings.Contains(seen.String(), "\x1b]52") || !strings.Contains(ansi.Strip(seen.String()), "titlesafe") {
		t.Fatalf("title was not sanitized: %q", seen.String())
	}
}

func TestMiningNilAndEmptyReports(t *testing.T) {
	if err := RunMiningReview(context.Background(), MiningReviewConfig{}); err == nil {
		t.Fatal("nil report accepted")
	}
	m := newMiningReview(MiningReviewConfig{Report: &miner.Report{}})
	for _, key := range []rune{'1', 'r', 'd', 'u', 'n', 'p'} {
		miningKey(m, key)
	}
	if !strings.Contains(ansi.Strip(m.viewport.View()), "No candidates") {
		t.Fatalf("empty report: %s", m.viewport.View())
	}
	miningKey(m, 'e')
	miningKey(m, 'y')
	if !strings.Contains(m.notice, "unavailable") {
		t.Fatalf("missing callback: %s", m.notice)
	}
}

func TestMiningMissingSaveDoesNotChangeDecision(t *testing.T) {
	report := miningTestReport()
	m := newMiningReview(MiningReviewConfig{Report: report})
	miningKey(m, '3')
	if report.Candidates[0].Review.Decision != "" || !strings.Contains(m.notice, "Save unavailable") {
		t.Fatalf("unsaved decision: %+v %q", report.Candidates[0].Review, m.notice)
	}
}

func TestMiningDurabilityFailureKeepsCommittedVote(t *testing.T) {
	report := miningTestReport()
	m := newMiningReview(MiningReviewConfig{Report: report, Save: func() error {
		return &miner.DurabilityError{Err: errors.New("sync failed")}
	}})
	miningKey(m, '5')
	if report.Candidates[0].Review != (miner.Review{Decision: "keep", Rank: 5}) || m.selected != 0 || !strings.Contains(m.notice, "replaced on disk") {
		t.Fatalf("committed vote rolled back: %+v %s", report.Candidates[0].Review, m.notice)
	}
}

func TestMiningReviewResumesAtFirstOpenCard(t *testing.T) {
	report := miningTestReport()
	report.Candidates[0].Review = miner.Review{Decision: "reject"}
	report.Candidates[1].Review = miner.Review{Decision: "defer"}
	m := newMiningReview(MiningReviewConfig{Report: report})
	if m.selected != 2 {
		t.Fatalf("resume selection %d, want 2", m.selected)
	}
}
