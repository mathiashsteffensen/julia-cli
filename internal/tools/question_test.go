package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"jarvis/internal/core"
)

// questionWait bounds every wait for something that should already be happening.
const questionWait = 5 * time.Second

// stubExecutor records delegated calls and answers every one with reply.
type stubExecutor struct {
	mu    sync.Mutex
	defs  []core.ToolDefinition
	calls []core.ToolCall
	ctxs  []context.Context
	reply core.ToolResult
}

func (s *stubExecutor) Definitions() []core.ToolDefinition { return s.defs }
func (s *stubExecutor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	s.ctxs = append(s.ctxs, ctx)
	return s.reply
}

// askRecorder is an Asker that records the questions it receives and replies with a fixed answer.
type askRecorder struct {
	mu        sync.Mutex
	questions []core.Question
	answer    core.Answer
	err       error
}

func (r *askRecorder) ask(_ context.Context, q core.Question) (core.Answer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.questions = append(r.questions, q)
	return r.answer, r.err
}

func (r *askRecorder) asked() []core.Question {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.Question(nil), r.questions...)
}

func questionCall(raw string) core.ToolCall {
	return core.ToolCall{ID: "call-1", Name: core.QuestionTool, Arguments: json.RawMessage(raw)}
}

func questionJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func questionWorkspace(t *testing.T) (*Executor, string) {
	t.Helper()
	dir := t.TempDir()
	base, err := New(Config{Directory: dir, Mode: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Close() })
	return base, dir
}

// decodeQuestionResult requires a successful result whose content is exactly one QuestionResult object.
func decodeQuestionResult(t *testing.T, result core.ToolResult) core.QuestionResult {
	t.Helper()
	if result.IsError {
		t.Fatalf("error result: %s", result.Content)
	}
	var got core.QuestionResult
	d := json.NewDecoder(strings.NewReader(result.Content))
	d.DisallowUnknownFields()
	if err := d.Decode(&got); err != nil || d.Decode(new(any)) != io.EOF {
		t.Fatalf("content is not one QuestionResult: %q (%v)", result.Content, err)
	}
	return got
}

func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(questionWait):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

func TestQuestionDefinitionSchema(t *testing.T) {
	def := QuestionDefinition()
	data, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Name        string
		Description string
		Parameters  map[string]any
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if core.QuestionTool != "AskUserQuestion" || got.Name != core.QuestionTool || got.Description == "" {
		t.Fatalf("bad name/description: %q %q", got.Name, got.Description)
	}
	params := got.Parameters
	if params["type"] != "object" || params["additionalProperties"] != false || !reflect.DeepEqual(params["required"], []any{"question"}) {
		t.Fatalf("bad parameters: %v", params)
	}
	props, _ := params["properties"].(map[string]any)
	if len(props) != 2 {
		t.Fatalf("want exactly question and options properties: %v", props)
	}
	if question, _ := props["question"].(map[string]any); question["type"] != "string" {
		t.Fatalf("question is not a string: %v", props["question"])
	}
	options, _ := props["options"].(map[string]any)
	if options["type"] != "array" || options["minItems"] != float64(2) || options["maxItems"] != float64(8) {
		t.Fatalf("bad options bounds: %v", options)
	}
	items, _ := options["items"].(map[string]any)
	if items["type"] != "object" || items["additionalProperties"] != false || !reflect.DeepEqual(items["required"], []any{"label"}) {
		t.Fatalf("bad option items: %v", items)
	}
	itemProps, _ := items["properties"].(map[string]any)
	for _, field := range []string{"label", "description"} {
		if prop, _ := itemProps[field].(map[string]any); prop["type"] != "string" {
			t.Fatalf("option %s is not a string: %v", field, itemProps[field])
		}
	}
	if len(itemProps) != 2 {
		t.Fatalf("unexpected option properties: %v", itemProps)
	}
}

func TestQuestionDefinitionsAppendedLast(t *testing.T) {
	// Spare capacity makes an in-place append to the base slice observable.
	baseDefs := append(make([]core.ToolDefinition, 0, 32), Definitions(true)...)
	base := &stubExecutor{defs: baseDefs}
	executor := WithQuestions(base, (&askRecorder{}).ask)
	defs := executor.Definitions()
	if len(defs) != len(baseDefs)+1 {
		t.Fatalf("got %d definitions, want %d", len(defs), len(baseDefs)+1)
	}
	got, _ := json.Marshal(defs[:len(baseDefs)])
	want, _ := json.Marshal(baseDefs)
	if string(got) != string(want) {
		t.Fatal("base definitions were changed or reordered")
	}
	got, _ = json.Marshal(defs[len(defs)-1])
	want, _ = json.Marshal(QuestionDefinition())
	if string(got) != string(want) || defs[len(defs)-1].Name != core.QuestionTool {
		t.Fatalf("question definition is not last: %s", got)
	}
	if spare := baseDefs[:cap(baseDefs)][len(baseDefs)]; spare.Name != "" {
		t.Fatalf("Definitions wrote into the base slice: %q", spare.Name)
	}
}

