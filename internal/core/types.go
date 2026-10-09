package core

import (
	"context"
	_ "embed"
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

type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ModelOption describes a provider-advertised model and its reasoning controls.
type ModelOption struct {
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
	Messages     []Message       `json:"messages"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Event is the single provider-independent stream consumed by the UI and headless CLI.
// Kinds: status, text, tool_start, tool_end, usage, warning.
type Event struct {
	Kind         string      `json:"kind"`
	Text         string      `json:"text,omitempty"`
	Call         *ToolCall   `json:"call,omitempty"`
	Result       *ToolResult `json:"result,omitempty"`
	InputTokens  int         `json:"input_tokens,omitempty"`
	OutputTokens int         `json:"output_tokens,omitempty"`
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
	Run(context.Context, *Session, string, ToolExecutor, Emit) error
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

// SystemPrompt is the shared baseline for every executing provider.
//
//go:embed system.md
var SystemPrompt string
