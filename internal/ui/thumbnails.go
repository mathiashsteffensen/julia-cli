package ui

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"julia/internal/core"
	"julia/internal/images"
)

// Sent images are drawn inline as Kitty graphics, the same way as the wordmark:
// each is uploaded once as a virtual placement of a fixed cell size, and the
// conversation draws it with Unicode placeholder cells. Those cells are plain
// text, so a thumbnail scrolls, clips and caches like any other row.
const (
	thumbnailRows    = 8  // the height of a thumbnail, in rows, unless it is very wide
	thumbnailMaxCols = 48 // the widest a thumbnail is drawn, in columns
	thumbnailGap     = 2  // columns between thumbnails side by side
	// thumbnailBaseID numbers thumbnails apart from the wordmark. A placeholder's
	// foreground color carries its image ID, so IDs must fit in 24 bits.
	thumbnailBaseID = 0x100000
	// thumbnailMaxPixels bounds an uploaded thumbnail's longest side.
	thumbnailMaxPixels = 1600
)

// Until the terminal reports its cell size, assume a common one: twice as
// tall as wide.
const defaultCellWidth, defaultCellHeight = 9, 18

// graphicsAvailable reports whether thumbnails can be uploaded now: the
// terminal draws Kitty graphics and no screen clear is about to drop them.
func (m *model) graphicsAvailable() bool {
	return m.wordmarkEnabled && m.wordmarkTrueColor && !m.wordmarkPending && !m.quitting
}

// thumbnailsShown reports whether sent images are drawn, not just captioned:
// the terminal has accepted an upload, so the placeholders will draw.
func (m *model) thumbnailsShown() bool {
	return m.wordmarkEnabled && m.wordmarkTrueColor && m.graphicsConfirmed
}

// registerImages gives each of an entry's images a Kitty image ID.
func (m *model) registerImages(item *entry) {
	item.imageIDs = make([]int, len(item.images))
	for i, img := range item.images {
		if m.thumbnails == nil {
			m.thumbnails = make(map[int]core.Image)
		}
		id := thumbnailBaseID + len(m.thumbnailOrder)
		m.thumbnails[id] = img
		m.thumbnailOrder = append(m.thumbnailOrder, id)
		item.imageIDs[i] = id
	}
}

func (m *model) cellSize() (width, height int) {
	if m.cellWidth > 0 && m.cellHeight > 0 {
		return m.cellWidth, m.cellHeight
	}
	return defaultCellWidth, defaultCellHeight
}

// thumbnailSize is the cells a thumbnail of img fills: thumbnailRows tall
// with img's aspect ratio, narrowed (and so shortened) to fit maxCols.
func (m *model) thumbnailSize(img core.Image, maxCols int) (cols, rows int) {
	cellWidth, cellHeight := m.cellSize()
	width, height := max(1, img.Width), max(1, img.Height)
	maxCols = max(2, min(maxCols, thumbnailMaxCols))
	rows = thumbnailRows
	cols = (rows*cellHeight*width + height*cellWidth/2) / (height * cellWidth)
	if cols > maxCols {
		cols = maxCols
		rows = max(1, (cols*cellWidth*height+width*cellHeight/2)/(width*cellHeight))
	}
	return max(cols, 2), rows
}

// thumbnailColumns is the room a user message has for its thumbnails.
func (m *model) thumbnailColumns() int { return max(10, m.mainWidth-4) }

// uploadThumbnails sends the given images, or every registered one when ids is
// empty, as virtual placements sized for the current layout. Decoding and
// encoding run off the main loop.
func (m *model) uploadThumbnails(ids ...int) tea.Cmd {
	if !m.graphicsAvailable() || len(m.thumbnails) == 0 {
		return nil
	}
	if len(ids) == 0 {
		ids = m.thumbnailOrder
	}
	type job struct {
		id, cols, rows int
		img            core.Image
	}
	cellWidth, cellHeight := m.cellSize()
	jobs := make([]job, 0, len(ids))
	for _, id := range ids {
		if img, ok := m.thumbnails[id]; ok {
			cols, rows := m.thumbnailSize(img, m.thumbnailColumns())
			jobs = append(jobs, job{id, cols, rows, img})
		}
	}
	return func() tea.Msg {
		var out bytes.Buffer
		for _, j := range jobs {
			pixels, err := images.Thumbnail(j.img, min(j.cols*cellWidth, thumbnailMaxPixels), min(j.rows*cellHeight, thumbnailMaxPixels))
			if err != nil {
				continue // the caption under it still names the image
			}
			_ = kitty.EncodeGraphics(&out, pixels, &kitty.Options{
				Action: kitty.TransmitAndPut, Transmission: kitty.Direct, Format: kitty.PNG,
				ID: j.id, Columns: j.cols, Rows: j.rows,
				VirtualPlacement: true, DoNotMoveCursor: true, Chunk: true, Quiet: 2,
			})
		}
		if out.Len() == 0 {
			return nil
		}
		return tea.RawMsg{Msg: out.String()}
	}
}

