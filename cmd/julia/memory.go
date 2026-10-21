package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"julia/internal/config"
	"julia/internal/core"
	"julia/internal/memory"
)

const memoryUsage = "usage: julia memory [show NAME | forget NAME | confirm NAME | pin NAME | unpin NAME | stats [-days N] [-all] | path | workspaces | consolidate]"

// memoryCommand is `julia memory`: the notes a workspace sees, one shown as
// its file, one forgotten, one confirmed as read and stood behind, one pinned
// for every task it bears on or unpinned, and where
// the store and its workspaces are, and how memory has been used. args
// are the words after memory and dir is the workspace, absolute and with
// symlinks resolved, so it names the same store directory a session in it
// would. Consolidation is the agent's work, run with the Memory tool, so main
// intercepts that word before it reaches here; the case below only says so
// when it has not.
func memoryCommand(args []string, dir string, out io.Writer) error {
	store, err := memory.Default()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return listMemory(store, dir, out)
	}
	switch args[0] {
	case "show", "forget", "confirm", "pin", "unpin":
		if len(args) != 2 {
			return errors.New(memoryUsage)
		}
		note, err := findNote(store, dir, args[1])
		if err != nil {
			return err
		}
		if args[0] == "pin" || args[0] == "unpin" {
			text, err := pinNote(store, note, args[0] == "pin")
			if err != nil {
				return err
			}
			fmt.Fprintln(out, safe(text))
			return nil
		}
		if args[0] == "confirm" {
			if !note.Tainted {
				fmt.Fprintf(out, "The %s note %s is not marked unverified.\n", note.Scope, note.Name)
				return nil
			}
			if err := store.Confirm(note.Scope, note.Name); err != nil {
				return err
			}
			fmt.Fprintf(out, "Confirmed %s note %s: %s\n", note.Scope, note.Name, safe(note.Description))
			return nil
		}
		if args[0] == "show" {
			data, err := os.ReadFile(note.Path)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, note.Path)
			fmt.Fprint(out, safe(string(data)))
			return nil
		}
		if err := store.Forget(note.Scope, note.Name); err != nil {
			return err
		}
		fmt.Fprintf(out, "Forgot %s note %s: %s\n", note.Scope, note.Name, safe(note.Description))
		return nil
	case "stats":
		return memoryStats(store, dir, args[1:], time.Now(), out)
	case "path":
		if len(args) != 1 {
			return errors.New(memoryUsage)
		}
		fmt.Fprintln(out, store.Directory)
		return nil
	case "workspaces":
		if len(args) != 1 {
			return errors.New(memoryUsage)
		}
		workspaces, err := store.Workspaces()
		if err != nil {
			return err
		}
		for _, workspace := range workspaces {
			fmt.Fprintln(out, safe(workspace))
		}
		if len(workspaces) == 0 {
			fmt.Fprintln(out, "No workspace has notes yet.")
		}
		return nil
	}
	return errors.New(memoryUsage)
}

// markdownText escapes what Markdown would read as markup, so a lesson that
// names __init__.py or internal/**/*.go shows as written.
func markdownText(text string) string {
	return markdownEscaper.Replace(text)
}

var markdownEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", `\<`, ">", `\>`, "#", `\#`)

