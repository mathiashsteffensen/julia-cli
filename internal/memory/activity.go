package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"julia/internal/core"
)

// The activity log is how memory is measured. A note's own counters say how
// it fared; they cannot say how often a cue fired, how many turns saw an index
// and used none of it, how often the reflection step ran and what it cost, or
// which saves failed. Each of those is a line of JSON in activity.jsonl at the
// root of the store, appended as it happens and summed by `julia memory
// stats`. It names notes, tools and workspaces, never what a note says.

// Activity kinds.
const (
	// ActivityTurn is a turn that saw the notes: Notes of them, less the
	// Omitted ones its index had no room for, with Relevant the ones its
	// request brought in full.
	ActivityTurn = "turn"
	// ActivityCue is a note delivered in the result of Tool.
	ActivityCue = "cue"
	// ActivityRead is a note the agent read in full with the Memory tool.
	ActivityRead = "read"
	// ActivitySave and ActivityForget are changes to a note, by Source.
	ActivitySave   = "save"
	ActivityForget = "forget"
	// ActivityReflection is one turn's reflection step, as Outcome says.
	ActivityReflection = "reflection"
)

// Reflection outcomes.
const (
	// ReflectionSkipped is a turn that could not have taught anything, so no
	// reflection ran.
	ReflectionSkipped = "skipped"
	// ReflectionChanged and ReflectionUnchanged are a reflection that ran
	// and changed notes, or changed none.
	ReflectionChanged   = "changed"
	ReflectionUnchanged = "unchanged"
	// ReflectionFailed is one that could not run or whose answer was
	// unusable; Error says why.
	ReflectionFailed = "failed"
	// ReflectionCancelled is one the user skipped, or that ran out of time.
	ReflectionCancelled = "cancelled"
)

// SourceConsolidation marks what the consolidation pass did: it rereads and
// rewrites every note, which says nothing about whether a note is used.
const SourceConsolidation = "consolidation"

// Activity is one line of the activity log.
type Activity struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Session   string    `json:"session,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	// Note and Scope name the note a cue, read, save or forget was about.
	Note  string `json:"note,omitempty"`
	Scope string `json:"scope,omitempty"`
	// Source is who saved or forgot: SourceAgent, SourceReflection or
	// SourceConsolidation.
	Source string `json:"source,omitempty"`
	// Tool is the tool whose result carried a cue.
	Tool string `json:"tool,omitempty"`
	// Notes is how many notes a turn saw, Omitted the ones its index had no
	// room for, and Relevant the ones its request brought in full.
	Notes    int      `json:"notes,omitempty"`
	Omitted  []string `json:"omitted,omitempty"`
	Relevant []string `json:"relevant,omitempty"`
	// Outcome, Model and Tokens describe a reflection; Helped, Misled and
	// Ignored are the notes it judged.
	Outcome string           `json:"outcome,omitempty"`
	Model   string           `json:"model,omitempty"`
	Tokens  *core.TokenUsage `json:"tokens,omitempty"`
	Helped  []string         `json:"helped,omitempty"`
	Misled  []string         `json:"misled,omitempty"`
	Ignored []string         `json:"ignored,omitempty"`
	// Error is why a read, save, forget or reflection failed.
	Error string `json:"error,omitempty"`
}

const (
	activityFile = "activity.jsonl"
	// maxActivityBytes bounds the log; past it, the older half is dropped.
	// A busy month of work is well under a megabyte.
	maxActivityBytes = 4 << 20
	maxActivityError = 300
)

// Log appends entries to the activity log, each stamped now unless it has a
// time. The log is a measurement: callers ignore its failure, which must
// never fail the work it records.
func (s *Store) Log(entries ...Activity) error {
	if len(entries) == 0 {
		return nil
	}
	var b bytes.Buffer
	for _, entry := range entries {
		if entry.At.IsZero() {
			entry.At = s.now()
		}
		entry.Error = core.CapText(entry.Error, maxActivityError)
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(s.Directory, 0700); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}
	unlock, err := lock(s.Directory)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(s.Directory, activityFile)
	if info, err := os.Stat(path); err == nil && info.Size()+int64(b.Len()) > maxActivityBytes {
		if err := trimActivity(path); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("log memory activity: %w", err)
	}
	if _, err := f.Write(b.Bytes()); err != nil {
		f.Close()
		return fmt.Errorf("log memory activity: %w", err)
	}
	return f.Close()
}

// trimActivity keeps the newer half of the log, cut at a line.
func trimActivity(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("trim memory activity: %w", err)
	}
	data = data[len(data)/2:]
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		data = data[i+1:]
	}
	return writeFile(path, data)
}

// Activity reads the log from since on, oldest first. A line that is not an
// entry is skipped and counted, so a hand-edited log still reads.
func (s *Store) Activity(since time.Time) ([]Activity, int, error) {
	f, err := os.Open(filepath.Join(s.Directory, activityFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read memory activity: %w", err)
	}
	defer f.Close()
	var out []Activity
	skipped := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var entry Activity
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.Kind == "" {
			skipped++
			continue
		}
		if !entry.At.Before(since) {
			out = append(out, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return out, skipped, fmt.Errorf("read memory activity: %w", err)
	}
	return out, skipped, nil
}
