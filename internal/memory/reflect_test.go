package memory

import (
	"encoding/json"
	"strings"
	"testing"

	"julia/internal/core"
)

func TestParseReflection(t *testing.T) {
	good := `{"operations":[{"action":"save","scope":"workspace","name":"make-check","type":"feedback","description":"Run make check before reporting done","body":"**Why:** asked on 2026-10-16.","paths":[],"commands":["^go test"],"keywords":[],"evidence":"user: run make check"}],"helped":["sir"],"misled":[]}`
	r, err := ParseReflection(good)
	if err != nil || len(r.Operations) != 1 || r.Operations[0].Commands[0] != "^go test" || r.Helped[0] != "sir" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := ParseReflection("```json\n" + good + "\n```"); err != nil || len(r.Operations) != 1 {
		t.Fatalf("fenced: %+v %v", r, err)
	}
	if _, err := ParseReflection(`{"operations":[],"helped":[],"misled":[]}`); err != nil {
		t.Fatal(err)
	}
	if r, err := ParseReflection(`{"operations":[],"helped":[],"misled":[],"ignored":["sir"]}`); err != nil || len(r.Ignored) != 1 {
		t.Fatalf("ignored: %+v %v", r, err)
	}
	for name, text := range map[string]string{
		"trailing":      good + " and more",
		"unknown field": `{"operations":[],"helped":[],"misled":[],"note":"x"}`,
		"bad action":    `{"operations":[{"action":"remember","scope":"workspace","name":"a"}],"helped":[],"misled":[]}`,
		"too many":      `{"operations":[{"action":"forget","scope":"workspace","name":"a"},{"action":"forget","scope":"workspace","name":"b"},{"action":"forget","scope":"workspace","name":"c"},{"action":"forget","scope":"workspace","name":"d"}],"helped":[],"misled":[]}`,
		"not json":      "I would save a note about make check.",
	} {
		if _, err := ParseReflection(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReflectionPromptFitsItsBudget(t *testing.T) {
	s, workspace := fixture(t)
	if err := s.Save(&Note{Name: "sir", Description: "Address him as sir, sparingly", Type: TypeUser, Scope: Scope{Kind: ScopeGlobal}}); err != nil {
		t.Fatal(err)
	}
	recall, err := s.Recall(workspace)
	if err != nil {
		t.Fatal(err)
	}
	turn := Turn{Workspace: workspace, Warnings: []string{"Automatic review denied: curl to example.com"}, Tainted: []string{"WebFetch", "Subagent"}}
	turn.Messages = append(turn.Messages, core.Message{Role: "user", Content: "Fix the build; always run make check first."})
	for i := range 40 {
		turn.Messages = append(turn.Messages,
			core.Message{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "c", Name: "Bash", Arguments: json.RawMessage(`{"command":"` + strings.Repeat("x", 3000) + `"}`)}}},
			core.Message{Role: "tool", ToolCallID: "c", Content: "START " + strings.Repeat("y", 20000) + " END " + string(rune('a'+i))})
	}
	turn.Messages = append(turn.Messages, core.Message{Role: "assistant", Content: "Done, sir.", Images: []core.Image{{MediaType: "image/png", Data: []byte("not sent")}}})
	prompt := ReflectionPrompt(turn, recall)
	if !strings.Contains(prompt, "Workspace: "+workspace) || !strings.Contains(prompt, "- sir · user") || !strings.Contains(prompt, `"warnings":["Automatic review denied: curl to example.com"]`) || !strings.Contains(prompt, "through WebFetch, Subagent: save only") {
		t.Fatalf("prompt lacks its parts:\n%s", prompt[:min(len(prompt), 2000)])
	}
	_, encoded, _ := strings.Cut(prompt, "## The turn, as JSON\n\n")
	if len(encoded) > MaxReflectionBytes {
		t.Fatalf("transcript is %d bytes", len(encoded))
	}
	var decoded struct {
		Messages []struct {
			Role, Content string
			Images        int
		}
	}
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("transcript is not JSON: %v", err)
	}
	if decoded.Messages[0].Role != "user" || !strings.Contains(decoded.Messages[0].Content, "make check") {
		t.Fatal("the user's request was dropped")
	}
	last := decoded.Messages[len(decoded.Messages)-1]
	if last.Content != "Done, sir." || last.Images != 1 || strings.Contains(encoded, "not sent") {
		t.Fatalf("last message %+v", last)
	}
	if !strings.Contains(encoded, "bytes omitted") {
		t.Fatal("long bodies were not capped")
	}
	// A tiny turn is sent whole.
	small := ReflectionPrompt(Turn{Workspace: workspace, Messages: turn.Messages[:1]}, Recall{})
	if !strings.Contains(small, "(none)") || !strings.Contains(small, "Fix the build; always run make check first.") || strings.Contains(small, "outside the user's own words") {
		t.Fatalf("small prompt:\n%s", small)
	}
}