// memoryMarkdown answers the interface's /memory command with Markdown: the
// notes a session in dir sees, one note in full, or the forgetting of one. A
// run that reads notes only says so instead of forgetting.
func memoryMarkdown(store *memory.Store, dir, action, name string, readOnly bool) (string, error) {
	switch action {
	case "":
		recall, err := store.Recall(dir)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, section := range []struct {
			title string
			notes []*memory.Note
		}{{"This workspace", recall.Workspace}, {"Every workspace", recall.Global}} {
			fmt.Fprintf(&b, "**%s**\n\n", section.title)
			if len(section.notes) == 0 {
				b.WriteString("No notes yet.\n\n")
				continue
			}
			for _, note := range section.notes {
				fmt.Fprintf(&b, "- **%s** · %s · %s", note.Name, note.Type, note.Updated.Format("2006-01-02"))
				if counts := noteCounts(note); counts != "" {
					b.WriteString(" · " + counts)
				}
				if note.Pinned {
					b.WriteString(" · pinned")
				}
				if note.Tainted {
					b.WriteString(" · unverified")
				}
				b.WriteString(" — " + markdownText(note.Description) + "\n")
			}
			b.WriteString("\n")
		}
		for _, warning := range recall.Warnings {
			b.WriteString("> " + markdownText(warning) + "\n\n")
		}
		return strings.TrimRight(b.String(), "\n") + "\n", nil
	case "show":
		note, err := findNote(store, dir, name)
		if err != nil {
			return "", err
		}
		header := fmt.Sprintf("**%s** · %s · %s · updated %s", note.Name, note.Type, note.Scope, note.Updated.Format("2006-01-02"))
		if note.Source != "" {
			header += " · by " + note.Source
		}
		if counts := noteCounts(note); counts != "" {
			header += " · " + counts
		}
		if note.Pinned {
			header += " · pinned"
		}
		if note.Tainted {
			header += " · unverified"
		}
		text := header + "\n\n" + markdownText(note.Description) + "\n"
		if note.Body != "" {
			text += "\n" + note.Body + "\n"
		}
		var cues []string
		for _, cue := range []struct {
			label string
			items []string
		}{{"paths", note.Triggers.Paths}, {"commands", note.Triggers.Commands}, {"keywords", note.Triggers.Keywords}} {
			if len(cue.items) > 0 {
				cues = append(cues, cue.label+" "+strings.Join(cue.items, ", "))
			}
		}
		if len(cues) > 0 {
			text += "\nDelivered on " + strings.Join(cues, " · ") + "\n"
		}
		return text + "\n_" + note.Path + "_\n", nil
	case "forget":
		if readOnly {
			return "", errors.New("notes are read-only in this run; use julia memory forget " + name)
		}
		note, err := findNote(store, dir, name)
		if err != nil {
			return "", err
		}
		if err := store.Forget(note.Scope, note.Name); err != nil {
			return "", err
		}
		return fmt.Sprintf("Forgot %s note **%s**: %s\n", note.Scope, note.Name, markdownText(note.Description)), nil
	case "pin", "unpin":
		if readOnly {
			return "", errors.New("notes are read-only in this run; use julia memory " + action + " " + name)
		}
		note, err := findNote(store, dir, name)
		if err != nil {
			return "", err
		}
		text, err := pinNote(store, note, action == "pin")
		if err != nil {
			return "", err
		}
		return markdownText(text) + "\n", nil
	}
	return "", errors.New("use /memory, /memory NAME, /memory forget NAME, /memory pin NAME or /memory unpin NAME")
}

// listMemory prints the notes a session in dir sees, as the index orders
// them, with the counts that say whether each earns its place, then where the
// store is and every file that could not be read.
func listMemory(store *memory.Store, dir string, out io.Writer) error {
	recall, err := store.Recall(dir)
	if err != nil {
		return err
	}
	nameWidth, typeWidth := 0, 0
	for _, note := range append(append([]*memory.Note(nil), recall.Workspace...), recall.Global...) {
		nameWidth = max(nameWidth, len(note.Name))
	}
	for _, kind := range memory.Types {
		typeWidth = max(typeWidth, len(kind))
	}
	for _, section := range []struct {
		title string
		notes []*memory.Note
	}{{"This workspace (" + dir + ")", recall.Workspace}, {"Every workspace", recall.Global}} {
		fmt.Fprintln(out, section.title)
		if len(section.notes) == 0 {
			fmt.Fprintln(out, "  No notes yet.")
			continue
		}
		for _, note := range section.notes {
			// The lessons align; how a note has fared trails it.
			line := fmt.Sprintf("  %-*s  %-*s  %s  %s", nameWidth, note.Name, typeWidth, note.Type, note.Updated.Format("2006-01-02"), safe(note.Description))
			if counts := noteCounts(note); counts != "" {
				line += "  · " + counts
			}
			if note.Pinned {
				line += "  · pinned"
			}
			if note.Tainted {
				line += "  · unverified"
			}
			fmt.Fprintln(out, line)
		}
	}
	fmt.Fprintln(out, "Store: "+store.Directory)
	for _, warning := range recall.Warnings {
		fmt.Fprintln(out, safe(warning))
	}
	return nil
}

