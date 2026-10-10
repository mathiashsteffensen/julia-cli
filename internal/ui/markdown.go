package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/x/ansi"
)

// Code blocks are drawn by JARVIS rather than Glamour: a filled card with a
// language label, highlighting in the house palette, and wrapped lines marked
// as continuations so a copied command stays one command.
var (
	codeBackground   = lipgloss.Color("#1D1C22")
	codeHeaderColor  = lipgloss.Color("#26232D")
	codeCommentColor = lipgloss.Color("#7D7789")
	codeGold         = lipgloss.Color("#E9C891")
	codeBlue         = lipgloss.Color("#A6C8E8")
)

// codeGutter is the left padding inside a card; continuation rows draw ↳ in it.
const codeGutter = 2

type markdownSegment struct {
	code     bool
	language string
	text     string
}

// splitFences separates fenced code blocks from the prose around them. A fence
// may be indented (models nest them in lists); its indent is removed from the
// code. An unclosed fence runs to the end, so a streaming block is a card at once.
func splitFences(text string) []markdownSegment {
	var segments []markdownSegment
	var prose []string
	flush := func() {
		if joined := strings.Join(prose, "\n"); strings.TrimSpace(joined) != "" {
			segments = append(segments, markdownSegment{text: joined})
		}
		prose = nil
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		indent, marker, info, ok := openingFence(lines[i])
		if !ok {
			prose = append(prose, lines[i])
			continue
		}
		flush()
		var code []string
		for i++; i < len(lines); i++ {
			if closesFence(lines[i], marker) {
				break
			}
			code = append(code, trimIndent(lines[i], indent))
		}
		language, _, _ := strings.Cut(info, " ")
		segments = append(segments, markdownSegment{code: true, language: strings.ToLower(language), text: strings.Join(code, "\n")})
	}
	flush()
	return segments
}

func openingFence(line string) (indent int, marker, info string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	indent = len(line) - len(trimmed)
	for _, fence := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == fence {
			n++
		}
		if n < 3 {
			continue
		}
		info = strings.TrimSpace(trimmed[n:])
		if fence == '`' && strings.Contains(info, "`") {
			return 0, "", "", false // inline code such as ```x```, not a fence
		}
		return indent, trimmed[:n], info, true
	}
	return 0, "", "", false
}

func closesFence(line, marker string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, marker) && strings.Trim(trimmed, marker[:1]) == ""
}

func trimIndent(line string, indent int) string {
	n := 0
	for n < indent && n < len(line) && line[n] == ' ' {
		n++
	}
	return line[n:]
}

// trimTrailing drops the padding Glamour adds to every line, so rows end where
// their text does.
func trimTrailing(rendered string) string {
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, ansi.StringWidth(strings.TrimRight(ansi.Strip(line), " ")), "")
	}
	return strings.Join(lines, "\n")
}

type codeSpan struct {
	style lipgloss.Style
	text  string
}

// renderCodeBlock draws a card width cells wide: a header row, a padding row,
// the code, and a padding row. continued marks rows that carry on a wrapped line.
func renderCodeBlock(code, language string, width int) (rows []string, continued []bool) {
	width = max(codeGutter+8, width)
	inner := width - codeGutter - 1
	fill := lipgloss.NewStyle().Background(codeBackground)
	header := lipgloss.NewStyle().Background(codeHeaderColor)

	label := language
	if label == "" {
		label = "code"
	}
	left := header.Render(strings.Repeat(" ", codeGutter)) + header.Foreground(violet).Bold(true).Render(ansi.Truncate(label, max(1, width-codeGutter-12), "…"))
	right := header.Foreground(muted).Render("⧉ copy") + header.Render(" ")
	if ansi.StringWidth(left)+ansi.StringWidth(right) >= width {
		right = ""
	}
	rows = append(rows, left+header.Render(strings.Repeat(" ", max(0, width-ansi.StringWidth(left)-ansi.StringWidth(right))))+right)
	continued = append(continued, false)
	rows = append(rows, fill.Render(strings.Repeat(" ", width)))
	continued = append(continued, false)

	for _, line := range highlight(code, language) {
		for i, part := range wrapSpans(line, inner) {
			gutter := fill.Render(strings.Repeat(" ", codeGutter))
			if i > 0 {
				gutter = fill.Foreground(faint).Render("↳") + fill.Render(strings.Repeat(" ", codeGutter-1))
			}
			row, used := gutter, 0
			for _, span := range part {
				row += span.style.Background(codeBackground).Render(span.text)
				used += ansi.StringWidth(span.text)
			}
			rows = append(rows, row+fill.Render(strings.Repeat(" ", width-codeGutter-used)))
			continued = append(continued, i > 0)
		}
	}
	rows = append(rows, fill.Render(strings.Repeat(" ", width)))
	continued = append(continued, false)
	return rows, continued
}

