package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"julia/internal/core"
)

// Models reads Codex's selectable model catalog without creating a conversation
// or making an inference request. The temporary cwd avoids workspace integrations.
func (c *Codex) Models(ctx context.Context) ([]core.ModelOption, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "julia-models-")
	if err != nil {
		return nil, fmt.Errorf("create model catalog workspace: %w", err)
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
		return nil, fmt.Errorf("initialize Codex model catalog: %w", err)
	}
	if err := client.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	data, err := client.call(ctx, "config/read", map[string]any{"includeLayers": false, "cwd": directory})
	if err != nil {
		return nil, fmt.Errorf("read configured Codex model: %w", err)
	}
	var configuration struct {
		Config struct {
			Model string `json:"model"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &configuration); err != nil {
		return nil, fmt.Errorf("decode configured Codex model: %w", err)
	}
	models := []core.ModelOption{}
	seenModels, seenCursors := map[string]bool{}, map[string]bool{}
	cursor, configuredID := "", ""
	for {
		params := map[string]any{"includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := client.call(ctx, "model/list", params)
		if err != nil {
			return nil, fmt.Errorf("read Codex model catalog: %w", err)
		}
		var page struct {
			Data []struct {
				ID               string `json:"id"`
				Model            string `json:"model"`
				Name             string `json:"displayName"`
				Description      string `json:"description"`
				Hidden           bool   `json:"hidden"`
				IsDefault        bool   `json:"isDefault"`
				DefaultReasoning string `json:"defaultReasoningEffort"`
				Efforts          []struct {
					Effort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("decode Codex model catalog: %w", err)
		}
		for _, item := range page.Data {
			if item.Hidden || strings.TrimSpace(item.Model) == "" {
				continue
			}
			if configuration.Config.Model != "" && (configuration.Config.Model == item.Model || configuration.Config.Model == item.ID) {
				configuredID = item.Model
			}
			if seenModels[item.Model] {
				continue
			}
			seenModels[item.Model] = true
			model := core.ModelOption{Provider: "codex", ID: item.Model, Name: item.Name, Description: item.Description, DefaultReasoning: item.DefaultReasoning, IsDefault: item.IsDefault}
			if strings.TrimSpace(model.Name) == "" {
				model.Name = model.ID
			}
			seenEfforts := map[string]bool{}
			for _, option := range item.Efforts {
				if strings.TrimSpace(option.Effort) != "" && !seenEfforts[option.Effort] {
					model.ReasoningEfforts = append(model.ReasoningEfforts, option.Effort)
					seenEfforts[option.Effort] = true
				}
			}
			models = append(models, model)
		}
		if page.NextCursor == "" {
			break
		}
		if seenCursors[page.NextCursor] {
			return nil, errors.New("Codex model catalog repeated a pagination cursor")
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	if configuredID != "" {
		for i := range models {
			models[i].IsDefault = models[i].ID == configuredID
		}
	}
	return models, nil
}
