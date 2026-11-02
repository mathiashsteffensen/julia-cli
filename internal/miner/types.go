// Package miner selects and reviews saved conversations for supervised fine-tuning.
// Source sessions are read-only. Reports carry the evidence and human decisions;
// only explicitly accepted examples can enter an exported dataset.
package miner

import (
	"time"

	"julia/internal/core"
)

const Version = 1

// Report is a portable snapshot of a mining run, not a live view of the sessions.
// Tools are today's tool definitions, not historical definitions: sessions do not
// save their system instructions or tool schemas.
type Report struct {
	Version         int                   `json:"version"`
	CreatedAt       time.Time             `json:"created_at"`
	SourceDirectory string                `json:"source_directory"`
	Workspace       string                `json:"workspace,omitempty"`
	Judge           string                `json:"judge,omitempty"`
	Focus           string                `json:"focus"`
	Tools           []core.ToolDefinition `json:"tools"`
	Stats           ScanStats             `json:"stats"`
	Scan            *ScanSettings         `json:"scan,omitempty"`
	Rejections      []Rejection           `json:"rejections,omitempty"`
	Candidates      []Candidate           `json:"candidates"`
	Usage           core.TokenUsage       `json:"usage"`
}

type ScanStats struct {
	Sessions          int            `json:"sessions"`
	Turns             int            `json:"turns"`
	Duplicates        int            `json:"duplicates"`
	Omitted           int            `json:"omitted_by_limit"`
	Skipped           map[string]int `json:"skipped"`
	RejectionReasons  map[string]int `json:"rejection_reasons,omitempty"`
	RejectionsOmitted int            `json:"rejections_omitted,omitempty"`
}

// ScanSettings records effective limits and source filters. Workspace and focus
// already live in the report header. Older reports have no settings or diagnostics.
type ScanSettings struct {
	Limit          int    `json:"limit"`
	MaxBytes       int    `json:"max_bytes"`
	SourceProvider string `json:"source_provider,omitempty"`
	SourceModel    string `json:"source_model,omitempty"`
	Since          string `json:"since,omitempty"`
}

// Rejection records only a source reference, fixed reason codes and byte counts.
// Never add transcript excerpts, tool arguments, filenames or raw errors here.
// Start/End is a half-open message range; both are zero for a session-level skip.
type Rejection struct {
	SessionID     string `json:"session_id,omitempty"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	Stage         string `json:"stage"`
	Reason        string `json:"reason"`
	ContextBytes  int    `json:"context_bytes,omitempty"`
	TurnBytes     int    `json:"turn_bytes,omitempty"`
	FeedbackBytes int    `json:"feedback_bytes,omitempty"`
}

type Source struct {
	SessionID string    `json:"session_id"`
	GroupID   string    `json:"group_id"`
	Title     string    `json:"title"`
	Directory string    `json:"directory"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Start     int       `json:"start"` // Zero-based, inclusive message offset.
	End       int       `json:"end"`   // Exclusive message offset.
	UpdatedAt time.Time `json:"updated_at"`
}

// Candidate is one reviewable turn. Messages is the turn itself, the only
// training target. Context is the session's earlier turns, which the model saw
// before it; an export trains none of it.
type Candidate struct {
	ID         string         `json:"id"`
	Source     Source         `json:"source"`
	Context    []core.Message `json:"context,omitempty"`
	Messages   []core.Message `json:"messages"`
	Feedback   string         `json:"following_user_feedback,omitempty"`
	Signals    []string       `json:"signals"`
	Priority   int            `json:"priority"`
	Assessment *Assessment    `json:"assessment,omitempty"`
	JudgeError string         `json:"judge_error,omitempty"`
	Review     Review         `json:"review"`
}

// Assessment is advisory model output, never authority to export an example.
// Missing context, sensitive content and a proposed repair all block export.
type Assessment struct {
	Summary        string   `json:"summary"`
	Category       string   `json:"category"`
	Recommendation string   `json:"recommendation"` // keep, reject, needs_correction
	Reason         string   `json:"reason"`
	Evidence       []string `json:"evidence"`
	Initiative     int      `json:"initiative"` // 1–5
	Completion     int      `json:"completion"`
	Restraint      int      `json:"restraint"`
	MissingContext bool     `json:"missing_context"`
	Sensitive      bool     `json:"sensitive"`
}

type Review struct {
	Decision string `json:"decision,omitempty"` // keep, reject, defer, or empty
	Rank     int    `json:"rank,omitempty"`     // 1–5 for kept examples
}

const DefaultFocus = "Complete the user's actual request with bounded initiative: use necessary permitted tools without redundant permission questions; answer from evidence; ask only for a consequential missing choice; respect denied actions; stop when done or genuinely blocked."

// ExportBlock explains why a card cannot be accepted for supervised training.
// A human may disagree with a judge's rejection, but cannot silently export a
// transcript flagged as incomplete, sensitive, or needing a rewritten target.
func ExportBlock(c Candidate) string {
	if c.Assessment == nil {
		return "Judge this example before accepting it."
	}
	if c.Assessment.Sensitive {
		return "The judge flagged sensitive content. Exclude this example."
	}
	if c.Assessment.MissingContext {
		return "The example needs context that is not in its transcript."
	}
	if c.Assessment.Recommendation == "needs_correction" {
		return "This example needs a corrected target; original failures are not training targets."
	}
	return ""
}

// Excluded reports a judged card that no review decision can export. An
// unjudged card is not excluded: it can still be judged.
func Excluded(c Candidate) bool {
	return c.Assessment != nil && ExportBlock(c) != ""
}
