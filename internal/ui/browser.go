package ui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"julia/internal/core"
)

// A Browser result that shows a page is drawn as the page: collapsed, one row
// with its id, title and address and what needs attention (blocked requests
// and console errors in coral); expanded, its notes and then a card holding
// the accessibility snapshot, or the lines that changed, lit as a diff. The
// UI reads the same text the model does, so a resumed session draws the same.

type browserCall struct {
	Action      string   `json:"action"`
	Page        string   `json:"page"`
	PageURL     string   `json:"page_url"`
	PageTitle   string   `json:"page_title"`
	OpenedBy    string   `json:"opened_by"`
	URL         string   `json:"url"`
	Network     bool     `json:"network"`
	NewPage     bool     `json:"new_page"`
	Origins     []string `json:"origins"`
	Target      string   `json:"target"`
	To          string   `json:"to"`
	Text        *string  `json:"text"`
	Submit      bool     `json:"submit"`
	Keys        []string `json:"keys"`
	Values      []string `json:"values"`
	Paths       []string `json:"paths"`
	Button      string   `json:"button"`
	Double      bool     `json:"double"`
	DX          float64  `json:"dx"`
	DY          float64  `json:"dy"`
	Selector    string   `json:"selector"`
	NetworkIdle bool     `json:"network_idle"`
	Gone        bool     `json:"gone"`
	Script      string   `json:"script"`
	FullPage    bool     `json:"full_page"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	Scale       float64  `json:"scale"`
	Mobile      *bool    `json:"mobile"`
	ColorScheme string   `json:"color_scheme"`
	Media       string   `json:"media"`
}

// describedRef finds how a result or an approval named its targets:
// button "Save" (e12).
var describedRef = regexp.MustCompile(`([a-z][a-z-]*(?: "(?:[^"\\]|\\.)*")?) \((e[0-9]+)\)`)

// browserSummary titles a Browser call by what it does, as in
// "navigate · localhost:5173/settings" or `type · "hello" → textbox "Email"`.
// headline is the result's first line, which names the elements a ref meant.
func browserSummary(arguments json.RawMessage, headline string) (string, bool) {
	var a browserCall
	if json.Unmarshal(arguments, &a) != nil || a.Action == "" {
		return "", false
	}
	described := map[string]string{}
	for _, m := range describedRef.FindAllStringSubmatch(headline, -1) {
		described[m[2]] = m[1]
	}
	name := func(target string) string {
		if m := describedRef.FindStringSubmatch(target); m != nil && m[0] == target {
			return m[1] // an approval names it already
		}
		if d, ok := described[target]; ok {
			return d
		}
		return target
	}
	action, parts := a.Action, []string{}
	switch a.Action {
	case "navigate":
		parts = append(parts, shortURL(a.URL))
		if a.Network {
			parts = append(parts, "network")
		}
		if a.NewPage {
			parts = append(parts, "new page")
		}
	case "click":
		switch {
		case a.Double:
			action = "double-click"
		case a.Button == "right":
			action = "right-click"
		}
		parts = append(parts, name(a.Target))
	case "type":
		text := `""`
		if a.Text != nil {
			text = strconv.Quote(typedLine(*a.Text))
		}
		if a.Submit {
			text += " ↵"
		}
		parts = append(parts, text+" → "+name(a.Target))
	case "press":
		parts = append(parts, strings.Join(a.Keys, " "))
	case "hover", "upload", "select", "snapshot", "screenshot":
		switch {
		case a.Action == "select":
			parts = append(parts, quoted(a.Values)+" → "+name(a.Target))
		case a.Action == "upload":
			parts = append(parts, strings.Join(a.Paths, ", ")+" → "+name(a.Target))
		case a.Target != "":
			parts = append(parts, name(a.Target))
		case a.FullPage:
			parts = append(parts, "full page")
		}
	case "drag":
		parts = append(parts, name(a.Target)+" → "+name(a.To))
	case "scroll":
		move := ""
		for _, d := range []struct {
			by     float64
			arrows string
		}{{a.DX, "←→"}, {a.DY, "↑↓"}} {
			arrows := []rune(d.arrows)
			switch {
			case d.by < 0:
				move += fmt.Sprintf("%c %g ", arrows[0], -d.by)
			case d.by > 0:
				move += fmt.Sprintf("%c %g ", arrows[1], d.by)
			}
		}
		if move == "" {
			move = "↓ a screen"
		}
		parts = append(parts, strings.TrimSpace(move))
		if a.Target != "" {
			parts = append(parts, name(a.Target))
		}
	case "wait":
		switch {
		case a.Text != nil:
			parts = append(parts, strconv.Quote(*a.Text))
		case a.Selector != "":
			parts = append(parts, a.Selector)
		case a.URL != "":
			parts = append(parts, "url "+a.URL)
		case a.NetworkIdle:
			parts = append(parts, "network idle")
		}
		if a.Gone {
			parts = append(parts, "gone")
		}
	case "eval":
		first, _, _ := strings.Cut(strings.TrimSpace(a.Script), "\n")
		parts = append(parts, first)
	case "emulate":
		if a.Width != 0 || a.Height != 0 {
			parts = append(parts, fmt.Sprintf("%d×%d", a.Width, a.Height))
		}
		if a.Mobile != nil && *a.Mobile {
			parts = append(parts, "mobile")
		}
		if a.Scale != 0 {
			parts = append(parts, fmt.Sprintf("@%gx", a.Scale))
		}
		for _, v := range []string{a.ColorScheme, a.Media} {
			if v != "" {
				parts = append(parts, v)
			}
		}
	case "allow":
		parts = append(parts, strings.Join(a.Origins, ", "))
	}
	if a.Page != "" {
		parts = append(parts, a.Page)
	}
	title := action
	for _, part := range parts {
		if part != "" {
			title += " · " + part
		}
	}
	return title, true
}

// shortURL shows an address as a person reads it: without http(s):// or
// file://.
func shortURL(raw string) string {
	for _, scheme := range []string{"http://", "https://", "file://"} {
		if rest, ok := strings.CutPrefix(raw, scheme); ok {
			return rest
		}
	}
	return raw
}

func quoted(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Quote(v)
	}
	return strings.Join(parts, ", ")
}

