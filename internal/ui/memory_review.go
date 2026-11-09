package ui

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"julia/internal/memory"
)

// MemoryReviewConfig is the notes to review, one card each, and what the
// reviewer's decisions do the moment they are made: Confirm clears a note's
// unverified mark, Forget moves its file to the trash, Pin pins or unpins
// it, and Edit saves the text the reviewer wrote for it as their own and
// returns the note as saved.
type MemoryReviewConfig struct {
	Notes           []*memory.Note
	Confirm, Forget func(*memory.Note) error
	Pin             func(note *memory.Note, pinned bool) error
	Edit            func(note *memory.Note, text []byte) (*memory.Note, error)
	Input           io.Reader
	Output          io.Writer
}

// RunMemoryReview runs the standalone review of notes without a chat session.
func RunMemoryReview(ctx context.Context, cfg MemoryReviewConfig) error {
	if cfg.Confirm == nil || cfg.Forget == nil || cfg.Pin == nil || cfg.Edit == nil {
		return errors.New("memory review requires Confirm, Forget, Pin and Edit")
	}
	if err := runCardReview(ctx, newMemoryReview(cfg), cfg.Input, cfg.Output); err != nil {
		return fmt.Errorf("run memory review: %w", err)
	}
	return nil
}

type memoryReview struct {
	cardScreen
	cfg MemoryReviewConfig
	// notes are the ones not forgotten in this review.
	notes     []*memory.Note
	selected  int
	forgotten int
	// forgetting is set while the status line asks whether to forget the
	// selected note.
	forgetting bool
	// edit is set while the editor has a note's draft, and after a failed
	// save, while the status line asks whether to edit the draft again.
	edit *noteEdit
}

// noteEdit is a note being edited as a draft in a private temporary file.
type noteEdit struct {
	note *memory.Note
	path string
	// draft is what the file held before the editor first ran, so an edit
	// that changes nothing saves nothing.
	draft []byte
}

// noteEdited says the editor has exited, with its error.
type noteEdited struct{ err error }

func newMemoryReview(cfg MemoryReviewConfig) *memoryReview {
	m := &memoryReview{cardScreen: newCardScreen(), cfg: cfg, notes: slices.Clone(cfg.Notes)}
	m.wordWrap = true
	m.selectUnverified(-1)
	m.refresh()
	return m
}

func (m *memoryReview) Init() tea.Cmd { return nil }

func (m *memoryReview) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.scrollCard(msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.refresh()
	case noteEdited:
		m.saveEdit(msg.err)
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "q" || key == "esc" || key == "ctrl+c" {
			if m.edit != nil {
				os.Remove(m.edit.path)
			}
			return m, tea.Quit
		}
		if m.edit != nil {
			switch key {
			case "e":
				return m, m.openEditor()
			case "n":
				m.endEdit("Dropped your edit of " + m.edit.note.Name + ".")
			}
			return m, nil
		}
		if m.forgetting {
			switch key {
			case "y":
				m.forgetting = false
				m.forget()
			case "n":
				m.forgetting = false
				m.notice = "Kept " + m.current().Name + "."
			}
			return m, nil
		}
		switch key {
		case "left", "p":
			m.selectRelative(-1)
		case "right", "n":
			m.selectRelative(1)
		case "c":
			m.confirm()
		case "*":
			m.togglePin()
		case "e":
			return m, m.startEdit()
		case "f":
			if note := m.current(); note != nil {
				m.forgetting = true
				m.notice = "Forget " + note.Name + "? y forgets it, n keeps it."
			}
		}
	}
	return m, nil
}

// current returns the selected note, or nil when every note is forgotten.
func (m *memoryReview) current() *memory.Note {
	if m.selected >= len(m.notes) {
		return nil
	}
	return m.notes[m.selected]
}

// selectUnverified moves to the first unverified note after from, wrapping
// round, and leaves the selection alone when no note is unverified.
func (m *memoryReview) selectUnverified(from int) {
	n := len(m.notes)
	for k := 1; k <= n; k++ {
		if i := (from + k) % n; m.notes[i].Tainted {
			m.selected = i
			return
		}
	}
}

