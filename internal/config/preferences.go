package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Preferences is state JULIA remembers between launches, separate from the
// hand-edited configuration file.
type Preferences struct {
	// Reasoning holds the last explicitly selected effort per provider. Efforts
	// are provider-specific, so one provider's choice never applies to another.
	// An empty value records a deliberate choice of the provider default.
	Reasoning map[string]string `json:"reasoning,omitempty"`
}

func PreferencesPath() (string, error) {
	d := os.Getenv("XDG_STATE_HOME")
	if d == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(d, "julia", "preferences.json"), nil
}

// LoadPreferences returns empty preferences when the file is missing.
func LoadPreferences(path string) (Preferences, error) {
	var p Preferences
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	defer f.Close()
	if err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&p); err != nil {
		return Preferences{}, err
	}
	return p, nil
}

// RememberReasoning records effort as the provider's last used reasoning. It
// rereads the file first so concurrent sessions only overwrite their own provider.
func RememberReasoning(path, provider, effort string) error {
	p, err := LoadPreferences(path)
	if err != nil {
		p = Preferences{}
	}
	if current, ok := p.Reasoning[provider]; ok && current == effort {
		return nil
	}
	if p.Reasoning == nil {
		p.Reasoning = map[string]string{}
	}
	p.Reasoning[provider] = effort
	return savePreferences(path, p)
}

func savePreferences(path string, p Preferences) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".preferences-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ApplyStartingReasoning picks the effort a new run starts with: the model's
// own configured reasoning, else the provider's last used effort, unless that
// is no longer valid for the current configuration (for example a compatible
// endpoint that stopped supporting reasoning).
func (c *Config) ApplyStartingReasoning(p Preferences) {
	if effort := c.ReasoningFor(c.Provider, c.Model); effort != "" {
		c.Reasoning = effort
		return
	}
	effort, ok := p.Reasoning[c.Provider]
	if !ok {
		return
	}
	candidate := *c
	candidate.Reasoning = effort
	if candidate.Validate() == nil {
		*c = candidate
	}
}
