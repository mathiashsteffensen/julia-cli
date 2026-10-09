package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"jarvis/internal/config"
	"jarvis/internal/core"
	"jarvis/internal/session"
)

type delegationBackend struct{ parentID string }

func (b *delegationBackend) Name() string { return "fake" }
func (b *delegationBackend) Run(ctx context.Context, s *core.Session, prompt string, executor core.ToolExecutor, emit core.Emit) error {
	if s.ParentID == "" {
		result := executor.Execute(ctx, core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":["delegated task one","delegated task two"]}`)})
		if result.IsError || strings.Contains(result.Content, `"error"`) {
			return errors.New(result.Content)
		}
		if !strings.Contains(result.Content, "child answer") {
			return errors.New("child response missing")
		}
		return nil
	}
	if s.ParentID != b.parentID || len(s.Messages) != 0 || s.RemoteID != "" || len(s.State) != 0 {
		return errors.New("child session not isolated")
	}
	if s.Model != "selected-model" || s.Reasoning != "high" || !strings.Contains(s.Instructions, "delegated subagent") {
		return errors.New("child settings/instructions lost")
	}
	for _, def := range executor.Definitions() {
		if def.Name == "Subagent" {
			return errors.New("recursive delegation exposed")
		}
	}
	result := executor.Execute(ctx, core.ToolCall{Name: "Write", Arguments: json.RawMessage(`{"path":"blocked.txt","content":"test"}`)})
	if !result.IsError {
		return errors.New("approval denial bypassed")
	}
	emit(core.Event{Kind: "text", Text: "private child text"})
	emit(core.Event{Kind: "status", Text: "child status"})
	s.Messages = append(s.Messages, core.Message{Role: "user", Content: prompt}, core.Message{Role: "assistant", Content: "child answer"})
	return nil
}

func TestRuntimeSubagentsInheritSettingsAndAuthorization(t *testing.T) {
	cfg := config.Default()
	cfg.SkillPaths = []string{}
	cfg.Mode = "ask"
	s := session.New(t.TempDir(), "fake", "selected-model")
	s.Reasoning = "high"
	s.Messages = []core.Message{{Role: "user", Content: "original user request"}}
	backend := &delegationBackend{parentID: s.ID}
	var mu sync.Mutex
	saved := map[string]core.Session{}
	approvalCalls := 0
	runner := &runtimeBackend{backend: backend, cfg: cfg, timeout: 5 * time.Second, parent: context.Background(),
		makeBackend: func(_ context.Context, selected config.Config) (core.Backend, core.Approver, error) {
			if selected.Model != "selected-model" || selected.Reasoning != "high" || selected.Mode != "ask" {
				return nil, nil, errors.New("factory settings lost")
			}
			return backend, nil, nil
		},
		approve: func(_ context.Context, a core.Approval) (core.Decision, error) {
			if !strings.Contains(a.Task, "original user request") || !strings.Contains(a.Task, "current follow-up") || strings.Contains(a.Task, "delegated task") {
				return core.Decision{}, errors.New("wrong authorization context")
			}
			approvalCalls++
			return core.Decision{Allow: false, Reason: "test denial"}, nil
		},
		checkpoint: func(child *core.Session) error { mu.Lock(); defer mu.Unlock(); saved[child.ID] = *child; return nil },
	}
	err := runner.Run(context.Background(), s, "current follow-up", nil, func(event core.Event) {
		if event.Kind == "text" {
			t.Error("child text leaked into parent stream")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if approvalCalls != 2 || len(saved) != 2 {
		t.Fatalf("approvals=%d children=%d", approvalCalls, len(saved))
	}
	for _, child := range saved {
		if len(child.Audit) != 1 || len(child.Messages) != 2 {
			t.Fatal("child final transcript not saved")
		}
	}
	if err := selectSessionModel(&cfg, &core.Session{ParentID: s.ID}, s.Directory, "", "", false, true); err == nil {
		t.Fatal("child allowed to resume independently")
	}
}