// browserPage is a Browser result that shows a page, as parsed from its
// text: the headline, details (a script's value), notes and console errors,
// then the snapshot's header and its tree, or the lines that changed.
type browserPage struct {
	headline              string // the action's result; none for a snapshot
	details, notes, errs  []string
	id, title, url, extra string
	caption               string   // "Changes since …" or "No change since …"
	lines                 []string // the tree, or diff lines when diff
	diff                  bool
	shot                  string // a screenshot's caption, drawn under the card
}

var (
	browserPageHeader = regexp.MustCompile(`^Page (p[0-9]+): "(.*)"$`)
	blockedCount      = regexp.MustCompile(`^(?:p[0-9]+: )?blocked ([0-9]+) requests`)
)

func parseBrowserPage(content string) (browserPage, bool) {
	var p browserPage
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	at := -1
	for i := 0; i+1 < len(lines); i++ {
		if m := browserPageHeader.FindStringSubmatch(lines[i]); m != nil && strings.HasPrefix(lines[i+1], "URL: ") {
			at, p.id, p.title = i, m[1], m[2]
			if title, err := strconv.Unquote(`"` + m[2] + `"`); err == nil {
				p.title = title
			}
			p.url = strings.TrimPrefix(lines[i+1], "URL: ")
			break
		}
	}
	if at < 0 {
		return p, false
	}
	// An action's result leads with its headline; a snapshot's starts with
	// its notes, if any, or the page itself.
	start := 0
	if at > 0 && lines[0] != "Notes:" && lines[0] != "Console errors:" {
		p.headline, start = lines[0], 1
	}
	section := &p.details
	for _, line := range lines[start:at] {
		switch {
		case line == "Notes:":
			section = &p.notes
		case line == "Console errors:":
			section = &p.errs
		case line == "":
		case section != &p.details && strings.HasPrefix(line, "- "):
			*section = append(*section, strings.TrimPrefix(line, "- "))
		default:
			p.details = append(p.details, line)
		}
	}
	i := at + 2
	for ; i < len(lines) && lines[i] != ""; i++ {
		if strings.HasPrefix(lines[i], "Viewport: ") || strings.HasPrefix(lines[i], "Dialog: ") {
			p.extra = strings.TrimSpace(p.extra + " · " + lines[i])
		}
	}
	p.extra = strings.TrimPrefix(p.extra, "· ")
	for i++; i < len(lines); i++ {
		line := lines[i]
		switch {
		case p.caption == "" && len(p.lines) == 0 && (strings.HasPrefix(line, "Changes since ") || strings.HasPrefix(line, "No change since ")):
			p.caption, p.diff = line, strings.HasPrefix(line, "Changes since ")
		default:
			p.lines = append(p.lines, line)
		}
	}
	// A screenshot's caption ends the result: it goes under the card, not
	// into the tree, nor into what copying the snapshot gives.
	if n := len(p.lines); n > 0 && (strings.HasPrefix(p.lines[n-1], "[screenshot: ") || strings.HasPrefix(p.lines[n-1], "[screenshot unavailable: ")) {
		p.shot = strings.TrimSuffix(strings.TrimPrefix(p.lines[n-1], "["), "]")
		p.lines = p.lines[:n-1]
		for len(p.lines) > 0 && p.lines[len(p.lines)-1] == "" {
			p.lines = p.lines[:len(p.lines)-1]
		}
	}
	return p, true
}

