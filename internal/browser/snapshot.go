package browser

import (
	"cmp"
	"context"
	json "encoding/json/v2"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
)

// DefaultSnapshotBytes caps a snapshot's text.
const DefaultSnapshotBytes = 32 << 10

// SnapshotOptions configures Snapshot.
type SnapshotOptions struct {
	// Target limits the snapshot to one element's subtree.
	Target *Target
	// MaxBytes caps the text; zero means DefaultSnapshotBytes.
	MaxBytes int
	// Window, for a snapshot longer than MaxBytes, keeps the lines around
	// what is at the centre of the viewport, or else around the focused
	// element, rather than the first ones.
	Window bool
}

// Snapshot describes the page as a compact accessibility tree: a header with
// the title, URL, viewport and scroll position, then one line per
// meaningful node, indented by depth:
//
//   - heading "Checkout" [level=1] [ref=e3]
//   - textbox "Email" [ref=e4] [required]: ada@example.com
//   - checkbox "Subscribe" [ref=e5] [checked]
//   - button "Pay" [ref=e6] [disabled]
//   - link "Terms" [ref=e7] [href=/terms]
//   - text: Total $12.00
//
// Each node shows its role, accessible name, state (checked, disabled,
// expanded, selected, pressed, focused, required, invalid, level, href) and
// value. Interactive and named nodes carry a ref for actions. Generic
// wrappers are collapsed, text runs merged and hidden nodes left out;
// iframes (same-origin or not) and shadow roots appear in place. Past
// MaxBytes the text is cut with a note saying so.
//
// A ref names the same element in every snapshot while it stays on the page;
// refs from before a navigation are stale.
func (p *Page) Snapshot(ctx context.Context, opts SnapshotOptions) (string, error) {
	t, err := p.SnapshotTree(ctx, opts)
	if err != nil {
		return "", err
	}
	return t.Text(cmp.Or(opts.MaxBytes, DefaultSnapshotBytes)), nil
}

// Tree is a snapshot not yet cut to size.
type Tree struct {
	Header string   // the page, URL, viewport and any dialog or modal
	Lines  []string // the tree, refs included
	// Anchor is the line around which a window of a tree too long to show
	// whole is best kept: that of what is at the centre of the viewport, or
	// the focused element's; -1 keeps the first lines.
	Anchor int
	// Around says what the anchor is, for the note on a window.
	Around string
	// Partial says the page was too large to render whole: Lines are its
	// first part only.
	Partial bool
}

// SnapshotTree is Snapshot uncut: the whole tree, as far as it renders, for
// a caller that compares snapshots or cuts them itself. The anchor is found
// only when opts.Window asks and the tree is longer than opts.MaxBytes.
func (p *Page) SnapshotTree(ctx context.Context, opts SnapshotOptions) (Tree, error) {
	max := cmp.Or(opts.MaxBytes, DefaultSnapshotBytes)
	ctx, done, err := p.begin(ctx, 30*time.Second, false)
	if err != nil {
		return Tree{}, err
	}
	defer done()
	defer p.release()
	var focus *element
	if opts.Target != nil {
		if err := opts.Target.check(); err != nil {
			return Tree{}, err
		}
		if focus, err = p.resolve(ctx, *opts.Target); err != nil {
			return Tree{}, fail(ctx, err)
		}
	}
	trees, err := p.axTrees(ctx)
	if err != nil {
		return Tree{}, fail(ctx, err)
	}
	r := &renderer{ctx: ctx, trees: trees, page: p, budget: 2 * max}
	defer r.disable()
	header := p.snapshotHeader(ctx)
	if focus != nil {
		node, err := cdp.Call(ctx, focus.s, dom.DescribeNode, dom.DescribeNodeParams{ObjectID: focus.object})
		if err != nil || node.Node == nil {
			return Tree{}, fail(ctx, fmt.Errorf("snapshot: %s is gone", opts.Target))
		}
		t, n := r.find(focus.s, node.Node.BackendNodeID)
		if n == nil {
			return Tree{}, fmt.Errorf("snapshot: %s is not in the accessibility tree (it is hidden)", focus.desc)
		}
		header += fmt.Sprintf("Subtree: %s\n", focus.desc)
		r.render(t, n, 0, true)
	} else if main := trees[treeKey{p.root.id, cdp.FrameID(p.targetID)}]; main != nil && main.root != nil {
		r.children(main, main.root, 0)
	}
	if focus == nil {
		header += r.modalLine()
	}
	p.mu.Lock()
	tree := Tree{Header: header, Lines: r.layout(&p.refs), Anchor: -1, Partial: r.full}
	p.mu.Unlock()
	if opts.Window && tree.size() > max {
		tree.Anchor, tree.Around = r.anchor()
	}
	return tree, nil
}

