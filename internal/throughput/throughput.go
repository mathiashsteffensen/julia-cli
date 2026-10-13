// Package throughput keeps a record of how fast each model has produced
// output, so the model picker can show tokens per second alongside each model.
package throughput

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Window is how far back a model's samples are kept. It is applied only when
// a model is used again: a model idle for a month still shows the statistics
// of its last week of use.
const Window = 7 * 24 * time.Hour

// maxSamples bounds one model's samples, so a busy week never grows the file
// without limit.
const maxSamples = 5000

// Sample is one model call: when it finished, how many output tokens it
// produced, and how long the model took to produce them, not counting time
// spent running tools in between.
type Sample struct {
	At      time.Time `json:"at"`
	Output  int       `json:"output"`
	Seconds float64   `json:"seconds"`
}

// Log holds each model's recent samples, keyed by provider/model.
type Log map[string][]Sample

// Stats summarizes a model's samples. TokensPerSecond weights every call by
// its length, so a short reply does not count as much as a long one.
type Stats struct {
	TokensPerSecond float64
	Calls           int
	LastUsed        time.Time
}

// Path is the log's location, beside the other state JULIA keeps.
func Path() (string, error) {
	d := os.Getenv("XDG_STATE_HOME")
	if d == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(d, "julia", "throughput.json"), nil
}

// Load returns an empty log when the file is missing.
func Load(path string) (Log, error) {
	var file struct {
		Models Log `json:"models"`
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Log{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&file); err != nil {
		return nil, err
	}
	if file.Models == nil {
		file.Models = Log{}
	}
	return file.Models, nil
}

// Add records one call of model. It rereads the file first so concurrent
// sessions only append their own samples, drops the model's samples older than
// Window before the new one, and leaves every other model's samples as they
// are. A sample with no output or no duration says nothing and is ignored.
func Add(path, model string, sample Sample) error {
	if model == "" || sample.Output <= 0 || sample.Seconds <= 0 {
		return nil
	}
	log, err := Load(path)
	if err != nil {
		log = Log{}
	}
	log.add(model, sample)
	return save(path, log)
}

func (l Log) add(model string, sample Sample) {
	cutoff := sample.At.Add(-Window)
	kept := slices.DeleteFunc(slices.Clone(l[model]), func(s Sample) bool { return s.At.Before(cutoff) })
	kept = append(kept, sample)
	if len(kept) > maxSamples {
		kept = kept[len(kept)-maxSamples:]
	}
	l[model] = kept
}

// Stats summarizes a model's recorded calls, false when it has none.
func (l Log) Stats(model string) (Stats, bool) {
	var tokens, seconds float64
	var stats Stats
	for _, s := range l[model] {
		if s.Output <= 0 || s.Seconds <= 0 {
			continue
		}
		tokens += float64(s.Output)
		seconds += s.Seconds
		stats.Calls++
		if s.At.After(stats.LastUsed) {
			stats.LastUsed = s.At
		}
	}
	if stats.Calls == 0 {
		return Stats{}, false
	}
	stats.TokensPerSecond = tokens / seconds
	return stats, true
}

// All summarizes every model with recorded calls.
func (l Log) All() map[string]Stats {
	all := map[string]Stats{}
	for model := range l {
		if stats, ok := l.Stats(model); ok {
			all[model] = stats
		}
	}
	return all
}

func save(path string, log Log) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Models Log `json:"models"`
	}{log})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".throughput-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(name, 0600); err != nil {
		return err
	}
	return os.Rename(name, path)
}