// blocked counts the requests the notes say were blocked.
func (p browserPage) blocked() int {
	n := 0
	for _, note := range p.notes {
		if m := blockedCount.FindStringSubmatch(note); m != nil {
			count, _ := strconv.Atoi(m[1])
			n += count
		} else if strings.HasPrefix(strings.TrimPrefix(note, p.id+": "), "blocked ") {
			n++
		}
	}
	return n
}

func (p browserPage) dialog() bool {
	return slices.ContainsFunc(p.notes, func(note string) bool { return strings.Contains(note, " dialog ") })
}

// text is what copying the card gives: the snapshot's lines.
func (p browserPage) text() string { return strings.Join(p.lines, "\n") }

// summary is the collapsed card's one row, at most width cells: the page,
// then what changed and what needs attention, which keep their place while
// the address gives way.
func (p browserPage) summary(width int) string {
	sep := faintStyle.Render(" · ")
	var tail []string
	add := func(style lipgloss.Style, text string) { tail = append(tail, style.Render(text)) }
	switch {
	case p.diff:
		changed := 0
		for _, line := range p.lines {
			if strings.HasPrefix(line, "+ ") || strings.HasPrefix(line, "- ") {
				changed++
			}
		}
		add(mutedStyle.Italic(true), count(changed, "line changed", "lines changed"))
	case p.caption != "":
		add(mutedStyle.Italic(true), "no change")
	}
	if n := p.blocked(); n > 0 {
		add(coralStyle, fmt.Sprintf("%d blocked", n))
	}
	if n := len(p.errs); n > 0 {
		add(coralStyle, count(n, "error", "errors"))
	}
	if p.dialog() {
		add(mutedStyle.Italic(true), "dialog")
	}
	if len(p.details) > 0 {
		// A script's value is what it was run for.
		add(creamStyle, "→ "+p.details[0])
	}
	row := faintStyle.Render(p.id)
	if p.title != "" {
		row += sep + creamStyle.Render(ansi.Truncate(p.title, 40, "…"))
	}
	status := ""
	for _, part := range tail {
		status += sep + part
	}
	if room := width - ansi.StringWidth(row+status) - 3; room >= 12 {
		row += sep + mutedStyle.Render(ansi.Truncate(shortURL(p.url), room, "…"))
	}
	return ansi.Truncate(row+status, width, "…")
}

// count is n of something: "1 error", "3 errors".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// noteStyle colours a note: coral for what failed or was refused, muted for
// the rest (dialogs, popups, navigations).
func noteStyle(note string) lipgloss.Style {
	for _, bad := range []string{"blocked", "crashed", "refused", "cannot be intercepted"} {
		if strings.Contains(note, bad) {
			return coralStyle
		}
	}
	return mutedStyle.Italic(true)
}

