package miner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"julia/internal/core"
)

const (
	defaultLimit    = 100
	defaultMaxBytes = 48 * 1024
	maxSessionBytes = 64 * 1024 * 1024
	maxCandidates   = 10000
	maxTurnBytes    = 16 * 1024 * 1024
)

type ScanOptions struct {
	Workspace, Provider, Model string
	Since                      time.Time
	Limit, MaxBytes            int
	Focus                      string // Defaults to DefaultFocus.
}

var sessionIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)
var imagePattern = regexp.MustCompile(`(?i)\[image(?:\s*#\d+|\s*:|\s+not sent:|\s+elided:)`)
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN (?:[A-Z0-9 ]* )?PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)\b(?:sk|pk|rk|ghp|gho|ghu|ghs|ghr|github_pat|xox[baprs]|glpat)[-_][a-z0-9_-]{12,}`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`(?i)\b(?:[a-z0-9]+[_-])*(?:password|passwd|pwd|api[_-]?key|api[_-]?token|access[_-]?token|token|secret(?:_key)?)\b["']?\s*[:=]\s*["']?[^\s"',;}]{4,}`),
}

// SensitiveText detects common secret formats. It is not exhaustive redaction.
func SensitiveText(s string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(s) {
			return true
		}
	}
	return false
}

// sensitiveJSON checks a value as judge prompts and exported rows carry it.
// JSON escaping turns a newline into \n and < into <, which can lengthen a
// short value past a secret pattern's minimum: token=$1 before a line break.
func sensitiveJSON(value any) bool {
	data, err := json.Marshal(value)
	return err != nil || SensitiveText(string(data))
}

// canonicalArguments re-encodes tool arguments compactly, with sorted keys and
// exact number literals. Deduplication, validation and export share this form.
func canonicalArguments(raw json.RawMessage) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// ValidateMessages checks a complete single turn against today's tool definitions.
// This is dataset hygiene, not a guarantee that content is safe or context-complete.
func ValidateMessages(messages []core.Message, definitions []core.ToolDefinition) error {
	if len(messages) < 2 || messages[0].Role != "user" || messages[len(messages)-1].Role != "assistant" {
		return invalidTranscript("incomplete_turn", "turn must start with a user and end with an assistant")
	}
	for i, m := range messages {
		if m.Role == "user" && i != 0 {
			return invalidTranscript("unexpected_user", "invalid user message")
		}
		if imagePattern.MatchString(m.Content) {
			return invalidTranscript("image", "image in turn")
		}
		if strings.Contains(strings.ToLower(m.Content), "[response interrupted]") {
			return invalidTranscript("interrupted", "interrupted turn")
		}
	}
	return validateTranscript(messages, definitions)
}

// validateContext checks the earlier turns a candidate carries as context.
// They are not training targets, so a steer, an interrupted reply or an image's
// marker line stays as the model saw it. The hygiene and tool schema checks
// still apply, and image bytes stay refused: see elideImages.
func validateContext(messages []core.Message, definitions []core.ToolDefinition) error {
	if len(messages) == 0 {
		return nil
	}
	if messages[0].Role != "user" {
		return invalidTranscript("invalid_context_start", "context must start with a user message")
	}
	return validateTranscript(messages, definitions)
}

// elideImages returns a turn as a candidate's context carries it: each image
// becomes the marker line transcripts already use. Its bytes stay out of the
// report, the judge's prompt and the dataset, while the judge can still see that
// an image was there and flag a turn that depends on it.
func elideImages(messages []core.Message) []core.Message {
	elided := make([]core.Message, len(messages))
	for i, m := range messages {
		for _, image := range m.Images {
			if m.Content != "" {
				m.Content += "\n"
			}
			m.Content += core.ImageMarker(image.Info())
		}
		m.Images = nil
		elided[i] = m
	}
	return elided
}

// validateTranscript checks the messages, tool calls and results of one or
// more turns: no image bytes or secrets, known public tools with valid
// arguments, and every call answered before the conversation moves on.
func validateTranscript(messages []core.Message, definitions []core.ToolDefinition) error {
	defs := make(map[string]core.ToolDefinition, len(definitions))
	for _, d := range definitions {
		if d.Name == "" {
			return invalidTranscript("invalid_tool_definitions", "empty tool name")
		}
		if _, ok := defs[d.Name]; ok {
			return invalidTranscript("invalid_tool_definitions", "duplicate tool definition")
		}
		defs[d.Name] = d
	}
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, m := range messages {
		if len(m.Images) != 0 {
			return invalidTranscript("image", "image in turn")
		}
		if SensitiveText(m.Content) || SensitiveText(m.ToolCallID) {
			return invalidTranscript("secret_text", "sensitive text")
		}
		switch m.Role {
		case "user":
			if len(pending) > 0 || len(m.ToolCalls) != 0 || m.ToolCallID != "" || strings.TrimSpace(m.Content) == "" {
				return invalidTranscript("unexpected_user", "invalid user message")
			}
		case "assistant":
			if len(pending) > 0 || m.ToolCallID != "" {
				return invalidTranscript("unresolved_call", "unresolved tool calls")
			}
			if len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) == "" {
				return invalidTranscript("empty_assistant", "invalid assistant final")
			}
			for _, call := range m.ToolCalls {
				if SensitiveText(call.ID) || SensitiveText(call.Name) {
					return invalidTranscript("secret_metadata", "sensitive tool metadata")
				}
				if call.ID == "" || seen[call.ID] {
					return invalidTranscript("invalid_call_id", "missing or duplicate tool call ID")
				}
				d, ok := defs[call.Name]
				if !ok || sensitiveTool(call.Name) {
					reason := "unknown_tool"
					if sensitiveTool(call.Name) {
						reason = "private_tool"
					}
					return invalidTranscript(reason, fmt.Sprintf("unknown or private tool: %s", call.Name))
				}
				if SensitiveText(string(call.Arguments)) {
					return invalidTranscript("secret_arguments", "sensitive tool arguments")
				}
				var args any
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					return invalidTranscript("invalid_arguments_json", "invalid tool arguments")
				}
				// An exported row carries the arguments as a JSON string, so they
				// are checked encoded once more as well.
				canonical, err := canonicalArguments(call.Arguments)
				if err != nil || SensitiveText(string(canonical)) || sensitiveJSON(string(canonical)) {
					return invalidTranscript("secret_arguments", "sensitive tool arguments")
				}
				if _, ok := args.(map[string]any); !ok {
					return invalidTranscript("arguments_not_object", "tool arguments must be an object")
				}
				if err := validateSchema(args, d.Parameters); err != nil {
					return fmt.Errorf("tool %s: %w", call.Name, err)
				}
				seen[call.ID], pending[call.ID] = true, true
			}
		case "tool":
			if !pending[m.ToolCallID] || len(m.ToolCalls) != 0 {
				return invalidTranscript("orphan_result", "orphan or duplicate tool result")
			}
			delete(pending, m.ToolCallID)
		default:
			return invalidTranscript("unexpected_role", "unexpected message role")
		}
	}
	if len(pending) != 0 || len(messages[len(messages)-1].ToolCalls) != 0 {
		return invalidTranscript("dangling_call", "dangling tool calls")
	}
	if sensitiveJSON(messages) {
		return invalidTranscript("secret_text", "sensitive text")
	}
	return nil
}

func privateSession(messages []core.Message) bool {
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if sensitiveTool(call.Name) {
				return true
			}
		}
	}
	return false
}

func sensitiveTool(name string) bool {
	n := strings.ToLower(name)
	for _, part := range []string{"mail", "slack", "discord", "telegram", "whatsapp", "messag", "email", "linear", "github"} {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func validateSchema(value any, schema map[string]any) error {
	if schema == nil {
		return nil
	}
	if enums, ok := schema["enum"].([]any); ok {
		v, _ := json.Marshal(value)
		matched := false
		for _, e := range enums {
			b, _ := json.Marshal(e)
			if string(b) == string(v) {
				matched = true
				break
			}
		}
		if !matched {
			return invalidTranscript("argument_enum", "value outside enum")
		}
	}
	if enums, ok := schema["enum"].([]string); ok {
		s, ok := value.(string)
		matched := false
		for _, e := range enums {
			if ok && s == e {
				matched = true
				break
			}
		}
		if !matched {
			return invalidTranscript("argument_enum", "value outside enum")
		}
	}
	if typ, ok := schema["type"].(string); ok {
		valid := false
		switch typ {
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "number":
			_, valid = value.(float64)
		case "integer":
			if n, ok := value.(float64); ok {
				valid = n == float64(int64(n))
			}
		case "null":
			valid = value == nil
		default:
			return invalidTranscript("unsupported_schema", fmt.Sprintf("unsupported schema type %q", typ))
		}
		if !valid {
			return invalidTranscript("argument_type", fmt.Sprintf("expected %s", typ))
		}
	}
	if obj, ok := value.(map[string]any); ok {
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if key, ok := r.(string); ok {
					if _, exists := obj[key]; !exists {
						return invalidTranscript("missing_argument", fmt.Sprintf("missing required property %s", key))
					}
				}
			}
		}
		if req, ok := schema["required"].([]string); ok {
			for _, key := range req {
				if _, exists := obj[key]; !exists {
					return invalidTranscript("missing_argument", fmt.Sprintf("missing required property %s", key))
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		// Report the same first failure when several properties are invalid.
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			v := obj[key]
			if raw, exists := props[key]; exists {
				if sub, ok := raw.(map[string]any); ok {
					if err := validateSchema(v, sub); err != nil {
						return fmt.Errorf("property %s: %w", key, err)
					}
				}
				continue
			}
			if allowed, ok := schema["additionalProperties"].(bool); ok && !allowed {
				return invalidTranscript("unexpected_argument", fmt.Sprintf("unexpected property %s", key))
			}
			if sub, ok := schema["additionalProperties"].(map[string]any); ok {
				if err := validateSchema(v, sub); err != nil {
					return err
				}
			}
		}
	}
	if arr, ok := value.([]any); ok {
		if sub, ok := schema["items"].(map[string]any); ok {
			for _, v := range arr {
				if err := validateSchema(v, sub); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type sessionMeta struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id"`
	Title     string    `json:"title"`
	Directory string    `json:"directory"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	UpdatedAt time.Time `json:"updated_at"`
	private   bool
}

// Index only call names; do not retain transcript bodies or decode image bytes
// while resolving privacy and parent relationships across the whole corpus.
type indexedSession struct {
	sessionMeta
	MemoryTaint []string `json:"memory_taint"`
	Messages    []struct {
		ToolCalls []struct {
			Name string `json:"name"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