func (t Tree) size() int {
	n := len(t.Header) + 1
	for _, l := range t.Lines {
		n += len(l) + 1
	}
	return n
}

// Text lays the tree out in at most max bytes: whole when it fits, else a
// window of it, around the anchor or from the top, between lines saying
// what is left out.
func (t Tree) Text(max int) string {
	var b strings.Builder
	b.WriteString(t.Header)
	b.WriteString("\n")
	if len(t.Lines) == 0 {
		if t.Partial {
			b.WriteString("… the page is too large to show: snapshot one part of it by giving a target, such as css=main\n")
		} else {
			b.WriteString("(the page shows nothing: it is empty, or everything on it is hidden)\n")
		}
		return b.String()
	}
	room := max - b.Len() - 400
	lo, hi := 0, 0
	if t.Anchor >= 0 && t.Anchor < len(t.Lines) {
		lo, hi = t.Anchor, t.Anchor
	}
	used := 0
	// Grow the window a line at a time, below then above, while it fits.
	for grew := true; grew; {
		grew = false
		if hi < len(t.Lines) && used+len(t.Lines[hi])+1 <= room {
			used += len(t.Lines[hi]) + 1
			hi++
			grew = true
		}
		if lo > 0 && used+len(t.Lines[lo-1])+1 <= room {
			lo--
			used += len(t.Lines[lo]) + 1
			grew = true
		}
	}
	if lo > 0 {
		fmt.Fprintf(&b, "… %s above not shown\n", plural(lo, "line", "lines"))
	}
	for _, l := range t.Lines[lo:hi] {
		b.WriteString(l)
		b.WriteString("\n")
	}
	switch {
	case lo > 0 || hi < len(t.Lines) && t.Anchor >= 0:
		below := ""
		if hi < len(t.Lines) {
			below = plural(len(t.Lines)-hi, "line", "lines") + " below not shown; "
		}
		fmt.Fprintf(&b, "… %sthe snapshot stops at %d KiB, so it shows the part around %s. Snapshot one part of the page by giving a target, such as a ref above.\n", below, max>>10, t.Around)
	case hi < len(t.Lines):
		fmt.Fprintf(&b, "… %d more lines not shown: the snapshot stops at %d KiB. Snapshot one part of the page by giving a target, such as a ref above.\n", len(t.Lines)-hi, max>>10)
	case t.Partial:
		b.WriteString("… the rest of the page is not shown: it is too large to read whole. Snapshot one part of it by giving a target, such as a ref above.\n")
	}
	return b.String()
}