// card draws the snapshot in at most width cells: a header with the page and
// its address, then the tree, or the changed lines lit as a diff (added in
// green, removed in coral, the lines that place them muted). Long lines are
// cut at the edge; edge marks header and padding rows, which are never
// selected.
func (p browserPage) card(width int) (rows []string, edge []bool) {
	width = max(codeGutter+8, width)
	// As wide as what it holds, as a terminal's card is, with room for the
	// header's page and address.
	natural := ansi.StringWidth(p.id+" · "+p.title) + ansi.StringWidth(shortURL(p.url)) + 4
	caption := p.caption
	if p.diff {
		caption = "changes since the last snapshot"
	}
	for _, line := range append([]string{p.extra, caption}, p.lines...) {
		natural = max(natural, ansi.StringWidth(line))
	}
	width = min(width, max(natural+codeGutter+2, 48))
	inner := width - codeGutter - 1
	fill := lipgloss.NewStyle().Background(codeBackground)
	header := lipgloss.NewStyle().Background(codeHeaderColor)
	pad := func(row string) string {
		return row + fill.Render(strings.Repeat(" ", max(0, width-ansi.StringWidth(row))))
	}
	add := func(row string, isEdge bool) {
		rows, edge = append(rows, row), append(edge, isEdge)
	}
	label := p.id
	if p.title != "" {
		label += " · " + p.title
	}
	right := header.Foreground(muted).Render(ansi.Truncate(shortURL(p.url), max(8, width/2), "…")) + header.Render(" ")
	if width-codeGutter-ansi.StringWidth(right)-2 < min(ansi.StringWidth(label), 12) {
		right = ""
	}
	left := header.Render(strings.Repeat(" ", codeGutter)) + header.Foreground(violet).Bold(true).Render(ansi.Truncate(label, max(1, width-codeGutter-ansi.StringWidth(right)-2), "…"))
	add(left+header.Render(strings.Repeat(" ", max(0, width-ansi.StringWidth(left)-ansi.StringWidth(right))))+right, true)
	add(fill.Render(strings.Repeat(" ", width)), true)
	gutter := fill.Render(strings.Repeat(" ", codeGutter))
	if p.extra != "" {
		add(pad(gutter+fill.Foreground(faint).Render(ansi.Truncate(p.extra, inner, "…"))), true)
	}
	if caption != "" {
		add(pad(gutter+fill.Foreground(faint).Italic(true).Render(ansi.Truncate(caption, inner, "…"))), true)
	}
	for _, line := range p.lines {
		style := fill.Foreground(cream)
		switch {
		case !p.diff && strings.HasPrefix(line, "… "):
			style = fill.Foreground(faint).Italic(true)
		case p.diff && strings.HasPrefix(line, "+ "):
			style = fill.Foreground(green)
		case p.diff && strings.HasPrefix(line, "- "):
			style = fill.Foreground(coral)
		case p.diff:
			style = fill.Foreground(muted)
		}
		add(pad(gutter+style.Render(ansi.Truncate(line, inner, "…"))), false)
	}
	add(fill.Render(strings.Repeat(" ", width)), true)
	return rows, edge
}

// browserApproval is what a Browser permission request shows above its
// arguments: the page it acts on and any origins it would approve.
func browserApproval(arguments json.RawMessage) (page string, origins []string) {
	var a browserCall
	if json.Unmarshal(arguments, &a) != nil {
		return "", nil
	}
	if a.PageURL != "" {
		page = a.PageURL
		if a.PageTitle != "" {
			page = strconv.Quote(a.PageTitle) + " · " + page
		}
		if a.Page != "" {
			page = a.Page + " · " + page
		}
		if a.OpenedBy != "" {
			page += " · opened by " + a.OpenedBy
		}
	}
	return page, a.Origins
}

