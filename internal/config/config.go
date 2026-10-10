package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"jarvis/internal/core"
)

const CompatibleDecisionsModel = "typesafe/jev-1.13"

// MinContextTokens is the smallest accepted context_tokens. The system prompt
// and tool schemas alone use several thousand tokens and compaction fires at 85%
// of the window, so anything smaller would compact on nearly every turn.
const MinContextTokens = 32000

type Config struct {
	SkillPaths       []string `json:"skill_paths,omitempty"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model,omitempty"`
	CompatibleModels []string `json:"compatible_models,omitempty"`
	BaseURL          string   `json:"base_url,omitempty"`
	APIKeyEnv        string   `json:"api_key_env,omitempty"`
	Mode             string   `json:"mode"`
	CodexBinary      string   `json:"codex_binary,omitempty"`
	Reasoning        string   `json:"reasoning,omitempty"`
	MaxSteps         int      `json:"max_steps"`
	// ContextTokens is the context window assumed for every model; compaction
	// triggers at a fraction of it.
	ContextTokens int `json:"context_tokens"`
	// ContextBytes is deprecated and ignored: compaction is driven by
	// ContextTokens. The field stays only because configs are decoded with
	// DisallowUnknownFields and earlier 'config init' runs wrote context_bytes
	// into every user's config, which must keep loading.
	ContextBytes int    `json:"context_bytes,omitempty"`
	Timeout      string `json:"timeout"`
}

func Default() Config {
	return Config{Provider: "codex", Mode: "auto", CodexBinary: "codex", MaxSteps: 256, ContextTokens: core.DefaultContextWindow, Timeout: "4h"}
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
	if c.Provider == "compatible" && IsDecisionsOnlyCompatibleModel(c.Model) {
		return fmt.Errorf("model %q is decisions-only and cannot generate chat responses; choose a chat model (it can remain in compatible_models for approval reviews)", c.Model)
	}
	if c.BaseURL == "" && containsDecisionsModel(c.CompatibleModels) {
		return fmt.Errorf("compatible_models contains the decisions-only model %q, which reviews actions through base_url; set base_url to the compatible API endpoint", CompatibleDecisionsModel)
	}
	if len(c.CompatibleModels) > 100 {
		return errors.New("compatible_models may contain at most 100 model IDs")
	}
	seenCompatibleModels := make(map[string]bool, len(c.CompatibleModels))
	for _, model := range c.CompatibleModels {
		if model == "" || len(model) > 512 || model != strings.TrimSpace(model) || strings.IndexFunc(model, unicode.IsControl) >= 0 {
			return errors.New("compatible_models must contain model IDs without surrounding whitespace or control characters")
		}
		if seenCompatibleModels[model] {
			return fmt.Errorf("compatible_models contains duplicate model ID %q", model)
		}
		seenCompatibleModels[model] = true
	}
	if c.Reasoning != "" {
		if !SupportsReasoning(c.Provider, c.BaseURL) {
			return errors.New("reasoning is configurable for Codex and OpenRouter-compatible sessions only")
		}
		// Codex advertises effort names per model as an open string. OpenRouter
		// documents this shared set for its reasoning_effort request parameter.
		if c.Provider == "compatible" && !contains(OpenRouterReasoningEfforts(), c.Reasoning) {
			return errors.New("OpenRouter reasoning must be one of none, minimal, low, medium, high, xhigh, or max")
		}
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
	if c.ContextTokens < MinContextTokens {
		return fmt.Errorf("context_tokens must be at least %d", MinContextTokens)
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return errors.New("timeout must be a positive Go duration, such as 4h")
	}
	return nil
}

func IsDecisionsOnlyCompatibleModel(model string) bool {
	return model == CompatibleDecisionsModel
}

func containsDecisionsModel(models []string) bool {
	return contains(models, CompatibleDecisionsModel)
}

// SupportsReasoning reports providers where JARVIS can send a mapped effort
// setting instead of silently ignoring it.
func SupportsReasoning(provider, baseURL string) bool {
	if provider == "codex" {
		return true
	}
	if provider != "compatible" {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

// OpenRouterReasoningEfforts returns the reasoning_effort values accepted by
// OpenRouter's chat-completions API. Model-specific support can still vary.
func OpenRouterReasoningEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
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