func TestQuestionNilAskerLeavesToolUnregistered(t *testing.T) {
	stub := &stubExecutor{defs: Definitions(false)}
	if got := WithQuestions(stub, nil); got != core.ToolExecutor(stub) {
		t.Fatal("nil asker did not return the base executor")
	}
	base, _ := questionWorkspace(t)
	executor := WithQuestions(base, nil)
	if executor != core.ToolExecutor(base) {
		t.Fatal("nil asker did not return the base executor")
	}
	for _, def := range executor.Definitions() {
		if def.Name == core.QuestionTool {
			t.Fatal("question tool registered without an asker")
		}
	}
	if !executor.Execute(context.Background(), questionCall(`{"question":"q"}`)).IsError {
		t.Fatal("unregistered tool executed")
	}
}

func TestQuestionValidationRejects(t *testing.T) {
	two := `[{"label":"a"},{"label":"b"}]`
	withQuestion := func(q string) string { return questionJSON(map[string]string{"question": q}) }
	withOption := func(option string) string { return `{"question":"q","options":[{"label":"ok"},` + option + `]}` }
	withLabels := func(labels ...string) string {
		options := make([]core.QuestionOption, len(labels))
		for i, label := range labels {
			options[i].Label = label
		}
		return questionJSON(core.Question{Text: "q", Options: options})
	}
	nine := withLabels("1", "2", "3", "4", "5", "6", "7", "8", "9")
	cases := []struct{ name, raw string }{
		{"empty arguments", ``},
		{"whitespace arguments", " \n"},
		{"null", `null`},
		{"array", `[]`},
		{"string", `"q"`},
		{"number", `42`},
		{"empty object", `{}`},
		{"truncated object", `{"question":"q"`},
		{"two objects", `{"question":"q"} {"question":"q"}`},
		{"trailing garbage", `{"question":"q"} x`},
		{"missing question", `{"options":` + two + `}`},
		{"null question", `{"question":null}`},
		{"numeric question", `{"question":7}`},
		{"object question", `{"question":{}}`},
		{"empty question", `{"question":""}`},
		{"blank question", withQuestion(" \t\r\n ")},
		{"nul in question", withQuestion("a\x00b")},
		{"oversized question", withQuestion(strings.Repeat("a", 4097))},
		{"oversized multibyte question", withQuestion(strings.Repeat("世", 1366))},
		{"unknown field", `{"question":"q","extra":true}`},
		{"unknown field beside options", `{"question":"q","options":` + two + `,"multiple":true}`},
		{"options string", `{"question":"q","options":"a,b"}`},
		{"options object", `{"question":"q","options":{}}`},
		{"one option", withLabels("only")},
		{"nine options", nine},
		{"null option entries", `{"question":"q","options":[null,null]}`},
		{"string option entries", `{"question":"q","options":["a","b"]}`},
		{"array option entry", withOption(`[]`)},
		{"missing label", withOption(`{"description":"d"}`)},
		{"null label", withOption(`{"label":null}`)},
		{"numeric label", withOption(`{"label":5}`)},
		{"empty label", withOption(`{"label":""}`)},
		{"blank label", withOption(`{"label":" \t\n"}`)},
		{"oversized label", withLabels("ok", strings.Repeat("a", 121))},
		{"oversized multibyte label", withLabels("ok", strings.Repeat("世", 41))},
		{"numeric description", withOption(`{"label":"x","description":5}`)},
		{"oversized description", questionJSON(core.Question{Text: "q", Options: []core.QuestionOption{{Label: "a"}, {Label: "b", Description: strings.Repeat("d", 401)}}})},
		{"oversized multibyte description", questionJSON(core.Question{Text: "q", Options: []core.QuestionOption{{Label: "a"}, {Label: "b", Description: strings.Repeat("世", 134)}}})},
		{"unknown option field", withOption(`{"label":"x","value":"y"}`)},
		{"duplicate label", withLabels("a", "a")},
		{"duplicate label ignoring case", withLabels("Yes", "No", "yes")},
		{"duplicate label after collapsing whitespace", withLabels("a  b", " A\tb\n")},
		{"duplicate label ignoring unicode case", withLabels("Éclair", "éCLAIR")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &askRecorder{answer: core.Answer{Text: "should not matter"}}
			executor := WithQuestions(&stubExecutor{}, rec.ask)
			result := executor.Execute(context.Background(), questionCall(tc.raw))
			if !result.IsError || result.Content == "" {
				t.Fatalf("accepted %q: %+v", tc.raw, result)
			}
			if asked := rec.asked(); len(asked) != 0 {
				t.Fatalf("asker called for invalid arguments: %+v", asked)
			}
		})
	}
}