func (s indexedSession) hasPrivateContent() bool {
	for _, tool := range s.MemoryTaint {
		if sensitiveTool(tool) {
			return true
		}
	}
	for _, message := range s.Messages {
		for _, call := range message.ToolCalls {
			if sensitiveTool(call.Name) {
				return true
			}
		}
	}
	return false
}

func readSession(ctx context.Context, path string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not regular")
	}
	if info.Size() > maxSessionBytes {
		return errors.New("session too large")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSessionBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxSessionBytes {
		return errors.New("session too large")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// Scan reads saved sessions without modifying them. Parent metadata is gathered
// before filtering so child groups do not depend on workspace/provider filters.
func Scan(ctx context.Context, directory string, options ScanOptions, definitions []core.ToolDefinition) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Limit == 0 {
		options.Limit = defaultLimit
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultMaxBytes
	}
	if options.Focus == "" {
		options.Focus = DefaultFocus
	}
	if options.Limit < 0 || options.Limit > maxCandidates || options.MaxBytes < 0 || options.MaxBytes > maxTurnBytes {
		return nil, errors.New("scan limits out of range")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	settings := &ScanSettings{
		Limit: options.Limit, MaxBytes: options.MaxBytes,
		SourceProvider: options.Provider, SourceModel: options.Model,
	}
	if !options.Since.IsZero() {
		settings.Since = options.Since.Format(time.RFC3339Nano)
	}
	r := &Report{
		Version: Version, CreatedAt: time.Now(), SourceDirectory: directory,
		Workspace: options.Workspace, Focus: options.Focus, Tools: definitions,
		Scan: settings, Stats: ScanStats{Skipped: map[string]int{}},
	}
	if err := validateReport(r); err != nil {
		return nil, err
	}
	metas := map[string]sessionMeta{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			r.reject(Rejection{Stage: "non_regular", Reason: "non_regular"})
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !sessionIDPattern.MatchString(id) {
			r.reject(Rejection{Stage: "invalid_session", Reason: "invalid_session_id"})
			continue
		}
		var indexed indexedSession
		if err := readSession(ctx, filepath.Join(directory, entry.Name()), &indexed); err != nil || indexed.ID != id || indexed.UpdatedAt.IsZero() {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			r.reject(Rejection{SessionID: id, Stage: "invalid_session", Reason: "unreadable_or_invalid_session"})
			continue
		}
		indexed.sessionMeta.private = indexed.hasPrivateContent()
		metas[id] = indexed.sessionMeta
	}
	privateGroups := make(map[string]bool)
	for _, m := range metas {
		if m.private {
			if group, ok := rootGroup(m.ID, metas); ok {
				privateGroups[group] = true
			}
		}
	}
	ordered := make([]sessionMeta, 0, len(metas))
	for _, m := range metas {
		ordered = append(ordered, m)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].UpdatedAt.Equal(ordered[j].UpdatedAt) {
			return ordered[i].UpdatedAt.Before(ordered[j].UpdatedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})
	seen := map[string]bool{}
	buckets := map[string][]Candidate{}
	unique := 0
	for _, m := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if options.Workspace != "" && m.Directory != options.Workspace || options.Provider != "" && m.Provider != options.Provider || options.Model != "" && m.Model != options.Model || !options.Since.IsZero() && m.UpdatedAt.Before(options.Since) {
			continue
		}
		group, ok := rootGroup(m.ID, metas)
		if !ok {
			r.reject(Rejection{SessionID: m.ID, Stage: "cyclic_parent", Reason: "invalid_parent_chain"})
			continue
		}
		if SensitiveText(m.Title) || SensitiveText(m.Directory) || SensitiveText(m.Provider) || SensitiveText(m.Model) {
			r.reject(Rejection{SessionID: m.ID, Stage: "sensitive", Reason: "secret_metadata"})
			continue
		}
		var s core.Session
		if err := readSession(ctx, filepath.Join(directory, m.ID+".json"), &s); err != nil || s.ID != m.ID || s.ParentID != m.ParentID || s.Title != m.Title || s.Directory != m.Directory || s.Provider != m.Provider || s.Model != m.Model || !s.UpdatedAt.Equal(m.UpdatedAt) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			r.reject(Rejection{SessionID: m.ID, Stage: "changed_or_invalid_session", Reason: "unreadable_or_invalid_session"})
			continue
		}
		r.Stats.Sessions++
		// A child can return private correspondence through Subagent. Exclude
		// its whole root group, even when the child fails the source filters.
		if privateGroups[group] || privateSession(s.Messages) {
			r.reject(Rejection{SessionID: m.ID, Stage: "private_session", Reason: "private_group"})
			continue
		}
		steers := steeredMessages(s.Audit)

		// Each candidate carries the turns before it, as the model saw them,
		// so a later turn keeps the conversation it answers. One unusable
		// earlier turn makes every turn after it in the session unusable.
		var earlier []core.Message
		var earlierBytes int
		var earlierErr error
		for start := 0; start < len(s.Messages); {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if s.Messages[start].Role != "user" {
				r.reject(Rejection{SessionID: m.ID, Start: start, End: start + 1, Stage: "orphan_message", Reason: "orphan_message"})
				start++
				continue
			}
			// A steer belongs to the turn it was sent into. Split there, the turn
			// would become a fragment that stops mid-task and a continuation
			// without the original request, so the whole turn is skipped.
			end := start + 1
			steered := false
			for end < len(s.Messages) {
				next := s.Messages[end]
				if next.Role == "user" && !steers[next.Content] {
					break
				}
				steered = steered || next.Role == "user"
				end++
			}
			r.Stats.Turns++
			messages := trimReflection(s.Messages[start:end], start)
			candidateEnd := start + len(messages)
			asContext := elideImages(messages)

			// A turn valid as a target has no images, so this is its size too.
			encoded, encodeErr := json.Marshal(asContext)

			// The cap stops a later append from writing into this candidate's
			// context.
			prior, priorBytes, priorErr := earlier[:len(earlier):len(earlier)], earlierBytes, earlierErr
			earlier = append(earlier, asContext...)
			earlierBytes += len(encoded)
			if earlierErr == nil {
				earlierErr = errors.Join(encodeErr, validateContext(asContext, definitions))
			}

			rejection := Rejection{
				SessionID: m.ID, Start: start, End: candidateEnd,
				ContextBytes: priorBytes, TurnBytes: len(encoded),
			}
			if steered {
				rejection.Stage, rejection.Reason = "steered_turn", "steered_turn"
				r.reject(rejection)
				start = end
				continue
			}
			if end == len(s.Messages) && s.PendingTool != nil {
				rejection.Stage, rejection.Reason = "invalid_turn", "pending_tool"
				r.reject(rejection)
				start = end
				continue
			}
			if err := ValidateMessages(messages, definitions); err != nil {
				rejection.Stage, rejection.Reason = "invalid_turn", rejectionReason(err)
				r.reject(rejection)
				start = end
				continue
			}
			if priorErr != nil {
				rejection.Stage, rejection.Reason = "invalid_context", rejectionReason(priorErr)
				r.reject(rejection)
				start = end
				continue
			}
			feedback := ""
			if end < len(s.Messages) && isFeedback(s.Messages[end].Content) {
				f := s.Messages[end]
				if len(f.Images) > 0 || imagePattern.MatchString(f.Content) || SensitiveText(f.Content) || sensitiveJSON(f.Content) {
					rejection.Stage, rejection.Reason = "sensitive_feedback", "secret_text"
					if len(f.Images) > 0 || imagePattern.MatchString(f.Content) {
						rejection.Reason = "image"
					}
					r.reject(rejection)
					start = end
					continue
				}
				feedback = f.Content
			}
			// The context counts as the sum of its turns' encoded sizes. That is
			// a byte or so per turn more than one array, so the bound errs toward
			// skipping.
			feedbackJSON, feedbackErr := json.Marshal(feedback)
			if encodeErr != nil || feedbackErr != nil || priorBytes+len(encoded)+len(feedbackJSON) > options.MaxBytes {
				rejection.Stage, rejection.Reason = "too_large", "size_limit"
				rejection.FeedbackBytes = len(feedbackJSON)
				if encodeErr != nil || feedbackErr != nil {
					rejection.Reason = "encoding_error"
				}
				r.reject(rejection)
				start = end
				continue
			}
			hash := transcriptHash(append(prior, messages...))
			if seen[hash] {
				r.Stats.Duplicates++
				start = end
				continue
			}
			seen[hash] = true
			unique++
			signals, priority, category := classify(messages, feedback)
			c := Candidate{
				ID:       hash,
				Source:   Source{SessionID: m.ID, GroupID: group, Title: m.Title, Directory: m.Directory, Provider: m.Provider, Model: m.Model, Start: start, End: candidateEnd, UpdatedAt: m.UpdatedAt},
				Context:  prior,
				Messages: append([]core.Message(nil), messages...),
				Feedback: feedback,
				Signals:  signals,
				Priority: priority,
			}
			bucket := append(buckets[category], c)
			sortCandidates(bucket)
			if len(bucket) > options.Limit {
				bucket = bucket[:options.Limit]
			}
			buckets[category] = bucket
			start = end
		}
	}
	categories := []string{"correction", "permission", "blocker", "tool", "direct"}
	for len(r.Candidates) < options.Limit {
		added := false
		for _, cat := range categories {
			if len(buckets[cat]) > 0 && len(r.Candidates) < options.Limit {
				r.Candidates = append(r.Candidates, buckets[cat][0])
				buckets[cat] = buckets[cat][1:]
				added = true
			}
		}
		if !added {
			break
		}
	}
	r.Stats.Omitted = unique - len(r.Candidates)
	return r, nil
}

