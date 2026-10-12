package terminal

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// registerQueries answers the terminal queries vt leaves unanswered. Programs
// probe their terminal at startup and some wait for the answer; vt itself
// answers DA1, DA2, DSR, DECRQM and OSC 10/11/12. Handlers run inside
// emu.Write with s.mu held, and those registered later run first.
func registerQueries(s *Session, emu *vt.Emulator) {
	reply := func(format string, args ...any) {
		fmt.Fprintf(emu.InputPipe(), format, args...)
	}

	// XTVERSION: some programs pick features by terminal name.
	emu.RegisterCsiHandler(ansi.Command('>', 0, 'q'), func(params ansi.Params) bool {
		reply("\x1bP>|julia-vt\x1b\\")
		return true
	})

	// XTWINOPS reports: text area in pixels, cell size and size in cells.
	emu.RegisterCsiHandler('t', func(params ansi.Params) bool {
		op, _, _ := params.Param(0, 0)
		switch op {
		case 14:
			reply("\x1b[4;%d;%dt", s.rows*cellHeight, s.cols*cellWidth)
		case 16:
			reply("\x1b[6;%d;%dt", cellHeight, cellWidth)
		case 18:
			reply("\x1b[8;%d;%dt", s.rows, s.cols)
		}
		return true
	})

	// Colour scheme query (Contour, Ghostty, kitty): the background is dark.
	emu.RegisterCsiHandler(ansi.Command('?', 0, 'n'), func(params ansi.Params) bool {
		if n, _, _ := params.Param(0, 0); n == 996 {
			reply("\x1b[?997;1n")
			return true
		}
		return false
	})

	// Titles may contain semicolons, which vt's handler rejects.
	for _, cmd := range []int{0, 1, 2} {
		emu.RegisterOscHandler(cmd, func(data []byte) bool {
			_, title, ok := bytes.Cut(data, []byte{';'})
			if ok && cmd != 1 {
				s.title = cleanTitle(string(title))
			}
			return true
		})
	}

	// OSC 4 queries and sets palette entries; OSC 104 resets them.
	emu.RegisterOscHandler(4, func(data []byte) bool {
		fields := strings.Split(string(data), ";")[1:]
		for i := 0; i+1 < len(fields); i += 2 {
			n, err := strconv.Atoi(fields[i])
			if err != nil || n < 0 || n > 255 {
				continue
			}
			if fields[i+1] == "?" {
				reply("\x1b]4;%d;%s\x1b\\", n, ansi.XRGBColor{Color: emu.IndexedColor(n)})
			} else if c := ansi.XParseColor(fields[i+1]); c != nil {
				emu.SetIndexedColor(n, c)
			}
		}
		return true
	})
	emu.RegisterOscHandler(104, func(data []byte) bool {
		fields := strings.Split(string(data), ";")[1:]
		if len(fields) == 0 || fields[0] == "" {
			fields = nil
			for i := range 256 {
				fields = append(fields, strconv.Itoa(i))
			}
		}
		for _, field := range fields {
			if n, err := strconv.Atoi(field); err == nil && n >= 0 && n < 256 {
				emu.SetIndexedColor(n, defaultPalette(n))
			}
		}
		return true
	})
}

func cleanTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, title)
	if len(title) > maxTitle {
		title = strings.ToValidUTF8(title[:maxTitle], "") + "…"
	}
	return title
}