// encoding/json silently rewrites invalid UTF-8 to U+FFFD, so this has to be caught on the raw arguments.
func TestQuestionRejectsInvalidUTF8(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"question", "{\"question\":\"a\xffb\"}"},
		{"label", "{\"question\":\"q\",\"options\":[{\"label\":\"a\xff\"},{\"label\":\"b\"}]}"},
		{"description", "{\"question\":\"q\",\"options\":[{\"label\":\"a\",\"description\":\"d\xc3\"},{\"label\":\"b\"}]}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &askRecorder{answer: core.Answer{Text: "x"}}
			result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(tc.raw))
			if !result.IsError {
				t.Fatalf("accepted invalid UTF-8 in %s; the asker saw %+v", tc.name, rec.asked())
			}
			if asked := rec.asked(); len(asked) != 0 {
				t.Fatalf("asker called for invalid UTF-8: %+v", asked)
			}
		})
	}
}

func TestQuestionAskerReceivesNormalizedQuestion(t *testing.T) {
	eight := make([]core.QuestionOption, 8)
	for i := range eight {
		eight[i].Label = string(rune('A' + i))
	}
	cases := []struct {
		name string
		args string
		want core.Question
	}{
		{"free-form trimmed", `{"question":"  \n What now?\t\n"}`, core.Question{Text: "What now?"}},
		{"null options mean free-form", `{"question":"q","options":null}`, core.Question{Text: "q"}},
		{"empty options mean free-form", `{"question":"q","options":[]}`, core.Question{Text: "q"}},
		{"question keeps inner whitespace", "{\"question\":\"  Line one\\n\\n  Line two \\n\"}", core.Question{Text: "Line one\n\n  Line two"}},
		{
			"labels and descriptions collapsed",
			`{"question":" Pick one ","options":[{"label":"  Ship \t it\n","description":"  do\n the   thing "},{"label":"Wait"}]}`,
			core.Question{Text: "Pick one", Options: []core.QuestionOption{{Label: "Ship it", Description: "do the thing"}, {Label: "Wait"}}},
		},
		{
			"unicode spaces collapse like strings.Fields",
			questionJSON(core.Question{Text: "q", Options: []core.QuestionOption{{Label: "x\u00a0\u2003y", Description: "p\u00a0q"}, {Label: "z"}}}),
			core.Question{Text: "q", Options: []core.QuestionOption{{Label: "x y", Description: "p q"}, {Label: "z"}}},
		},
		{
			"length is measured after collapsing",
			questionJSON(core.Question{Text: "q", Options: []core.QuestionOption{
				{Label: "a" + strings.Repeat(" ", 200) + "b", Description: strings.Repeat("x   ", 120)},
				{Label: "c"},
			}}),
			core.Question{Text: "q", Options: []core.QuestionOption{
				{Label: "a b", Description: strings.TrimSuffix(strings.Repeat("x ", 120), " ")},
				{Label: "c"},
			}},
		},
		{
			"blank description dropped",
			`{"question":"q","options":[{"label":"a","description":"  \n "},{"label":"b","description":null}]}`,
			core.Question{Text: "q", Options: []core.QuestionOption{{Label: "a"}, {Label: "b"}}},
		},
		{
			"limits are inclusive",
			questionJSON(core.Question{Text: strings.Repeat("q", 1000), Options: []core.QuestionOption{
				{Label: strings.Repeat("l", 120), Description: strings.Repeat("d", 400)},
				{Label: strings.Repeat("世", 40), Description: strings.Repeat("世", 133)},
			}}),
			core.Question{Text: strings.Repeat("q", 1000), Options: []core.QuestionOption{
				{Label: strings.Repeat("l", 120), Description: strings.Repeat("d", 400)},
				{Label: strings.Repeat("世", 40), Description: strings.Repeat("世", 133)},
			}},
		},
		{"eight options", questionJSON(core.Question{Text: "q", Options: eight}), core.Question{Text: "q", Options: eight}},
		{
			"same label in different case is fine across questions, not within one",
			`{"question":"q","options":[{"label":"Yes"},{"label":"Yes, but"}]}`,
			core.Question{Text: "q", Options: []core.QuestionOption{{Label: "Yes"}, {Label: "Yes, but"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &askRecorder{answer: core.Answer{Text: "ok"}}
			result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(tc.args))
			if result.IsError {
				t.Fatalf("rejected: %s", result.Content)
			}
			asked := rec.asked()
			if len(asked) != 1 {
				t.Fatalf("asker called %d times", len(asked))
			}
			// DeepEqual also pins a free-form question to nil, not empty, Options.
			if !reflect.DeepEqual(asked[0], tc.want) {
				t.Fatalf("asker got %#v, want %#v", asked[0], tc.want)
			}
		})
	}
}

func TestQuestionAskerReceivesCallerContext(t *testing.T) {
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "marker"))
	defer cancel()
	var seen atomic.Bool
	executor := WithQuestions(&stubExecutor{}, func(askCtx context.Context, _ core.Question) (core.Answer, error) {
		if askCtx.Value(key{}) == "marker" {
			seen.Store(true)
		}
		cancel()
		select {
		case <-askCtx.Done():
		default:
			t.Error("asker context does not follow the caller's cancellation")
		}
		return core.Answer{Text: "ok"}, nil
	})
	executor.Execute(ctx, questionCall(`{"question":"q"}`))
	if !seen.Load() {
		t.Fatal("asker did not receive the caller's context")
	}
}