func (p *Page) snapshotHeader(ctx context.Context) string {
	p.refreshTitle(ctx)
	p.mu.Lock()
	title, u, v := p.title, p.url, p.viewport
	p.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "Page %s: %q\nURL: %s\n", p.id, clip(title, 200), clip(u, 500))
	m, err := cdp.Call(ctx, p.root, page.GetLayoutMetrics, cdp.Empty{})
	if err == nil && m.CSSContentSize != nil && m.CSSVisualViewport != nil {
		fmt.Fprintf(&b, "Viewport: %d×%d", v.Width, v.Height)
		if v.Mobile {
			b.WriteString(" mobile")
		}
		if v.scale() != 1 {
			fmt.Fprintf(&b, " @%gx", v.scale())
		}
		fmt.Fprintf(&b, ", scrolled to %d,%d of a %d×%d page\n",
			int(m.CSSVisualViewport.PageX), int(m.CSSVisualViewport.PageY), int(m.CSSContentSize.Width), int(m.CSSContentSize.Height))
	}
	if d := p.Dialog(); d != nil {
		fmt.Fprintf(&b, "Dialog: %s is open\n", d)
	}
	return b.String()
}

// refreshTitle reads the document's title, which Chrome reports late.
func (p *Page) refreshTitle(ctx context.Context) {
	res, err := cdp.Call(ctx, p.root, cdpruntime.Evaluate, cdpruntime.EvaluateParams{Expression: "document.title", ReturnByValue: ptr(true)})
	if err != nil || res.Result == nil || res.ExceptionDetails != nil {
		return
	}
	var title string
	if json.Unmarshal(res.Result.Value, &title, decodeOptions) == nil {
		p.mu.Lock()
		p.title = title
		p.mu.Unlock()
	}
}

type treeKey struct {
	session string
	frame   cdp.FrameID
}

type ownerKey struct {
	session string
	node    cdp.BackendNodeID
}

type axTree struct {
	s     *session
	frame cdp.FrameID
	nodes map[accessibility.NodeID]*accessibility.Node
	root  *accessibility.Node
	// lazy marks a tree too large to fetch whole: it starts with its root,
	// and the renderer fetches children as it reaches them.
	lazy bool
}

type axTrees map[treeKey]*axTree

// axTrees fetches the accessibility tree of every frame: same-process frames
// from their page's session, out-of-process ones from their own.
func (p *Page) axTrees(ctx context.Context) (axTrees, error) {
	trees := axTrees{}
	p.mu.Lock()
	oopif := map[cdp.FrameID]bool{}
	for id := range p.frames {
		oopif[id] = true
	}
	p.mu.Unlock()
	for _, s := range p.frameSessions() {
		ft, err := cdp.Call(ctx, s, page.GetFrameTree, cdp.Empty{})
		if err != nil {
			if s == p.root {
				return nil, err
			}
			continue // a frame going away
		}
		var frames []cdp.FrameID
		var walk func(t *page.FrameTree)
		walk = func(t *page.FrameTree) {
			if t == nil || t.Frame == nil || len(frames) >= 100 {
				return
			}
			if !oopif[t.Frame.ID] || t.Frame.ID == cdp.FrameID(s.targetID) {
				frames = append(frames, t.Frame.ID)
			}
			for _, c := range t.ChildFrames {
				walk(c)
			}
		}
		walk(ft.FrameTree)
		for _, frame := range frames {
			res, err := cdp.Call(ctx, s, accessibility.GetFullAXTree, accessibility.GetFullAXTreeParams{FrameID: frame})
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if s == p.root && frame == cdp.FrameID(p.targetID) {
					// Too large to fetch whole (over the message limit):
					// fetched a part at a time instead.
					t, lazyErr := lazyTree(ctx, s, frame)
					if lazyErr != nil {
						return nil, fmt.Errorf("the page's accessibility tree could not be read (%v); snapshot one part of it by giving a target, such as css=main", err)
					}
					trees[treeKey{s.id, frame}] = t
				}
				continue
			}
			t := &axTree{s: s, frame: frame, nodes: make(map[accessibility.NodeID]*accessibility.Node, len(res.Nodes))}
			for _, n := range res.Nodes {
				t.nodes[n.NodeID] = n
				if t.root == nil && n.ParentID == "" {
					t.root = n
				}
			}
			trees[treeKey{s.id, frame}] = t
		}
	}
	return trees, nil
}

