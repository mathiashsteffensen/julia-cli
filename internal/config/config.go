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
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"julia/agents"
	"julia/internal/core"
)

const CompatibleDecisionsModel = "typesafe/jev-1.13"

// MinContextTokens is the smallest accepted context_tokens, and the smallest
// context_window a model entry may give. The system prompt and tool schemas
// alone use several thousand tokens and compaction fires at 85% of the window,
// so anything smaller would compact on nearly every turn.
const MinContextTokens = 32000

type Config struct {
	// Provider, Model, ModelReasoning and ModelContextWindow come from the
	// "model" entry: provider/<model-id>, or an object with that id, the
	// reasoning effort the model starts with and its context window. A bare
	// "codex" is Codex's own configured model, and a bare "claude" is Claude
	// Code's default model.
	Provider           string `json:"-"`
	Model              string `json:"-"`
	ModelReasoning     string `json:"-"`
	ModelContextWindow int    `json:"-"`
	// EnabledModels restricts selection when nonempty and ctrl+n cycles it, each
	// provider/<model-id> or an object with that id, a reasoning effort and a
	// context window.
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
	// ClaudeBinary is the Claude Code CLI the claude provider runs; empty
	// finds claude on PATH or in its usual install locations.
	ClaudeBinary string `json:"claude_binary,omitempty"`
	Reasoning    string `json:"reasoning,omitempty"`
	MaxSteps     int    `json:"max_steps"`
	// ContextTokens is the context window assumed for every model without a
	// context_window of its own; compaction triggers at a fraction of it.
	ContextTokens int `json:"context_tokens"`
	// ContextBytes is deprecated and ignored: compaction is driven by
	// ContextTokens. The field stays only because configs are decoded with
	// DisallowUnknownFields and earlier 'config init' runs wrote context_bytes
	// into every user's config, which must keep loading.
	ContextBytes int `json:"context_bytes,omitempty"`
	// LegacySkillPaths is deprecated and ignored: JULIA offers every skill in
	// ~/.agents/skills. It stays so configs that still name skill_paths keep
	// loading under DisallowUnknownFields; doctor points it out.
	LegacySkillPaths []string `json:"skill_paths,omitempty"`
	Timeout          string   `json:"timeout"`
	// Browser is the Chrome or Chromium the Browser tool launches: an
	// executable or a macOS .app bundle. Empty finds one; JULIA_BROWSER
	// overrides it.
	Browser string `json:"browser,omitempty"`
	// BrowserAuth signs the Browser tool in to sites: for each origin, such as
	// https://grafana.example.com, the headers its own requests carry. Each
	// value is expanded by bash as a double-quoted string, so
	// "Bearer $(gcloud secrets versions access …)" sends the secret, and runs
	// on the host the first time a turn uses the origin.
	BrowserAuth map[string]map[string]string `json:"browser_auth,omitempty"`
	// ReviewerModel is the model that reviews risky actions, as
	// provider/<model-id>: codex/gpt-6-luna, claude/sonnet, or <id>/<model-id>
	// for a model served through a named provider's Decisions API. Empty is
	// the default Codex reviewer.
	ReviewerModel string `json:"reviewer_model,omitempty"`
	// Subagents assigns models to subagent roles by name, each
	// provider/<model-id> or an object with that id and a reasoning effort. A
	// role without one runs on the delegating agent's model.
	Subagents map[string]ModelEntry `json:"subagents,omitempty"`
	// WorkspaceInstructions names files at the workspace root, such as
	// AGENTS.md, whose contents join the instructions as repository context.
	// Empty reads none: a cloned repository's files are not trusted by default.
	WorkspaceInstructions []string `json:"workspace_instructions,omitempty"`
	// DisabledTools names tools, such as WebSearch or Subagent, that neither the
	// agent nor its subagents are offered: a benchmark compares models on a
	// fixed tool set.
	DisabledTools []string `json:"disabled_tools,omitempty"`
	// SystemPrompt is full (empty is full) or compact: JULIA's working rules
	// in a shorter form, for a small local model trained on them.
	SystemPrompt string `json:"system_prompt,omitempty"`
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
var BuiltinProviders = []string{"codex", "claude", "openai", "anthropic", "google"}

// CLIProviders are the providers that run a locally installed agent CLI under
// its own sign-in, with no API key: Codex's app-server and Claude Code.
var CLIProviders = []string{"codex", "claude"}

// IsCLIProvider reports whether provider is one of CLIProviders.
func IsCLIProvider(provider string) bool { return contains(CLIProviders, provider) }

// CLIProviderName is how a CLI provider is called in the interface.
func CLIProviderName(provider string) string {
	switch provider {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude Code"
	}
	return provider
}

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
// {"id": "provider/<model-id>", "reasoning": "high", "context_window": 400000}
// to give it a starting reasoning effort and a context window of its own.
type ModelEntry struct {
	ID        string `json:"id"`
	Reasoning string `json:"reasoning,omitempty"`
	// ContextWindow is the model's context size in tokens; zero means the
	// configuration's context_tokens.
	ContextWindow int `json:"context_window,omitempty"`
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
		return fmt.Errorf(`a model is "provider/<model-id>" or {"id": "provider/<model-id>", "reasoning": "<effort>", "context_window": <tokens>}: %w`, err)
	}
	*m = ModelEntry(e)
	return nil
}

