package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bugsnagFixtureToken = "bugsnag_fixture_0123456789abcdef"

func TestBugsnagAuthLoading(t *testing.T) {
	t.Setenv(BugsnagTokenEnv, "")
	file := filepath.Join(t.TempDir(), "bugsnag.json")
	if auth, err := LoadBugsnagAuth(file); err != nil || auth.Enabled() {
		t.Fatalf("missing file: enabled=%v err=%v", auth.Enabled(), err)
	}
	if err := os.WriteFile(file, []byte(`{"auth_token":"`+bugsnagFixtureToken+`","endpoint":"https://api.bugsnag.smartbear.com"}`), 0600); err != nil {
		t.Fatal(err)
	}
	auth, err := LoadBugsnagAuth(file)
	if err != nil || !auth.Enabled() || auth.token != bugsnagFixtureToken || auth.Summary() != "api.bugsnag.smartbear.com" {
		t.Fatalf("configured auth: enabled=%v summary=%q err=%v", auth.Enabled(), auth.Summary(), err)
	}
	t.Setenv(BugsnagTokenEnv, "bugsnag_environment_0123456789")
	auth, err = LoadBugsnagAuth(file)
	if err != nil || auth.token != "bugsnag_environment_0123456789" || auth.Summary() != "api.bugsnag.smartbear.com" {
		t.Fatalf("environment override: summary=%q err=%v", auth.Summary(), err)
	}
	auth, err = LoadBugsnagAuth("")
	if err != nil || auth.Summary() != "api.bugsnag.com" {
		t.Fatalf("environment without file: summary=%q err=%v", auth.Summary(), err)
	}
}

func TestBugsnagAuthRejectsInvalidConfigurationWithoutSecrets(t *testing.T) {
	t.Setenv(BugsnagTokenEnv, "")
	file := filepath.Join(t.TempDir(), "bugsnag.json")
	for _, data := range []string{
		`{"auth_token":"invalid secret"}`,
		`{"auth_token":"` + bugsnagFixtureToken + `","endpoint":"https://other.example"}`,
		`{"auth_token":"` + bugsnagFixtureToken + `","endpoint":"http://api.bugsnag.com"}`,
		`{"auth_token":"` + bugsnagFixtureToken + `","endpoint":"https://api.bugsnag.com/"}`,
		`{"auth_token":"` + bugsnagFixtureToken + `","endpoint":"https://api.bugsnag.com@other.example"}`,
		`{"auth_token":"` + bugsnagFixtureToken + `","endpoint":` + bugsnagFixtureToken + `}`,
		`{"auth_token":"` + bugsnagFixtureToken + `"} {}`,
		`{"auth_token":"` + strings.Repeat("a", 17000) + `"}`,
		`{"endpoint":"https://api.bugsnag.com"}`,
	} {
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		auth, err := LoadBugsnagAuth(file)
		if err == nil || auth.Enabled() || strings.Contains(err.Error(), bugsnagFixtureToken) || strings.Contains(err.Error(), "invalid secret") {
			t.Fatalf("invalid configuration: enabled=%v err=%v", auth.Enabled(), err)
		}
	}
	if err := os.WriteFile(file, []byte(`{"auth_token":"`+bugsnagFixtureToken+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BugsnagTokenEnv, "invalid secret")
	if _, err := LoadBugsnagAuth(file); err == nil {
		t.Fatal("invalid environment token fell back to file")
	}
}