func (m *memoryReview) selectRelative(delta int) {
	if n := len(m.notes); n > 0 {
		m.selected = (m.selected + delta + n) % n
	}
	m.notice = ""
	m.refresh()
	m.viewport.GotoTop()
}

// confirm clears the selected note's unverified mark, then moves on to the
// next unverified note.
func (m *memoryReview) confirm() {
	note := m.current()
	if note == nil {
		return
	}
	if !note.Tainted {
		m.notice = note.Name + " is not marked unverified."
		return
	}
	if err := m.cfg.Confirm(note); err != nil {
		m.notice = "Not confirmed: " + err.Error()
		return
	}
	note.Tainted = false

	m.notice = "Confirmed " + note.Name + "."
	if m.unverified() == 0 {
		m.notice += " No note is unverified now."
	}
	m.selectUnverified(m.selected)
	m.refresh()
	m.viewport.GotoTop()
}

// forget moves the selected note to the trash and out of the review, then
// moves on to the next unverified note.
func (m *memoryReview) forget() {
	note := m.current()
	if note == nil {
		return
	}
	if err := m.cfg.Forget(note); err != nil {
		m.notice = "Not forgotten: " + err.Error()
		return
	}
	m.notes = slices.Delete(m.notes, m.selected, m.selected+1)
	m.forgotten++

	m.notice = "Forgot " + note.Name + "; move its file back from .trash to restore it."
	// The note after the forgotten one takes its place.
	if m.selected == len(m.notes) {
		m.selected = 0
	}
	m.selectUnverified(m.selected - 1)
	m.refresh()
	m.viewport.GotoTop()
}

// togglePin pins the selected note, so every turn shows it in full, or
// unpins it. Pinning vouches for the note, so it clears the unverified mark
// as confirming does.
func (m *memoryReview) togglePin() {
	note := m.current()
	if note == nil {
		return
	}
	pinned := !note.Pinned
	if err := m.cfg.Pin(note, pinned); err != nil {
		m.notice = "Pin unchanged: " + err.Error()
		return
	}
	note.Pinned = pinned

	switch {
	case !pinned:
		m.notice = "Unpinned " + note.Name + ": it is one line of the index again."
	case note.Tainted:
		note.Tainted = false
		m.notice = "Pinned " + note.Name + ": every turn shows it in full. It is no longer unverified."
	default:
		m.notice = "Pinned " + note.Name + ": every turn shows it in full."
	}
	m.refresh()
}

// startEdit writes the selected note's draft to a private temporary file and
// opens it in the reviewer's editor.
func (m *memoryReview) startEdit() tea.Cmd {
	note := m.current()
	if note == nil {
		return nil
	}
	draft := note.Draft()
	file, err := os.CreateTemp("", note.Name+"-*.md")
	if err != nil {
		m.notice = "Cannot edit: " + err.Error()
		return nil
	}
	_, err = file.Write(draft)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(file.Name())
		m.notice = "Cannot edit: " + err.Error()
		return nil
	}

	m.edit = &noteEdit{note: note, path: file.Name(), draft: draft}
	return m.openEditor()
}

// openEditor hands the terminal to the editor until it exits with the draft.
func (m *memoryReview) openEditor() tea.Cmd {
	return tea.ExecProcess(editorCommand(m.edit.path), func(err error) tea.Msg { return noteEdited{err} })
}

// editorCommand opens path in $VISUAL, else $EDITOR, else vi. The variable
// may hold arguments, as "code --wait" does.
func editorCommand(path string) *exec.Cmd {
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	return exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
}

// saveEdit saves what the editor left in the draft. An editor that exits
// with an error, as vim's :cq does, drops the edit; a failed save keeps the
// draft and asks whether to edit it again.
func (m *memoryReview) saveEdit(editorErr error) {
	name := m.edit.note.Name
	if editorErr != nil {
		m.endEdit("Dropped your edit of " + name + ": the editor ended with " + editorErr.Error() + ".")
		return
	}
	text, err := os.ReadFile(m.edit.path)
	if err == nil && bytes.Equal(text, m.edit.draft) {
		m.endEdit("No change to " + name + ".")
		return
	}

	var saved *memory.Note
	if err == nil {
		saved, err = m.cfg.Edit(m.edit.note, text)
	}
	if err != nil {
		m.notice = "Not saved: " + err.Error()
		return
	}
	m.notes[slices.Index(m.notes, m.edit.note)] = saved
	m.endEdit("Saved your edit of " + name + ".")
}