// lazyTree starts a tree too large to fetch whole at its root.
func lazyTree(ctx context.Context, s *session, frame cdp.FrameID) (*axTree, error) {
	// Node ids stay the same between calls only while the domain is on.
	if _, err := cdp.Call(ctx, s, accessibility.Enable, cdp.Empty{}); err != nil {
		return nil, err
	}
	res, err := cdp.Call(ctx, s, accessibility.GetRootAXNode, accessibility.GetRootAXNodeParams{FrameID: frame})
	if err != nil || res.Node == nil {
		s.send(accessibility.CommandDisable, nil)
		return nil, fmt.Errorf("root node: %w", err)
	}
	return &axTree{s: s, frame: frame, nodes: map[accessibility.NodeID]*accessibility.Node{res.Node.NodeID: res.Node}, root: res.Node, lazy: true}, nil
}

// renderer turns accessibility trees into snapshot lines.
type renderer struct {
	ctx     context.Context
	trees   axTrees
	page    *Page
	owners  map[ownerKey]*axTree
	lines   []line
	nodes   int
	bytes   int  // the text of lines so far, roughly
	budget  int  // how much of a lazy tree to fetch
	full    bool // more of the page was left out than lines say
	stopped bool // a lazy tree spent the budget: nothing more is rendered
}

type line struct {
	depth int
	text  string
	ref   *refCandidate
	tail  string // after the ref: state and value
	node  ownerKey
}

func (r *renderer) add(l line) {
	r.lines = append(r.lines, l)
	r.bytes += 2*l.depth + len(l.text) + len(l.tail) + 16
}

// load fetches n's children, in a lazy tree, until the budget is spent.
func (r *renderer) load(t *axTree, n *accessibility.Node) {
	if !t.lazy || !slices.ContainsFunc(n.ChildIDs, func(id accessibility.NodeID) bool { return t.nodes[id] == nil }) {
		return
	}
	if r.bytes > r.budget {
		r.full, r.stopped = true, true
		return
	}
	res, err := cdp.Call(r.ctx, t.s, accessibility.GetChildAXNodes, accessibility.GetChildAXNodesParams{ID: n.NodeID, FrameID: t.frame})
	if err != nil {
		r.full, r.stopped = true, true
		return
	}
	for _, c := range res.Nodes {
		t.nodes[c.NodeID] = c
	}
}

// disable turns off what lazy trees turned on.
func (r *renderer) disable() {
	for _, t := range r.trees {
		if t.lazy {
			t.s.send(accessibility.CommandDisable, nil)
		}
	}
}

// lazy reports whether the page's own tree is lazy, and so needs the
// domain on until disable.
func (r *renderer) lazy() bool {
	t := r.trees[treeKey{r.page.root.id, cdp.FrameID(r.page.targetID)}]
	return t != nil && t.lazy
}

type refCandidate struct {
	s    *session
	node cdp.BackendNodeID
	desc string
}

func (r *renderer) find(s *session, backend cdp.BackendNodeID) (*axTree, *accessibility.Node) {
	for k, t := range r.trees {
		if k.session != s.id {
			continue
		}
		for _, n := range t.nodes {
			if n.BackendDOMNodeID == backend {
				return t, n
			}
		}
	}
	// A lazy tree holds only what was fetched: ask for the node itself.
	for k, t := range r.trees {
		if k.session != s.id || !t.lazy {
			continue
		}
		res, err := cdp.Call(r.ctx, s, accessibility.GetPartialAXTree, accessibility.GetPartialAXTreeParams{BackendNodeID: backend, FetchRelatives: ptr(false)})
		if err != nil {
			continue
		}
		for _, n := range res.Nodes {
			if n.BackendDOMNodeID == backend {
				t.nodes[n.NodeID] = n
				return t, n
			}
		}
	}
	return nil, nil
}

