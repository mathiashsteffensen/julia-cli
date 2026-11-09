package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"julia/internal/memory"
)

func memoryReviewNotes() []*memory.Note {
	workspace := memory.Scope{Kind: memory.ScopeWorkspace, Workspace: "/work"}
	return []*memory.Note{
		{Name: "make-check", Description: "Run make check before reporting done", Type: memory.TypeFeedback, Scope: workspace},
		{Name: "from-page", Description: "Something a page said", Type: memory.TypeReference, Scope: workspace, Tainted: true},
		{Name: "from-thread", Description: "Something a thread said", Type: memory.TypeProject, Scope: memory.Scope{Kind: memory.ScopeGlobal}, Tainted: true},
	}
}

func memoryKey(m *memoryReview, code rune) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code})
	return cmd
}

func TestMemoryReviewConfirmsAndMovesToTheNextUnverifiedNote(t *testing.T) {
	notes := memoryReviewNotes()
	var confirmed []string
	m := newMemoryReview(MemoryReviewConfig{Notes: notes, Confirm: func(note *memory.Note) error {
		confirmed = append(confirmed, note.Name)
		return nil
	}})
	if m.selected != 1 {
		t.Fatalf("opened on note %d, want the first unverified one", m.selected)
	}

	memoryKey(m, 'c')
	if notes[1].Tainted || m.selected != 2 {
		t.Fatalf("confirm left the mark or did not move on: tainted %v, note %d", notes[1].Tainted, m.selected)
	}
	memoryKey(m, 'c')
	if m.selected != 2 || !strings.Contains(m.notice, "No note is unverified now") {
		t.Fatalf("the last confirmation moved or did not say so: note %d, %q", m.selected, m.notice)
	}
	memoryKey(m, 'c')
	if strings.Join(confirmed, " ") != "from-page from-thread" || !strings.Contains(m.notice, "not marked unverified") {
		t.Fatalf("confirmed %v, notice %q", confirmed, m.notice)
	}

	memoryKey(m, tea.KeyRight)
	if m.selected != 0 {
		t.Fatalf("next did not wrap: %d", m.selected)
	}
}

func TestMemoryReviewKeepsTheMarkWhenConfirmFails(t *testing.T) {
	notes := memoryReviewNotes()
	m := newMemoryReview(MemoryReviewConfig{Notes: notes, Confirm: func(*memory.Note) error {
		return errors.New("from-page changed after the review opened")
	}})
	memoryKey(m, 'c')
	if !notes[1].Tainted || m.selected != 1 || !strings.Contains(ansi.Strip(m.View().Content), "Not confirmed: from-page changed") {
		t.Fatalf("failed confirm: tainted %v, note %d, notice %q", notes[1].Tainted, m.selected, m.notice)
	}
}

func TestMemoryReviewForgetsOnlyOnceAsked(t *testing.T) {
	var forgotten []string
	fail := false
	m := newMemoryReview(MemoryReviewConfig{Notes: memoryReviewNotes(), Forget: func(note *memory.Note) error {
		if fail {
			return errors.New("disk full")
		}
		forgotten = append(forgotten, note.Name)
		return nil
	}})

	memoryKey(m, 'f')
	memoryKey(m, tea.KeyRight) // Ignored while the review asks.
	memoryKey(m, 'n')
	if len(forgotten) != 0 || m.forgetting || m.selected != 1 || m.notice != "Kept from-page." {
		t.Fatalf("n forgot or moved: %v, note %d, %q", forgotten, m.selected, m.notice)
	}

	memoryKey(m, 'f')
	memoryKey(m, 'y')
	if strings.Join(forgotten, " ") != "from-page" || len(m.notes) != 2 || m.current().Name != "from-thread" {
		t.Fatalf("forget: %v, %d notes, on %s", forgotten, len(m.notes), m.current().Name)
	}
	if title := ansi.Strip(m.View().Content); !strings.Contains(title, "2 notes · 1 unverified · 1 forgotten") {
		t.Fatalf("title does not count the forgotten note: %s", title)
	}

	fail = true
	memoryKey(m, 'f')
	memoryKey(m, 'y')
	if len(m.notes) != 2 || !strings.Contains(m.notice, "Not forgotten: disk full") {
		t.Fatalf("a failed forget took the note out: %d notes, %q", len(m.notes), m.notice)
	}

	fail = false
	for range 2 {
		memoryKey(m, 'f')
		memoryKey(m, 'y')
	}
	if len(m.notes) != 0 || !strings.Contains(ansi.Strip(m.viewport.View()), "No notes left to review.") {
		t.Fatalf("forgetting every note: %d left, %s", len(m.notes), m.viewport.View())
	}
	for _, key := range []rune{'c', 'f', 'y', tea.KeyLeft, tea.KeyRight} {
		memoryKey(m, key)
	}
	if len(forgotten) != 3 {
		t.Fatalf("keys acted on an empty review: %v", forgotten)
	}
}

