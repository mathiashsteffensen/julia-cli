package main

import (
	"errors"

	"julia/internal/core"
	"julia/internal/session"
)

// openResumeSession re-reads the selected session after taking its lock. The
// first read selects an ID only; it must not determine the active profile.
func openResumeSession(store *session.Store, directory, id string, latest, lock bool) (*core.Session, error) {
	if id != "" && latest {
		return nil, errors.New("choose either --resume or --continue")
	}
	var s *core.Session
	var err error
	if id != "" {
		s, err = store.Load(id)
	} else {
		s, err = store.Latest(directory)
	}
	if err != nil || !lock {
		return s, err
	}
	if err := store.Lock(s.ID); err != nil {
		return nil, err
	}
	return store.Load(s.ID)
}

// saveSessionProfile associates the selected configuration with the new
// conversation, while retaining the old profile on a forked transcript.
func saveSessionProfile(save func(*core.Session) error, current *core.Session, path, profile, provider, model string) func(*core.Session) error {
	fork := (provider != current.Provider || (provider != "codex" && model != current.Model)) && hasSessionContinuation(current)
	return func(candidate *core.Session) error {
		if !fork || candidate.ID != current.ID {
			candidate.ConfigPath, candidate.ConfigProfile = path, profile
		}
		return save(candidate)
	}
}