// frameFor finds the tree of the document an iframe element holds.
func (r *renderer) frameFor(t *axTree, n *accessibility.Node) *axTree {
	if r.owners == nil {
		r.owners = map[ownerKey]*axTree{}
		for k, child := range r.trees {
			if k.frame == cdp.FrameID(r.page.targetID) {
				continue
			}
			owner := child.s // a same-process frame's element is in the same session
			if k.frame == cdp.FrameID(child.s.targetID) {
				owner = child.s.parent // an out-of-process frame's is in its parent's
			}
			if owner == nil {
				continue
			}
			res, err := cdp.Call(r.ctx, owner, dom.GetFrameOwner, dom.GetFrameOwnerParams{FrameID: k.frame})
			if err == nil {
				r.owners[ownerKey{owner.id, res.BackendNodeID}] = child
			}
		}
	}
	return r.owners[ownerKey{t.s.id, n.BackendDOMNodeID}]
}

// Roles that are only structure: their children take their place.
var transparentRoles = map[string]bool{
	"generic": true, "none": true, "presentation": true, "GenericContainer": true, "Div": true,
	"Section": true, "LabelText": true, "LayoutTable": true, "LayoutTableRow": true, "LayoutTableCell": true,
	"strong": true, "emphasis": true, "code": true, "mark": true, "time": true, "Abbr": true,
	"subscript": true, "superscript": true, "deletion": true, "insertion": true, "Ruby": true,
	"group": true, "Unknown": true, "Legend": true, "ScrollView": true, "Pane": true, "MenuListPopup": true,
}

// Roles that are never shown, nor their subtrees' text duplicated.
var skippedRoles = map[string]bool{"InlineTextBox": true, "ListMarker": true, "LineBreak": true}

// Roles that act, and so always get a ref.
var interactiveRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true, "checkbox": true, "radio": true,
	"switch": true, "combobox": true, "listbox": true, "option": true, "menuitem": true,
	"menuitemcheckbox": true, "menuitemradio": true, "tab": true, "slider": true, "spinbutton": true,
	"treeitem": true, "DisclosureTriangle": true, "ToggleButton": true, "PopUpButton": true,
	"MenuListOption": true, "gridcell": true, "columnheader": true, "scrollbar": true, "ColorWell": true,
	"Date": true, "DateTime": true, "InputTime": true, "Iframe": true,
}

// Roles that show a value after a colon.
var valueRoles = map[string]bool{
	"textbox": true, "searchbox": true, "combobox": true, "spinbutton": true, "slider": true,
	"ColorWell": true, "Date": true, "DateTime": true, "InputTime": true, "progressbar": true, "meter": true, "Meter": true,
}

// roleNames shows Chrome's internal role names as their ARIA equivalents.
var roleNames = map[string]string{
	"Iframe": "iframe", "IframePresentational": "iframe", "DisclosureTriangle": "button",
	"ToggleButton": "button", "PopUpButton": "combobox", "MenuListOption": "option", "MenuListPopup": "listbox",
	"RootWebArea": "document", "image": "img", "Canvas": "canvas", "Pre": "code block", "Figcaption": "caption",
	"DescriptionList": "list", "Details": "group", "Video": "video", "Audio": "audio", "Meter": "meter",
	"ColorWell": "color picker", "Date": "date picker", "DateTime": "date picker", "InputTime": "time picker",
	"graphics-document": "img", "SvgRoot": "img",
}

func axString(v *accessibility.Value) string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v.Value, &s, decodeOptions) == nil {
		return s
	}
	return strings.Trim(string(v.Value), `"`)
}

