package main

import "testing"

func TestPromptShorthand(t *testing.T) {
	root := newCommand("config.toml")
	if err := root.ParseFlags([]string{"-p", "fix the build"}); err != nil {
		t.Fatal(err)
	}
	if prompt, _ := root.Flags().GetString("prompt"); prompt != "fix the build" {
		t.Fatalf("-p set --prompt to %q", prompt)
	}
}

// -p would take -profile=work as the task "rofile=work", so a long flag with
// one dash is an error.
func TestSingleDashLongFlagsAreRejected(t *testing.T) {
	root := newCommand("config.toml")
	for _, args := range [][]string{{"-profile=work"}, {"-prompt", "fix the build"}, {"--dir", ".", "-json"}} {
		if err := rejectSingleDashFlags(root, args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	for _, args := range [][]string{{"-p", "fix the build"}, {"--prompt=fix the build"}, {"memory", "stats", "--days", "7"}, {"--", "-profile"}} {
		if err := rejectSingleDashFlags(root, args); err != nil {
			t.Errorf("rejected %q: %v", args, err)
		}
	}
}
