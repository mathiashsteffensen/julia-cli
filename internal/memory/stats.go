package memory

import (
	"slices"
	"strings"
	"time"

	"julia/internal/core"
)

// Stats sums the activity log: whether memory reaches turns, whether what
// reaches them is used, and what keeping it costs.
type Stats struct {
	// Turns saw the notes, in Sessions; IndexNotes sums the notes their
	// indexes held, and Relevant counts the turns whose request brought a
	// note in full.
	Turns, Sessions, IndexNotes, Relevant int
	// Cues were delivered in CueSessions; Reads are full reads by the agent.
	Cues, CueSessions, Reads int
	// Saves and Forgets count changes by source; Failures count the reads,
	// saves and forgets that failed, by kind.
	Saves, Forgets, Failures map[string]int
	// Reflections count reflection steps by outcome, and ReflectionTokens
	// is what the ones that ran spent.
	Reflections      map[string]int
	ReflectionTokens core.TokenUsage
	// Helped, Misled and Ignored count the reflection's judgments.
	Helped, Misled, Ignored int
	// Notes are every note the log or the store knows, most used first.
	Notes []*NoteStats
}

// NoteStats is how one note fared over the log.
type NoteStats struct {
	Name, Scope string
	// Workspace is the note's workspace; empty for a global note.
	Workspace string
	// Index counts the turns whose index held the note, as a line or, when
	// pinned, in full; Cues and Shown count deliveries beyond it, in a tool
	// result or in full for a request that named it. Sessions is how many
	// sessions a cue, showing or read reached, of the sessions in its
	// workspaces (Of) that saw the notes at all.
	Index, Cues, Shown, Sessions, Of int
	Reads                     int
	Helped, Misled, Ignored   int
	Saves                     int
	// Live says the note is in the store now, Cued that it has cues, and
	// Pinned that the user pinned it; Created is when it was first saved,
	// when known, and Last when it was last delivered or read.
	Live, Cued, Pinned bool
	Created, Last      time.Time
}

// Used is how often the note reached the agent beyond its line in the index.
func (n *NoteStats) Used() int { return n.Cues + n.Shown + n.Reads }

// Noisy reports whether a note's cues fire in most of the sessions that could
// see it: a lesson delivered everywhere is no longer a cue, and the index
// already shows every note's description. A pinned note is meant to be seen
// everywhere.
func (n *NoteStats) Noisy() bool {
	return !n.Pinned && n.Sessions >= 3 && n.Sessions*2 > n.Of
}

// Silent reports whether a note has cues that have never fired: it reached
// the agent only as a line of the index, which its cues were meant to beat.
func (n *NoteStats) Silent() bool {
	return n.Live && n.Cued && !n.Pinned && n.Used() == 0
}