func (r *renderer) children(t *axTree, n *accessibility.Node, depth int) {
	// Text that only repeats the name of the node it belongs to, or of the
	// control beside it (a label's text), says nothing new.
	parent := normSpace(axString(n.Name))
	var text []string
	previous := ""
	flush := func(next string) {
		joined := normSpace(strings.Join(text, " "))
		text = text[:0]
		if joined != "" && joined != parent && joined != previous && joined != next {
			r.add(line{depth: depth, text: "- text: " + clip(joined, 400)})
		}
	}
	var visit func(t *axTree, id accessibility.NodeID, hops int)
	visit = func(t *axTree, id accessibility.NodeID, hops int) {
		c := t.nodes[id]
		if c == nil || hops > 200 || r.stopped {
			return
		}
		role := axString(c.Role)
		switch {
		case skippedRoles[role]:
			return
		case role == "StaticText":
			if !c.Ignored {
				text = append(text, axString(c.Name))
			}
			return
		case c.Ignored || (transparentRoles[role] && axString(c.Name) == "" && !focusable(c)):
			r.load(t, c)
			for _, g := range c.ChildIDs {
				visit(t, g, hops+1)
			}
			return
		}
		name := normSpace(axString(c.Name))
		flush(name)
		r.render(t, c, depth, false)
		previous = name
	}
	r.load(t, n)
	for _, id := range n.ChildIDs {
		visit(t, id, 0)
	}
	flush("")
}

// render writes a node's line and its children's. top renders a node even
// when it would otherwise be collapsed (the root of a subtree snapshot).
func (r *renderer) render(t *axTree, n *accessibility.Node, depth int, top bool) {
	r.nodes++
	if r.nodes > 50000 || depth > 60 || r.stopped {
		r.full = true
		return
	}
	role := axString(n.Role)
	if top && (n.Ignored || transparentRoles[role] || role == "StaticText") {
		if role == "StaticText" {
			r.add(line{depth: depth, text: "- text: " + clip(normSpace(axString(n.Name)), 400)})
			return
		}
		r.children(t, n, depth)
		return
	}
	name := normSpace(axString(n.Name))
	shown := role
	if alias, ok := roleNames[role]; ok {
		shown = alias
	}
	head := "- " + shown
	if name != "" {
		head += " " + strconv.Quote(clip(name, 150))
	}
	var tail strings.Builder
	props := properties(n)
	for _, flag := range []struct{ prop, show string }{
		{"checked", "checked"}, {"pressed", "pressed"}, {"selected", "selected"}, {"expanded", "expanded"},
		{"disabled", "disabled"}, {"focused", "focused"}, {"required", "required"}, {"invalid", "invalid"},
		{"readonly", "readonly"}, {"modal", "modal"},
	} {
		v, ok := props[flag.prop]
		switch {
		case !ok || v == "" || v == "false":
			if flag.prop == "expanded" && v == "false" {
				tail.WriteString(" [collapsed]")
			}
		case v == "true":
			tail.WriteString(" [" + flag.show + "]")
		default:
			if flag.prop == "readonly" && role != "textbox" && role != "searchbox" {
				continue
			}
			tail.WriteString(" [" + flag.show + "=" + v + "]")
		}
	}
	if level := props["level"]; level != "" && role == "heading" {
		tail.WriteString(" [level=" + level + "]")
	}
	if popup := props["hasPopup"]; popup != "" && popup != "false" && role != "combobox" {
		tail.WriteString(" [haspopup=" + popup + "]")
	}
	if href := props["url"]; href != "" && role == "link" {
		tail.WriteString(" [href=" + clip(r.page.shortHref(href), 200) + "]")
	}
	if desc := normSpace(axString(n.Description)); desc != "" && desc != name {
		tail.WriteString(" [description=" + strconv.Quote(clip(desc, 120)) + "]")
	}
	value := ""
	if valueRoles[role] {
		value = normSpace(axString(n.Value))
		if value == "" && (role == "slider" || role == "progressbar" || role == "spinbutton") {
			value = props["valuetext"]
		}
	}
	var ref *refCandidate
	if n.BackendDOMNodeID != 0 && (interactiveRoles[role] || name != "" || focusable(n)) && !t.inSelect(n) {
		ref = &refCandidate{s: t.s, node: n.BackendDOMNodeID, desc: strings.TrimPrefix(head, "- ")}
	}
	at := len(r.lines)
	r.add(line{depth: depth, text: head, ref: ref, tail: tail.String(), node: ownerKey{t.s.id, n.BackendDOMNodeID}})
	if child := r.frameFor(t, n); child != nil && child.root != nil && len(n.ChildIDs) == 0 {
		r.children(child, child.root, depth+1)
	} else {
		r.children(t, n, depth+1)
	}
	// Children that only repeat the name, as a button's text does, go;
	// short text alone becomes the node's own value.
	kids := r.lines[at+1:]
	if len(kids) == 1 && kids[0].ref == nil && strings.HasPrefix(kids[0].text, "- text: ") {
		text := strings.TrimPrefix(kids[0].text, "- text: ")
		r.lines = r.lines[:at+1]
		if text != name && value == "" {
			value = text
		}
	}
	// A caption or legend that repeats the name, as a table's or a
	// fieldset's does, goes too.
	kept := r.lines[:at+1]
	for i := at + 1; i < len(r.lines); i++ {
		l := r.lines[i]
		leaf := i+1 == len(r.lines) || r.lines[i+1].depth <= l.depth
		if l.depth == depth+1 && l.ref == nil && leaf && name != "" && strings.HasSuffix(l.text+l.tail, ": "+name) {
			continue
		}
		kept = append(kept, l)
	}
	r.lines = kept
	if value != "" {
		r.lines[at].tail += ": " + clip(value, 300)
	} else if len(r.lines) > at+1 {
		r.lines[at].tail += ":"
	}
}