// Successful background reflection appends one Memory-call message and its
// results after the real final answer. It is harness bookkeeping, not an agent
// decision to imitate. Only trim that exact recorded suffix, never ordinary
// trailing tool calls or an unfinished assistant turn.
func trimReflection(messages []core.Message, start int) []core.Message {
	callIndex := len(messages) - 1
	for callIndex >= 0 && messages[callIndex].Role == "tool" {
		callIndex--
	}
	if callIndex < 2 || callIndex == len(messages)-1 {
		return messages
	}
	calls := messages[callIndex]
	final := messages[callIndex-1]
	if calls.Role != "assistant" || calls.Content != "" || len(calls.Images) != 0 ||
		len(calls.ToolCalls) == 0 || len(calls.ToolCalls) != len(messages)-callIndex-1 ||
		final.Role != "assistant" || len(final.ToolCalls) != 0 || strings.TrimSpace(final.Content) == "" {
		return messages
	}
	prefix := fmt.Sprintf("reflection-%d-", start)
	previous := 0
	for i, call := range calls.ToolCalls {
		ordinal, err := strconv.Atoi(strings.TrimPrefix(call.ID, prefix))
		if call.Name != "Memory" || !strings.HasPrefix(call.ID, prefix) || err != nil || ordinal <= previous {
			return messages
		}
		previous = ordinal // Failed reflection operations leave gaps in the IDs.
		result := messages[callIndex+1+i]
		if result.ToolCallID != call.ID || len(result.ToolCalls) != 0 || len(result.Images) != 0 {
			return messages
		}
	}
	return messages[:callIndex]
}

