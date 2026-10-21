package main

import (
	"os"
	"path/filepath"
	"testing"

	"julia/internal/core"
)

func TestResumeCommandNamesForeignWorkspace(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wd, _ = filepath.EvalSymlinks(wd)
	if got := resumeCommand(&core.Session{ID: "abc", Directory: wd}); got != "julia --resume abc" {
		t.Errorf("same workspace: %q", got)
	}
	if got := resumeCommand(&core.Session{ID: "abc", Directory: "/tmp/it's here"}); got != `julia --resume abc --dir '/tmp/it'\''s here'` {
		t.Errorf("foreign workspace: %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"/a/b-c_d.e": "/a/b-c_d.e", "/a b": "'/a b'", "~/x": "'~/x'", "$HOME": "'$HOME'", "": "''"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