// deleteThumbnails frees every uploaded thumbnail in the terminal and forgets
// them, for a conversation that no longer shows them.
func (m *model) deleteThumbnails() tea.Cmd {
	deletes := thumbnailDeletes(m.thumbnailOrder)
	m.thumbnails, m.thumbnailOrder = nil, nil
	if deletes == "" || !m.wordmarkEnabled {
		return nil
	}
	return tea.Raw(deletes)
}

func thumbnailDeletes(ids []int) string {
	var out strings.Builder
	for _, id := range ids {
		out.WriteString(ansi.KittyGraphics(nil, "a=d", "d=I", "i="+strconv.Itoa(id), "q=2"))
	}
	return out.String()
}

// requestCellSize asks the terminal for its cell size in pixels, which it
// answers with a uv.CellSizeEvent.
func (m *model) requestCellSize() tea.Cmd {
	if !m.wordmarkEnabled || !m.wordmarkTrueColor {
		return nil
	}
	return tea.Raw(ansi.WindowOp(ansi.RequestCellSizeWinOp))
}

// receiveCellSize relays out thumbnails for the terminal's real cell size and
// uploads them again at that size.
func (m *model) receiveCellSize(size uv.CellSizeEvent) tea.Cmd {
	if size.Width <= 0 || size.Height <= 0 || size.Width == m.cellWidth && size.Height == m.cellHeight {
		return nil
	}
	m.cellWidth, m.cellHeight = size.Width, size.Height
	m.graphicsEpoch++
	m.refresh(false)
	return m.uploadThumbnails()
}

// placeholderRow draws one row of image id's placement, cols cells wide. The
// foreground color is the image ID, not a visible color; it stays explicit so
// Lip Gloss preserves it through padding and joins.
func placeholderRow(id, row, cols int) string {
	var out strings.Builder
	fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm", id>>16&0xff, id>>8&0xff, id&0xff)
	for col := 0; col < cols; col++ {
		out.WriteRune(kitty.Placeholder)
		out.WriteRune(kitty.Diacritic(row))
		out.WriteRune(kitty.Diacritic(col))
	}
	out.WriteString("\x1b[39m")
	return out.String()
}

// thumbnailRowsFor lays out an entry's images as thumbnails side by side, each
// captioned, wrapping onto further bands when they do not fit in width.
// Thumbnails in a band share a baseline so captions align.
func (m *model) thumbnailRowsFor(item entry, width int) []string {
	type block struct {
		id, cols, rows, width int
		caption               string
	}
	var bands [][]block
	used := 0
	for i, img := range item.images {
		cols, rows := m.thumbnailSize(img, width)
		caption := thumbnailCaption(item, i, cols)
		b := block{id: item.imageIDs[i], cols: cols, rows: rows, width: max(cols, ansi.StringWidth(caption)), caption: caption}
		if len(bands) == 0 || used+thumbnailGap+b.width > width {
			bands = append(bands, nil)
			used = -thumbnailGap
		}
		bands[len(bands)-1] = append(bands[len(bands)-1], b)
		used += thumbnailGap + b.width
	}
	var lines []string
	for n, band := range bands {
		if n > 0 {
			lines = append(lines, "")
		}
		height := 0
		for _, b := range band {
			height = max(height, b.rows)
		}
		for row := 0; row <= height; row++ {
			var line strings.Builder
			for i, b := range band {
				if i > 0 {
					line.WriteString(strings.Repeat(" ", thumbnailGap))
				}
				cell := b.caption
				if offset := height - b.rows; row < height {
					cell = ""
					if row >= offset {
						cell = placeholderRow(b.id, row-offset, b.cols)
					}
				}
				line.WriteString(cell + strings.Repeat(" ", max(0, b.width-ansi.StringWidth(cell))))
			}
			lines = append(lines, strings.TrimRight(line.String(), " "))
		}
	}
	return lines
}

// thumbnailCaption is the line under a thumbnail cols wide. A user's image is
// named by its [Image #N] token, with its size when that fits under it; a
// tool's is described as the model received it, as its caption is without one.
func thumbnailCaption(item entry, i, cols int) string {
	img := item.images[i]
	if item.role == "tool" {
		return imageCaption(img.Info().String())
	}
	label := violetStyle.Render(fmt.Sprintf("▣ Image #%d", i+1))
	if size := fmt.Sprintf("%d×%d", img.Width, img.Height); ansi.StringWidth(label)+2+ansi.StringWidth(size) <= max(cols, ansi.StringWidth(label)) {
		label += "  " + faintStyle.Render(size)
	}
	return label
}
