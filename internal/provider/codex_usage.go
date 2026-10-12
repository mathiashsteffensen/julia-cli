package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"julia/internal/core"
)

// WeeklyUsage reads account metadata without starting a conversation, consuming
// reset credits, or opting into a reserve model. Nil means quota is unavailable.
func (c *Codex) WeeklyUsage(ctx context.Context) (*core.WeeklyUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "julia-usage-")
	if err != nil {
		return nil, fmt.Errorf("create usage lookup workspace: %w", err)
	}
	defer os.RemoveAll(directory)
	start := c.start
	if start == nil {
		start = startCodex
	}
	client, err := start(ctx, c.Binary, directory)
	if err != nil {
		return nil, err
	}
	defer client.close()
	if _, err := client.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "julia_cli", "title": "JULIA", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return nil, fmt.Errorf("initialize Codex usage lookup: %w", err)
	}
	if err := client.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	data, err := client.call(ctx, "account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true, "supportsLunaReserve": false})
	if err != nil {
		return nil, fmt.Errorf("read Codex account usage: %w", err)
	}
	return parseCodexWeeklyUsage(data)
}

func parseCodexWeeklyUsage(data []byte) (*core.WeeklyUsage, error) {
	type window struct {
		UsedPercent        *float64 `json:"usedPercent"`
		WindowDurationMins *int64   `json:"windowDurationMins"`
		ResetsAt           *int64   `json:"resetsAt"`
	}
	type snapshot struct {
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
	}
	var response struct {
		RateLimits          *snapshot            `json:"rateLimits"`
		RateLimitsByLimitID map[string]*snapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode Codex account usage: %w", err)
	}
	selected := response.RateLimits
	if codex, exists := response.RateLimitsByLimitID["codex"]; exists {
		// Do not fill missing data in the authoritative bucket from a potentially
		// unrelated or stale legacy snapshot.
		selected = codex
	}
	if selected == nil {
		return nil, nil
	}
	for _, candidate := range []*window{selected.Primary, selected.Secondary} {
		if candidate == nil || candidate.UsedPercent == nil || candidate.WindowDurationMins == nil || *candidate.WindowDurationMins != 7*24*60 {
			continue
		}
		usage := &core.WeeklyUsage{RemainingPercent: math.Max(0, math.Min(100, 100-*candidate.UsedPercent))}
		if candidate.ResetsAt != nil && *candidate.ResetsAt > 0 {
			usage.ResetsAt = time.Unix(*candidate.ResetsAt, 0)
		}
		return usage, nil
	}
	return nil, nil
}
