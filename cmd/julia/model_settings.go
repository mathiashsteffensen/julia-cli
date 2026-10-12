package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/provider"
	sessions "julia/internal/session"
)

// The executing model follows session settings; approval uses the configured reviewer.
func newBackend(ctx context.Context, cfg config.Config, checkpoint func(*core.Session) error) (core.Backend, core.Approver, error) {
	if cfg.Provider == "codex" {
		binary, err := findCodex(cfg.CodexBinary)
		if err != nil {
			return nil, nil, err
		}
		reviewer, reviewModel := cfg.Reviewer()
		backend := &provider.Codex{Binary: binary, Model: cfg.Model, Reasoning: cfg.Reasoning, ReadOnly: cfg.Mode == "read-only", Checkpoint: checkpoint, ReviewModel: reviewModel}
		if reviewer == "codex" {
			return backend, backend.Review, nil
		}
		return backend, approvalReviewer(cfg), nil
	}
	key := os.Getenv(cfg.KeyEnv())
	if key == "" && (cfg.Provider != "compatible" || cfg.Enables("compatible")) {
		return nil, nil, fmt.Errorf("set %s for the %s provider", cfg.KeyEnv(), cfg.Provider)
	}
	// Only the compatible provider has a configurable endpoint.
	baseURL := ""
	if cfg.Provider == "compatible" {
		baseURL = cfg.CompatibleBaseURL
	}
	backend, err := provider.NewAPI(ctx, provider.APIConfig{Provider: cfg.Provider, Model: cfg.Model, BaseURL: baseURL, APIKey: key, Reasoning: cfg.Reasoning, MaxSteps: cfg.MaxSteps, ContextTokens: cfg.ContextTokens, Checkpoint: checkpoint})
	if err != nil {
		return nil, nil, err
	}
	return backend, approvalReviewer(cfg), nil
}

// approvalReviewer is the reviewer_model's reviewer, never the executing model.
func approvalReviewer(cfg config.Config) core.Approver {
	reviewer, model := cfg.Reviewer()
	if reviewer == "compatible" {
		return compatibleModelApprovalReviewer(cfg, model)
	}
	return codexApprovalReviewer(cfg.CodexBinary, model)
}

// Resolve lazily so API sessions can still use reads and human approval when
// Codex is unavailable. Review failures use the existing human fallback.
func codexApprovalReviewer(binary, model string) core.Approver {
	return func(ctx context.Context, approval core.Approval) (core.Decision, error) {
		resolved, err := findCodex(binary)
		if err != nil {
			return core.Decision{}, fmt.Errorf("Codex approval reviewer unavailable: %w", err)
		}
		return (&provider.Codex{Binary: resolved, ReviewModel: model}).Review(ctx, approval)
	}
}

func compatibleModelApprovalReviewer(cfg config.Config, model string) core.Approver {
	return func(ctx context.Context, approval core.Approval) (core.Decision, error) {
		keyEnv := compatibleAPIKeyEnv(cfg)
		key := os.Getenv(keyEnv)
		if key == "" {
			return core.Decision{}, fmt.Errorf("set %s for the compatible approval reviewer", keyEnv)
		}
		return provider.ReviewDecisionAPI(ctx, provider.APIConfig{
			Provider: "compatible",
			Model:    model,
			BaseURL:  cfg.CompatibleBaseURL,
			APIKey:   key,
		}, approval)
	}
}

// compatibleAPIKeyEnv names the key for the compatible endpoint. api_key_env is
// honored only when no other API provider owns it: Codex has no key of its own,
// so there it describes the compatible endpoint, but under openai, anthropic or
// google it belongs to that provider and must not be sent elsewhere.
func compatibleAPIKeyEnv(cfg config.Config) string {
	if cfg.APIKeyEnv != "" && (cfg.Provider == "compatible" || cfg.Provider == "codex") {
		return cfg.APIKeyEnv
	}
	cfg.Provider, cfg.APIKeyEnv = "compatible", ""
	return cfg.KeyEnv()
}

// Save before publishing the new settings. Failed validation or persistence must
// leave the live conversation and its continuation state untouched.
func applySessionSettings(cfg config.Config, current *core.Session, selectedProvider, model, reasoning string, save func(*core.Session) error) error {
	if selectedProvider != current.Provider {
		// Efforts belong to a provider; do not carry them across providers.
		reasoning = ""
	}
	candidate := *current
	candidate.Provider = selectedProvider
	candidate.Model, candidate.Reasoning, candidate.ReasoningSet = model, reasoning, true
	cfg.Provider, cfg.Model, cfg.Reasoning, cfg.ModelReasoning = selectedProvider, model, reasoning, ""
	if err := cfg.Validate(); err != nil {
		return err
	}
	if selectedProvider != "codex" && model == "" {
		return errors.New("this provider requires a model ID")
	}
	newConversation := selectedProvider != current.Provider || (selectedProvider != "codex" && model != current.Model)
	if newConversation && hasSessionContinuation(current) {
		if save != nil {
			previous := *current
			if err := save(&previous); err != nil {
				return fmt.Errorf("save current conversation: %w", err)
			}
		}
		fresh := sessions.New(current.Directory, selectedProvider, model)
		fresh.Reasoning, fresh.ReasoningSet = reasoning, true
		if save != nil {
			if err := save(fresh); err != nil {
				return fmt.Errorf("save new conversation: %w", err)
			}
		}
		*current = *fresh
		return nil
	}
	if save != nil {
		if err := save(&candidate); err != nil {
			return fmt.Errorf("save model settings: %w", err)
		}
	}
	*current = candidate
	return nil
}

func hasSessionContinuation(current *core.Session) bool {
	return len(current.Messages) > 0 || len(current.State) > 0 || current.RemoteID != "" || current.PendingTool != nil || len(current.Audit) > 0
}
