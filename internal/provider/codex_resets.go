package provider

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"julia/internal/core"
)

// A run can spend at most one existing reset credit. An uncertain response is
// never retried, and missing backend permission is not inferred from usage.
type codexUsageReset struct {
	enabled   bool
	attempted bool
}

func (r *codexUsageReset) try(ctx context.Context, client *codexClient, emit core.Emit) bool {
	if !r.enabled || r.attempted {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := client.call(ctx, "account/rateLimits/read", map[string]any{
		"supportsLunaReserve":       false,
		"excludeResetCreditDetails": true,
	})
	if err != nil {
		r.attempted = true
		emit(core.Event{Kind: "warning", Text: "Codex usage reset unavailable: " + err.Error()})
		return false
	}
	var limits struct {
		OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
		ResetCredits         *struct {
			AvailableCount int64 `json:"availableCount"`
		} `json:"rateLimitResetCredits"`
	}
	if err = json.Unmarshal(data, &limits); err != nil {
		r.attempted = true
		emit(core.Event{Kind: "warning", Text: "Codex returned invalid usage reset details."})
		return false
	}
	if limits.OrdinaryUsageAllowed == nil || *limits.OrdinaryUsageAllowed || limits.ResetCredits == nil || limits.ResetCredits.AvailableCount <= 0 {
		return false
	}

	r.attempted = true
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		emit(core.Event{Kind: "warning", Text: "Could not create Codex usage reset idempotency key."})
		return false
	}
	key[6] = key[6]&0x0f | 0x40
	key[8] = key[8]&0x3f | 0x80
	idempotencyKey := fmt.Sprintf("%x-%x-%x-%x-%x", key[:4], key[4:6], key[6:8], key[8:10], key[10:])
	data, err = client.call(ctx, "account/rateLimitResetCredit/consume", map[string]any{"idempotencyKey": idempotencyKey})
	if err != nil {
		emit(core.Event{Kind: "warning", Text: "Codex usage reset failed or its outcome is uncertain. It will not be retried: " + err.Error()})
		return false
	}
	var result struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(data, &result) != nil || result.Outcome != "reset" {
		emit(core.Event{Kind: "warning", Text: "Codex did not confirm a usage reset. Continuing without another reset attempt."})
		return false
	}
	emit(core.Event{Kind: "status", Text: "Codex usage reset applied using an available reset credit."})
	return true
}
