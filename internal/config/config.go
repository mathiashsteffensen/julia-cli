package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"julia/agents"
	"julia/internal/core"
)

const CompatibleDecisionsModel = "typesafe/jev-1.13"

// MinContextTokens is the smallest accepted context_tokens. The system prompt
// and tool schemas alone use several thousand tokens and compaction fires at 85%
// of the window, so anything smaller would compact on nearly every turn.
const MinContextTokens = 32000

type Config struct {
	SkillPaths []string `json:"skill_paths,omitempty"`
	// Provider, Model and ModelReasoning come from the "model" entry:
	// provider/<model-id>, or an object with that id and the reasoning effort
	// the model starts with. A bare "codex" is Codex's own configured model.
	Provider       string `json:"-"`
	Model          string `json:"-"`
	ModelReasoning string `json:"-"`
	// EnabledModels are the models the picker lists and ctrl+n cycles, each
	// provider/<model-id> or an object with that id and a reasoning effort.
	EnabledModels []ModelEntry `json:"enabled_models,omitempty"`
	// Providers are named OpenAI-compatible endpoints. Each id is a provider
	// a model ID may name, as <id>/<model-id>.
	Providers []ProviderConfig `json:"providers,omitempty"`
	// Mode is the approval mode: auto, ask, read-only, or yolo. Yolo runs
	// Bash and terminals directly on the host, outside any sandbox and with
	// the process environment, and grants every action without review. It is
	// for an unattended agent inside a container that is itself the trust
	// boundary, and is never a fallback.
	Mode        string `json:"mode"`
	CodexBinary string `json:"codex_binary,omitempty"`
	Reasoning   string `json:"reasoning,omitempty"`
	MaxSteps    int    `json:"max_steps"`
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
	// executable or a macOS .app bundle. Empty finds one; JULIA_BROWSER
	// overrides it.
	Browser string `json:"browser,omitempty"`
	// ReviewerModel is the model that reviews risky actions, as
	// provider/<model-id>: codex/gpt-6-luna, or <id>/<model-id> for a model
	// served through a named provider's Decisions API. Empty is the default
	// Codex reviewer.
	ReviewerModel string `json:"reviewer_model,omitempty"`
	// Subagents assigns models to subagent roles by name, each
	// provider/<model-id> or an object with that id and a reasoning effort. A
	// role without one runs on the delegating agent's model.
	Subagents map[string]ModelEntry `json:"subagents,omitempty"`
	// WorkspaceInstructions names files at the workspace root, such as
	// AGENTS.md, whose contents join the instructions as repository context.
	// Empty reads none: a cloned repository's files are not trusted by default.
	WorkspaceInstructions []string `json:"workspace_instructions,omitempty"`
	// Memory configures what JULIA keeps between sessions; see memory.Store.
	Memory MemoryConfig `json:"memory"`
}

// MemoryConfig is the "memory" entry: how notes are kept and reviewed.
type MemoryConfig struct {
	// Reflection is on or off: whether a reflection step reviews each
	// finished turn in auto and yolo mode and saves what a later session
	// should remember. Empty is on.
	Reflection string `json:"reflection,omitempty"`
	// Model runs the reflection step, as provider/<model-id> or
	// {"id": ..., "reasoning": ...}; empty runs it on the session's model.
	Model *ModelEntry `json:"model,omitempty"`
	// IndexBytes caps how much of the instructions the index of notes may
	// take; zero is memory.DefaultIndexBytes.
	IndexBytes int `json:"index_bytes,omitempty"`
}

// Index bounds for memory.index_bytes: below the minimum the index could not
// hold its framing and a handful of notes, above the maximum it would crowd
// out the instructions it closes, which skills and repository instructions
// may already fill to 128 KiB.
const (
	MinMemoryIndexBytes = 4096
	MaxMemoryIndexBytes = 65536
)

// ReflectionEnabled reports whether a reflection step reviews finished
// turns; it is on unless memory.reflection says off.
func (c Config) ReflectionEnabled() bool {
	return c.Memory.Reflection != "off"
}