func TestMemoryReviewFitsTheTerminal(t *testing.T) {
	notes := memoryReviewNotes()
	notes[1].Body = strings.Repeat("A sentence of detail. ", 20) + strings.Repeat("x", 90)
	notes[1].Path = "/state/julia/memory/workspaces/-private-var-folders-a-very-long-workspace-slug-a2321a4d/from-page.md"
	m := newMemoryReview(MemoryReviewConfig{Notes: notes})
	for _, size := range [][2]int{{1, 1}, {3, 3}, {10, 8}, {20, 12}, {40, 20}, {90, 30}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		lines := strings.Split(m.View().Content, "\n")
		if len(lines) > size[1] {
			t.Errorf("%v: %d lines", size, len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Errorf("%v: line width %d: %q", size, ansi.StringWidth(line), ansi.Strip(line))
			}
		}
	}
}

func TestMemoryReviewCardShowsTheWholeNote(t *testing.T) {
	notes := memoryReviewNotes()
	notes[1].Body = "**Why:** detail-sentinel"
	notes[1].Triggers = memory.Triggers{Paths: []string{"internal/**/*.go"}, Keywords: []string{"deploy"}}
	notes[1].Path = "/state/memory/from-page.md"
	m := newMemoryReview(MemoryReviewConfig{Notes: notes})
	card := m.cardContent()
	for _, want := range []string{"from-page · reference · workspace", "UNVERIFIED: saved in a turn", "LESSON\nSomething a page said", "DETAIL\n**Why:** detail-sentinel", "Paths: internal/**/*.go\nKeywords: deploy", "File: /state/memory/from-page.md"} {
		if !strings.Contains(card, want) {
			t.Errorf("card misses %q:\n%s", want, card)
		}
	}

	m.selected = 0
	card = m.cardContent()
	if strings.Contains(card, "UNVERIFIED") || !strings.Contains(card, "DETAIL\n(none)") || !strings.Contains(card, "(no cues: turns see its index line only)") {
		t.Errorf("verified note without body or cues:\n%s", card)
	}

	if err := RunMemoryReview(context.Background(), MemoryReviewConfig{Notes: notes}); err == nil {
		t.Fatal("ran without Confirm and Forget")
	}
}

func TestMemoryReviewPinsAndUnpins(t *testing.T) {
	notes := memoryReviewNotes()
	var pins []string
	var pinErr error
	m := newMemoryReview(MemoryReviewConfig{Notes: notes, Pin: func(note *memory.Note, pinned bool) error {
		if pinErr != nil {
			return pinErr
		}
		pins = append(pins, fmt.Sprintf("%s %v", note.Name, pinned))
		return nil
	}})

	memoryKey(m, '*')
	if !notes[1].Pinned || notes[1].Tainted || m.selected != 1 || !strings.Contains(m.notice, "It is no longer unverified.") {
		t.Fatalf("pin: pinned %v, tainted %v, note %d, %q", notes[1].Pinned, notes[1].Tainted, m.selected, m.notice)
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "Note 2/3 · workspace · pinned") || !strings.Contains(view, "* unpin") {
		t.Fatalf("the pin does not show:\n%s", view)
	}
	memoryKey(m, '*')
	if notes[1].Pinned || strings.Join(pins, ", ") != "from-page true, from-page false" {
		t.Fatalf("unpin: pinned %v, calls %v", notes[1].Pinned, pins)
	}

	pinErr = errors.New("the workspace scope already has 5 pinned notes, its limit; unpin one first")
	memoryKey(m, '*')
	if notes[1].Pinned || !strings.Contains(m.notice, "Pin unchanged: the workspace scope already has 5 pinned notes") {
		t.Fatalf("a refused pin: pinned %v, %q", notes[1].Pinned, m.notice)
	}
}

