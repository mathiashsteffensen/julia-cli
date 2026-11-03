package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"julia/internal/terminal"
)

// Run the real CLI in a child: its signal handlers must not affect the test
// process, and the terminal owns cleanup even when the demo fails to exit.
func TestDemoExitsOnHangup(t *testing.T) {
	if os.Getenv("JULIA_DEMO_HANGUP_HELPER") == "1" {
		os.Args = []string{"julia", "--demo", "--dir", os.Getenv("HOME")}
		main()
		os.Exit(0)
	}

	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cmd := exec.Command(binary, "-test.run=^TestDemoExitsOnHangup$")
	cmd.Env = append(os.Environ(),
		"JULIA_DEMO_HANGUP_HELPER=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
	)
	screen, err := terminal.Start(cmd, 120, 40)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { screen.Close() })

	ctx := context.Background()
	if err := screen.WaitFor(ctx, regexp.MustCompile("What shall we work on"), 10*time.Second); err != nil {
		t.Fatalf("demo did not become ready: %v\n%s", err, screen.Snapshot(0).Text())
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	state, err := screen.WaitExit(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("demo survived terminal hangup: %v", err)
	}
	if state.Code != 0 || state.Signal != "" {
		t.Fatalf("demo did not shut down cleanly: %+v", state)
	}
}