func TestQuestionAnswerHandling(t *testing.T) {
	const choices = `{"question":"Which?","options":[{"label":"Alpha"},{"label":"  Beta \t (default) ","description":"d"},{"label":"Gamma"}]}`
	const open = `{"question":"Name?"}`
	cases := []struct {
		name    string
		args    string
		answer  core.Answer
		want    core.QuestionResult
		wantErr bool
	}{
		{"first option", choices, core.Answer{Option: 1, Text: "Alpha"}, core.QuestionResult{Answer: "Alpha", Source: "option", Option: 1}, false},
		{"middle option uses normalized label", choices, core.Answer{Option: 2, Text: "Beta"}, core.QuestionResult{Answer: "Beta (default)", Source: "option", Option: 2}, false},
		{"last option", choices, core.Answer{Option: 3}, core.QuestionResult{Answer: "Gamma", Source: "option", Option: 3}, false},
		{"option label ignores the asker's text", choices, core.Answer{Option: 1, Text: "Gamma"}, core.QuestionResult{Answer: "Alpha", Source: "option", Option: 1}, false},
		{"option beyond the last", choices, core.Answer{Option: 4, Text: "Gamma"}, core.QuestionResult{}, true},
		{"negative option", choices, core.Answer{Option: -1, Text: "Alpha"}, core.QuestionResult{}, true},
		{"option on a free-form question", open, core.Answer{Option: 1, Text: "x"}, core.QuestionResult{}, true},
		{"free-form text equal to a label stays free-form", choices, core.Answer{Text: "Alpha"}, core.QuestionResult{Answer: "Alpha", Source: "freeform"}, false},
		{"free-form on an options question", choices, core.Answer{Text: "  my own  idea\n"}, core.QuestionResult{Answer: "my own  idea", Source: "freeform"}, false},
		{"free-form on a free-form question", open, core.Answer{Text: "Ada"}, core.QuestionResult{Answer: "Ada", Source: "freeform"}, false},
		{"free-form keeps inner newlines", open, core.Answer{Text: "\tline one\n\nline two \n"}, core.QuestionResult{Answer: "line one\n\nline two", Source: "freeform"}, false},
		{"free-form strips invalid UTF-8", open, core.Answer{Text: "caf\xffe\xfe"}, core.QuestionResult{Answer: "cafe", Source: "freeform"}, false},
		{"free-form keeps valid multibyte text", open, core.Answer{Text: " 世界 "}, core.QuestionResult{Answer: "世界", Source: "freeform"}, false},
		{"empty free-form", open, core.Answer{}, core.QuestionResult{}, true},
		{"blank free-form", choices, core.Answer{Text: " \n\t "}, core.QuestionResult{}, true},
		{"free-form of only invalid UTF-8", open, core.Answer{Text: "\xff\xfe"}, core.QuestionResult{}, true},
		{"free-form of invalid UTF-8 and spaces", open, core.Answer{Text: " \xff "}, core.QuestionResult{}, true},
		{"skipped options question", choices, core.Answer{Skipped: true}, core.QuestionResult{Skipped: true}, false},
		{"skipped free-form question", open, core.Answer{Skipped: true}, core.QuestionResult{Skipped: true}, false},
		{"skipped wins over leaked text and option", choices, core.Answer{Skipped: true, Text: "leak", Option: 2}, core.QuestionResult{Skipped: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &askRecorder{answer: tc.answer}
			result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(tc.args))
			if len(rec.asked()) != 1 {
				t.Fatalf("asker called %d times", len(rec.asked()))
			}
			if tc.wantErr {
				if !result.IsError || result.Content == "" {
					t.Fatalf("accepted answer %+v: %+v", tc.answer, result)
				}
				return
			}
			got := decodeQuestionResult(t, result)
			if tc.want.Skipped {
				var keys map[string]any
				if err := json.Unmarshal([]byte(result.Content), &keys); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"answer", "source", "option"} {
					if _, ok := keys[key]; ok {
						t.Fatalf("skipped result carries %q: %s", key, result.Content)
					}
				}
				if got.Note == "" {
					t.Fatalf("skipped result has no note: %s", result.Content)
				}
				got.Note = ""
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestQuestionAnswerTruncation(t *testing.T) {
	const marker = "[answer truncated]"
	cases := []struct {
		name      string
		text      string
		truncated bool
	}{
		{"exactly 16 KiB", strings.Repeat("a", 16<<10), false},
		{"one byte over 16 KiB", strings.Repeat("a", 16<<10+1), true},
		{"far over 16 KiB", strings.Repeat("b", 200<<10), true},
		{"multibyte text cut inside a rune", strings.Repeat("世", 6000), true},
		{"exactly 16 KiB of multibyte text", strings.Repeat("世", 5461) + "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &askRecorder{answer: core.Answer{Text: tc.text}}
			got := decodeQuestionResult(t, WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(`{"question":"q"}`)))
			if got.Source != "freeform" || got.Option != 0 || got.Skipped {
				t.Fatalf("bad result: %+v", got)
			}
			if !tc.truncated {
				if got.Answer != tc.text {
					t.Fatalf("answer within the limit was altered (%d -> %d bytes)", len(tc.text), len(got.Answer))
				}
				return
			}
			if !strings.Contains(got.Answer, marker) {
				t.Fatalf("no truncation marker in %d-byte answer", len(got.Answer))
			}
			if !utf8.ValidString(got.Answer) || strings.ContainsRune(got.Answer, utf8.RuneError) {
				t.Fatal("truncation produced invalid UTF-8")
			}
			if len(got.Answer) > 16<<10+len(marker)+8 {
				t.Fatalf("truncated answer is %d bytes", len(got.Answer))
			}
			if !strings.HasPrefix(got.Answer, tc.text[:15000]) {
				t.Fatal("truncation dropped the start of the answer")
			}
		})
	}
}

func TestQuestionResultSurvivesJSONEscaping(t *testing.T) {
	// Characters that expand when JSON-escaped must still round-trip as valid JSON.
	for _, text := range []string{
		strings.Repeat("<", 16<<10),
		strings.Repeat(`"`, 16<<10),
		strings.Repeat("\x00", 16<<10),
		strings.Repeat("a\u2028b", 3000),
	} {
		rec := &askRecorder{answer: core.Answer{Text: text}}
		result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(`{"question":"q"}`))
		got := decodeQuestionResult(t, result)
		// An answer whose escaped form exceeds the output limit is shortened, never mangled.
		answer, shortened := strings.CutSuffix(got.Answer, "\n[answer truncated]")
		if got.Source != "freeform" || len(result.Content) > maxOutput || !strings.HasPrefix(text, answer) || (!shortened && answer != text) {
			t.Fatalf("answer %.12q... did not round-trip: %.80q (%d bytes)", text, got.Answer, len(result.Content))
		}
	}
}