// withoutPage drops from a Browser request's arguments the page it acts on,
// which the panel shows on its own.
func withoutPage(arguments json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(arguments, &fields) != nil {
		return arguments
	}
	for _, key := range []string{"page", "page_url", "page_title", "opened_by"} {
		delete(fields, key)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return arguments
	}
	return out
}

// allowLabel says what y grants: this one action, or, for a Browser
// request that approves origins, those origins for the rest of the turn.
func allowLabel(a core.Approval) string {
	if a.Call.Name == "Browser" {
		if _, origins := browserApproval(a.Call.Arguments); len(origins) > 0 {
			return "Allow for this turn"
		}
	}
	return "Allow once"
}

// browserRows draws a Browser result that is not a page, in the page rows'
// style: a console log, a screenshot, or the list of pages. Collapsed it is
// one row; expanded, that row and what it sums up. ok is false for anything
// else, which is drawn as plain output.
func browserRows(action, content string, width int, expanded bool) (rows []string, ok bool) {
	switch action {
	case "console":
		return consoleRows(content, width, expanded)
	case "screenshot":
		return shotRows(content, width, expanded)
	case "tabs", "close":
		return tabRows(content, width, expanded)
	}
	return nil, false
}

var consoleHead = regexp.MustCompile(`^Console of (p[0-9]+) since you last looked|^(p[0-9]+) has logged nothing since you last looked`)

// consoleRows sums up a console log by level, errors in coral, and lists
// its entries whole, wrapped at words.
func consoleRows(content string, width int, expanded bool) ([]string, bool) {
	lines := strings.Split(content, "\n")
	m := consoleHead.FindStringSubmatch(lines[0])
	if m == nil {
		return nil, false
	}
	id := m[1] + m[2]
	levels := map[string]int{}
	type entry struct{ level, text string }
	var entries []entry
	dropped := ""
	for _, line := range lines[1:] {
		line, ok := strings.CutPrefix(line, "- ")
		switch {
		case !ok:
		case strings.HasPrefix(line, "… "):
			dropped = strings.TrimPrefix(line, "… ")
		default:
			level, text, _ := strings.Cut(line, " ")
			levels[level]++
			entries = append(entries, entry{level, text})
		}
	}
	sep := faintStyle.Render(" · ")
	row := faintStyle.Render(id)
	if len(entries) == 0 && dropped == "" {
		row += sep + mutedStyle.Italic(true).Render("nothing logged")
	}
	for _, l := range []struct {
		level, one, many string
		style            lipgloss.Style
	}{{"error", "error", "errors", coralStyle}, {"warning", "warning", "warnings", creamStyle}, {"info", "info", "info", mutedStyle}, {"log", "log", "logs", mutedStyle}, {"debug", "debug", "debug", mutedStyle}} {
		if n := levels[l.level]; n > 0 {
			row += sep + l.style.Render(count(n, l.one, l.many))
		}
	}
	rows := []string{ansi.Truncate(row, width, "…")}
	if !expanded {
		return rows, true
	}
	if dropped != "" {
		rows = append(rows, mutedStyle.Italic(true).Render(ansi.Truncate("· "+dropped, width, "…")))
	}
	for _, e := range entries {
		// An error's ! says what it is; the other levels say it in words.
		style, lead, text := mutedStyle, "· ", e.level+" "+e.text
		switch e.level {
		case "error":
			style, lead, text = coralStyle, "! ", e.text
		case "warning":
			style = creamStyle
		}
		for i, line := range strings.Split(ansi.Wrap(text, max(8, width-2), ""), "\n") {
			if i > 0 {
				lead = "  "
			}
			rows = append(rows, style.Render(lead+line))
		}
	}
	return rows, true
}

var shotHead = regexp.MustCompile(`^Screenshot of (.+) in (p[0-9]+) "(.*)" · (.*)$`)
var shotSize = regexp.MustCompile(`^\[screenshot: ([0-9]+×[0-9]+) CSS px`)

