package core

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"time"
)

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is what a tool returns. A tool that produces a picture, such as a
// screenshot, returns it in Images, prepared by images.Prepare or images.Load,
// and the model sees it with Content. Providers send at most MaxToolImages, and
// none with an error; Sendable enforces that.
//
// Image bytes travel in memory only. The JSON form of a ToolResult is a record,
// as audit logs, tool journals and headless events keep it: each image appears
// as its ImageInfo, so a decoded result's images describe but carry no data.
// Transcripts keep Transcript's text, which names each image in a marker line.
type ToolResult struct {
	Content string  `json:"content"`
	IsError bool    `json:"is_error,omitempty"`
	Images  []Image `json:"images,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Images     []Image    `json:"images,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Image is a picture the user attached to a message, or a tool returned, already
// encoded in a format every provider accepts. Width and Height are its pixel size.
type Image struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// DataURL is the image as a base64 data: URL.
func (i Image) DataURL() string {
	return "data:" + i.MediaType + ";base64," + base64.StdEncoding.EncodeToString(i.Data)
}

// Prompt is one message the user sends: its text and any attached images. The
// text names each image by an [Image #N] token, numbered in the order of Images.
type Prompt struct {
	Text   string
	Images []Image
}

// ModelOption describes a provider-advertised model and its reasoning controls.
type ModelOption struct {
	// Provider is the model's provider, such as codex or compatible.
	Provider              string
	ID, Name, Description string
	ReasoningEfforts      []string
	DefaultReasoning      string
	IsDefault             bool
}

// WeeklyUsage is a provider's current weekly allowance, separate from per-turn
// token usage. A zero reset time means the provider did not supply one.
type WeeklyUsage struct {
	RemainingPercent float64
	ResetsAt         time.Time
}

type Session struct {
	// Instructions is rebuilt from the current environment and user skills per turn.
	// It is deliberately excluded from saved sessions.
	Instructions string          `json:"-"`
	ID           string          `json:"id"`
	ParentID     string          `json:"parent_id,omitempty"`
	Title        string          `json:"title"`
	Directory    string          `json:"directory"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Reasoning    string          `json:"reasoning,omitempty"`
	ReasoningSet bool            `json:"reasoning_set,omitempty"`
	RemoteID     string          `json:"remote_id,omitempty"`
	State        json.RawMessage `json:"state,omitempty"`
	PendingTool  *ToolCall       `json:"pending_tool,omitempty"`
	Audit        []AuditEntry    `json:"audit,omitempty"`
	Usage        SessionUsage    `json:"usage"`
	Messages     []Message       `json:"messages"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Event is the single provider-independent stream consumed by the UI and headless CLI.
// Kinds: status, text, tool_start, tool_end, usage, warning.
//
// A usage event normally describes one model call. InputTokens is the whole
// prompt sent (uncached, cache reads and cache writes together);
// CachedInputTokens and CacheWriteTokens are the portions of it served from, or
// written to, the provider's prompt cache. ContextTokens is the estimated
// context-window occupancy once the call's output is part of the conversation;
// zero leaves the previous estimate unchanged, so a call whose provider reported
// no usage carries only that. An Aggregate event instead sums every call a
// finished subagent made. Summing the usage events of a run gives what the run
// added to the session's totals.
type Event struct {
	Kind              string      `json:"kind"`
	Text              string      `json:"text,omitempty"`
	Call              *ToolCall   `json:"call,omitempty"`
	Result            *ToolResult `json:"result,omitempty"`
	InputTokens       int         `json:"input_tokens,omitempty"`
	CachedInputTokens int         `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  int         `json:"cache_write_tokens,omitempty"`
	OutputTokens      int         `json:"output_tokens,omitempty"`
	ContextTokens     int         `json:"context_tokens,omitempty"`
	Aggregate         bool        `json:"aggregate,omitempty"`
}

// DefaultContextWindow is the context size, in tokens, assumed for every model:
// providers rarely report one, so the harness budgets against this fixed window.
const DefaultContextWindow = 256_000

// CompactionThreshold is the estimated context size at which an API conversation
// is summarized. The remaining headroom absorbs the next response, tool results
// and estimation error. It does not guarantee the summarization request fits:
// that request re-encodes the whole history, so the provider sizes it separately.
func CompactionThreshold(window int) int {
	if window <= 0 {
		window = DefaultContextWindow
	}
	return window / 100 * 85
}

// TokenUsage counts tokens by billing class. The classes are disjoint, so Prompt
// is the full size of what was sent to the model.
type TokenUsage struct {
	Input      int `json:"input"`       // prompt tokens processed without a cache
	CacheRead  int `json:"cache_read"`  // prompt tokens served from the provider's cache
	CacheWrite int `json:"cache_write"` // prompt tokens written to the provider's cache
	Output     int `json:"output"`
}

// Prompt is every input token sent to the model, cached or not.
func (u TokenUsage) Prompt() int { return u.Input + u.CacheRead + u.CacheWrite }

func (u TokenUsage) IsZero() bool { return u == TokenUsage{} }

func (u *TokenUsage) Add(o TokenUsage) {
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Output += o.Output
}

// SessionUsage is the token accounting persisted with a session. Total is
// cumulative over every model call made for the session, including context
// compaction and subagents. Context is the latest estimate of the live
// conversation's size in tokens, which can shrink when history is compacted.
type SessionUsage struct {
	Total   TokenUsage `json:"total"`
	Context int        `json:"context"`
}

// UsageEvent reports one model call's usage; context is zero when unknown.
func UsageEvent(u TokenUsage, context int) Event {
	return Event{Kind: "usage", InputTokens: u.Prompt(), CachedInputTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite, OutputTokens: u.Output, ContextTokens: context}
}

// AggregateUsageEvent reports the combined spend of calls made elsewhere, such as
// a finished subagent's. It is no single call and says nothing about the live
// context, so consumers must not show it as the last call.
func AggregateUsageEvent(u TokenUsage) Event {
	e := UsageEvent(u, 0)
	e.Aggregate = true
	return e
}

// Usage returns a usage event's counts split into disjoint billing classes.
func (e Event) Usage() TokenUsage {
	cached, written := max(e.CachedInputTokens, 0), max(e.CacheWriteTokens, 0)
	return TokenUsage{Input: max(e.InputTokens-cached-written, 0), CacheRead: cached, CacheWrite: written, Output: max(e.OutputTokens, 0)}
}

// RecordUsage folds a usage event into the session's running totals. Other event
// kinds are ignored, so callers may pass every event they emit.
func (s *Session) RecordUsage(e Event) {
	if e.Kind != "usage" {
		return
	}
	s.Usage.Total.Add(e.Usage())
	if e.ContextTokens > 0 {
		s.Usage.Context = e.ContextTokens
	}
}

type Emit func(Event)

type AuditEntry struct {
	At    time.Time `json:"at"`
	Event Event     `json:"event"`
}

type ToolExecutor interface {
	Definitions() []ToolDefinition
	Execute(context.Context, ToolCall) ToolResult
}

// Backend owns provider conversation mechanics, never tool execution or UI state.
// Implementations must honor cancellation and return only after all callbacks stop.
type Backend interface {
	Name() string
	Run(context.Context, *Session, Prompt, ToolExecutor, Emit) error
}

type Approval struct {
	ExecutionMode string
	Directory     string
	Justification string
	Call          ToolCall
	Reason        string
	Network       bool
	Task          string
}

type Decision struct {
	Allow  bool
	Reason string
}

type Approver func(context.Context, Approval) (Decision, error)

// QuestionOption is one predefined answer the user can pick.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is a clarification the agent asks the human. With no Options the
// answer is free-form text. With Options the user picks one or writes their own.
type Question struct {
	Text    string           `json:"question"`
	Options []QuestionOption `json:"options,omitempty"`
}

// Answer is the human's reply to a Question. A picked option has Option set to
// its 1-based index and Text set to its label; a written answer has Option 0.
// Skipped means the user declined to answer; Text is then empty.
type Answer struct {
	Text    string
	Option  int
	Skipped bool
}

// Asker puts a Question in front of the human and waits. It honors cancellation.
type Asker func(context.Context, Question) (Answer, error)

// QuestionTool is the model-facing name of the tool that asks the user a question.
const QuestionTool = "AskUserQuestion"

// QuestionResult is the JSON object a QuestionTool call returns to the model,
// and the shape the UI and session history decode to show or replay an answer.
// Source is "option" (Option is its 1-based index) or "freeform". A skipped
// question has Skipped set, no Answer, and a Note telling the model how to proceed.
type QuestionResult struct {
	Answer  string `json:"answer,omitempty"`
	Source  string `json:"source,omitempty"`
	Option  int    `json:"option,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
	Note    string `json:"note,omitempty"`
}

// SystemPrompt is the shared baseline for every executing provider.
//
//go:embed system.md
var SystemPrompt string
