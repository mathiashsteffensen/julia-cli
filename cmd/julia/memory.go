package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"julia/internal/config"
	"julia/internal/memory"
)

const memoryUsage = "usage: julia memory [show NAME | forget NAME | confirm NAME | path | workspaces | consolidate]"

// memoryCommand is `julia memory`: the notes a workspace sees, one shown as
// its file, one forgotten, one confirmed as read and stood behind, and where
// the store and its workspaces are. args
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
	case "show", "forget", "confirm":
		if len(args) != 2 {
			return errors.New(memoryUsage)
		}
		note, err := findNote(store, dir, args[1])
		if err != nil {
			return err
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
	}
	return "", errors.New("use /memory, /memory NAME or /memory forget NAME")
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

// noteCounts is how a note has fared, the nonzero counts only: a note that
// has never been read or judged says nothing rather than three zeros.
func noteCounts(note *memory.Note) string {
	var counts []string
	for _, count := range []struct {
		label string
		n     int
	}{{"read", note.Reads}, {"helped", note.Helped}, {"misled", note.Misled}} {
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