// Subagent keeps its JSON valid and within maxOutput; an escape-heavy answer (control
// characters) encodes to several times its 16 KiB, so it is shortened until it fits.
func TestQuestionResultStaysWithinOutputBound(t *testing.T) {
	for _, ch := range []string{"<", "&", "\x00"} {
		rec := &askRecorder{answer: core.Answer{Text: strings.Repeat(ch, 16<<10)}}
		result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(`{"question":"q"}`))
		got := decodeQuestionResult(t, result)
		if len(result.Content) > maxOutput || got.Answer == "" {
			t.Fatalf("%q answer encodes to %d bytes, limit %d", ch, len(result.Content), maxOutput)
		}
	}
}

func TestQuestionAskerFailure(t *testing.T) {
	boom := errors.New("terminal went away")
	cases := []struct {
		name   string
		answer core.Answer
		err    error
	}{
		{"plain error", core.Answer{}, boom},
		{"error beats a returned answer", core.Answer{Text: "x"}, boom},
		{"error beats a returned option", core.Answer{Option: 1}, boom},
		{"error beats skipped", core.Answer{Skipped: true}, boom},
		{"cancellation error", core.Answer{}, context.Canceled},
		{"enormous error", core.Answer{}, errors.New(strings.Repeat("e", 100<<10))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &askRecorder{answer: tc.answer, err: tc.err}
			result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(`{"question":"q","options":[{"label":"a"},{"label":"b"}]}`))
			if !result.IsError || result.Content == "" || len(result.Content) > maxOutput {
				t.Fatalf("bad error result: %d bytes, IsError=%v", len(result.Content), result.IsError)
			}
			if len(rec.asked()) != 1 {
				t.Fatalf("asker called %d times", len(rec.asked()))
			}
		})
	}
}

