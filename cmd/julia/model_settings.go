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
	key, err := apiKey(cfg, cfg.Provider)
	if err != nil {
		return nil, nil, err
	}
	api := provider.APIConfig{Provider: cfg.Provider, Model: cfg.Model, APIKey: key, Reasoning: cfg.Reasoning, MaxSteps: cfg.MaxSteps, ContextTokens: cfg.ContextTokens, Checkpoint: checkpoint}
	// A named provider is an OpenAI-compatible endpoint of its own.
	if endpoint, ok := cfg.Endpoint(cfg.Provider); ok {
		api.Provider, api.Name, api.BaseURL = "compatible", endpoint.ID, endpoint.BaseURL
	}
	backend, err := provider.NewAPI(ctx, api)
	if err != nil {
		return nil, nil, err
	}
	return backend, approvalReviewer(cfg), nil
}

// approvalReviewer is the reviewer_model's reviewer, never the executing model.
func approvalReviewer(cfg config.Config) core.Approver {
	reviewer, model := cfg.Reviewer()
	if reviewer != "codex" {
		return decisionsApprovalReviewer(cfg, reviewer, model)
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

// decisionsApprovalReviewer reviews through the Decisions API of the named
// provider id.
func decisionsApprovalReviewer(cfg config.Config, id, model string) core.Approver {
	return func(ctx context.Context, approval core.Approval) (core.Decision, error) {
		endpoint, ok := cfg.Endpoint(id)
		if !ok {
			return core.Decision{}, fmt.Errorf("reviewer provider %q is not configured", id)
		}
		key, err := apiKey(cfg, id)
		if err != nil {
			return core.Decision{}, err
		}
		return provider.ReviewDecisionAPI(ctx, provider.APIConfig{
			Provider: "compatible",
			Name:     id,
			Model:    model,
			BaseURL:  endpoint.BaseURL,
			APIKey:   key,
		}, approval)
	}
}

// apiKey reads the API key of provider from its environment variable. Each
// key belongs to one provider and is never sent to another's endpoint. A
// named provider without api_key_env is keyless.
func apiKey(cfg config.Config, provider string) (string, error) {
	env := cfg.KeyEnv(provider)
	if env == "" {
		if cfg.IsNamed(provider) {
			return "", nil
		}
		return "", fmt.Errorf("the %s provider has no API key", provider)
	}
	key := os.Getenv(env)
	if key == "" {
		return "", fmt.Errorf("set %s for the %s provider", env, provider)
	}
	return key, nil
}

// Save before publishing the new settings. Failed validation or persistence must
// leave the live conversation and its continuation state untouched.
func applySessionSettings(cfg config.Config, current *core.Session, selectedProvider, model, reasoning string, save func(*core.Session) error) error {
	if selectedProvider != current.Provider {
		// Efforts belong to a provider; do not carry them across providers. The
		// destination model still starts at the effort its config entry gives.
		reasoning = cfg.ReasoningFor(selectedProvider, model)
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
