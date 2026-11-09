package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"julia/agents"
	"julia/internal/core"
)

func TestSubagentsConcurrentOrderedAndConfined(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir(), Mode: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	executor := WithSubagents(base, agents.Builtin(), func(ctx context.Context, role agents.Role, task string, child core.ToolExecutor) (string, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		for _, def := range child.Definitions() {
			if def.Name == "Subagent" {
				return "", errors.New("recursive delegation exposed")
			}
		}
		for _, call := range []core.ToolCall{
			{Name: "Write", Arguments: json.RawMessage(`{"path":"x","content":"bad"}`)},
			{Name: "Read", Arguments: json.RawMessage(`{"path":"../secret"}`)},
			{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"delegate","task":"recurse"}]}`)},
		} {
			if !child.Execute(ctx, call).IsError {
				return "", errors.New("permission boundary bypassed")
			}
		}
		if task == "second" {
			return task, errors.New("child failed")
		}
		return role.Name + ":" + task, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan core.ToolResult, 1)
	go func() {
		done <- executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"scout","task":"first"},{"role":"delegate","task":"second"},{"role":"worker","task":"third"}]}`)})
	}()
	for range 3 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("children did not start concurrently")
		}
	}
	close(release)
	result := <-done
	var got []struct {
		Task                int
		Role, Output, Error string
	}
	if result.IsError || json.Unmarshal([]byte(result.Content), &got) != nil || len(got) != 3 {
		t.Fatalf("bad result: %+v", result)
	}
	for i, want := range []struct{ role, output string }{{"scout", "scout:first"}, {"delegate", "second"}, {"worker", "worker:third"}} {
		if got[i].Task != i+1 || got[i].Role != want.role || got[i].Output != want.output {
			t.Fatalf("wrong results: %+v", got)
		}
	}
	if got[1].Error != "child failed" || got[0].Error != "" || got[2].Error != "" {
		t.Fatalf("errors lost: %+v", got)
	}
}