func TestQuestionCancelledBeforeExecute(t *testing.T) {
	rec := &askRecorder{answer: core.Answer{Text: "x"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := WithQuestions(&stubExecutor{}, rec.ask).Execute(ctx, questionCall(`{"question":"q"}`))
	if !result.IsError {
		t.Fatalf("ran with a cancelled context: %+v", result)
	}
	if len(rec.asked()) != 0 {
		t.Fatal("asker called with a cancelled context")
	}
}

func TestQuestionCancelWhileAskerBlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	executor := WithQuestions(&stubExecutor{}, func(ctx context.Context, _ core.Question) (core.Answer, error) {
		close(entered)
		<-ctx.Done()
		return core.Answer{}, ctx.Err()
	})
	done := make(chan core.ToolResult, 1)
	go func() { done <- executor.Execute(ctx, questionCall(`{"question":"q"}`)) }()
	within(t, entered, "asker to start")
	cancel()
	if result := within(t, done, "Execute to return after cancellation"); !result.IsError {
		t.Fatalf("cancelled question succeeded: %+v", result)
	}
}

func TestQuestionQueuedCallsDoNotAskAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	entered := make(chan struct{}, 4)
	executor := WithQuestions(&stubExecutor{}, func(ctx context.Context, _ core.Question) (core.Answer, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		return core.Answer{}, ctx.Err()
	})
	done := make(chan core.ToolResult, 4)
	for range 4 {
		go func() { done <- executor.Execute(ctx, questionCall(`{"question":"q"}`)) }()
	}
	within(t, entered, "first asker to start")
	cancel()
	for range 4 {
		if result := within(t, done, "queued Execute to return after cancellation"); !result.IsError {
			t.Fatalf("cancelled question succeeded: %+v", result)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("asker called %d times; queued calls must see the cancellation", n)
	}
}

// A call queued behind a question that is still on screen must honor its own
// cancellation instead of waiting for that question to be answered.
func TestQuestionQueuedCallHonorsItsOwnCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})
	var calls atomic.Int32
	executor := WithQuestions(&stubExecutor{}, func(ctx context.Context, _ core.Question) (core.Answer, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return core.Answer{Text: "late"}, nil
	})
	go executor.Execute(context.Background(), questionCall(`{"question":"first"}`))
	within(t, entered, "first asker to start")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan core.ToolResult, 1)
	go func() { done <- executor.Execute(ctx, questionCall(`{"question":"second"}`)) }()
	cancel()
	select {
	case result := <-done:
		if !result.IsError {
			t.Fatalf("cancelled queued question succeeded: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("queued call did not return after its own context was cancelled")
	}
	if calls.Load() != 1 {
		t.Fatal("cancelled queued call reached the asker")
	}
}

func TestQuestionDelegatesOtherTools(t *testing.T) {
	type key struct{}
	cancelled, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "marker"))
	cancel()
	live := context.WithValue(context.Background(), key{}, "marker")
	replies := []core.ToolResult{{Content: "base output"}, {Content: "base failure", IsError: true}}
	for _, name := range []string{"Read", "Subagent", "Bash", "unknown_tool", "AskUserQuestions", "ask_user_question", "askUserQuestion", "ASK_USER_QUESTION", " AskUserQuestion", ""} {
		for _, reply := range replies {
			for _, ctx := range []context.Context{live, cancelled} {
				stub := &stubExecutor{reply: reply}
				rec := &askRecorder{}
				call := core.ToolCall{ID: "id-7", Name: name, Arguments: json.RawMessage(`{"question":"q","path":"x"}`)}
				got := WithQuestions(stub, rec.ask).Execute(ctx, call)
				if got != reply {
					t.Fatalf("%q: result %+v, want %+v", name, got, reply)
				}
				if len(stub.calls) != 1 || !reflect.DeepEqual(stub.calls[0], call) {
					t.Fatalf("%q: base received %+v", name, stub.calls)
				}
				if stub.ctxs[0].Value(key{}) != "marker" || stub.ctxs[0].Err() != ctx.Err() {
					t.Fatalf("%q: base did not receive the caller's context", name)
				}
				if len(rec.asked()) != 0 {
					t.Fatalf("%q: asker called for a non-question tool", name)
				}
			}
		}
	}
}