func (m ModelEntry) MarshalJSON() ([]byte, error) {
	if m.Reasoning == "" && m.ContextWindow == 0 {
		return json.Marshal(m.ID)
	}
	type entry ModelEntry
	return json.Marshal(entry(m))
}

// UnmarshalJSON reads "model" as a ModelEntry into Provider, Model,
// ModelReasoning and ModelContextWindow.
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
		c.ModelReasoning, c.ModelContextWindow = file.Model.Reasoning, file.Model.ContextWindow
	}
	return nil
}

func (c Config) MarshalJSON() ([]byte, error) {
	type fields Config
	model := c.modelEntry()
	file := struct {
		fields
		Model *ModelEntry `json:"model,omitempty"`
	}{fields: fields(c), Model: &model}
	return json.Marshal(file)
}

// modelEntry is the "model" entry as configured.
func (c Config) modelEntry() ModelEntry {
	return ModelEntry{ID: JoinModel(c.Provider, c.Model), Reasoning: c.ModelReasoning, ContextWindow: c.ModelContextWindow}
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

// ModelSelectable reports whether an explicit selection is allowed. An absent
// or empty list leaves selection unrestricted.
func (c Config) ModelSelectable(provider, model string) bool {
	if len(c.EnabledModels) == 0 {
		return true
	}
	id := JoinModel(provider, model)
	return slices.ContainsFunc(c.EnabledModels, func(entry ModelEntry) bool { return entry.ID == id })
}

// ContextWindowFor is the context window, in tokens, of a provider's model:
// the "model" entry's, else its enabled_models entry's, else context_tokens,
// which every model without a context_window of its own is assumed to have.
func (c Config) ContextWindowFor(provider, model string) int {
	if provider == c.Provider && model == c.Model && c.ModelContextWindow > 0 {
		return c.ModelContextWindow
	}
	id := JoinModel(provider, model)
	for _, entry := range c.EnabledModels {
		if entry.ID == id && entry.ContextWindow > 0 {
			return entry.ContextWindow
		}
	}
	return c.ContextTokens
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
	return Config{Provider: "codex", Mode: "auto", CodexBinary: "codex", ClaudeBinary: "claude", MaxSteps: 256, ContextTokens: core.DefaultContextWindow, Timeout: "4h", Memory: MemoryConfig{Reflection: "on"}}
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
	return ProfilePath(filepath.Join(d, "julia"), DefaultProfile), nil
}
func Load(path string) (Config, error) {
	c := Default()
	shared := map[string]any{}
	// An explicitly selected shared file is loaded once, even when the other
	// format is the preferred shared file.
	base := filepath.Base(path)
	if base != "config.shared.toml" && base != "config.shared.json" {
		sharedPath := ProfilePath(filepath.Dir(path), "shared")
		var err error
		shared, err = readConfigLayer(sharedPath)
		if err != nil {
			return c, fmt.Errorf("shared configuration %s: %w", sharedPath, err)
		}
	}
	profile, err := readConfigLayer(path)
	if err != nil {
		return c, err
	}
	mergeConfig(shared, profile)
	data, err := json.Marshal(shared)
	if err != nil {
		return c, fmt.Errorf("read configuration: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("read configuration: %w", err)
	}
	return c, c.Validate()
}

// readConfigLayer checks syntax and schema, but not semantic values: a model
// object, for example, may get its id from the other layer.
func readConfigLayer(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// A dangling symlink is an existing configuration, not defaults.
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return map[string]any{}, nil
		} else if statErr != nil {
			return nil, statErr
		}
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, errors.New("configuration exceeds 1 MiB")
	}
	if filepath.Ext(path) == ".toml" {
		var values map[string]any
		if err = toml.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("read configuration: %w", err)
		}
		if containsUnsupportedTOMLValue(values) {
			return nil, errors.New("read configuration: TOML floating-point, date and time values are not configuration values")
		}
		data, err = json.Marshal(values)
		if err != nil {
			return nil, fmt.Errorf("read configuration: %w", err)
		}
	}
	// Check each layer before merging, so an override cannot hide an unknown
	// field or a value of the wrong type.
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var schema Config
	if err = d.Decode(&schema); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("configuration contains trailing data")
	}
	if err := rejectDuplicateConfigFields(json.NewDecoder(bytes.NewReader(data)), reflect.TypeFor[Config]()); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var values map[string]any
	if err := d.Decode(&values); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if values == nil {
		values = map[string]any{}
	}
	normalizeConfigKeys(values, reflect.TypeFor[Config]())
	return values, nil
}