// steeredMessages returns the texts the user steered turns with. A session
// saves a steer as an ordinary user message; only its audit marks one. A new
// prompt that repeats an earlier steer word for word is treated as a steer
// too, which skips a turn rather than split one.
func steeredMessages(audit []core.AuditEntry) map[string]bool {
	steers := map[string]bool{}
	for _, entry := range audit {
		if entry.Event.Kind == "steer" {
			steers[entry.Event.Text] = true
		}
	}
	return steers
}

func rootGroup(id string, metas map[string]sessionMeta) (string, bool) {
	seen := map[string]bool{}
	for {
		if seen[id] {
			return "", false
		}
		seen[id] = true
		m, ok := metas[id]
		if !ok || m.ParentID == "" {
			return id, true
		}
		if !sessionIDPattern.MatchString(m.ParentID) {
			return "", false
		}
		id = m.ParentID
	}
}

func transcriptHash(messages []core.Message) string {
	copyMessages := make([]core.Message, len(messages))
	copy(copyMessages, messages)
	ids := map[string]string{}
	next := 0
	for i := range copyMessages {
		m := &copyMessages[i]
		m.ToolCalls = append([]core.ToolCall(nil), m.ToolCalls...)
		for j := range m.ToolCalls {
			c := &m.ToolCalls[j]
			next++
			ids[c.ID] = fmt.Sprint(next)
			c.ID = ids[c.ID]
			c.Arguments, _ = canonicalArguments(c.Arguments)
		}
		if m.ToolCallID != "" {
			m.ToolCallID = ids[m.ToolCallID]
		}
	}
	b, _ := json.Marshal(copyMessages)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func isFeedback(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, term := range []string{"no,", "no ", "wrong", "incorrect", "actually", "instead", "i meant", "you should", "you forgot", "you missed", "don't", "do not", "stop asking", "thank you", "thanks", "perfect", "not what", "expected", "do it", "go ahead", "already asked", "on your own", "why didn't", "why did you"} {
		if strings.Contains(s, term) {
			return true
		}
	}
	return false
}

func classify(messages []core.Message, feedback string) ([]string, int, string) {
	var signals []string
	priority := 0
	category := "direct"
	tools := 0
	blocker := false
	for _, m := range messages {
		tools += len(m.ToolCalls)
		if m.Role == "tool" {
			s := strings.ToLower(m.Content)
			if strings.Contains(s, "error") || strings.Contains(s, "failed") || strings.Contains(s, "denied") || strings.Contains(s, "blocked") {
				blocker = true
			}
		}
	}
	if tools == 0 {
		signals = append(signals, "direct_answer")
	} else {
		signals = append(signals, "tools_used")
		priority += 1
		category = "tool"
		if len(messages[1].ToolCalls) > 0 {
			signals = append(signals, "prompt_tool_use")
			priority++
		}
	}
	if blocker {
		signals = append(signals, "error_or_blocker")
		priority += 2
		category = "blocker"
	}
	final := strings.ToLower(messages[len(messages)-1].Content)
	if strings.Contains(final, "permission") || strings.Contains(final, "may i ") || strings.Contains(final, "would you like me to") {
		signals = append(signals, "possible_permission_question")
		priority += 2
		category = "permission"
	}
	if feedback != "" {
		signals = append(signals, "following_feedback")
		priority += 3
		category = "correction"
	}
	return signals, priority, category
}

func sortCandidates(c []Candidate) {
	sort.Slice(c, func(i, j int) bool {
		if c[i].Priority != c[j].Priority {
			return c[i].Priority > c[j].Priority
		}
		if !c[i].Source.UpdatedAt.Equal(c[j].Source.UpdatedAt) {
			return c[i].Source.UpdatedAt.Before(c[j].Source.UpdatedAt)
		}
		if c[i].Source.SessionID != c[j].Source.SessionID {
			return c[i].Source.SessionID < c[j].Source.SessionID
		}
		return c[i].Source.Start < c[j].Source.Start
	})
}
