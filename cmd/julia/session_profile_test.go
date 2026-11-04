package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/session"
)

func TestResumeLoadsSessionConfigBeforeLaunchSetup(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configDir := filepath.Join(configHome, "julia")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The default cannot load, so reaching the terminal check proves that
	// saved or explicit configuration was selected before validation.
	write(filepath.Join(configDir, "config.json"), `{"mode":"invalid"}`)
	profilePath := filepath.Join(configDir, "config.work.json")
	valid := `{"model":"local/model","providers":[{"id":"local","base_url":"http://127.0.0.1:1/v1"}]}`
	write(profilePath, valid)
	customPath := filepath.Join(configHome, "custom.json")
	write(customPath, valid)

	store, err := session.Default()
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(workspace, "local", "model")
	s.ConfigPath, s.ConfigProfile = profilePath, "work"
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"resume", []string{"--resume", s.ID}, "interactive mode requires a terminal"},
		{"continue", []string{"--continue"}, "interactive mode requires a terminal"},
		{"explicit profile", []string{"--resume", s.ID, "--profile", "work"}, "interactive mode requires a terminal"},
		{"explicit config", []string{"--resume", s.ID, "--config", customPath}, "interactive mode requires a terminal"},
		{"explicit default profile overrides saved", []string{"--resume", s.ID, "--profile", "default"}, "unknown approval mode"},
		{"explicit empty profile overrides saved", []string{"--resume", s.ID, "--profile="}, "unknown approval mode"},
		{"conflicting resume flags", []string{"--resume", s.ID, "--continue"}, "choose either --resume or --continue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = append([]string{"julia", "--dir", workspace, "--memory", "off"}, tc.flags...)
			err := run()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run() = %v, want %q", err, tc.want)
			}
		})
	}

	// Legacy sessions use the default configuration, not a guessed profile.
	store, err = session.Default()
	if err != nil {
		t.Fatal(err)
	}
	legacy := session.New(workspace, "local", "model")
	if err := store.Save(legacy); err != nil {
		t.Fatal(err)
	}
	store.Close()
	os.Args = []string{"julia", "--dir", workspace, "--resume", legacy.ID}
	if err := run(); err == nil || !strings.Contains(err.Error(), "unknown approval mode") {
		t.Fatalf("legacy run() = %v, want default config error", err)
	}
}

