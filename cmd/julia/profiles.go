package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/ui"
)

// launchOverrides are the launch flags that hold whichever profile the
// session switches to: --mode and --timeout. --model and --reasoning choose
// the launch profile's model only; a switched profile brings its own.
type launchOverrides struct{ mode, timeout string }

func (o launchOverrides) apply(cfg *config.Config) {
	if o.mode != "" {
		cfg.Mode = o.mode
	}
	if o.timeout != "" {
		cfg.Timeout = o.timeout
	}
}

// profileLabel is the profile the configuration file at path holds, or its
// file name when --config named a file that holds none.
func profileLabel(path string) string {
	if name, ok := config.ProfileName(path); ok {
		return name
	}
	return filepath.Base(path)
}

// profileChoices are the profiles in the configuration directory dir as the
// profile picker lists them, each with its model and approval mode, or why
// it does not load.
func profileChoices(dir string) ([]ui.Profile, error) {
	names, err := config.Profiles(dir)
	if err != nil {
		return nil, err
	}
	profiles := make([]ui.Profile, 0, len(names))
	for _, name := range names {
		path := config.ProfilePath(dir, name)
		file := filepath.Base(path)
		// Profiles lists only files that exist, or the default, which may not.
		cfg, err := config.Load(path)
		if err != nil {
			first, _, _ := strings.Cut(err.Error(), "\n")
			profiles = append(profiles, ui.Profile{Name: name, Detail: file + " · does not load: " + safe(first)})
			continue
		}
		profiles = append(profiles, ui.Profile{Name: name, Detail: file + " · " + safe(config.JoinModel(cfg.Provider, cfg.Model)) + " · " + cfg.Mode})
	}
	return profiles, nil
}

// loadSwitchedProfile reads profile name from the configuration directory dir
// as a launch would, with the launch overrides and the reasoning last chosen
// for its provider, and checks that it can run: its subagent roles exist and
// its provider has a key.
func loadSwitchedProfile(dir, name string, overrides launchOverrides, preferencesPath string) (config.Config, error) {
	cfg, err := config.LoadProfile(dir, name)
	if err != nil {
		return cfg, err
	}
	overrides.apply(&cfg)
	var prefs config.Preferences
	if preferencesPath != "" {
		prefs, _ = config.LoadPreferences(preferencesPath)
	}
	cfg.ApplyStartingReasoning(prefs)
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("profile %s: %w", name, err)
	}
	if _, err := loadRoles(config.ProfilePath(dir, name), cfg); err != nil {
		return cfg, fmt.Errorf("profile %s: %w", name, err)
	}
	if !config.IsCLIProvider(cfg.Provider) {
		if _, err := apiKey(cfg, cfg.Provider); err != nil {
			return cfg, fmt.Errorf("profile %s: %w", name, err)
		}
	}
	return cfg, nil
}

// profileSettings are what a profile sets in the interface besides the
// session's model.
func profileSettings(cfg config.Config) ui.ProfileSettings {
	settings := ui.ProfileSettings{Mode: cfg.Mode, ContextWindow: cfg.ContextTokens, NamedProviders: map[string]bool{}}
	for _, entry := range cfg.EnabledModels {
		provider, id := config.SplitModel(entry.ID)
		settings.EnabledModels = append(settings.EnabledModels, core.ModelOption{Provider: provider, ID: id, Name: id, Description: "Enabled model", DefaultReasoning: entry.Reasoning})
	}
	for _, p := range cfg.Providers {
		settings.NamedProviders[p.ID] = cfg.SupportsReasoning(p.ID)
	}
	return settings
}