// endEdit removes the draft and says how the edit ended.
func (m *memoryReview) endEdit(notice string) {
	os.Remove(m.edit.path)
	m.edit = nil
	m.notice = notice
	m.refresh()
}

func (m *memoryReview) unverified() int {
	count := 0
	for _, note := range m.notes {
		if note.Tainted {
			count++
		}
	}
	return count
}

func (m *memoryReview) refresh() { m.showCard(m.cardContent()) }

func (m *memoryReview) cardContent() string {
	note := m.current()
	if note == nil {
		return "No notes left to review."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · %s\n", note.Name, note.Type, note.Scope)
	if note.Tainted {
		b.WriteString("\nUNVERIFIED: saved in a turn that had read content from outside your own words, such as a web page or a subagent's report. Confirm it only once you stand behind it.\n")
	}

	fmt.Fprintf(&b, "\nLESSON\n%s\n", note.Description)
	b.WriteString("\nDETAIL\n")
	if note.Body == "" {
		b.WriteString("(none)\n")
	} else {
		fmt.Fprintf(&b, "%s\n", note.Body)
	}

	b.WriteString("\nDELIVERED ON\n")
	for _, cue := range []struct {
		label string
		items []string
	}{{"Paths", note.Triggers.Paths}, {"Commands", note.Triggers.Commands}, {"Keywords", note.Triggers.Keywords}} {
		if len(cue.items) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", cue.label, strings.Join(cue.items, ", "))
		}
	}
	switch {
	case note.Pinned:
		b.WriteString("Every turn, in full: the note is pinned.\n")
	case note.Triggers.IsZero():
		b.WriteString("(no cues: turns see its index line only)\n")
	}

	b.WriteString("\nSOURCE\n")
	if note.Source != "" {
		fmt.Fprintf(&b, "Written by: %s\n", note.Source)
	}
	if note.Session != "" {
		fmt.Fprintf(&b, "Session: %s\n", note.Session)
	}
	fmt.Fprintf(&b, "Created: %s\nUpdated: %s\n", note.Created.Format("2006-01-02 15:04 MST"), note.Updated.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "Read: %d times", note.Reads)
	if !note.LastRead.IsZero() {
		fmt.Fprintf(&b, ", last %s", note.LastRead.Format("2006-01-02"))
	}
	fmt.Fprintf(&b, "\nVerdicts: helped %d · misled %d · ignored %d\nFile: %s\n", note.Helped, note.Misled, note.Ignored, note.Path)
	return b.String()
}

func (m *memoryReview) View() tea.View {
	title := fmt.Sprintf("MEMORY REVIEW · %d notes · %d unverified · %d forgotten", len(m.notes), m.unverified(), m.forgotten)
	selection := "No note selected"
	if note := m.current(); note != nil {
		selection = fmt.Sprintf("Note %d/%d · %s", m.selected+1, len(m.notes), note.Scope)
		if note.Tainted {
			selection += " · unverified"
		}
		if note.Pinned {
			selection += " · pinned"
		}
	}
	help := "←/→ p/n note · ↑/↓ pgup/pgdown scroll"
	moreHelp := "c confirm · e edit · * pin · f forget · q/esc quit"
	if note := m.current(); note != nil && note.Pinned {
		moreHelp = "c confirm · e edit · * unpin · f forget · q/esc quit"
	}
	switch {
	case m.edit != nil:
		help = "e edit again · n drop your text · q/esc quit"
		moreHelp = "↑/↓ pgup/pgdown scroll"
	case m.forgetting:
		help = "y forget · n keep · q/esc quit"
		moreHelp = "↑/↓ pgup/pgdown scroll"
	}
	return m.render(title, selection, help, moreHelp)
}