// pinNote pins a note for every task it bears on, or unpins it, and says
// what changed. Pinning vouches for the note, so it clears the unverified
// mark too.
func pinNote(store *memory.Store, note *memory.Note, pinned bool) (string, error) {
	state := map[bool]string{true: "pinned", false: "unpinned"}[pinned]
	if note.Pinned == pinned {
		return fmt.Sprintf("The %s note %s is already %s.", note.Scope, note.Name, state), nil
	}
	if err := store.Pin(note.Scope, note.Name, pinned); err != nil {
		return "", err
	}
	if !pinned {
		return fmt.Sprintf("Unpinned %s note %s: it is one line of the index again.", note.Scope, note.Name), nil
	}
	text := fmt.Sprintf("Pinned %s note %s; every turn shows it in full: %s", note.Scope, note.Name, note.Description)
	if note.Tainted {
		text += " It is no longer marked unverified."
	}
	return text, nil
}

// noteCounts is how a note has fared, the nonzero counts only: a note that
// has never been read or judged says nothing rather than three zeros.
func noteCounts(note *memory.Note) string {
	var counts []string
	for _, count := range []struct {
		label string
		n     int
	}{{"read", note.Reads}, {"helped", note.Helped}, {"misled", note.Misled}, {"ignored", note.Ignored}} {
		if count.n > 0 {
			counts = append(counts, fmt.Sprintf("%s %d", count.label, count.n))
		}
	}
	return strings.Join(counts, " · ")
}