// A role that lists tools gives its children those and no others, whatever
// the model calls; a role that lists none gives every tool.
func TestSubagentRoleLimitsTools(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	roles := []agents.Role{
		{Name: "looker", Description: "Looks.", Tools: []string{"Grep", "Read"}, Prompt: "Look."},
		{Name: "anything", Description: "Does anything.", Prompt: "Do."},
	}
	seen := map[string][]string{}
	executor := WithSubagents(base, roles, func(ctx context.Context, role agents.Role, _ string, child core.ToolExecutor) (string, error) {
		for _, def := range child.Definitions() {
			seen[role.Name] = append(seen[role.Name], def.Name)
		}
		write := child.Execute(ctx, core.ToolCall{Name: "Write", Arguments: json.RawMessage(`{"path":"made.txt","content":"x"}`)})
		read := child.Execute(ctx, core.ToolCall{Name: "Read", Arguments: json.RawMessage(`{"path":"made.txt"}`)})
		return write.Content + "\n" + read.Content, nil
	})
	var got []struct{ Role, Output string }
	for _, role := range []string{"looker", "anything"} {
		result := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"` + role + `","task":"go"}]}`)})
		var one []struct{ Role, Output string }
		if result.IsError || json.Unmarshal([]byte(result.Content), &one) != nil || len(one) != 1 {
			t.Fatalf("%s: %+v", role, result)
		}
		got = append(got, one...)
	}
	if !slices.Equal(seen["looker"], []string{"Read", "Grep"}) {
		t.Fatalf("looker sees %v, want its tools in registry order", seen["looker"])
	}
	if !slices.Equal(seen["anything"], toolNames(Definitions(nil, false, false, false, false))) {
		t.Fatalf("anything sees %v, want every tool", seen["anything"])
	}
	if !strings.Contains(got[0].Output, "the looker role cannot use Write; its tools are Grep, Read") || strings.Contains(got[0].Output, "cannot use Read") {
		t.Fatalf("looker wrote, or was refused a tool it has: %q", got[0].Output)
	}
	if !strings.Contains(got[1].Output, "1\tx") {
		t.Fatalf("anything could not write and read back: %q", got[1].Output)
	}
}

// toolNames lists definitions' tool names, for comparing registries.
func toolNames(definitions []core.ToolDefinition) []string {
	var names []string
	for _, def := range definitions {
		names = append(names, def.Name)
	}
	return names
}

func TestSubagentDefinitionOffersEachRole(t *testing.T) {
	roles := append(agents.Builtin(), agents.Role{Name: "custom", Description: "A role of the user's own.", Tools: []string{"Read"}, Prompt: "Be custom."})
	def := subagentDefinition(roles)
	tasks := def.Parameters["properties"].(map[string]any)["tasks"].(map[string]any)
	item := tasks["items"].(map[string]any)
	if required := item["required"].([]string); !slices.Equal(required, []string{"role", "task"}) || item["additionalProperties"] != false {
		t.Fatalf("a task must be exactly a role and a task: %v", item)
	}
	role := item["properties"].(map[string]any)["role"].(map[string]any)
	if !slices.Equal(role["enum"].([]string), agents.Names(roles)) {
		t.Fatalf("role enum %v, want %v", role["enum"], agents.Names(roles))
	}
	for _, r := range roles {
		want := "\n- " + r.Name
		if len(r.Tools) > 0 {
			want += " (" + strings.Join(r.Tools, ", ") + ")"
		}
		if !strings.Contains(def.Description, want+": "+r.Description) {
			t.Errorf("description lacks %q: %s", want, def.Description)
		}
	}
	if _, err := json.Marshal(def); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentValidationAndOutputBounds(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	var calls atomic.Int32
	executor := WithSubagents(base, agents.Builtin(), func(context.Context, agents.Role, string, core.ToolExecutor) (string, error) {
		calls.Add(1)
		return strings.Repeat("\x00世", 20000), errors.New(strings.Repeat("\x00", 20000))
	})
	task := func(role, text string) string { return `{"role":"` + role + `","task":"` + text + `"}` }
	for _, raw := range []string{
		`{}`, `null`, `{"tasks":null}`, `{"tasks":[]}`, `{"tasks":[1]}`,
		`{"tasks":["a"]}`,
		`{"tasks":[{"task":"a"}]}`,
		`{"tasks":[{"role":"scout"}]}`,
		`{"tasks":[` + task("scout", "") + `]}`,
		`{"tasks":[` + task("nobody", "a") + `]}`,
		`{"tasks":[` + task("Scout", "a") + `]}`,
		`{"tasks":[{"role":"scout","task":"a","model":"other"}]}`,
		`{"tasks":[` + task("scout", "a") + `],"model":"other"}`,
		`{"tasks":[` + strings.Repeat(task("scout", "a")+",", 3) + task("scout", "a") + `]}`,
		`{"tasks":[` + task("scout", "a") + `]} {}`,
	} {
		if !executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(raw)}).IsError {
			t.Fatalf("accepted %s", raw)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments spawned child")
	}
	unknown := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[` + task("nobody", "a") + `]}`)})
	if !strings.Contains(unknown.Content, `unknown role "nobody"; roles are delegate, oracle, researcher, reviewer, scout, worker`) {
		t.Fatalf("unknown role not explained: %s", unknown.Content)
	}
	legacy := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["a"]}`)})
	if !strings.Contains(legacy.Content, `each task is {"role": ..., "task": ...}`) {
		t.Fatalf("a bare task is not explained: %s", legacy.Content)
	}
	result := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[` + task("scout", "a") + `,` + task("reviewer", "b") + `,` + task("delegate", "c") + `]}`)})
	if result.IsError || len(result.Content) > maxOutput || !json.Valid([]byte(result.Content)) {
		t.Fatalf("invalid/big output: %d", len(result.Content))
	}
}

func TestSubagentCancellationDrainsChildren(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 3)
	var finished atomic.Int32
	executor := WithSubagents(base, agents.Builtin(), func(ctx context.Context, _ agents.Role, _ string, _ core.ToolExecutor) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		finished.Add(1)
		return "", ctx.Err()
	})
	three := `{"tasks":[{"role":"scout","task":"a"},{"role":"scout","task":"b"},{"role":"scout","task":"c"}]}`
	done := make(chan core.ToolResult, 1)
	go func() {
		done <- executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(three)})
	}()
	for range 3 {
		<-started
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation hung")
	}
	if finished.Load() != 3 {
		t.Fatal("returned before children exited")
	}
	if !executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"scout","task":"a"}]}`)}).IsError {
		t.Fatal("started after cancellation")
	}
}

func TestRegistryMatchesExecutors(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	for _, roles := range [][]agents.Role{nil, agents.Builtin()} {
		executor := WithSubagents(base, roles, nil)
		if roles == nil && executor != core.ToolExecutor(base) {
			t.Fatal("without roles, delegation was offered")
		}
		actual, _ := json.Marshal(executor.Definitions())
		registered, _ := json.Marshal(Definitions(roles, false, false, false, false))
		if string(actual) != string(registered) {
			t.Fatal("display registry differs from executor")
		}
	}
}
