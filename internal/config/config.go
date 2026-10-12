package config

import (
	"bytes"
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
	SkillPaths []string `json:"skill_paths,omitempty"`
	Provider   string   `json:"provider"`
	// Model and ModelReasoning are the "model" entry: a model ID, or an object
	// with an id and the reasoning effort that model starts with.
	Model          string `json:"-"`
	ModelReasoning string `json:"-"`
	// CompatibleModels are the switchable compatible models, each a model ID
	// or an object with an id and the reasoning effort it starts with.
	CompatibleModels []ModelEntry `json:"compatible_models,omitempty"`
	BaseURL          string       `json:"base_url,omitempty"`
	APIKeyEnv        string       `json:"api_key_env,omitempty"`
	Mode             string       `json:"mode"`
	CodexBinary      string       `json:"codex_binary,omitempty"`
	Reasoning        string       `json:"reasoning,omitempty"`
	MaxSteps         int          `json:"max_steps"`
	// ContextTokens is the context window assumed for every model; compaction
	// triggers at a fraction of it.
	ContextTokens int `json:"context_tokens"`
	// ContextBytes is deprecated and ignored: compaction is driven by
	// ContextTokens. The field stays only because configs are decoded with
	// DisallowUnknownFields and earlier 'config init' runs wrote context_bytes
	// into every user's config, which must keep loading.
	ContextBytes int    `json:"context_bytes,omitempty"`
	Timeout      string `json:"timeout"`
	// Browser is the Chrome or Chromium the Browser tool launches: an
	// executable or a macOS .app bundle. Empty finds one; JARVIS_BROWSER
	// overrides it.
	Browser string `json:"browser,omitempty"`
	// ReviewerModel is the model that reviews risky actions, as
	// provider/<model-id>: codex/gpt-6-luna, or compatible/<model-id> for a
	// model served through base_url's Decisions API. Empty is the default
	// Codex reviewer.
	ReviewerModel string `json:"reviewer_model,omitempty"`
}

// ReviewerProviders are the providers reviewer_model may name.
var ReviewerProviders = []string{"codex", "compatible"}

// Reviewer splits reviewer_model into its provider and model ID. The model ID
// may itself contain slashes; only the first separates the provider. Unset,
// it is Codex with an empty model, which means Codex's default reviewer.
func (c Config) Reviewer() (provider, model string) {
	if c.ReviewerModel == "" {
		return "codex", ""
	}
	provider, model, _ = strings.Cut(c.ReviewerModel, "/")
	return provider, model
}

// ModelEntry is a model in the configuration: a bare model ID in JSON, or
// {"id": "...", "reasoning": "high"} to give it a starting reasoning effort.
type ModelEntry struct {
	ID        string `json:"id"`
	Reasoning string `json:"reasoning,omitempty"`
}

func (m *ModelEntry) UnmarshalJSON(data []byte) error {
	var id string
	if err := json.Unmarshal(data, &id); err == nil {
		*m = ModelEntry{ID: id}
		return nil
	}
	type entry ModelEntry
	var e entry
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return fmt.Errorf(`a model is a model ID or {"id": "<model-id>", "reasoning": "<effort>"}: %w`, err)
	}
	if e.ID == "" {
		return errors.New(`a model object needs an "id"`)
	}
	*m = ModelEntry(e)
	return nil
}

func (m ModelEntry) MarshalJSON() ([]byte, error) {
	if m.Reasoning == "" {
		return json.Marshal(m.ID)
	}
	type entry ModelEntry
	return json.Marshal(entry(m))
}

// UnmarshalJSON reads "model" as a ModelEntry into Model and ModelReasoning.
// Unknown keys are refused here too: Load's DisallowUnknownFields does not
// reach a custom unmarshaler.
func (c *Config) UnmarshalJSON(data []byte) error {
	type fields Config
	file := struct {
		*fields
		Model *ModelEntry `json:"model,omitempty"`
	}{fields: (*fields)(c)}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&file); err != nil {
		return err
	}
	if file.Model != nil {
		c.Model, c.ModelReasoning = file.Model.ID, file.Model.Reasoning
	}
	return nil
}

func (c Config) MarshalJSON() ([]byte, error) {
	type fields Config
	file := struct {
		fields
		Model *ModelEntry `json:"model,omitempty"`
	}{fields: fields(c)}
	if c.Model != "" {
		file.Model = &ModelEntry{ID: c.Model, Reasoning: c.ModelReasoning}
	}
	return json.Marshal(file)
}