// configFields describes the JSON schema, including Config's custom model field.
func configFields(typ reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			fields[name] = field.Type
		}
	}
	if typ == reflect.TypeFor[Config]() {
		fields["model"] = reflect.TypeFor[ModelEntry]()
	}
	return fields
}

// Check the token stream before decoding into maps, which discard duplicate
// keys. Only schema fields are case-insensitive; dynamic map keys are not.
func rejectDuplicateConfigFields(d *json.Decoder, typ reflect.Type) error {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		var fields map[string]reflect.Type
		var mapValue reflect.Type
		if typ != nil {
			switch typ.Kind() {
			case reflect.Struct:
				fields = configFields(typ)
			case reflect.Map:
				mapValue = typ.Elem()
			}
		}
		seen := make(map[string]bool)
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return err
			}
			key := keyToken.(string)
			childType := mapValue
			for name, fieldType := range fields {
				if strings.EqualFold(key, name) {
					if seen[name] {
						return fmt.Errorf("duplicate configuration field %q", name)
					}
					seen[name] = true
					childType = fieldType
					break
				}
			}
			if err := rejectDuplicateConfigFields(d, childType); err != nil {
				return err
			}
		}
	case '[':
		var elem reflect.Type
		if typ != nil && typ.Kind() == reflect.Slice {
			elem = typ.Elem()
		}
		for d.More() {
			if err := rejectDuplicateConfigFields(d, elem); err != nil {
				return err
			}
		}
	}
	_, err = d.Token() // closing delimiter
	return err
}

// JSON accepts struct field names case-insensitively. Canonicalize those keys
// before merging, while keeping map keys such as header names unchanged.
func normalizeConfigKeys(value any, typ reflect.Type) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return
		}
		fields := configFields(typ)
		for _, key := range slices.Sorted(maps.Keys(object)) {
			for name, fieldType := range fields {
				if strings.EqualFold(key, name) {
					child := object[key]
					normalizeConfigKeys(child, fieldType)
					if key != name {
						delete(object, key)
						if _, exists := object[name]; !exists {
							object[name] = child
						}
					}
					break
				}
			}
		}
	case reflect.Map:
		if object, ok := value.(map[string]any); ok {
			for _, child := range object {
				normalizeConfigKeys(child, typ.Elem())
			}
		}
	case reflect.Slice:
		if items, ok := value.([]any); ok {
			for _, child := range items {
				normalizeConfigKeys(child, typ.Elem())
			}
		}
	}
}

