package ui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
	"julia/internal/images"
)

// attachment is an image pasted into the draft. The draft names it by its
// [Image #N] token; deleting the token drops the image.
type attachment struct {
	number int
	image  core.Image
}

// imagesMsg delivers images read off the main loop. textPaste asks for an
// ordinary text paste instead when the clipboard held no image.
type imagesMsg struct {
	images    []core.Image
	err       error
	textPaste bool
}

var imageTokenPattern = regexp.MustCompile(`\[Image #(\d+)\]`)

func imageToken(number int) string { return fmt.Sprintf("[Image #%d]", number) }

// pasteImage attaches the image on the clipboard. Ctrl+V is also the composer's
// text paste, so fromKey falls back to that when the clipboard holds no image.
func (m *model) pasteImage(fromKey bool) tea.Cmd {
	read, ctx := m.clipboardImage, m.lifetime
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		image, err := read(ctx)
		if errors.Is(err, images.ErrNoImage) {
			return imagesMsg{textPaste: fromKey}
		}
		if err != nil {
			return imagesMsg{err: err}
		}
		return imagesMsg{images: []core.Image{image}}
	}
}

// pasteText handles a bracketed paste. An empty one is what some terminals send
// for Cmd+V when the clipboard holds only an image; a paste of nothing but image
// file paths is what dragging files onto the terminal produces. Anything else is
// text for the composer.
func (m *model) pasteText(content string) (tea.Cmd, bool) {
	if strings.TrimSpace(content) == "" {
		return m.pasteImage(false), true
	}
	paths := images.Paths(content)
	if len(paths) == 0 {
		return nil, false
	}
	return func() tea.Msg {
		loaded := make([]core.Image, 0, len(paths))
		for _, path := range paths {
			image, err := images.Load(path)
			if err != nil {
				return imagesMsg{images: loaded, err: err}
			}
			loaded = append(loaded, image)
		}
		return imagesMsg{images: loaded}
	}, true
}

func (m *model) receiveImages(msg imagesMsg) tea.Cmd {
	for _, image := range msg.images {
		m.imageSeq++
		m.attachments = append(m.attachments, attachment{number: m.imageSeq, image: image})
		m.input.InsertString(imageToken(m.imageSeq) + " ")
	}
	if msg.err != nil {
		m.notice = "Could not attach image: " + msg.err.Error()
	}
	if msg.textPaste {
		return textarea.Paste
	}
	return nil
}

// draftImages are the attachments the draft text still names, in the order it
// first names them.
func (m *model) draftImages(text string) []attachment {
	var named []attachment
	seen := make(map[int]bool)
	for _, match := range imageTokenPattern.FindAllStringSubmatch(text, -1) {
		number, _ := strconv.Atoi(match[1])
		if seen[number] {
			continue
		}
		seen[number] = true
		for _, a := range m.attachments {
			if a.number == number {
				named = append(named, a)
			}
		}
	}
	return named
}

// takeImages turns the draft into a prompt: the images its text names, with the
// tokens renumbered 1, 2, … in that order so they match the images sent. The
// draft's attachments are released either way.
func (m *model) takeImages(text string) core.Prompt {
	named := m.draftImages(text)
	m.attachments, m.imageSeq = nil, 0
	if len(named) == 0 {
		return core.Prompt{Text: text}
	}
	prompt := core.Prompt{Images: make([]core.Image, len(named))}
	renumber := make(map[int]int, len(named))
	for i, a := range named {
		prompt.Images[i] = a.image
		renumber[a.number] = i + 1
	}
	prompt.Text = imageTokenPattern.ReplaceAllStringFunc(text, func(token string) string {
		number, _ := strconv.Atoi(imageTokenPattern.FindStringSubmatch(token)[1])
		if next, ok := renumber[number]; ok {
			return imageToken(next)
		}
		return token
	})
	return prompt
}

// attachmentChips names the draft's images beside the composer title, as many
// as fit in width cells.
func (m *model) attachmentChips(width int) string {
	named := m.draftImages(m.input.Value())
	if len(named) == 0 {
		return ""
	}
	var chips string
	for i, a := range named {
		// The chips share the composer title's row, so they carry its background.
		chip := ink(panel, violet).Render(fmt.Sprintf("  ▣ Image #%d", a.number)) + ink(panel, faint).Render(fmt.Sprintf(" %d×%d", a.image.Width, a.image.Height))
		rest := ""
		if left := len(named) - i - 1; left > 0 {
			rest = ink(panel, muted).Render(fmt.Sprintf("  +%d", left))
		}
		if ansi.StringWidth(chips+chip+rest) > width {
			if chips == "" {
				return ink(panel, violet).Render(fmt.Sprintf("  ▣ %d image%s", len(named), plural(len(named))))
			}
			return chips + ink(panel, muted).Render(fmt.Sprintf("  +%d", len(named)-i))
		}
		chips += chip
	}
	return chips
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// imageRows render a sent message's images, one caption per row.
func imageRows(attached []core.Image) []string {
	rows := make([]string, len(attached))
	for i, image := range attached {
		rows[i] = violetStyle.Render(fmt.Sprintf("▣ Image #%d", i+1)) + "  " + mutedStyle.Render(image.Info().String())
	}
	return rows
}

// imageCaption describes an image a tool returned, as in "▣ PNG · 1440×900 ·
// 312 KB". The same row stands under its thumbnail, in its place in terminals
// without graphics, and for a saved session, whose transcript keeps only this.
func imageCaption(description string) string {
	return violetStyle.Render("▣") + " " + mutedStyle.Render(description)
}

// splitImageMarkers separates a tool result's text from the image markers a
// transcript ends it with, returning each marker's description.
func splitImageMarkers(content string) (string, []string) {
	lines := strings.Split(content, "\n")
	end := len(lines)
	for end > 0 {
		if _, ok := core.ParseImageMarker(lines[end-1]); !ok {
			break
		}
		end--
	}
	var descriptions []string
	for _, line := range lines[end:] {
		description, _ := core.ParseImageMarker(line)
		descriptions = append(descriptions, description)
	}
	return strings.TrimSpace(strings.Join(lines[:end], "\n")), descriptions
}

// highlightImageTokens draws [Image #N] tokens in a line of the user's message
// in the accent color, and the rest in the message color.
func highlightImageTokens(line string) string {
	var out strings.Builder
	last := 0
	for _, span := range imageTokenPattern.FindAllStringIndex(line, -1) {
		out.WriteString(ink(panel, cream).Render(line[last:span[0]]))
		out.WriteString(ink(panel, violet).Render(line[span[0]:span[1]]))
		last = span[1]
	}
	out.WriteString(ink(panel, cream).Render(line[last:]))
	return out.String()
}