// ReasoningFor is the reasoning effort configured for a provider's model: the
// "model" entry's, or for the compatible provider its compatible_models
// entry's. Empty means the model has none of its own.
func (c Config) ReasoningFor(provider, model string) string {
	if provider == c.Provider && model == c.Model && c.ModelReasoning != "" {
		return c.ModelReasoning
	}
	if provider == "compatible" {
		for _, entry := range c.CompatibleModels {
			if entry.ID == model {
				return entry.Reasoning
			}
		}
	}
	return ""
}

// BrowserEnv names the environment variable that overrides Config.Browser.
const BrowserEnv = "JARVIS_BROWSER"

// BrowserPath is the browser to launch: $JARVIS_BROWSER, else the
// configured one, else empty, which finds one.
func (c Config) BrowserPath() string {
	if path := os.Getenv(BrowserEnv); path != "" {
		return path
	}
	return c.Browser
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
		return fmt.Errorf("model %q is decisions-only and cannot generate chat responses; choose a chat model (set reviewer_model to compatible/%s to review actions with it)", c.Model, c.Model)
	}
	if c.ReviewerModel != "" {
		reviewer, model := c.Reviewer()
		if !contains(ReviewerProviders, reviewer) || model == "" || len(c.ReviewerModel) > 512 || c.ReviewerModel != strings.TrimSpace(c.ReviewerModel) || strings.IndexFunc(c.ReviewerModel, unicode.IsControl) >= 0 {
			return fmt.Errorf("reviewer_model must be provider/<model-id> with provider %s, such as codex/gpt-6-luna or compatible/%s", strings.Join(ReviewerProviders, " or "), CompatibleDecisionsModel)
		}
		if reviewer == "compatible" && c.BaseURL == "" {
			return errors.New("a compatible reviewer_model reviews actions through base_url; set base_url to the compatible API endpoint")
		}
	}
	if len(c.CompatibleModels) > 100 {
		return errors.New("compatible_models may contain at most 100 model IDs")
	}
	seenCompatibleModels := make(map[string]bool, len(c.CompatibleModels))
	for _, entry := range c.CompatibleModels {
		model := entry.ID
		if model == "" || len(model) > 512 || model != strings.TrimSpace(model) || strings.IndexFunc(model, unicode.IsControl) >= 0 {
			return errors.New("compatible_models must contain model IDs without surrounding whitespace or control characters")
		}
		if seenCompatibleModels[model] {
			return fmt.Errorf("compatible_models contains duplicate model ID %q", model)
		}
		seenCompatibleModels[model] = true
		if err := validateReasoning("compatible", c.BaseURL, entry.Reasoning); err != nil {
			return fmt.Errorf("compatible_models %q: %w", model, err)
		}
	}
	if err := validateReasoning(c.Provider, c.BaseURL, c.Reasoning); err != nil {
		return err
	}
	if c.ModelReasoning != "" {
		if c.Model == "" {
			return errors.New("model reasoning needs a model id")
		}
		if err := validateReasoning(c.Provider, c.BaseURL, c.ModelReasoning); err != nil {
			return fmt.Errorf("model %q: %w", c.Model, err)
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
	if len(c.Browser) > 4096 || c.Browser != strings.TrimSpace(c.Browser) || strings.IndexFunc(c.Browser, unicode.IsControl) >= 0 {
		return errors.New("browser must be a path to a Chrome or Chromium executable or .app, without surrounding whitespace or control characters")
	}
	return nil
}

// validateReasoning checks an effort for provider; empty is always valid.
func validateReasoning(provider, baseURL, effort string) error {
	if effort == "" {
		return nil
	}
	if !SupportsReasoning(provider, baseURL) {
		return errors.New("reasoning is configurable for Codex and OpenRouter-compatible sessions only")
	}
	// Codex advertises effort names per model as an open string. OpenRouter
	// documents this shared set for its reasoning_effort request parameter.
	if provider == "compatible" && !contains(OpenRouterReasoningEfforts(), effort) {
		return errors.New("OpenRouter reasoning must be one of none, minimal, low, medium, high, xhigh, or max")
	}
	if len(effort) > 64 || strings.IndexFunc(effort, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) >= 0 {
		return errors.New("reasoning must be a model-supported effort name using lowercase letters, digits, '-' or '_'")
	}
	return nil
}

func IsDecisionsOnlyCompatibleModel(model string) bool {
	return model == CompatibleDecisionsModel
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