// inSelect reports whether n is an option of a native <select>, which is
// chosen with Select, not clicked, and so gets no ref.
func (t *axTree) inSelect(n *accessibility.Node) bool {
	role := axString(n.Role)
	if role != "option" && role != "MenuListOption" {
		return false
	}
	for p, hops := t.nodes[n.ParentID], 0; p != nil && hops < 3; p, hops = t.nodes[p.ParentID], hops+1 {
		if r := axString(p.Role); r == "MenuListPopup" || r == "combobox" || r == "PopUpButton" {
			return true
		}
	}
	return false
}

func focusable(n *accessibility.Node) bool {
	for _, p := range n.Properties {
		if p.Name == "focusable" && p.Value != nil && string(p.Value.Value) == "true" {
			return true
		}
	}
	return false
}

func properties(n *accessibility.Node) map[string]string {
	props := map[string]string{}
	for _, p := range n.Properties {
		if p.Value == nil {
			continue
		}
		props[string(p.Name)] = axString(p.Value)
	}
	return props
}

// layout writes the lines, giving refs to the nodes that act or are named.
func (r *renderer) layout(refs *refTable) []string {
	out := make([]string, len(r.lines))
	for i, l := range r.lines {
		s := strings.Repeat("  ", l.depth) + l.text
		if l.ref != nil {
			if ref := refs.assign(l.ref); ref != "" {
				s += " [ref=" + ref + "]"
			}
		}
		out[i] = s + l.tail
	}
	return out
}

// modalLine is the header's line for a modal dialog the page shows: it
// says why the rest of the page left the tree, rather than a diff saying it
// was removed.
func (r *renderer) modalLine() string {
	for i, l := range r.lines {
		if !strings.Contains(l.tail, "[modal]") {
			continue
		}
		end := i + 1
		for end < len(r.lines) && r.lines[end].depth > l.depth {
			end++
		}
		name := strings.TrimPrefix(l.text, "- ")
		if len(r.lines) == end-i {
			return fmt.Sprintf("Modal: %s is open; the rest of the page is inert behind it\n", name)
		}
		return fmt.Sprintf("Modal: %s is open\n", name)
	}
	return ""
}

// anchor is the line a window of a long snapshot is best kept around: the
// one nearest what is at the centre of the viewport, where the agent last
// acted or scrolled to, or else the focused element's; and how to say which.
func (r *renderer) anchor() (int, string) {
	if i := r.inView(); i >= 0 {
		return i, "what is in view"
	}
	for i, l := range r.lines {
		if strings.Contains(l.tail, "[focused]") {
			return i, "the focused element"
		}
	}
	return -1, ""
}

