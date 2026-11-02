package main

import (
	"fmt"

	"julia/internal/tools"
)

// githubSummary reports configuration without opening the credential store or
// making a GitHub request. Authentication is resolved when the tool is used.
func githubSummary() string {
	auth, err := tools.LoadGitHubAuth()
	if err != nil {
		return fmt.Sprintf("GitHub    auth unavailable: %s", safe(err.Error()))
	}
	if !auth.Enabled() {
		return "GitHub    off; sign in with gh auth login or set GH_TOKEN"
	}
	return "GitHub    github.com · " + safe(auth.Summary()) + " · writes follow approval mode"
}

// loadGitHubAuth turns off the GitHub tool, and only it, when the selected
// credential source is unusable. GH_TOKEN and GITHUB_TOKEN belong to other tools
// too, so a placeholder in them must not stop a session that never uses GitHub.
// The warning says why the tool is off.
func loadGitHubAuth() (auth tools.GitHubAuth, warning string) {
	auth, err := tools.LoadGitHubAuth()
	if err != nil {
		return tools.GitHubAuth{}, "GitHub tool off: " + err.Error()
	}
	return auth, ""
}