// Summarize sums entries, oldest first as Activity returns them, over the
// notes the store holds now, which joins every live note to the summary even
// when the log never mentions it.
func Summarize(entries []Activity, live []*Note) Stats {
	st := Stats{Saves: map[string]int{}, Forgets: map[string]int{}, Failures: map[string]int{}, Reflections: map[string]int{}}
	notes := map[string]*NoteStats{}
	get := func(workspace, scope, name string) *NoteStats {
		if scope == ScopeGlobal {
			workspace = ""
		}
		key := workspace + "\x00" + name
		if n := notes[key]; n != nil {
			return n
		}
		n := &NoteStats{Name: name, Scope: scope, Workspace: workspace}
		notes[key] = n
		return n
	}
	type visible struct {
		stats *NoteStats
		note  *Note
	}
	var known []visible
	for _, note := range live {
		n := get(note.Scope.Workspace, note.Scope.String(), note.Name)
		n.Live, n.Created, n.Cued, n.Pinned = true, note.Created, !note.Triggers.IsZero(), note.Pinned
		known = append(known, visible{n, note})
	}
	// A reflection names notes as the index does, without a scope: the
	// workspace's note wins over a global one of the same name.
	judged := func(workspace, name string) *NoteStats {
		if n := notes[workspace+"\x00"+name]; n != nil {
			return n
		}
		if n := notes["\x00"+name]; n != nil {
			return n
		}
		return get(workspace, ScopeWorkspace, name)
	}
	sessions := map[string]bool{}
	cueSessions := map[string]bool{}
	workspaceSessions := map[string]map[string]bool{}
	reached := map[*NoteStats]map[string]bool{}
	reach := func(n *NoteStats, entry Activity) {
		if reached[n] == nil {
			reached[n] = map[string]bool{}
		}
		reached[n][entry.Session+"\x00"+entry.Workspace] = true
		if entry.At.After(n.Last) {
			n.Last = entry.At
		}
	}
	for _, entry := range entries {
		switch entry.Kind {
		case ActivityTurn:
			st.Turns++
			st.IndexNotes += entry.Notes
			sessions[entry.Session] = true
			if workspaceSessions[entry.Workspace] == nil {
				workspaceSessions[entry.Workspace] = map[string]bool{}
			}
			workspaceSessions[entry.Workspace][entry.Session] = true
			if len(entry.Relevant) > 0 {
				st.Relevant++
			}
			// Every note the turn could see was in its index unless it had
			// no room: the store as it is now stands in for the store then,
			// less the notes saved after the turn.
			for _, v := range known {
				if (v.stats.Workspace == "" || v.stats.Workspace == entry.Workspace) && !v.note.Created.After(entry.At) && !slices.Contains(entry.Omitted, v.note.Name) {
					v.stats.Index++
				}
			}
			for _, name := range entry.Relevant {
				n := judged(entry.Workspace, name)
				n.Shown++
				reach(n, entry)
			}
		case ActivityCue:
			st.Cues++
			cueSessions[entry.Session] = true
			n := get(entry.Workspace, entry.Scope, entry.Note)
			n.Cues++
			reach(n, entry)
		case ActivityRead:
			if entry.Error != "" {
				st.Failures[entry.Kind]++
				continue
			}
			if entry.Source == SourceConsolidation {
				continue
			}
			st.Reads++
			n := get(entry.Workspace, entry.Scope, entry.Note)
			n.Reads++
			reach(n, entry)
		case ActivitySave, ActivityForget:
			if entry.Error != "" {
				st.Failures[entry.Kind]++
				continue
			}
			n := get(entry.Workspace, entry.Scope, entry.Note)
			if entry.Kind == ActivitySave {
				st.Saves[entry.Source]++
				n.Saves++
				if n.Created.IsZero() {
					n.Created = entry.At
				}
			} else {
				st.Forgets[entry.Source]++
			}
		case ActivityReflection:
			st.Reflections[entry.Outcome]++
			if entry.Tokens != nil {
				st.ReflectionTokens.Add(*entry.Tokens)
			}
			for _, verdict := range []struct {
				names []string
				count *int
				note  func(*NoteStats) *int
			}{
				{entry.Helped, &st.Helped, func(n *NoteStats) *int { return &n.Helped }},
				{entry.Misled, &st.Misled, func(n *NoteStats) *int { return &n.Misled }},
				{entry.Ignored, &st.Ignored, func(n *NoteStats) *int { return &n.Ignored }},
			} {
				for _, name := range verdict.names {
					*verdict.count++
					*verdict.note(judged(entry.Workspace, name))++
				}
			}
		}
	}
	st.Sessions, st.CueSessions = len(sessions), len(cueSessions)
	for n, keys := range reached {
		n.Sessions = len(keys)
	}
	for _, n := range notes {
		if n.Workspace != "" {
			n.Of = len(workspaceSessions[n.Workspace])
			continue
		}
		for _, sessions := range workspaceSessions {
			n.Of += len(sessions)
		}
	}
	for _, n := range notes {
		st.Notes = append(st.Notes, n)
	}
	slices.SortFunc(st.Notes, func(a, b *NoteStats) int {
		if a.Used() != b.Used() {
			return b.Used() - a.Used()
		}
		if a.Workspace != b.Workspace {
			return strings.Compare(a.Workspace, b.Workspace)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return st
}