// findNote resolves the NAME a show or forget gives: workspace/NAME or
// global/NAME picks a scope, and a bare name is looked up in the workspace,
// then globally, as the Memory tool looks it up for the agent.
func findNote(store *memory.Store, dir, arg string) (*memory.Note, error) {
	workspace := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: dir}
	global := memory.Scope{Kind: memory.ScopeGlobal}
	if kind, name, ok := strings.Cut(arg, "/"); ok {
		var scope memory.Scope
		switch kind {
		case memory.ScopeWorkspace:
			scope = workspace
		case memory.ScopeGlobal:
			scope = global
		default:
			return nil, fmt.Errorf("scope %q must be %s or %s", kind, memory.ScopeWorkspace, memory.ScopeGlobal)
		}
		note, err := store.Load(scope, name)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no %s note is named %s", scope, name)
		}
		return note, err
	}
	for _, scope := range []memory.Scope{workspace, global} {
		note, err := store.Load(scope, arg)
		if err == nil {
			return note, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no workspace or global note is named %s", arg)
}

// memorySummary is the doctor's lines about memory: where the store is, what
// a session in dir sees, and whether a reflection step reviews finished turns
// and on which model, then one line per note file that could not be read.
func memorySummary(c config.Config, dir string) []string {
	store, err := memory.Default()
	if err != nil {
		return []string{"Memory    " + safe(err.Error())}
	}
	recall, err := store.Recall(dir)
	if err != nil {
		return []string{"Memory    " + store.Directory + " · " + safe(err.Error())}
	}
	reflection := "reflection off"
	if c.ReflectionEnabled() {
		reflection = "reflection on"
		if c.Memory.Model != nil {
			reflection += " " + safe(c.Memory.Model.ID)
		}
		// An effort is shown only where JULIA sends one.
		if settings := reflectionSettings(c, c); settings.SupportsReasoning(settings.Provider) {
			reflection += " · reasoning " + safe(settings.Reasoning)
		}
	}
	lines := []string{"Memory    " + store.Directory + " · " + recall.Summary() + " · " + reflection}
	for _, warning := range recall.Warnings {
		lines = append(lines, "Memory    "+safe(warning))
	}
	return lines
}

// silentAfter is how long a note's cues may go without firing before stats
// says so: younger notes have not had their chance yet.
const silentAfter = 7 * 24 * time.Hour

// memoryStats prints how memory was used over the last days, from the
// activity log: whether notes reach turns, whether the agent uses what
// reaches it, what the reflection step does and costs, then how each note
// fared. It covers the notes a session in dir sees unless -all is given.
func memoryStats(store *memory.Store, dir string, args []string, now time.Time, out io.Writer) error {
	flags := flag.NewFlagSet("julia memory stats", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	days := flags.Int("days", 30, "how many days back to count; 0 counts the whole log")
	all := flags.Bool("all", false, "count every workspace, not only this one")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 || *days < 0 {
		return errors.New("usage: julia memory stats [-days N] [-all]")
	}
	var since time.Time
	if *days > 0 {
		since = now.Add(-time.Duration(*days) * 24 * time.Hour)
	}
	entries, skipped, err := store.Activity(since)
	if err != nil {
		return err
	}
	var live []*memory.Note
	workspaces := []string{dir}
	if *all {
		if workspaces, err = store.Workspaces(); err != nil {
			return err
		}
	}
	for _, workspace := range workspaces {
		notes, _, err := store.List(memory.Scope{Kind: memory.ScopeWorkspace, Workspace: workspace})
		if err != nil {
			return err
		}
		live = append(live, notes...)
	}
	global, _, err := store.List(memory.Scope{Kind: memory.ScopeGlobal})
	if err != nil {
		return err
	}
	live = append(live, global...)
	if !*all {
		kept := entries[:0]
		for _, entry := range entries {
			if entry.Workspace == dir {
				kept = append(kept, entry)
			}
		}
		entries = kept
	}
	st := memory.Summarize(entries, live)

	where := "this workspace (" + safe(dir) + ")"
	if *all {
		where = "every workspace"
	}
	window := "the whole log"
	if *days > 0 {
		window = fmt.Sprintf("the last %d %s", *days, plural(*days, "day", "days"))
	}
	fmt.Fprintf(out, "Memory over %s · %s\n", window, where)
	if len(entries) == 0 {
		fmt.Fprintln(out, "  Nothing recorded yet: turns record what memory did as they run.")
		return nil
	}
	row := func(label, text string) { fmt.Fprintf(out, "  %-11s %s\n", label, text) }
	turns := fmt.Sprintf("%d in %d %s", st.Turns, st.Sessions, plural(st.Sessions, "session", "sessions"))
	if st.Turns > 0 {
		turns += fmt.Sprintf(" · %.1f notes in the index on average · %d brought a note in full", float64(st.IndexNotes)/float64(st.Turns), st.Relevant)
	}
	row("Turns", turns)
	row("Cues", fmt.Sprintf("%d in %d of %d sessions", st.Cues, st.CueSessions, st.Sessions))
	row("Reads", fmt.Sprint(st.Reads))
	row("Saves", bySource(st.Saves))
	row("Forgets", bySource(st.Forgets))
	if len(st.Failures) > 0 {
		var failed []string
		for _, kind := range []string{memory.ActivitySave, memory.ActivityForget, memory.ActivityRead} {
			if n := st.Failures[kind]; n > 0 {
				failed = append(failed, fmt.Sprintf("%d %s", n, plural(n, kind, kind+"s")))
			}
		}
		row("Failed", strings.Join(failed, " · "))
	}
	ran := st.Reflections[memory.ReflectionChanged] + st.Reflections[memory.ReflectionUnchanged]
	reflection := fmt.Sprintf("%d ran", ran)
	for _, outcome := range []struct{ key, label string }{
		{memory.ReflectionChanged, "changed notes"},
		{memory.ReflectionUnchanged, "changed nothing"},
		{memory.ReflectionFailed, "failed"},
		{memory.ReflectionCancelled, "cancelled"},
		{memory.ReflectionSkipped, "turns skipped"},
	} {
		if n := st.Reflections[outcome.key]; n > 0 {
			if n == 1 {
				outcome.label = strings.Replace(outcome.label, "turns", "turn", 1)
			}
			reflection += fmt.Sprintf(" · %d %s", n, outcome.label)
		}
	}
	if tokens := st.ReflectionTokens; !tokens.IsZero() {
		reflection += fmt.Sprintf(" · %s tokens in, %s out", core.CompactTokens(tokens.Prompt()), core.CompactTokens(tokens.Output))
	}
	row("Reflection", reflection)
	row("Verdicts", fmt.Sprintf("%d helped · %d misled · %d ignored", st.Helped, st.Misled, st.Ignored))
	if skipped > 0 {
		fmt.Fprintf(out, "  %d %s of the log could not be read\n", skipped, plural(skipped, "line", "lines"))
	}

	var notes []*memory.NoteStats
	for _, n := range st.Notes {
		if n.Live || n.Used() > 0 {
			notes = append(notes, n)
		}
	}
	if len(notes) == 0 {
		return nil
	}
	label := func(n *memory.NoteStats) string {
		switch {
		case n.Workspace == "":
			return "global/" + n.Name
		case *all:
			return filepath.Base(n.Workspace) + "/" + n.Name
		}
		return n.Name
	}
	width := len("Note")
	for _, n := range notes {
		width = max(width, len(safe(label(n))))
	}
	// The index column is every turn the note was in the instructions; the
	// columns after it are what reached the agent beyond that line.
	fmt.Fprintf(out, "\n  %-*s  %5s  %5s  %5s  %5s  %6s  %6s  %7s  %s\n", width, "Note", "index", "cues", "shown", "reads", "helped", "misled", "ignored", "sessions")
	for _, n := range notes {
		line := fmt.Sprintf("  %-*s  %5d  %5d  %5d  %5d  %6d  %6d  %7d  %d of %d", width, safe(label(n)), n.Index, n.Cues, n.Shown, n.Reads, n.Helped, n.Misled, n.Ignored, n.Sessions, n.Of)
		var marks []string
		switch {
		case !n.Live:
			marks = append(marks, "gone")
		case n.Pinned:
			marks = append(marks, "pinned")
		case !n.Cued:
			marks = append(marks, "index only")
		case n.Silent() && !n.Created.IsZero() && now.Sub(n.Created) > silentAfter:
			marks = append(marks, "cues never fired")
		}
		if n.Noisy() {
			marks = append(marks, "noisy: its cues fire in most sessions; narrow them, or pin it if it bears on most work")
		}
		if n.Ignored > 0 && n.Ignored >= n.Helped {
			if n.Pinned {
				marks = append(marks, "often ignored though pinned: sharpen its description")
			} else {
				marks = append(marks, "often ignored: pin it if it bears on most work, else sharpen its description or cues")
			}
		}
		if len(marks) > 0 {
			line += "  · " + strings.Join(marks, " · ")
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

// bySource is a count of changes by who made them, the agent's first.
func bySource(counts map[string]int) string {
	var parts []string
	for _, source := range []struct{ key, label string }{
		{memory.SourceAgent, "by the agent"},
		{memory.SourceReflection, "by reflection"},
		{memory.SourceConsolidation, "by consolidation"},
	} {
		if n := counts[source.key]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, source.label))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, " · ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