func TestQuestionAskerNeverRunsConcurrently(t *testing.T) {
	const callers = 6
	var active, peak atomic.Int32
	entered := make(chan string, callers)
	proceed := make(chan struct{})
	executor := WithQuestions(&stubExecutor{}, func(_ context.Context, q core.Question) (core.Answer, error) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		entered <- q.Text
		<-proceed
		active.Add(-1)
		return core.Answer{Text: "re: " + q.Text}, nil
	})
	results := make([]core.ToolResult, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			results[i] = executor.Execute(context.Background(), questionCall(questionJSON(map[string]string{"question": "q" + string(rune('0'+i))})))
		})
	}
	within(t, entered, "first asker to start")
	// A second asker would have started by now if the executor did not serialize them.
	select {
	case text := <-entered:
		t.Fatalf("second question %q reached the asker while the first was pending", text)
	case <-time.After(25 * time.Millisecond):
	}
	for i := range callers {
		proceed <- struct{}{}
		if i < callers-1 {
			within(t, entered, "next asker to start")
		}
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Fatalf("up to %d askers ran at once", p)
	}
	for i, result := range results {
		if got := decodeQuestionResult(t, result); got.Answer != "re: q"+string(rune('0'+i)) {
			t.Fatalf("call %d got another call's answer: %+v", i, got)
		}
	}
}