// inView is the line nearest what is at the centre of the viewport, or -1.
func (r *renderer) inView() int {
	p := r.page
	m, err := cdp.Call(r.ctx, p.root, page.GetLayoutMetrics, cdp.Empty{})
	if err != nil || m.CSSVisualViewport == nil {
		return -1
	}
	vv := m.CSSVisualViewport
	at, err := p.documentOffset(r.ctx, p.root)
	if err != nil {
		return -1
	}
	node, _, err := p.nodeAt(r.ctx, p.root, at, Point{vv.ClientWidth / 2, vv.ClientHeight / 2})
	if err != nil {
		return -1
	}
	if _, err := cdp.Call(r.ctx, p.root, accessibility.Enable, cdp.Empty{}); err != nil {
		return -1
	}
	res, err := cdp.Call(r.ctx, p.root, accessibility.GetAXNodeAndAncestors, accessibility.GetAXNodeAndAncestorsParams{BackendNodeID: node})
	if !r.lazy() {
		p.root.send(accessibility.CommandDisable, nil)
	}
	if err != nil {
		return -1
	}
	shown := map[cdp.BackendNodeID]int{}
	for i, l := range r.lines {
		if l.node.session == p.root.id && l.node.node != 0 {
			if _, ok := shown[l.node.node]; !ok {
				shown[l.node.node] = i
			}
		}
	}
	best := -1
	for _, n := range res.Nodes {
		if i, ok := shown[n.BackendDOMNodeID]; ok && (best < 0 || r.lines[i].depth > r.lines[best].depth) {
			best = i
		}
	}
	return best
}

func normSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// shortHref drops the scheme and host of a link on the page's own origin.
func (p *Page) shortHref(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	p.mu.Lock()
	base, _ := url.Parse(p.url)
	p.mu.Unlock()
	if base != nil && u.Scheme == base.Scheme && u.Host == base.Host && u.Host != "" {
		return u.RequestURI() + fragment(u)
	}
	return raw
}

func fragment(u *url.URL) string {
	if u.Fragment == "" {
		return ""
	}
	return "#" + u.EscapedFragment()
}

// refTable gives snapshot nodes their refs. Numbers only grow, so a ref from
// before a navigation can never name something else after it.
type refTable struct {
	next  int
	floor int // refs at or below floor belong to earlier documents
	byKey map[ownerKey]string
	byRef map[string]refEntry
}

type refEntry struct {
	s     *session
	node  cdp.BackendNodeID
	label string
}

const maxRefs = 20000

func (t *refTable) newDocument() {
	t.floor, t.byKey, t.byRef = t.next, nil, nil
}

func (t *refTable) assign(c *refCandidate) string {
	key := ownerKey{c.s.id, c.node}
	if ref, ok := t.byKey[key]; ok {
		return ref
	}
	if len(t.byRef) >= maxRefs {
		return ""
	}
	if t.byKey == nil {
		t.byKey, t.byRef = map[ownerKey]string{}, map[string]refEntry{}
	}
	t.next++
	ref := "e" + strconv.Itoa(t.next)
	t.byKey[key] = ref
	t.byRef[ref] = refEntry{s: c.s, node: c.node, label: c.desc}
	return ref
}

func (p *Page) lookupRef(ref string) (refEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.refs.byRef[ref]; ok {
		return e, nil
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(ref, "e"))
	switch {
	case n > p.refs.next:
		return refEntry{}, &targetError{msg: fmt.Sprintf("no ref %s yet: refs come from a snapshot", ref), kind: "stale"}
	case n <= p.refs.floor:
		return refEntry{}, &targetError{msg: fmt.Sprintf("ref %s is from before the page navigated; take a new snapshot", ref), kind: "stale"}
	}
	return refEntry{}, &targetError{msg: fmt.Sprintf("ref %s is stale; take a new snapshot", ref), kind: "stale"}
}