func TestResumeProfileResolvesCurrentProfileFormatAndCustomPath(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "julia")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	// The saved named profile used to be JSON. Resolve it through ProfilePath
	// so a later migration to TOML is picked up.
	tomlPath := filepath.Join(configDir, "config.work.toml")
	if err := os.WriteFile(tomlPath, []byte("model = \"codex\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(configDir, "custom.json")
	if err := os.WriteFile(customPath, []byte(`{"model":"codex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := session.Default()
	if err != nil {
		t.Fatal(err)
	}
	named := session.New(workspace, "codex", "")
	named.ConfigPath, named.ConfigProfile = filepath.Join(configDir, "config.work.json"), "work"
	custom := session.New(workspace, "codex", "")
	custom.ConfigPath = customPath
	for _, s := range []*core.Session{named, custom} {
		if err := store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	for _, s := range []*core.Session{named, custom} {
		os.Args = []string{"julia", "--dir", workspace, "--resume", s.ID, "--memory", "off"}
		if err := run(); err == nil || !strings.Contains(err.Error(), "interactive mode requires a terminal") {
			t.Fatalf("resume %s: %v", s.ID, err)
		}
	}
	if err := os.Remove(customPath); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"julia", "--dir", workspace, "--resume", custom.ID}
	if err := run(); err == nil || !strings.Contains(err.Error(), "saved configuration") {
		t.Fatalf("missing saved custom configuration: %v", err)
	}
	os.Args = []string{"julia", "--dir", workspace, "--resume", custom.ID, "--profile", "work", "--memory", "off"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "interactive mode requires a terminal") {
		t.Fatalf("override missing saved configuration: %v", err)
	}
}

func TestSessionProfileSavePreservesForkAndRollsBackOnError(t *testing.T) {
	for _, fork := range []bool{false, true} {
		current := &core.Session{ID: "old", Provider: "codex", Model: "old", ConfigPath: "/old", ConfigProfile: "old"}
		if fork {
			current.Messages = []core.Message{{}}
		}
		cfg := config.Default()
		cfg.Provider, cfg.Model = "claude", "new"
		var saved []core.Session
		save := func(s *core.Session) error { saved = append(saved, *s); return nil }
		err := applySessionSettings(cfg, current, "claude", "new", "", saveSessionProfile(save, current, "/new", "new", "claude", "new"))
		if err != nil {
			t.Fatal(err)
		}
		if current.ConfigPath != "/new" || current.ConfigProfile != "new" {
			t.Fatalf("active configuration: %+v", current)
		}
		if fork && (len(saved) != 2 || saved[0].ConfigPath != "/old" || saved[1].ConfigPath != "/new") {
			t.Fatalf("forked saves: %+v", saved)
		}
		if !fork && (len(saved) != 1 || saved[0].ConfigPath != "/new") {
			t.Fatalf("in-place save: %+v", saved)
		}
	}

	current := &core.Session{ID: "old", Provider: "codex", ConfigPath: "/old", ConfigProfile: "old"}
	save := func(*core.Session) error { return errors.New("disk full") }
	err := applySessionSettings(config.Default(), current, "codex", "new", "", saveSessionProfile(save, current, "/new", "new", "codex", "new"))
	if err == nil || current.ConfigPath != "/old" || current.ConfigProfile != "old" {
		t.Fatalf("failed save changed active config: %+v, %v", current, err)
	}
}

func TestOpenResumeSessionLocksAndRereads(t *testing.T) {
	dir := t.TempDir()
	owner := &session.Store{Directory: dir}
	s := session.New("/work", "codex", "")
	s.ConfigProfile = "first"
	if err := owner.Save(s); err != nil {
		t.Fatal(err)
	}
	other := &session.Store{Directory: dir}
	if _, err := openResumeSession(other, "/work", s.ID, false, true); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("locked session: %v", err)
	}
	s.ConfigProfile = "latest"
	if err := owner.Save(s); err != nil {
		t.Fatal(err)
	}
	owner.Close()
	got, err := openResumeSession(other, "/work", "", true, true)
	if err != nil || got.ConfigProfile != "latest" {
		t.Fatalf("latest session = %+v, %v", got, err)
	}
	other.Close()
}

func TestSessionProfilePersistsAndResumesHeadless(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Done.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	configDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "julia")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "config.work.json")
	data := fmt.Sprintf(`{"model":"local/model","providers":[{"id":"local","base_url":%q}]}`, server.URL+"/v1")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"mode":"invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Stdout = out
	os.Args = []string{"julia", "--dir", workspace, "--profile", "work", "--prompt", "hello", "--memory", "off", "--json"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	store, err := session.Default()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if s.ConfigPath != path || s.ConfigProfile != "work" {
		t.Fatalf("saved config = %q, profile = %q", s.ConfigPath, s.ConfigProfile)
	}
	store.Close()
	os.Args = []string{"julia", "--dir", workspace, "--continue", "--prompt", "again", "--memory", "off", "--json"}
	if err := run(); err != nil {
		t.Fatal(err)
	}

	custom := filepath.Join(configDir, "custom.json")
	if err := os.WriteFile(custom, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"julia", "--dir", workspace, "--resume", s.ID, "--config", custom, "--prompt", "override", "--memory", "off", "--json"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	store, err = session.Default()
	if err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if s.ConfigPath != custom || s.ConfigProfile != "" {
		t.Fatalf("override saved config = %q, profile = %q", s.ConfigPath, s.ConfigProfile)
	}
	os.Args = []string{"julia", "--dir", workspace, "--resume", s.ID, "--prompt", "custom", "--memory", "off", "--json"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeReloadsSharedConfiguration(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := filepath.Join(configHome, "julia")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, dir, "work", `{"model":"codex"}`)
	writeProfile(t, dir, "shared", `{"mode":"ask"}`)
	store, err := session.Default()
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(workspace, "codex", "")
	s.ConfigPath, s.ConfigProfile = filepath.Join(dir, "config.work.json"), "work"
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"julia", "--dir", workspace, "--resume", s.ID, "--memory", "off"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "interactive mode requires a terminal") {
		t.Fatalf("resume with shared config: %v", err)
	}
	writeProfile(t, dir, "shared", `{"mode":"invalid"}`)
	if err := run(); err == nil || !strings.Contains(err.Error(), "unknown approval mode") {
		t.Fatalf("resume must reload shared config: %v", err)
	}
	os.Args = []string{"julia", "--dir", workspace, "--profile", "shared"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("shared profile flag: %v", err)
	}
}