func TestQuestionDoesNotBlockWorkspaceTools(t *testing.T) {
	for _, stacked := range []bool{false, true} {
		name := "plain executor"
		if stacked {
			name = "with subagents"
		}
		t.Run(name, func(t *testing.T) {
			base, dir := questionWorkspace(t)
			if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello workspace\n"), 0644); err != nil {
				t.Fatal(err)
			}
			var inner core.ToolExecutor = base
			if stacked {
				inner = WithSubagents(base, func(context.Context, string, core.ToolExecutor) (string, error) { return "", nil })
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			release := make(chan struct{})
			executor := WithQuestions(inner, func(ctx context.Context, _ core.Question) (core.Answer, error) {
				close(entered)
				select {
				case <-release:
					return core.Answer{Text: "finally"}, nil
				case <-ctx.Done():
					return core.Answer{}, ctx.Err()
				}
			})
			pending := make(chan core.ToolResult, 1)
			go func() { pending <- executor.Execute(ctx, questionCall(`{"question":"q"}`)) }()
			within(t, entered, "asker to start")

			workspace := make(chan []core.ToolResult, 1)
			go func() {
				workspace <- []core.ToolResult{
					executor.Execute(ctx, core.ToolCall{Name: "Read", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}),
					executor.Execute(ctx, core.ToolCall{Name: "List", Arguments: json.RawMessage(`{}`)}),
				}
			}()
			results := within(t, workspace, "workspace tools to finish while a question is pending")
			if results[0].IsError || !strings.Contains(results[0].Content, "hello workspace") {
				t.Fatalf("Read: %+v", results[0])
			}
			if results[1].IsError || !strings.Contains(results[1].Content, "notes.txt") {
				t.Fatalf("List: %+v", results[1])
			}
			select {
			case result := <-pending:
				t.Fatalf("question returned before it was answered: %+v", result)
			default:
			}
			close(release)
			if got := decodeQuestionResult(t, within(t, pending, "question to finish")); got.Answer != "finally" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestQuestionHiddenFromSubagents(t *testing.T) {
	base, _ := questionWorkspace(t)
	var asked, ran atomic.Int32
	runner := func(ctx context.Context, task string, child core.ToolExecutor) (string, error) {
		for _, def := range child.Definitions() {
			if def.Name == core.QuestionTool {
				return "", errors.New("child sees the question tool")
			}
		}
		if !child.Execute(ctx, questionCall(`{"question":"may I?"}`)).IsError {
			return "", errors.New("child asked the user")
		}
		ran.Add(1)
		return task, nil
	}
	executor := WithQuestions(WithSubagents(base, runner), func(context.Context, core.Question) (core.Answer, error) {
		asked.Add(1)
		return core.Answer{Text: "yes"}, nil
	})

	defs := executor.Definitions()
	names := map[string]int{}
	for _, def := range defs {
		names[def.Name]++
	}
	if names["Subagent"] != 1 || names[core.QuestionTool] != 1 || defs[len(defs)-1].Name != core.QuestionTool {
		t.Fatalf("parent definitions wrong: %v, last %q", names, defs[len(defs)-1].Name)
	}

	result := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["a","b"]}`)})
	var children []struct {
		Task          int
		Output, Error string
	}
	if result.IsError || json.Unmarshal([]byte(result.Content), &children) != nil || len(children) != 2 {
		t.Fatalf("bad subagent result: %+v", result)
	}
	for _, child := range children {
		if child.Error != "" {
			t.Fatalf("child failed: %+v", children)
		}
	}
	if ran.Load() != 2 || asked.Load() != 0 {
		t.Fatalf("ran %d children, asked %d times", ran.Load(), asked.Load())
	}

	if got := decodeQuestionResult(t, executor.Execute(context.Background(), questionCall(`{"question":"q"}`))); got.Answer != "yes" || asked.Load() != 1 {
		t.Fatalf("parent could not ask: %+v, asked %d", got, asked.Load())
	}
}

// Text the user cannot see must not reach the user's answer or the reviewer.
func TestQuestionStripsInvisibleAndControlCharacters(t *testing.T) {
	tags := "\U000E0049\U000E0047\U000E004E"
	raw, _ := json.Marshal(map[string]any{
		"question": "Proceed\u200b?\u202e\x1b[2J" + tags + "\n\nSecond\u00adline\ttab\u2028end",
		"options": []map[string]any{
			{"label": "Keep it\u200b simple" + tags + "\x07", "description": "safe\u2060 choice\ufeff"},
			{"label": "Fa\u200dmily \u200cmark"},
		},
	})
	rec := &askRecorder{answer: core.Answer{Option: 1}}
	result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(string(raw)))
	if result.IsError || len(rec.asked()) != 1 {
		t.Fatalf("sanitized question was rejected: %s", result.Content)
	}
	got := rec.asked()[0]
	if got.Text != "Proceed?[2J\n\nSecondline tab end" {
		t.Fatalf("question text %q", got.Text)
	}
	if got.Options[0].Label != "Keep it simple" || got.Options[0].Description != "safe choice" {
		t.Fatalf("first option %+v", got.Options[0])
	}
	if got.Options[1].Label != "Fa\u200dmily \u200cmark" {
		t.Fatalf("joiners needed by emoji and some scripts were removed: %q", got.Options[1].Label)
	}
	if decodeQuestionResult(t, result).Answer != "Keep it simple" {
		t.Fatalf("the answer must come from the cleaned label: %s", result.Content)
	}
}

func TestQuestionRejectsNULAnywhere(t *testing.T) {
	for _, raw := range []string{
		`{"question":"a\u0000b"}`,
		`{"question":"q","options":[{"label":"a\u0000"},{"label":"b"}]}`,
		`{"question":"q","options":[{"label":"a","description":"d\u0000"},{"label":"b"}]}`,
	} {
		rec := &askRecorder{}
		if result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(raw)); !result.IsError || len(rec.asked()) != 0 {
			t.Fatalf("NUL was accepted: %s", raw)
		}
	}
}

func TestQuestionLimitCountsOnlyVisibleText(t *testing.T) {
	zeroWidth := strings.Repeat("\u200b", 5000)
	raw, _ := json.Marshal(map[string]any{"question": strings.Repeat("q", 1000) + zeroWidth})
	rec := &askRecorder{answer: core.Answer{Text: "x"}}
	if result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(string(raw))); result.IsError {
		t.Fatalf("hidden characters should not count against the limit: %s", result.Content)
	}
}

func TestQuestionRequiresSomethingVisible(t *testing.T) {
	for name, raw := range map[string]string{
		"question of joiners":      `{"question":"‍‌"}`,
		"question of fillers":      `{"question":"ㅤ⠀ ͏"}`,
		"label of joiners":         `{"question":"q","options":[{"label":"‍"},{"label":"b"}]}`,
		"label of a lone mark":     `{"question":"q","options":[{"label":"́"},{"label":"b"}]}`,
		"label of blank cells":     `{"question":"q","options":[{"label":"a"},{"label":"ㅤ⠀"}]}`,
		"variation selectors only": `{"question":"q","options":[{"label":"󠄁󠄂"},{"label":"b"}]}`,
	} {
		rec := &askRecorder{answer: core.Answer{Text: "x"}}
		if result := WithQuestions(&stubExecutor{}, rec.ask).Execute(context.Background(), questionCall(raw)); !result.IsError || len(rec.asked()) != 0 {
			t.Errorf("%s was accepted: %s", name, raw)
		}
	}
}