func mergeConfig(base, override map[string]any) {
	for key, value := range override {
		if object, ok := value.(map[string]any); ok {
			if existing, ok := base[key].(map[string]any); ok {
				mergeConfig(existing, object)
				continue
			}
		}
		base[key] = value
	}
}

// JSON can turn TOML dates into strings and whole floats into integers. None
// of the configuration fields accepts dates, times or floating-point values.
func containsUnsupportedTOMLValue(value any) bool {
	switch value := value.(type) {
	case float64, time.Time, toml.LocalDate, toml.LocalTime, toml.LocalDateTime:
		return true
	case map[string]any:
		for _, child := range value {
			if containsUnsupportedTOMLValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsUnsupportedTOMLValue(child) {
				return true
			}
		}
	}
	return false
}

func (c Config) Validate() error {
	if err := c.validateProviders(); err != nil {
		return err
	}
	if err := c.validateModel("model", c.modelEntry()); err != nil {
		return err
	}
	if c.ReviewerModel != "" {
		reviewer, model := c.Reviewer()
		if (!IsCLIProvider(reviewer) && !c.IsNamed(reviewer)) || model == "" || len(c.ReviewerModel) > 512 || c.ReviewerModel != strings.TrimSpace(c.ReviewerModel) || strings.IndexFunc(c.ReviewerModel, unicode.IsControl) >= 0 {
			return fmt.Errorf("reviewer_model must be provider/<model-id> with provider codex, claude or one of providers, such as codex/gpt-6-luna, claude/sonnet or openrouter/%s", CompatibleDecisionsModel)
		}
	}
	if len(c.EnabledModels) > 100 {
		return errors.New("enabled_models may contain at most 100 models")
	}
	seenModels := make(map[string]bool, len(c.EnabledModels))
	for _, entry := range c.EnabledModels {
		if err := c.validateModel("enabled_models", entry); err != nil {
			return err
		}
		if _, model := SplitModel(entry.ID); model == "" {
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
		if err := c.validateModel("subagents."+role, entry); err != nil {
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
	switch c.SystemPrompt {
	case "", "full", "compact":
	default:
		return fmt.Errorf("system_prompt must be full or compact, not %q", c.SystemPrompt)
	}
	if len(c.DisabledTools) > 32 {
		return errors.New("disabled_tools may name at most 32 tools")
	}
	for _, name := range c.DisabledTools {
		if name == "" || len(name) > 64 || strings.IndexFunc(name, func(r rune) bool { return (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') }) >= 0 {
			return fmt.Errorf("disabled_tools %q must be a tool name, such as WebSearch", name)
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
	if err := c.validateBrowserAuth(); err != nil {
		return err
	}
	switch c.Memory.Reflection {
	case "", "on", "off":
	default:
		return fmt.Errorf("memory.reflection must be on or off, not %q", c.Memory.Reflection)
	}
	if c.Memory.Model != nil {
		if err := c.validateModel("memory.model", *c.Memory.Model); err != nil {
			return err
		}
	}
	if c.Memory.IndexBytes != 0 && (c.Memory.IndexBytes < MinMemoryIndexBytes || c.Memory.IndexBytes > MaxMemoryIndexBytes) {
		return fmt.Errorf("memory.index_bytes must be between %d and %d, or 0 for the default", MinMemoryIndexBytes, MaxMemoryIndexBytes)
	}
	return nil
}

// validateModel checks one model entry of the named field: provider/<model-id>,
// its reasoning effort and its context window. Only a CLI provider may leave
// the model ID out, for the model its CLI is configured with.
func (c Config) validateModel(field string, entry ModelEntry) error {
	id, reasoning := entry.ID, entry.Reasoning
	provider, model := SplitModel(id)
	if !contains(c.ProviderIDs(), provider) {
		return fmt.Errorf("%s %q must be provider/<model-id> with provider %s", field, id, strings.Join(c.ProviderIDs(), ", "))
	}
	if !IsCLIProvider(provider) && model == "" {
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
	if entry.ContextWindow != 0 && entry.ContextWindow < MinContextTokens {
		return fmt.Errorf("%s %q: context_window must be at least %d, or left out to assume context_tokens", field, id, MinContextTokens)
	}
	return nil
}

// Bounds for browser_auth.
const (
	maxBrowserAuthOrigins = 32
	maxBrowserAuthHeaders = 16
	maxBrowserAuthValue   = 4096
)

// validateBrowserAuth checks the "browser_auth" entry: origins written as
// the browser names them, header names HTTP allows, and values bash can be
// given on one line.
func (c Config) validateBrowserAuth() error {
	if len(c.BrowserAuth) > maxBrowserAuthOrigins {
		return fmt.Errorf("browser_auth may name at most %d origins", maxBrowserAuthOrigins)
	}
	for _, origin := range slices.Sorted(maps.Keys(c.BrowserAuth)) {
		if canonical, ok := CanonicalOrigin(origin); !ok {
			return fmt.Errorf("browser_auth %q is not an origin such as https://grafana.example.com", origin)
		} else if canonical != origin {
			return fmt.Errorf("browser_auth %q is written %q", origin, canonical)
		}

		headers := c.BrowserAuth[origin]
		if len(headers) == 0 || len(headers) > maxBrowserAuthHeaders {
			return fmt.Errorf("browser_auth %q needs between 1 and %d headers", origin, maxBrowserAuthHeaders)
		}
		for _, name := range slices.Sorted(maps.Keys(headers)) {
			if !validHeaderName(name) {
				return fmt.Errorf("browser_auth %q: %q is not a header name", origin, name)
			}
			value := headers[name]
			if value == "" || len(value) > maxBrowserAuthValue || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return fmt.Errorf("browser_auth %q %s must be 1 to %d characters on one line", origin, name, maxBrowserAuthValue)
			}
		}
	}
	return nil
}

// CanonicalOrigin writes an http or https origin as the browser names it:
// scheme and host in lowercase, without a default port or a trailing slash.
// ok is false for anything that is not an origin.
func CanonicalOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if scheme != "http" && scheme != "https" || host == "" {
		return "", false
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(scheme == "http" && port == "80" || scheme == "https" && port == "443") {
		host += ":" + port
	}
	return scheme + "://" + host, true
}

// validHeaderName reports whether name is an HTTP field name: a token.
func validHeaderName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	return strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r))
	}) < 0
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
		return errors.New("reasoning is configurable for Codex, Claude Code and your providers only")
	}
	// Codex advertises effort names per model as an open string. Claude Code
	// takes its own fixed levels. A named provider is sent reasoning_effort,
	// whose values OpenRouter documents.
	if provider == "claude" && !contains(ClaudeEfforts(), effort) {
		return errors.New("reasoning for Claude Code must be one of low, medium, high, xhigh, or max")
	}
	if provider != "codex" && provider != "claude" && !contains(OpenRouterReasoningEfforts(), effort) {
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
// instead of silently ignoring it: Codex, Claude Code, and every named
// provider, which is sent reasoning_effort whether or not its model supports it.
func (c Config) SupportsReasoning(provider string) bool {
	return IsCLIProvider(provider) || c.IsNamed(provider)
}

// ClaudeEfforts are the effort levels Claude Code's --effort accepts.
func ClaudeEfforts() []string {
	return []string{"low", "medium", "high", "xhigh", "max"}
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
	var data []byte
	var err error
	if filepath.Ext(path) == ".toml" {
		data, err = defaultTOML()
	} else {
		data, err = json.MarshalIndent(Default(), "", "  ")
		data = append(data, '\n')
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
