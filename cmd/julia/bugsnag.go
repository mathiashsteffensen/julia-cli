package main

import (
	"fmt"
	"path/filepath"

	"julia/internal/tools"
)

// bugsnagAuthFile keeps the personal auth token beside the configuration file.
func bugsnagAuthFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "bugsnag.json")
}

// bugsnagSummary reports the configured host without revealing the auth token.
func bugsnagSummary(configPath string) string {
	path := bugsnagAuthFile(configPath)
	auth, err := tools.LoadBugsnagAuth(path)
	if err != nil {
		return fmt.Sprintf("Bugsnag   auth unavailable: %s", safe(err.Error()))
	}
	if !auth.Enabled() {
		return fmt.Sprintf("Bugsnag   off. Save a personal auth token in %s or set %s", safe(path), tools.BugsnagTokenEnv)
	}
	return "Bugsnag   " + safe(auth.Summary()) + " · read-only"
}