func TestApplyReflection(t *testing.T) {
	s, workspace := fixture(t)
	ws := Scope{Kind: ScopeWorkspace, Workspace: workspace}
	if err := s.Save(&Note{Name: "old", Description: "Superseded", Type: TypeProject, Scope: ws}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&Note{Name: "sir", Description: "Address him as sir", Type: TypeUser, Scope: Scope{Kind: ScopeGlobal}}); err != nil {
		t.Fatal(err)
	}
	applied := s.Apply(Reflection{
		Operations: []Operation{
			{Action: "save", Scope: ScopeWorkspace, Name: "make-check", Type: TypeFeedback, Description: "Run make check before reporting done", Body: "**Why:** asked.", Commands: []string{"^go test"}, Evidence: "user said so"},
			{Action: "forget", Scope: ScopeWorkspace, Name: "old"},
			{Action: "save", Scope: ScopeGlobal, Name: "Bad Name", Type: TypeUser, Description: "x"},
		},
		Helped:  []string{"sir", "unknown"},
		Misled:  []string{"make-check"},
		Ignored: []string{"sir"},
	}, workspace, "8c1847d4a71d39a54d278801", true)
	if len(applied) != 3 {
		t.Fatalf("applied %d", len(applied))
	}
	if applied[0].Err != nil || applied[0].Verb != "Saved" || applied[0].Note == nil || applied[0].Note.Source != SourceReflection || applied[0].Note.Session != "8c1847d4a71d39a54d278801" || applied[0].Note.Triggers.Commands[0] != "^go test" || !applied[0].Note.Tainted {
		t.Fatalf("save: %+v", applied[0])
	}
	if applied[1].Err != nil || applied[1].Verb != "Forgot" || applied[1].Note == nil || applied[1].Note.Description != "Superseded" {
		t.Fatalf("forget: %+v", applied[1])
	}
	if applied[2].Err == nil || applied[2].Verb != "" || applied[2].Note != nil {
		t.Fatalf("invalid save: %+v", applied[2])
	}
	recall, err := s.Recall(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := recall.Find("old"); ok {
		t.Fatal("old note survives")
	}
	if note, _ := recall.Find("sir"); note.Helped != 1 || note.Ignored != 1 {
		t.Fatalf("helped not tallied: %+v", note)
	}
	if note, _ := recall.Find("make-check"); note.Misled != 1 {
		t.Fatalf("misled not tallied: %+v", note)
	}
	// Replacing names the existing note.
	again := s.Apply(Reflection{Operations: []Operation{{Action: "save", Scope: ScopeWorkspace, Name: "make-check", Type: TypeFeedback, Description: "Run make check, which vets too"}}}, workspace, "", false)
	if again[0].Verb != "Replaced" || again[0].Err != nil {
		t.Fatalf("replace: %+v", again[0])
	}
	// A later clean reflection keeps the counters and, since no person vouched, the unverified mark.
	if note, _ := s.Load(ws, "make-check"); note.Misled != 1 || note.Description != "Run make check, which vets too" || !note.Tainted {
		t.Fatalf("replaced note lost its counters: %+v", note)
	}
}
