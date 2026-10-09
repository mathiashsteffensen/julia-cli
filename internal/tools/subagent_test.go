package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jarvis/internal/core"
)

func TestSubagentsConcurrentOrderedAndConfined(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir(), Mode: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	executor := WithSubagents(base, func(ctx context.Context, task string, child core.ToolExecutor) (string, error) {
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
			{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["recurse"]}`)},
		} {
			if !child.Execute(ctx, call).IsError {
				return "", errors.New("permission boundary bypassed")
			}
		}
		if task == "second" {
			return task, errors.New("child failed")
		}
		return task, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan core.ToolResult, 1)
	go func() {
		done <- executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["first","second","third"]}`)})
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
		Task          int
		Output, Error string
	}
	if result.IsError || json.Unmarshal([]byte(result.Content), &got) != nil || len(got) != 3 {
		t.Fatalf("bad result: %+v", result)
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].Task != i+1 || got[i].Output != want {
			t.Fatalf("wrong results: %+v", got)
		}
	}
	if got[1].Error != "child failed" || got[0].Error != "" || got[2].Error != "" {
		t.Fatalf("errors lost: %+v", got)
	}
}

func TestSubagentValidationAndOutputBounds(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	var calls atomic.Int32
	executor := WithSubagents(base, func(context.Context, string, core.ToolExecutor) (string, error) {
		calls.Add(1)
		return strings.Repeat("\x00世", 20000), errors.New(strings.Repeat("\x00", 20000))
	})
	for _, raw := range []string{`{}`, `null`, `{"tasks":null}`, `{"tasks":[]}`, `{"tasks":[""]}`, `{"tasks":["a","b","c","d"]}`, `{"tasks":["a"],"model":"other"}`, `{"tasks":[1]}`, `{"tasks":["a"]} {}`} {
		if !executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(raw)}).IsError {
			t.Fatalf("accepted %s", raw)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments spawned child")
	}
	result := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["a","b","c"]}`)})
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
	executor := WithSubagents(base, func(ctx context.Context, _ string, _ core.ToolExecutor) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		finished.Add(1)
		return "", ctx.Err()
	})
	done := make(chan core.ToolResult, 1)
	go func() {
		done <- executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["a","b","c"]}`)})
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
	if !executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["a"]}`)}).IsError {
		t.Fatal("started after cancellation")
	}
}

func TestRegistryMatchesExecutors(t *testing.T) {
	base, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	for _, subagents := range []bool{false, true} {
		var executor core.ToolExecutor = base
		if subagents {
			executor = WithSubagents(base, nil)
		}
		actual, _ := json.Marshal(executor.Definitions())
		registered, _ := json.Marshal(Definitions(subagents))
		if string(actual) != string(registered) {
			t.Fatal("display registry differs from executor")
		}
	}
}