// shotRows draws a screenshot's row as a page's: the page, then what was
// captured and its size; expanded, its notes too.
func shotRows(content string, width int, expanded bool) ([]string, bool) {
	lines := strings.Split(content, "\n")
	m := shotHead.FindStringSubmatch(lines[0])
	if m == nil {
		return nil, false
	}
	what := strings.TrimPrefix(m[1], "the ")
	size, caption := "", ""
	var notes []string
	for _, line := range lines[1:] {
		switch {
		case shotSize.MatchString(line):
			size = shotSize.FindStringSubmatch(line)[1]
			caption = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
		case strings.HasPrefix(line, "- "):
			notes = append(notes, strings.TrimPrefix(line, "- "))
		}
	}
	title, _ := strconv.Unquote(`"` + m[3] + `"`)
	sep := faintStyle.Render(" · ")
	row := faintStyle.Render(m[2])
	if title != "" {
		row += sep + creamStyle.Render(ansi.Truncate(title, 40, "…"))
	}
	tail := sep + creamStyle.Render(strings.TrimSpace(what+" "+size))
	if room := width - ansi.StringWidth(row+tail) - 3; room >= 12 {
		row += sep + mutedStyle.Render(ansi.Truncate(shortURL(m[4]), room, "…"))
	}
	rows := []string{ansi.Truncate(row+tail, width, "…")}
	if expanded {
		if caption != "" {
			for _, line := range strings.Split(ansi.Wrap(caption, width, ""), "\n") {
				rows = append(rows, mutedStyle.Italic(true).Render(line))
			}
		}
		for _, note := range notes {
			for i, line := range strings.Split(ansi.Wrap(note, max(8, width-2), ""), "\n") {
				lead := map[bool]string{true: "· ", false: "  "}[i == 0]
				rows = append(rows, noteStyle(note).Render(lead+line))
			}
		}
	}
	return rows, true
}

var tabRow = regexp.MustCompile(`^(p[0-9]+) · "(.*)" · (.*?)( · opened by p[0-9]+)?( · current)?$`)

// tabRows counts the open pages and names the current one; expanded, it
// lists them as page rows.
func tabRows(content string, width int, expanded bool) ([]string, bool) {
	var pages, details []string
	current, approved := "", ""
	for _, line := range strings.Split(content, "\n") {
		m := tabRow.FindStringSubmatch(line)
		switch {
		case m != nil:
			title, _ := strconv.Unquote(`"` + m[2] + `"`)
			sep := faintStyle.Render(" · ")
			row := faintStyle.Render(m[1])
			if title != "" {
				row += sep + creamStyle.Render(ansi.Truncate(title, 40, "…"))
			}
			tail := ""
			if m[4] != "" {
				tail += sep + mutedStyle.Italic(true).Render(strings.TrimPrefix(m[4], " · "))
			}
			if m[5] != "" {
				tail += sep + violetStyle.Render("current")
				current = m[1]
			}
			if room := width - ansi.StringWidth(row+tail) - 3; room >= 12 {
				row += sep + mutedStyle.Render(ansi.Truncate(shortURL(m[3]), room, "…"))
			}
			pages = append(pages, ansi.Truncate(row+tail, width, "…"))
		case strings.HasPrefix(line, "Approved this turn: "):
			approved = strings.TrimPrefix(line, "Approved this turn: ")
		case line == "No pages are open.":
		case strings.HasPrefix(line, "Closed p"):
			details = append(details, line)
		default:
			return nil, false
		}
	}
	sep := faintStyle.Render(" · ")
	summary := mutedStyle.Render(count(len(pages), "page", "pages"))
	if len(pages) == 0 {
		summary = mutedStyle.Italic(true).Render("no pages open")
	}
	if current != "" {
		summary += sep + violetStyle.Render(current+" current")
	}
	if approved != "" {
		summary += sep + coralStyle.Render(count(len(strings.Split(approved, ", ")), "origin approved", "origins approved"))
	}
	for _, d := range details {
		summary = mutedStyle.Render(strings.TrimSuffix(d, ".")) + sep + summary
	}
	rows := []string{ansi.Truncate(summary, width, "…")}
	if expanded {
		rows = append(rows, pages...)
		if approved != "" {
			rows = append(rows, coralStyle.Render(ansi.Truncate("approved this turn: "+approved, width, "…")))
		}
	}
	return rows, true
}