// BuiltinProviders are the providers a model ID may name besides the
// configured ones.
var BuiltinProviders = []string{"codex", "openai", "anthropic", "google"}

// ProviderConfig is a named OpenAI-compatible endpoint in "providers". Its key
// is read from APIKeyEnv; without one the endpoint is called keyless.
type ProviderConfig struct {
	ID        string `json:"id"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// Endpoint is the named provider id, if one is configured.
func (c Config) Endpoint(id string) (ProviderConfig, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// IsNamed reports whether provider is one of the configured endpoints.
func (c Config) IsNamed(provider string) bool {
	_, ok := c.Endpoint(provider)
	return ok
}

// ProviderIDs are every provider a model ID may name: the built-in ones,
// then the configured ones.
func (c Config) ProviderIDs() []string {
	ids := slices.Clone(BuiltinProviders)
	for _, p := range c.Providers {
		ids = append(ids, p.ID)
	}
	return ids
}

// SplitModel splits provider/<model-id> at its first slash; the model ID may
// itself contain slashes. A bare provider name has an empty model ID.
func SplitModel(id string) (provider, model string) {
	provider, model, _ = strings.Cut(id, "/")
	return provider, model
}

// JoinModel is SplitModel's inverse; an empty model ID leaves the provider.
func JoinModel(provider, model string) string {
	if model == "" {
		return provider
	}
	return provider + "/" + model
}

// Reviewer splits reviewer_model into its provider and model ID. The model ID
// may itself contain slashes; only the first separates the provider. Unset,
// it is Codex with an empty model, which means Codex's default reviewer.
func (c Config) Reviewer() (provider, model string) {
	if c.ReviewerModel == "" {
		return "codex", ""
	}
	return SplitModel(c.ReviewerModel)
}

// ModelEntry is a model in the configuration: provider/<model-id> in JSON, or
// {"id": "provider/<model-id>", "reasoning": "high"} to give it a starting
// reasoning effort.
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
		return fmt.Errorf(`a model is "provider/<model-id>" or {"id": "provider/<model-id>", "reasoning": "<effort>"}: %w`, err)
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

// UnmarshalJSON reads "model" as a ModelEntry into Provider, Model and
// ModelReasoning.
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
		c.Provider, c.Model = SplitModel(file.Model.ID)
		c.ModelReasoning = file.Model.Reasoning
	}
	return nil
}

func (c Config) MarshalJSON() ([]byte, error) {
	type fields Config
	file := struct {
		fields
		Model *ModelEntry `json:"model,omitempty"`
	}{fields: fields(c), Model: &ModelEntry{ID: JoinModel(c.Provider, c.Model), Reasoning: c.ModelReasoning}}
	return json.Marshal(file)
}

// ReasoningFor is the reasoning effort configured for a provider's model: the
// "model" entry's, else its enabled_models entry's. Empty means the model has
// none of its own.
func (c Config) ReasoningFor(provider, model string) string {
	if provider == c.Provider && model == c.Model && c.ModelReasoning != "" {
		return c.ModelReasoning
	}
	id := JoinModel(provider, model)
	for _, entry := range c.EnabledModels {
		if entry.ID == id {
			return entry.Reasoning
		}
	}
	return ""
}

// BrowserEnv names the environment variable that overrides Config.Browser.
const BrowserEnv = "JULIA_BROWSER"

// BrowserPath is the browser to launch: $JULIA_BROWSER, else the
// configured one, else empty, which finds one.
func (c Config) BrowserPath() string {
	if path := os.Getenv(BrowserEnv); path != "" {
		return path
	}
	return c.Browser
}

func Default() Config {
	return Config{Provider: "codex", Mode: "auto", CodexBinary: "codex", MaxSteps: 256, ContextTokens: core.DefaultContextWindow, Timeout: "4h", Memory: MemoryConfig{Reflection: "on"}}
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
	return filepath.Join(d, "julia", "config.json"), nil
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
	if err := c.validateProviders(); err != nil {
		return err
	}
	if len(c.SkillPaths) > 8 {
		return errors.New("at most 8 skill_paths are allowed")
	}
	for _, path := range c.SkillPaths {
		if path == "" {
			return errors.New("skill_paths must not contain empty paths")
		}
	}
	if err := c.validateModel("model", c.Provider, c.Model, c.ModelReasoning); err != nil {
		return err
	}
	if c.ReviewerModel != "" {
		reviewer, model := c.Reviewer()
		if (reviewer != "codex" && !c.IsNamed(reviewer)) || model == "" || len(c.ReviewerModel) > 512 || c.ReviewerModel != strings.TrimSpace(c.ReviewerModel) || strings.IndexFunc(c.ReviewerModel, unicode.IsControl) >= 0 {
			return fmt.Errorf("reviewer_model must be provider/<model-id> with provider codex or one of providers, such as codex/gpt-6-luna or openrouter/%s", CompatibleDecisionsModel)
		}
	}
	if len(c.EnabledModels) > 100 {
		return errors.New("enabled_models may contain at most 100 models")
	}
	seenModels := make(map[string]bool, len(c.EnabledModels))
	for _, entry := range c.EnabledModels {
		provider, model := SplitModel(entry.ID)
		if err := c.validateModel("enabled_models", provider, model, entry.Reasoning); err != nil {
			return err
		}
		if model == "" {
			return fmt.Errorf("enabled_models %q needs a model ID: provider/<model-id>", entry.ID)
		}
		if seenModels[entry.ID] {
			return fmt.Errorf("enabled_models contains %q twice", entry.ID)
		}
		seenModels[entry.ID] = true
	}
	if len(c.Subagents) > 100 {
		return errors.New("subagents may assign at most 100 roles")
	}
	for _, role := range slices.Sorted(maps.Keys(c.Subagents)) {
		entry := c.Subagents[role]
		if !agents.ValidName(role) {
			return fmt.Errorf("subagents %q is not a role name, which is %s", role, agents.NameRule)
		}
		provider, model := SplitModel(entry.ID)
		if err := c.validateModel("subagents."+role, provider, model, entry.Reasoning); err != nil {
			return err
		}
	}
	if err := c.validateReasoning(c.Provider, c.Reasoning); err != nil {
		return err
	}
	switch c.Mode {
	case "auto", "ask", "read-only", "yolo":
	default:
		return fmt.Errorf("unknown approval mode %q", c.Mode)
	}
	if len(c.WorkspaceInstructions) > 8 {
		return errors.New("at most 8 workspace_instructions files are allowed")
	}
	for _, name := range c.WorkspaceInstructions {
		if name == "" || name != strings.TrimSpace(name) || len(name) > 255 || filepath.Base(name) != name || name == "." || name == ".." || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return fmt.Errorf("workspace_instructions %q must be a plain file name at the workspace root, such as AGENTS.md", name)
		}
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
	switch c.Memory.Reflection {
	case "", "on", "off":
	default:
		return fmt.Errorf("memory.reflection must be on or off, not %q", c.Memory.Reflection)
	}
	if c.Memory.Model != nil {
		provider, model := SplitModel(c.Memory.Model.ID)
		if err := c.validateModel("memory.model", provider, model, c.Memory.Model.Reasoning); err != nil {
			return err
		}
	}
	if c.Memory.IndexBytes != 0 && (c.Memory.IndexBytes < MinMemoryIndexBytes || c.Memory.IndexBytes > MaxMemoryIndexBytes) {
		return fmt.Errorf("memory.index_bytes must be between %d and %d, or 0 for the default", MinMemoryIndexBytes, MaxMemoryIndexBytes)
	}
	return nil
}

// validateModel checks one provider/<model-id> entry of the named field. Only
// Codex may leave the model ID out, for its own configured model.
func (c Config) validateModel(field, provider, model, reasoning string) error {
	id := JoinModel(provider, model)
	if !contains(c.ProviderIDs(), provider) {
		return fmt.Errorf("%s %q must be provider/<model-id> with provider %s", field, id, strings.Join(c.ProviderIDs(), ", "))
	}
	if provider != "codex" && model == "" {
		return fmt.Errorf("%s %q needs a model ID: %s/<model-id>", field, id, provider)
	}
	if len(model) > 512 || model != strings.TrimSpace(model) || strings.IndexFunc(model, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s model IDs must not have surrounding whitespace or control characters", field)
	}
	if c.IsNamed(provider) && IsDecisionsOnlyCompatibleModel(model) {
		return fmt.Errorf("%s %q is decisions-only and cannot generate chat responses; set reviewer_model to %s to review actions with it", field, id, id)
	}
	if err := c.validateReasoning(provider, reasoning); err != nil {
		return fmt.Errorf("%s %q: %w", field, id, err)
	}
	return nil
}

// validateProviders checks the "providers" entries: each a distinct id that
// no built-in provider has, an http(s) base_url, and an optional key variable.
func (c Config) validateProviders() error {
	if len(c.Providers) > 32 {
		return errors.New("providers may contain at most 32 providers")
	}
	seen := make(map[string]bool, len(c.Providers))
	for _, p := range c.Providers {
		if !validProviderID(p.ID) {
			return fmt.Errorf("providers id %q must be 1 to 64 lowercase letters, digits, '-' or '_', starting with a letter", p.ID)
		}
		if contains(BuiltinProviders, p.ID) {
			return fmt.Errorf("providers id %q is a built-in provider", p.ID)
		}
		if seen[p.ID] {
			return fmt.Errorf("providers contains %q twice", p.ID)
		}
		seen[p.ID] = true
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || p.BaseURL != strings.TrimSpace(p.BaseURL) {
			return fmt.Errorf("providers %q needs a base_url such as https://openrouter.ai/api/v1", p.ID)
		}
		if p.APIKeyEnv != "" && !validEnvName(p.APIKeyEnv) {
			return fmt.Errorf("providers %q api_key_env %q is not an environment variable name", p.ID, p.APIKeyEnv)
		}
	}
	return nil
}

func validProviderID(id string) bool {
	if id == "" || len(id) > 64 || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}

func validEnvName(name string) bool {
	if len(name) > 256 || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
	}) < 0
}

// validateReasoning checks an effort for provider; empty is always valid.
func (c Config) validateReasoning(provider, effort string) error {
	if effort == "" {
		return nil
	}
	if !c.SupportsReasoning(provider) {
		return errors.New("reasoning is configurable for Codex and your providers only")
	}
	// Codex advertises effort names per model as an open string. A named
	// provider is sent reasoning_effort, whose values OpenRouter documents.
	if provider != "codex" && !contains(OpenRouterReasoningEfforts(), effort) {
		return errors.New("reasoning for a named provider must be one of none, minimal, low, medium, high, xhigh, or max")
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

// SupportsReasoning reports providers where JULIA sends a chosen effort
// instead of silently ignoring it: Codex, and every named provider, which is
// sent reasoning_effort whether or not its model supports it.
func (c Config) SupportsReasoning(provider string) bool {
	return provider == "codex" || c.IsNamed(provider)
}

// IsOpenRouter reports whether baseURL is an OpenRouter endpoint.
func IsOpenRouter(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

// OpenRouterReasoningEfforts returns the reasoning_effort values accepted by
// OpenRouter's chat-completions API, which every named provider is offered.
// Model and endpoint support can still vary.
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

// KeyEnv names the environment variable holding provider's API key. A named
// provider without api_key_env has none and is called keyless.
func (c Config) KeyEnv(provider string) string {
	if p, ok := c.Endpoint(provider); ok {
		return p.APIKeyEnv
	}
	switch provider {
	case "openai":
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