// highlight tokenizes code into styled spans per source line. An unknown or
// missing language falls back to a guess, then to plain text.
func highlight(code, language string) [][]codeSpan {
	code = strings.ReplaceAll(sanitize(code), "\t", "    ")
	lexer := lexers.Get(language)
	if lexer == nil && language == "" {
		lexer = lexers.Analyse(code)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lines := [][]codeSpan{nil}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		iterator = chroma.Literator(chroma.Token{Type: chroma.Text, Value: code})
	}
	for _, token := range iterator.Tokens() {
		style := tokenStyle(token.Type)
		for i, piece := range strings.Split(token.Value, "\n") {
			if i > 0 {
				lines = append(lines, nil)
			}
			if piece != "" {
				lines[len(lines)-1] = append(lines[len(lines)-1], codeSpan{style, piece})
			}
		}
	}
	// Lexers end the last line with a newline the source did not have.
	if len(lines) > 1 && len(lines[len(lines)-1]) == 0 && !strings.HasSuffix(code, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func tokenStyle(t chroma.TokenType) lipgloss.Style {
	s := lipgloss.NewStyle().Foreground(cream)
	switch {
	case t.InCategory(chroma.Comment):
		return s.Foreground(codeCommentColor).Italic(true)
	case t == chroma.KeywordConstant, t.InCategory(chroma.LiteralNumber), t == chroma.NameConstant, t == chroma.LiteralStringEscape:
		return s.Foreground(coral)
	case t.InCategory(chroma.Keyword), t == chroma.NameTag:
		return s.Foreground(violet)
	case t.InCategory(chroma.LiteralString):
		return s.Foreground(green)
	case t == chroma.NameBuiltin, t == chroma.NameFunction, t == chroma.NameClass, t == chroma.NameDecorator, t == chroma.NameBuiltinPseudo:
		return s.Foreground(codeGold)
	case t == chroma.NameVariable, t == chroma.NameAttribute, t.InSubCategory(chroma.NameVariable):
		return s.Foreground(codeBlue)
	case t.InCategory(chroma.Operator), t.InCategory(chroma.Punctuation):
		return s.Foreground(muted)
	case t == chroma.GenericInserted:
		return s.Foreground(green)
	case t == chroma.GenericDeleted:
		return s.Foreground(coral)
	case t == chroma.GenericHeading, t == chroma.GenericSubheading:
		return s.Foreground(violet).Bold(true)
	}
	return s
}

// wrapSpans breaks one source line into rows of at most width cells.
func wrapSpans(line []codeSpan, width int) [][]codeSpan {
	rows := [][]codeSpan{nil}
	used := 0
	for _, span := range line {
		var piece strings.Builder
		for _, r := range span.text {
			w := ansi.StringWidth(string(r))
			if used+w > width && used > 0 {
				if piece.Len() > 0 {
					rows[len(rows)-1] = append(rows[len(rows)-1], codeSpan{span.style, piece.String()})
					piece.Reset()
				}
				rows = append(rows, nil)
				used = 0
			}
			piece.WriteRune(r)
			used += w
		}
		if piece.Len() > 0 {
			rows[len(rows)-1] = append(rows[len(rows)-1], codeSpan{span.style, piece.String()})
		}
	}
	return rows
}
