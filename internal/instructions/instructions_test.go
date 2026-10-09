package instructions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"jarvis/internal/core"
)

func writeSkill(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBuildReloadsPersonalSkillAndCurrentEnvironment(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "skills", "coding-assistance-style", "SKILL.md")
	writeSkill(t, path, "# Personal style\nUse the first policy.")
	options := Options{HomeDir: home, Directory: "/work/one", Mode: "auto", Provider: "codex", Reasoning: "high", BashEnvironment: map[string]string{"GOROOT": "/toolchain/go", "GOCACHE": "/private/cache/workspace/go-build"}, Sandbox: "Seatbelt", Timeout: "4h", Now: time.Date(2026, 10, 8, 22, 0, 0, 0, time.FixedZone("CEST", 7200))}
	first, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{core.SystemPrompt, "Use the first policy.", `"workspace": "/work/one"`, "2026-10-08T22:00:00+02:00", `"human_approval_available": true`, `"approval_mode": "auto"`, `"reasoning_effort": "high"`, `"GOROOT": "/toolchain/go"`, `"GOCACHE": "/private/cache/workspace/go-build"`, path} {
		if !strings.Contains(first.Text, expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	if len(first.Skills) != 1 || len(first.Warnings) != 0 {
		t.Fatalf("unexpected discovery: %+v", first)
	}
	writeSkill(t, path, "# Personal style\nUse the revised policy.")
	options.Directory, options.Mode, options.Headless = "/work/two", "read-only", true
	second, err := Build(options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.Text, "Use the first policy.") || !strings.Contains(second.Text, "Use the revised policy.") || !strings.Contains(second.Text, `"workspace": "/work/two"`) || !strings.Contains(second.Text, `"human_approval_available": false`) {
		t.Fatal("reused stale instructions or environment")
	}
	data, err := json.Marshal(core.Session{Instructions: second.Text})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "revised policy") || strings.Contains(string(data), "Instructions") {
		t.Fatal("runtime instructions were persisted")
	}
}

func TestSkillDiscoveryAndExplicitSelection(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	writeSkill(t, filepath.Join(workspace, ".codex", "skills", "coding-assistance-style", "SKILL.md"), "untrusted repository substitute")
	opts := Options{HomeDir: home, Directory: workspace}
	missing, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Skills) != 0 || len(missing.Warnings) != 1 || strings.Contains(missing.Text, "untrusted repository substitute") {
		t.Fatal("loaded repository skill or silently lost missing skill")
	}
	fallback := filepath.Join(home, ".agents", "skills", "coding-assistance-style", "SKILL.md")
	writeSkill(t, fallback, "agents fallback")
	bundle, err := Build(opts)
	if err != nil || !strings.Contains(bundle.Text, "agents fallback") {
		t.Fatal("personal fallback missing", err)
	}
	opts.SkillPaths = []string{}
	bundle, err = Build(opts)
	if err != nil || len(bundle.Skills) != 0 || len(bundle.Warnings) != 0 {
		t.Fatal("explicit empty list did not disable discovery", err)
	}
	opts.SkillPaths = []string{"~/.agents/skills/coding-assistance-style/SKILL.md", fallback}
	bundle, err = Build(opts)
	if err != nil || len(bundle.Skills) != 1 {
		t.Fatal("tilde or deduplication failed", err)
	}
	opts.SkillPaths = []string{"relative/SKILL.md"}
	if _, err := Build(opts); err == nil {
		t.Fatal("accepted cwd-relative trusted skill path")
	}
	opts.SkillPaths = []string{filepath.Join(home, "missing.md")}
	if _, err := Build(opts); err == nil {
		t.Fatal("silently ignored configured missing skill")
	}
}

func TestMalformedSkillsFailBeforeProviderUse(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "style.md")
	for _, content := range []string{"", "  \n", string([]byte{0xff}), "bad\x00text", strings.Repeat("a", maxSkillBytes+1)} {
		writeSkill(t, path, content)
		if _, err := Build(Options{HomeDir: home, SkillPaths: []string{path}}); err == nil {
			t.Fatal("accepted malformed/oversized skill")
		}
	}
	if _, err := Build(Options{HomeDir: home, SkillPaths: []string{home}}); err == nil {
		t.Fatal("accepted directory as skill")
	}
}

func TestSpecialSkillFileCannotBlockStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Build(Options{SkillPaths: []string{path}}); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO as skill")
		}
	case <-time.After(time.Second):
		t.Fatal("skill loader blocked on a FIFO")
	}
}
