package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	SkillPaths       []string `json:"skill_paths,omitempty"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model,omitempty"`
	OpenRouterModels []string `json:"openrouter_models,omitempty"`
	BaseURL          string   `json:"base_url,omitempty"`
	APIKeyEnv        string   `json:"api_key_env,omitempty"`
	Mode             string   `json:"mode"`
	CodexBinary      string   `json:"codex_binary,omitempty"`
	Reasoning        string   `json:"reasoning,omitempty"`
	MaxSteps         int      `json:"max_steps"`
	ContextBytes     int      `json:"context_bytes"`
	Timeout          string   `json:"timeout"`
}

func Default() Config {
	return Config{Provider: "codex", Mode: "auto", CodexBinary: "codex", MaxSteps: 256, ContextBytes: 160000, Timeout: "4h"}
}
func Path() (string, error) {
	d := os.Getenv("XDG_CONFIG_HOME")
	if d == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, ".config")
	}
	return filepath.Join(d, "jarvis", "config.json"), nil
}
func Load(path string) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("read configuration: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("configuration contains trailing data")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if len(c.SkillPaths) > 8 {
		return errors.New("at most 8 skill_paths are allowed")
	}
	for _, path := range c.SkillPaths {
		if path == "" {
			return errors.New("skill_paths must not contain empty paths")
		}
	}
	switch c.Provider {
	case "codex", "openai", "anthropic", "google", "compatible":
	default:
		return fmt.Errorf("unknown provider %q", c.Provider)
	}
	if c.Model != strings.TrimSpace(c.Model) || strings.IndexFunc(c.Model, unicode.IsControl) >= 0 {
		return errors.New("model must be a provider model ID without surrounding whitespace or control characters")
	}
	if len(c.OpenRouterModels) > 100 {
		return errors.New("openrouter_models may contain at most 100 model IDs")
	}
	seenOpenRouterModels := make(map[string]bool, len(c.OpenRouterModels))
	for _, model := range c.OpenRouterModels {
		if model == "" || len(model) > 512 || model != strings.TrimSpace(model) || strings.IndexFunc(model, unicode.IsControl) >= 0 {
			return errors.New("openrouter_models must contain model IDs without surrounding whitespace or control characters")
		}
		if seenOpenRouterModels[model] {
			return fmt.Errorf("openrouter_models contains duplicate model ID %q", model)
		}
		seenOpenRouterModels[model] = true
	}
	if c.Reasoning != "" {
		if c.Provider != "codex" {
			return errors.New("reasoning is configurable for the codex provider only; clear reasoning for other providers")
		}
		// Codex advertises effort names per model as an open string, not a
		// fixed enum. Keep future names valid; the provider checks support.
		if len(c.Reasoning) > 64 || strings.IndexFunc(c.Reasoning, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) >= 0 {
			return errors.New("reasoning must be a model-supported effort name using lowercase letters, digits, '-' or '_'")
		}
	}
	switch c.Mode {
	case "auto", "ask", "read-only":
	default:
		return fmt.Errorf("unknown approval mode %q", c.Mode)
	}
	if c.MaxSteps < 1 || c.MaxSteps > 10000 {
		return errors.New("max_steps must be between 1 and 10000")
	}
	if c.ContextBytes < 16000 {
		return errors.New("context_bytes must be at least 16000")
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return errors.New("timeout must be a positive Go duration, such as 4h")
	}
	return nil
}
func (c Config) KeyEnv() string {
	if c.APIKeyEnv != "" {
		return c.APIKeyEnv
	}
	switch c.Provider {
	case "openai", "compatible":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "google":
		return "GEMINI_API_KEY"
	}
	return ""
}
func Init(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	err = enc.Encode(Default())
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
