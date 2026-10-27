package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"julia/internal/core"
)

// bootstrap starts a Claude Code process in a temporary directory that answers
// control requests, such as the model catalog or the account's usage, without
// starting a conversation or making an inference request.
func (c *Claude) bootstrap(ctx context.Context, purpose string) (*claudeClient, func(), error) {
	directory, err := os.MkdirTemp("", "julia-"+purpose+"-")
	if err != nil {
		return nil, nil, fmt.Errorf("create %s workspace: %w", purpose, err)
	}
	start := c.start
	if start == nil {
		start = startClaude
	}
	client, err := start(ctx, c.Binary, claudeBootstrapArgs(), directory)
	if err != nil {
		os.RemoveAll(directory)
		return nil, nil, err
	}
	return client, func() { client.close(); os.RemoveAll(directory) }, nil
}

// Models reads Claude Code's selectable models: its aliases, each with the
// efforts it takes, and the model its default resolves to, marked as such.
func (c *Claude) Models(ctx context.Context) ([]core.ModelOption, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, done, err := c.bootstrap(ctx, "models")
	if err != nil {
		return nil, err
	}
	defer done()
	data, err := client.call(ctx, map[string]any{"subtype": "list_models"}, nil)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code model catalog: %w", err)
	}
	return parseClaudeModels(data)
}

func parseClaudeModels(data []byte) ([]core.ModelOption, error) {
	var response struct {
		Models []struct {
			Value       string   `json:"value"`
			Resolved    string   `json:"resolvedModel"`
			DisplayName string   `json:"displayName"`
			Description string   `json:"description"`
			Efforts     []string `json:"supportedEffortLevels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode Claude Code model catalog: %w", err)
	}
	models := []core.ModelOption{}
	seen := map[string]bool{}
	defaultModel := ""
	for _, item := range response.Models {
		id := strings.TrimSpace(item.Value)
		if id == "default" {
			// The default is listed under the model it resolves to, which
			// --model takes as it is, so the picker names a real model.
			id = strings.TrimSpace(item.Resolved)
			defaultModel = id
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		model := core.ModelOption{Provider: "claude", ID: id, Name: strings.TrimSpace(item.DisplayName), Description: strings.TrimSpace(item.Description)}
		if model.Name == "" {
			model.Name = id
		}
		efforts := map[string]bool{}
		for _, effort := range item.Efforts {
			if effort = strings.TrimSpace(effort); effort != "" && !efforts[effort] {
				model.ReasoningEfforts = append(model.ReasoningEfforts, effort)
				efforts[effort] = true
			}
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("Claude Code listed no models")
	}
	for i := range models {
		models[i].IsDefault = models[i].ID == defaultModel
	}
	return models, nil
}

// WeeklyUsage reads the subscription's seven-day window from Claude Code
// without starting a conversation. Nil means quota is unavailable, as it is on
// an API key or when Claude Code has not fetched it.
func (c *Claude) WeeklyUsage(ctx context.Context) (*core.WeeklyUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, done, err := c.bootstrap(ctx, "usage")
	if err != nil {
		return nil, err
	}
	defer done()
	data, err := client.call(ctx, map[string]any{"subtype": "get_usage"}, nil)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code account usage: %w", err)
	}
	return parseClaudeWeeklyUsage(data)
}

func parseClaudeWeeklyUsage(data []byte) (*core.WeeklyUsage, error) {
	var response struct {
		Available bool `json:"rate_limits_available"`
		Limits    *struct {
			SevenDay *struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    string   `json:"resets_at"`
			} `json:"seven_day"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode Claude Code account usage: %w", err)
	}
	if !response.Available || response.Limits == nil || response.Limits.SevenDay == nil || response.Limits.SevenDay.Utilization == nil {
		return nil, nil
	}
	window := response.Limits.SevenDay
	usage := &core.WeeklyUsage{RemainingPercent: math.Max(0, math.Min(100, 100-*window.Utilization))}
	if window.ResetsAt != "" {
		if at, err := time.Parse(time.RFC3339, window.ResetsAt); err == nil {
			usage.ResetsAt = at
		}
	}
	return usage, nil
}
