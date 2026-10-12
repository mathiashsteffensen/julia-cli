package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"jarvis/internal/browser"
)

// The doctor names the Chrome the Browser tool would use and its version,
// or says why there is none.
func TestDoctorBrowserSummary(t *testing.T) {
	missing := browserSummary(context.Background(), "/nonexistent/Chromium.app")
	if missing != "Browser   browser /nonexistent/Chromium.app: not found" {
		t.Fatalf("missing browser: %q", missing)
	}
	explicit := os.Getenv("JARVIS_BROWSER")
	binary, err := browser.Find(explicit)
	if err != nil {
		t.Skip("no Chrome to check: " + err.Error())
	}
	got := browserSummary(context.Background(), explicit)
	if !strings.HasPrefix(got, "Browser   "+binary+" · ") || !strings.Contains(got, "Chrome/") {
		t.Fatalf("found browser: %q", got)
	}
}
