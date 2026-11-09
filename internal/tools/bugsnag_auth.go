package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

const (
	BugsnagTokenEnv = "BUGSNAG_AUTH_TOKEN"
	bugsnagOrigin   = "https://api.bugsnag.com"
)

// BugsnagAuth keeps the personal auth token out of tool arguments and output.
// Only the two hosted API origins are supported.
type BugsnagAuth struct {
	token    string
	endpoint string
}

func (a BugsnagAuth) Enabled() bool { return a.token != "" }

func (a BugsnagAuth) Summary() string {
	return strings.TrimPrefix(a.endpoint, "https://")
}

// LoadBugsnagAuth reads bugsnag.json beside the selected configuration.
// BUGSNAG_AUTH_TOKEN overrides its auth_token. A missing file is allowed.
func LoadBugsnagAuth(path string) (BugsnagAuth, error) {
	var stored struct {
		AuthToken string `json:"auth_token"`
		Endpoint  string `json:"endpoint"`
	}
	if path != "" {
		f, err := os.Open(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return BugsnagAuth{}, fmt.Errorf("open Bugsnag credentials: %w", err)
		}
		if err == nil {
			defer f.Close()
			body, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
			if err != nil || len(body) > 16<<10 || json.Unmarshal(body, &stored) != nil {
				return BugsnagAuth{}, errors.New("bugsnag.json must be a JSON object within 16 KiB")
			}
		}
	}
	if token := os.Getenv(BugsnagTokenEnv); token != "" {
		stored.AuthToken = token
	}
	if stored.AuthToken == "" {
		if stored.Endpoint != "" {
			return BugsnagAuth{}, errors.New("Bugsnag needs auth_token in bugsnag.json or " + BugsnagTokenEnv)
		}
		return BugsnagAuth{}, nil
	}
	if !validBugsnagToken(stored.AuthToken) {
		return BugsnagAuth{}, errors.New("Bugsnag auth_token must be a personal auth token of 16 to 4096 printable ASCII characters without spaces")
	}
	if stored.Endpoint == "" {
		stored.Endpoint = bugsnagOrigin
	}
	switch stored.Endpoint {
	case bugsnagOrigin, "https://api.bugsnag.smartbear.com":
	default:
		return BugsnagAuth{}, errors.New("Bugsnag endpoint must be https://api.bugsnag.com or https://api.bugsnag.smartbear.com")
	}
	return BugsnagAuth{token: stored.AuthToken, endpoint: stored.Endpoint}, nil
}

func validBugsnagToken(token string) bool {
	if len(token) < 16 || len(token) > 4096 {
		return false
	}
	for _, c := range token {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

func (a BugsnagAuth) url(path *url.URL) *url.URL {
	u := *path
	u.Scheme = "https"
	u.Host = strings.TrimPrefix(a.endpoint, "https://")
	return &u
}