func TestMemoryReviewEditsADraft(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var edits []string
	var editErr error
	m := newMemoryReview(MemoryReviewConfig{Notes: memoryReviewNotes(), Edit: func(note *memory.Note, text []byte) (*memory.Note, error) {
		if editErr != nil {
			return nil, editErr
		}
		edits = append(edits, string(text))
		saved := *note
		saved.Description, saved.Source, saved.Tainted = "Checked by hand", memory.SourceUser, false
		return &saved, nil
	}})
	gone := func(path string) bool {
		_, err := os.Stat(path)
		return errors.Is(err, os.ErrNotExist)
	}

	// The draft holds what a person writes, and an edit that changes
	// nothing saves nothing.
	if cmd := memoryKey(m, 'e'); cmd == nil || m.edit == nil {
		t.Fatal("e did not open the editor")
	}
	path := m.edit.path
	draft, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(draft), "description: Something a page said") || strings.Contains(string(draft), "tainted") {
		t.Fatalf("draft: %q %v", draft, err)
	}
	m.Update(noteEdited{})
	if m.edit != nil || len(edits) != 0 || m.notice != "No change to from-page." || !gone(path) {
		t.Fatalf("unchanged draft: edit %v, saves %v, %q", m.edit, edits, m.notice)
	}

	// A failed save keeps the draft and asks; e opens the same draft again.
	memoryKey(m, 'e')
	path = m.edit.path
	if err := os.WriteFile(path, []byte("my words"), 0600); err != nil {
		t.Fatal(err)
	}
	editErr = errors.New("description must be one line of visible text")
	m.Update(noteEdited{})
	if m.edit == nil || gone(path) || !strings.Contains(ansi.Strip(m.View().Content), "Not saved: description must be one line") {
		t.Fatalf("failed save: edit %v, %q", m.edit, m.notice)
	}
	memoryKey(m, tea.KeyRight)
	if cmd := memoryKey(m, 'e'); m.selected != 1 || cmd == nil || m.edit.path != path {
		t.Fatalf("while asking: note %d, draft %v", m.selected, m.edit)
	}

	// A save puts the note as saved on the card and removes the draft.
	editErr = nil
	m.Update(noteEdited{})
	if m.edit != nil || strings.Join(edits, "") != "my words" || m.notes[1].Description != "Checked by hand" || !gone(path) {
		t.Fatalf("save: edit %v, saves %v, note %+v", m.edit, edits, m.notes[1])
	}
	if card := m.cardContent(); !strings.Contains(card, "LESSON\nChecked by hand") || strings.Contains(card, "UNVERIFIED") {
		t.Fatalf("the card does not show the saved note:\n%s", card)
	}

	// An editor that fails drops the edit, n drops a draft that would not
	// save, and so does quitting.
	memoryKey(m, 'e')
	path = m.edit.path
	m.Update(noteEdited{err: errors.New("exit status 1")})
	if m.edit != nil || !gone(path) || !strings.Contains(m.notice, "the editor ended with exit status 1") {
		t.Fatalf("editor error: edit %v, %q", m.edit, m.notice)
	}
	editErr = errors.New("disk full")
	for _, key := range []rune{'n', 'q'} {
		memoryKey(m, 'e')
		path = m.edit.path
		if err := os.WriteFile(path, []byte("more words"), 0600); err != nil {
			t.Fatal(err)
		}
		m.Update(noteEdited{})
		memoryKey(m, key)
		if !gone(path) {
			t.Fatalf("%c left the draft behind", key)
		}
	}
}

func TestEditorCommandPrefersVisualAndTakesArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a note.md")
	t.Setenv("EDITOR", "false")
	t.Setenv("VISUAL", "printf edited >")
	if err := editorCommand(path).Run(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "edited" {
		t.Fatalf("VISUAL did not edit the file: %q %v", data, err)
	}
	t.Setenv("VISUAL", "")
	if err := editorCommand(path).Run(); err == nil {
		t.Fatal("EDITOR was not used when VISUAL is empty")
	}
}
