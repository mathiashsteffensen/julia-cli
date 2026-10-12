package browser

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
)

// Target is what an action aims at: a ref from the last snapshot, visible
// text, a CSS selector, or a point in the viewport. Exactly one is set.
type Target struct {
	// Ref is a snapshot ref such as "e12". It stays valid while its element
	// is on the page and the page has not navigated.
	Ref string
	// Text is visible text: the smallest element whose whole text equals it
	// (ignoring case and runs of spaces) wins, then one that contains it. An
	// aria-label, alt text, placeholder or button value counts as text.
	Text string
	// Selector is a CSS selector, tried in the document, same-origin
	// iframes and each open shadow root.
	Selector string
	// Point is a position in the viewport, in CSS pixels.
	Point *Point
}

// Point is a position in CSS pixels.
type Point struct{ X, Y float64 }

func (t Target) String() string {
	switch {
	case t.Ref != "":
		return t.Ref
	case t.Text != "":
		return fmt.Sprintf("text %q", t.Text)
	case t.Selector != "":
		return fmt.Sprintf("selector %q", t.Selector)
	case t.Point != nil:
		return fmt.Sprintf("point %g,%g", t.Point.X, t.Point.Y)
	}
	return "nothing"
}

var (
	refPattern   = regexp.MustCompile(`^e[1-9][0-9]*$`)
	pointPattern = regexp.MustCompile(`^\s*(-?[0-9]+(?:\.[0-9]+)?)\s*,\s*(-?[0-9]+(?:\.[0-9]+)?)\s*$`)
)

// ParseTarget reads a target written as one string: a ref ("e12"),
// "text=Save" (or a quoted "Save"), "css=.btn" or any other CSS selector,
// or "x,y" viewport coordinates ("120,48").
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Target{}, errors.New("target is empty")
	case refPattern.MatchString(s):
		return Target{Ref: s}, nil
	case strings.HasPrefix(s, "text="):
		return Target{Text: strings.TrimPrefix(s, "text=")}, nil
	case len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"':
		text, err := strconv.Unquote(s)
		if err != nil {
			text = s[1 : len(s)-1]
		}
		return Target{Text: text}, nil
	case strings.HasPrefix(s, "css="):
		return Target{Selector: strings.TrimPrefix(s, "css=")}, nil
	}
	if m := pointPattern.FindStringSubmatch(s); m != nil {
		x, _ := strconv.ParseFloat(m[1], 64)
		y, _ := strconv.ParseFloat(m[2], 64)
		return Target{Point: &Point{x, y}}, nil
	}
	return Target{Selector: s}, nil
}

func (t Target) check() error {
	set := 0
	for _, ok := range []bool{t.Ref != "", t.Text != "", t.Selector != "", t.Point != nil} {
		if ok {
			set++
		}
	}
	switch {
	case set == 0:
		return errors.New("no target given: use a ref from the snapshot, text, a selector or a point")
	case set > 1:
		return errors.New("give one target: a ref, text, a selector or a point")
	case t.Ref != "" && !refPattern.MatchString(t.Ref):
		return fmt.Errorf("%q is not a ref; refs look like e12", t.Ref)
	case len(t.Text) > 1000 || len(t.Selector) > 4000:
		return errors.New("target text or selector is too long")
	case t.Point != nil && (math.IsNaN(t.Point.X) || math.IsNaN(t.Point.Y) || t.Point.X < 0 || t.Point.Y < 0):
		return errors.New("a point must be inside the viewport")
	}
	return nil
}

// TargetTimeout bounds how long an action waits for its target to exist and
// become actionable (visible, enabled, still and uncovered).
const TargetTimeout = 5 * time.Second

// objectGroup names the remote objects an action holds, released after it.
const objectGroup = "julia"

// element is a resolved target: a node in one session's document.
type element struct {
	s      *session
	object cdpruntime.RemoteObjectID
	desc   string // how errors name it: button "Save", <div.card>
	note   string // a remark about how it was chosen
}

// targetError is a failure to act on a target; retry says whether waiting
// might cure it (the element may yet appear, show, enable or stop moving).
type targetError struct {
	msg   string
	retry bool
	kind  string
}

func (e *targetError) Error() string { return e.msg }

func retryable(err error) bool {
	var te *targetError
	return errors.As(err, &te) && te.retry
}

// release frees the remote objects of the page's sessions.
func (p *Page) release() {
	p.mu.Lock()
	sessions := []*session{p.root}
	for _, s := range p.frames {
		sessions = append(sessions, s)
	}
	p.mu.Unlock()
	for _, s := range sessions {
		s.send(cdpruntime.CommandReleaseObjectGroup, cdpruntime.ReleaseObjectGroupParams{ObjectGroup: objectGroup})
	}
}

// frameSessions lists the page's main session and its out-of-process frames.
func (p *Page) frameSessions() []*session {
	p.mu.Lock()
	defer p.mu.Unlock()
	sessions := []*session{p.root}
	for _, s := range p.frames {
		sessions = append(sessions, s)
	}
	return sessions
}

// resolve finds the element a target names.
func (p *Page) resolve(ctx context.Context, t Target) (*element, error) {
	switch {
	case t.Ref != "":
		entry, err := p.lookupRef(t.Ref)
		if err != nil {
			return nil, err
		}
		obj, err := cdp.Call(ctx, entry.s, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: entry.node, ObjectGroup: objectGroup})
		if err != nil || obj.Object == nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &targetError{msg: fmt.Sprintf("ref %s (%s) is no longer on the page; take a new snapshot", t.Ref, entry.label), kind: "stale"}
		}
		return &element{s: entry.s, object: obj.Object.ObjectID, desc: entry.label}, nil
	case t.Point != nil:
		if err := p.inViewport(ctx, *t.Point); err != nil {
			return nil, err
		}
		return p.elementAt(ctx, *t.Point)
	}
	kind, query := "text", t.Text
	if t.Selector != "" {
		kind, query = "selector", t.Selector
	}
	var hidden int
	for _, sc := range p.scopes(ctx) {
		el, info, err := p.find(ctx, sc, kind, query)
		if err != nil {
			return nil, err
		}
		hidden += info.Hidden
		if el != nil {
			if info.Count > 1 {
				el.note = fmt.Sprintf("%d elements match %s; used the first, %s", info.Count, t, el.desc)
			}
			return el, nil
		}
	}
	if hidden > 0 {
		return nil, &targetError{msg: fmt.Sprintf("%s matches %d hidden element(s) and nothing visible", t, hidden), retry: true, kind: "hidden"}
	}
	return nil, &targetError{msg: fmt.Sprintf("nothing on the page matches %s", t), retry: true, kind: "missing"}
}

type findInfo struct {
	Count  int    `json:"count"`
	Hidden int    `json:"hidden"`
	Desc   string `json:"desc"`
	Error  string `json:"error"`
}

// scope is where the finder runs: a session's main frame, whose script
// reaches its same-origin iframes and open shadow roots, or an iframe of
// another origin in the same process (localhost:3000 embedding
// localhost:4000), searched from an isolated world of its own.
type scope struct {
	s       *session
	context cdpruntime.ExecutionContextID
}

// scopes lists where a text or selector target may be: every session, and
// every in-process frame whose parent's script cannot reach into it.
func (p *Page) scopes(ctx context.Context) []scope {
	p.mu.Lock()
	oopif := map[cdp.FrameID]bool{}
	for id := range p.frames {
		oopif[id] = true
	}
	p.mu.Unlock()
	var out []scope
	for _, s := range p.frameSessions() {
		out = append(out, scope{s: s})
		tree, err := cdp.Call(ctx, s, page.GetFrameTree, cdp.Empty{})
		if err != nil || tree.FrameTree == nil {
			continue
		}
		var walk func(t *page.FrameTree, origin string, depth int)
		walk = func(t *page.FrameTree, origin string, depth int) {
			for _, c := range t.ChildFrames {
				if c.Frame == nil || oopif[c.Frame.ID] || len(out) > 50 || depth > 10 {
					continue
				}
				if c.Frame.SecurityOrigin != origin {
					world, err := cdp.Call(ctx, s, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{FrameID: c.Frame.ID, WorldName: "julia"})
					if err == nil {
						out = append(out, scope{s: s, context: world.ExecutionContextID})
					}
				}
				walk(c, c.Frame.SecurityOrigin, depth+1)
			}
		}
		if tree.FrameTree.Frame != nil {
			walk(tree.FrameTree, tree.FrameTree.Frame.SecurityOrigin, 0)
		}
	}
	return out
}

// find runs the finder in a scope.
func (p *Page) find(ctx context.Context, sc scope, kind, query string) (*element, findInfo, error) {
	s := sc.s
	args, _ := json.Marshal([]string{kind, query})
	res, err := cdp.Call(ctx, s, cdpruntime.Evaluate, cdpruntime.EvaluateParams{
		Expression:  "(" + findScript + ")(..." + string(args) + ")",
		ObjectGroup: objectGroup,
		ContextID:   sc.context,
	})
	var info findInfo
	if err != nil {
		if ctx.Err() != nil {
			return nil, info, ctx.Err()
		}
		return nil, info, nil // a frame between documents has nothing to find
	}
	if res.ExceptionDetails != nil || res.Result == nil || res.Result.ObjectID == "" {
		return nil, info, nil
	}
	arr := res.Result.ObjectID
	raw, err := p.callValue(ctx, s, arr, "function() { return this[1]; }")
	if err != nil {
		return nil, info, err
	}
	json.Unmarshal(raw, &info, decodeOptions)
	if info.Error != "" {
		return nil, info, &targetError{msg: info.Error, kind: "invalid"}
	}
	if info.Count == 0 {
		return nil, info, nil
	}
	el, err := cdp.Call(ctx, s, cdpruntime.CallFunctionOn, cdpruntime.CallFunctionOnParams{
		FunctionDeclaration: "function() { return this[0]; }", ObjectID: arr, ObjectGroup: objectGroup,
	})
	if err != nil || el.Result == nil || el.Result.ObjectID == "" {
		return nil, info, err
	}
	return &element{s: s, object: el.Result.ObjectID, desc: info.Desc}, info, nil
}

// callValue calls fn on an object and returns its JSON result.
func (p *Page) callValue(ctx context.Context, s *session, obj cdpruntime.RemoteObjectID, fn string, args ...any) (jsontext.Value, error) {
	params := cdpruntime.CallFunctionOnParams{FunctionDeclaration: fn, ObjectID: obj, ReturnByValue: ptr(true), AwaitPromise: ptr(true), ObjectGroup: objectGroup}
	for _, a := range args {
		if id, ok := a.(cdpruntime.RemoteObjectID); ok {
			params.Arguments = append(params.Arguments, &cdpruntime.CallArgument{ObjectID: id})
			continue
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		params.Arguments = append(params.Arguments, &cdpruntime.CallArgument{Value: raw})
	}
	res, err := cdp.Call(ctx, s, cdpruntime.CallFunctionOn, params)
	if err != nil {
		return nil, err
	}
	if res.ExceptionDetails != nil {
		return nil, errors.New(exceptionText(res.ExceptionDetails))
	}
	if res.Result == nil {
		return nil, nil
	}
	return res.Result.Value, nil
}

// elementAt finds the element at a point in the page's viewport, as the
// mouse would: through same-process frames, but not into another process's.
func (p *Page) elementAt(ctx context.Context, pt Point) (*element, error) {
	missing := &targetError{msg: fmt.Sprintf("nothing is at %g,%g", pt.X, pt.Y), kind: "missing"}
	offset, err := p.documentOffset(ctx, p.root)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, missing
	}
	hit, _, err := p.nodeAt(ctx, p.root, offset, pt)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, missing
	}
	obj, err := cdp.Call(ctx, p.root, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: hit, ObjectGroup: objectGroup})
	if err != nil || obj.Object == nil {
		return nil, missing
	}
	el := &element{s: p.root, object: obj.Object.ObjectID}
	raw, _ := p.callValue(ctx, p.root, el.object, "function() { return ("+describeScript+")(this.nodeType === 1 ? this : this.parentElement); }")
	json.Unmarshal(raw, &el.desc, decodeOptions)
	return el, nil
}

// inViewport checks that a point is inside the page's viewport, where a
// mouse can be. A point read off a full-page screenshot is in the page's
// coordinates instead, and below the fold.
func (p *Page) inViewport(ctx context.Context, pt Point) error {
	m, err := cdp.Call(ctx, p.root, page.GetLayoutMetrics, cdp.Empty{})
	if err != nil || m.CSSVisualViewport == nil {
		return fail(ctx, err)
	}
	vv := m.CSSVisualViewport
	if pt.X < vv.ClientWidth && pt.Y < vv.ClientHeight {
		return nil
	}
	return &targetError{msg: fmt.Sprintf("%g,%g is outside the %d×%d viewport (scrolled to %d,%d): x,y are viewport pixels; scroll first, or target a ref", pt.X, pt.Y, int(vv.ClientWidth), int(vv.ClientHeight), int(vv.PageX), int(vv.PageY)), kind: "invalid"}
}

type checkResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Desc  string `json:"desc"`
}

// actionable resolves a target and checks it can take the action: it is
// scrolled into view, visible, enabled, not moving and, for a click, what
// the mouse would hit at the point it will use. It waits up to TargetTimeout
// for that. It returns the element and the point, in the page's viewport.
func (p *Page) actionable(ctx context.Context, t Target, action string) (*element, Point, error) {
	if t.Point != nil {
		// A point clicks or hovers whatever is there, even nothing.
		el, err := p.resolve(ctx, t)
		var te *targetError
		if err != nil && (action != "click" && action != "hover" || !errors.As(err, &te) || te.kind != "missing") {
			return nil, Point{}, err
		}
		return el, *t.Point, nil
	}
	deadline := time.Now().Add(TargetTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-100 * time.Millisecond)
	}
	delay := 50 * time.Millisecond
	// When time runs out, why the target was not yet actionable says more
	// than that time ran out.
	var why error
	for {
		last := !time.Now().Before(deadline)
		el, pt, err := p.tryActionable(ctx, t, action, last)
		if ctx.Err() != nil {
			return nil, Point{}, cmpErr(why, ctx.Err())
		}
		if err == nil || !retryable(err) || last {
			return el, pt, err
		}
		why = err
		select {
		case <-ctx.Done():
			return nil, Point{}, why
		case <-time.After(min(delay, time.Until(deadline)+time.Millisecond)):
		}
		delay = min(delay*2, 500*time.Millisecond)
	}
}

// cmpErr is the first error that is not nil.
func cmpErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Page) tryActionable(ctx context.Context, t Target, action string, last bool) (*element, Point, error) {
	el, err := p.resolve(ctx, t)
	if err != nil {
		return nil, Point{}, err
	}
	if action == "type" || action == "select" || action == "files" {
		// A label stands for its control, as clicking it would.
		res, err := cdp.Call(ctx, el.s, cdpruntime.CallFunctionOn, cdpruntime.CallFunctionOnParams{
			FunctionDeclaration: "function() { return this.localName === 'label' && this.control ? this.control : this; }",
			ObjectID:            el.object, ObjectGroup: objectGroup,
		})
		if err == nil && res.Result != nil && res.Result.ObjectID != "" {
			el.object = res.Result.ObjectID
		}
	}
	raw, err := p.callValue(ctx, el.s, el.object, checkScript, action)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Point{}, ctx.Err()
		}
		return nil, Point{}, &targetError{msg: fmt.Sprintf("%s: %v", t, err), retry: true}
	}
	var check checkResult
	json.Unmarshal(raw, &check, decodeOptions)
	if check.Desc != "" {
		el.desc = check.Desc
	}
	if !check.OK && !(check.Error == "moving" && last) {
		return nil, Point{}, checkError(t, el, check.Error)
	}
	if action == "type" || action == "select" || action == "files" {
		return el, Point{}, nil // these act on the element, not at a point
	}
	box, err := p.visibleBox(ctx, el)
	if err != nil {
		return nil, Point{}, err
	}
	pt := box.centre()
	click := action != "scroll" && action != "hover"
	if click {
		if pt, err = p.hitTest(ctx, el, box); err != nil {
			return nil, Point{}, err
		}
	}
	at, err := p.toPage(ctx, el.s, pt, click, el)
	if err != nil {
		return nil, Point{}, err
	}
	return el, at, nil
}

func checkError(t Target, el *element, code string) error {
	what := el.desc
	if what == "" {
		what = t.String()
	}
	switch code {
	case "detached":
		if t.Ref != "" {
			return &targetError{msg: fmt.Sprintf("ref %s (%s) is no longer on the page; take a new snapshot", t.Ref, what), kind: "stale"}
		}
		return &targetError{msg: what + " left the page", retry: true}
	case "hidden":
		return &targetError{msg: what + " is not visible (hidden, or of zero size)", retry: true, kind: "hidden"}
	case "disabled":
		return &targetError{msg: what + " is disabled", retry: true, kind: "disabled"}
	case "no-pointer":
		return &targetError{msg: what + " has pointer-events: none, so a click goes through it", kind: "no-pointer"}
	case "moving":
		return &targetError{msg: what + " keeps moving (animating)", retry: true}
	case "not-editable":
		return &targetError{msg: what + " is not a text field or editable element", kind: "invalid"}
	case "readonly":
		return &targetError{msg: what + " is read-only", kind: "invalid"}
	}
	return &targetError{msg: fmt.Sprintf("%s: %s", what, code), kind: "invalid"}
}

// area is a rectangle in a session's viewport.
type area struct{ x0, y0, x1, y1 float64 }

func (a area) centre() Point {
	return Point{math.Round((a.x0+a.x1)/2*100) / 100, math.Round((a.y0+a.y1)/2*100) / 100}
}

// samples are where to try the mouse in a: the centre first, then a grid
// over the rest, for a target partly covered or cut off.
func (a area) samples() []Point {
	points := []Point{a.centre()}
	for _, fy := range []float64{1. / 6, 1. / 2, 5. / 6} {
		for _, fx := range []float64{1. / 6, 1. / 2, 5. / 6} {
			if fx != 1./2 || fy != 1./2 {
				points = append(points, Point{math.Round((a.x0+(a.x1-a.x0)*fx)*100) / 100, math.Round((a.y0+(a.y1-a.y0)*fy)*100) / 100})
			}
		}
	}
	return points
}

// visibleBox is the largest part of the element's box within its session's
// viewport (and the page's, for the page's own), where a mouse can be.
func (p *Page) visibleBox(ctx context.Context, el *element) (area, error) {
	quads, err := cdp.Call(ctx, el.s, dom.GetContentQuads, dom.GetContentQuadsParams{ObjectID: el.object})
	if err != nil || len(quads.Quads) == 0 {
		if ctx.Err() != nil {
			return area{}, ctx.Err()
		}
		return area{}, &targetError{msg: el.desc + " has no box on screen", retry: true, kind: "hidden"}
	}
	vw, vh := math.Inf(1), math.Inf(1)
	if el.s == p.root {
		if m, err := cdp.Call(ctx, p.root, page.GetLayoutMetrics, cdp.Empty{}); err == nil && m.CSSVisualViewport != nil {
			vw, vh = m.CSSVisualViewport.ClientWidth, m.CSSVisualViewport.ClientHeight
		}
	}
	best, bestArea := area{}, -1.0
	for _, q := range quads.Quads {
		if len(q) < 8 {
			continue
		}
		a := area{max(min(q[0], q[2], q[4], q[6]), 0), max(min(q[1], q[3], q[5], q[7]), 0), min(max(q[0], q[2], q[4], q[6]), vw), min(max(q[1], q[3], q[5], q[7]), vh)}
		if size := (a.x1 - a.x0) * (a.y1 - a.y0); a.x1 > a.x0 && a.y1 > a.y0 && size > bestArea {
			best, bestArea = a, size
		}
	}
	if bestArea < 0 {
		return area{}, &targetError{msg: el.desc + " is outside the viewport and could not be scrolled into it", retry: true, kind: "hidden"}
	}
	return best, nil
}

// verdict is what the mouse would hit, as hitScript sees it.
type verdict struct {
	OK     bool   `json:"ok"`
	Cover  string `json:"cover"`  // what is hit instead
	Within bool   `json:"within"` // that holds the element: it is cut off there, not covered
	Clip   string `json:"clip"`   // what cuts it off, and how
}

// hitTest finds where in its visible box the mouse would hit the element
// itself, as a person's click would, rather than a modal backdrop, a cookie
// banner or a sticky header over it, or the container it is cut off from:
// the centre if it can, else another point of the box. It fails, naming
// what is in the way, when no point does.
func (p *Page) hitTest(ctx context.Context, el *element, box area) (Point, error) {
	unknown := &targetError{msg: fmt.Sprintf("could not tell what the mouse would hit over %s", el.desc), retry: true, kind: "covered"}
	target, err := p.ownWorld(ctx, el)
	if err != nil {
		return Point{}, unknown
	}
	at, err := p.documentOffset(ctx, el.s)
	if err != nil {
		return Point{}, unknown
	}
	var parents map[cdp.FrameID]cdp.FrameID
	var covered, cut *verdict
	var coveredAt Point
	for _, pt := range box.samples() {
		v, err := p.hitAt(ctx, el.s, at, pt, target, &parents)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return Point{}, ctx.Err()
			}
		case v.OK:
			return pt, nil
		case !v.Within && covered == nil:
			covered, coveredAt = &v, pt
		case v.Within && cut == nil:
			cut = &v
		}
	}
	switch {
	case covered != nil:
		return Point{}, &targetError{msg: fmt.Sprintf("%s is covered by %s at %g,%g, which would get the click; close or move what covers it first", el.desc, covered.Cover, coveredAt.X, coveredAt.Y), retry: true, kind: "covered"}
	case cut != nil:
		by := cut.Clip
		if by == "" {
			by = cut.Cover
		}
		return Point{}, &targetError{msg: fmt.Sprintf("%s lies outside the visible box of %s, so a person cannot click it; scroll or open what reveals it", el.desc, by), retry: true, kind: "hidden"}
	}
	return Point{}, unknown
}

// hitAt judges what the mouse would hit at pt, in s's viewport: target, a
// node in its own document's main world, or something else. When the node
// hit is in another document, the frame holding that document stands in for
// it in the document around it, until the two are in one.
func (p *Page) hitAt(ctx context.Context, s *session, at viewportAt, pt Point, target cdpruntime.RemoteObjectID, parents *map[cdp.FrameID]cdp.FrameID) (verdict, error) {
	node, frame, err := p.nodeAt(ctx, s, at, pt)
	if err != nil {
		return verdict{}, err
	}
	hit := node
	for hops := 0; hops < 10; hops++ {
		obj, err := cdp.Call(ctx, s, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: hit, ObjectGroup: objectGroup})
		if err != nil || obj.Object == nil {
			return verdict{}, fmt.Errorf("resolve the node hit: %w", err)
		}
		if raw, err := p.callValue(ctx, s, target, hitScript, obj.Object.ObjectID); err == nil {
			var v verdict
			if err := json.Unmarshal(raw, &v, decodeOptions); err != nil {
				return verdict{}, err
			}
			if hops > 0 && !v.OK && !v.Within {
				v.Cover = p.describeNode(ctx, s, node) // what is hit, not the frame it is in
			}
			return v, nil
		}
		// Not the target's document: try the frame element holding it.
		if *parents == nil {
			*parents = p.frameParents(ctx, s)
		}
		parent, ok := (*parents)[frame]
		if !ok {
			break
		}
		owner, err := cdp.Call(ctx, s, dom.GetFrameOwner, dom.GetFrameOwnerParams{FrameID: frame})
		if err != nil {
			break
		}
		hit, frame = owner.BackendNodeID, parent
	}
	// The target is in a document inside the one hit: what is hit is over it.
	return verdict{Cover: p.describeNode(ctx, s, node)}, nil
}

// ownWorld is el's node as its own document's main world sees it, where
// DOM.resolveNode puts the nodes it resolves: el may have been found from
// a parent document's world, or an isolated one.
func (p *Page) ownWorld(ctx context.Context, el *element) (cdpruntime.RemoteObjectID, error) {
	node, err := cdp.Call(ctx, el.s, dom.DescribeNode, dom.DescribeNodeParams{ObjectID: el.object})
	if err != nil || node.Node == nil {
		return "", fmt.Errorf("describe the target: %w", err)
	}
	obj, err := cdp.Call(ctx, el.s, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: node.Node.BackendNodeID, ObjectGroup: objectGroup})
	if err != nil || obj.Object == nil {
		return "", fmt.Errorf("resolve the target: %w", err)
	}
	return obj.Object.ObjectID, nil
}

// frameParents maps each frame of a session to the frame holding it.
func (p *Page) frameParents(ctx context.Context, s *session) map[cdp.FrameID]cdp.FrameID {
	parents := map[cdp.FrameID]cdp.FrameID{}
	tree, err := cdp.Call(ctx, s, page.GetFrameTree, cdp.Empty{})
	if err != nil || tree.FrameTree == nil {
		return parents
	}
	var walk func(t *page.FrameTree)
	walk = func(t *page.FrameTree) {
		for _, c := range t.ChildFrames {
			if c.Frame != nil && t.Frame != nil && len(parents) < 500 {
				parents[c.Frame.ID] = t.Frame.ID
				walk(c)
			}
		}
	}
	walk(tree.FrameTree)
	return parents
}

// describeNode names a node of a session briefly, as describeScript does.
func (p *Page) describeNode(ctx context.Context, s *session, node cdp.BackendNodeID) string {
	obj, err := cdp.Call(ctx, s, dom.ResolveNode, dom.ResolveNodeParams{BackendNodeID: node, ObjectGroup: objectGroup})
	if err != nil || obj.Object == nil {
		return "something"
	}
	raw, _ := p.callValue(ctx, s, obj.Object.ObjectID, "function() { return ("+describeScript+")(this.nodeType === 1 ? this : this.parentElement); }")
	desc := "something"
	json.Unmarshal(raw, &desc, decodeOptions)
	return desc
}

// viewportAt is where a session's visual viewport is in its document: what
// turns a point in the viewport, which the mouse and DOM.getContentQuads use
// (CSS pixels, even on a phone's page shown zoomed out), into one in the
// document, which DOM.getNodeForLocation takes.
type viewportAt struct{ x, y float64 }

func (p *Page) documentOffset(ctx context.Context, s *session) (viewportAt, error) {
	m, err := cdp.Call(ctx, s, page.GetLayoutMetrics, cdp.Empty{})
	if err != nil || m.CSSVisualViewport == nil {
		return viewportAt{}, fmt.Errorf("layout metrics: %w", err)
	}
	return viewportAt{m.CSSVisualViewport.PageX, m.CSSVisualViewport.PageY}, nil
}

// nodeAt is the node the mouse would hit at pt in s's viewport, and the
// frame whose document holds it.
func (p *Page) nodeAt(ctx context.Context, s *session, at viewportAt, pt Point) (cdp.BackendNodeID, cdp.FrameID, error) {
	loc, err := cdp.Call(ctx, s, dom.GetNodeForLocation, dom.GetNodeForLocationParams{X: int64(math.Round(at.x + pt.X)), Y: int64(math.Round(at.y + pt.Y)), IgnorePointerEventsNone: ptr(false)})
	if err != nil {
		return 0, "", err
	}
	return loc.BackendNodeID, loc.FrameID, nil
}

// toPage turns a point in session s's viewport into one in the page's,
// crossing each out-of-process frame on the way. With check, the mouse at
// each step must hit the frame's own iframe element, so that nothing in a
// document around the frame covers it there.
func (p *Page) toPage(ctx context.Context, s *session, pt Point, check bool, el *element) (Point, error) {
	for s != p.root && s.parent != nil {
		owner, err := cdp.Call(ctx, s.parent, dom.GetFrameOwner, dom.GetFrameOwnerParams{FrameID: cdp.FrameID(s.targetID)})
		if err != nil {
			return pt, fmt.Errorf("locate frame: %w", err)
		}
		box, err := cdp.Call(ctx, s.parent, dom.GetBoxModel, dom.GetBoxModelParams{BackendNodeID: owner.BackendNodeID})
		if err != nil || box.Model == nil || len(box.Model.Content) < 2 {
			return pt, &targetError{msg: "the frame holding the element is not on screen", retry: true, kind: "hidden"}
		}
		pt = Point{pt.X + box.Model.Content[0], pt.Y + box.Model.Content[1]}
		if check {
			at, err := p.documentOffset(ctx, s.parent)
			if err != nil {
				return pt, err
			}
			hit, _, err := p.nodeAt(ctx, s.parent, at, pt)
			if err != nil {
				return pt, &targetError{msg: fmt.Sprintf("could not tell what the mouse would hit over %s", el.desc), retry: true, kind: "covered"}
			}
			if hit != owner.BackendNodeID {
				cover := p.describeNode(ctx, s.parent, hit)
				return pt, &targetError{msg: fmt.Sprintf("%s is covered by %s at %g,%g, outside the frame that holds it, which would get the click; close or move what covers it first", el.desc, cover, pt.X, pt.Y), retry: true, kind: "covered"}
			}
		}
		s = s.parent
	}
	return pt, nil
}

// frameOffset is where a session's viewport sits in the page's: zero for the
// page, the iframe's content box for an out-of-process frame.
func (p *Page) frameOffset(ctx context.Context, s *session) (Point, error) {
	return p.toPage(ctx, s, Point{}, false, nil)
}

// mouse sends one mouse event to the page.
func (p *Page) mouse(ctx context.Context, typ input.DispatchMouseEventType, pt Point, button input.MouseButton, clicks int64, modifiers int64) error {
	buttons := map[input.MouseButton]int64{input.MouseButtonLeft: 1, input.MouseButtonRight: 2, input.MouseButtonMiddle: 4}
	params := input.DispatchMouseEventParams{Type: typ, X: pt.X, Y: pt.Y, Button: button, ClickCount: clicks, Modifiers: modifiers}
	if typ == input.DispatchMouseEventTypeMousePressed || (typ == input.DispatchMouseEventTypeMouseMoved && button != input.MouseButtonNone && button != "") {
		params.Buttons = buttons[button]
	}
	if typ == input.DispatchMouseEventTypeMouseMoved && params.Buttons == 0 {
		params.Button = input.MouseButtonNone
	}
	_, err := p.root.call(ctx, input.CommandDispatchMouseEvent, params)
	return err
}

// describeScript names an element briefly: <button#save.primary> "Save".
const describeScript = `function describe(el) {
  if (!el || el.nodeType !== 1) return el ? el.nodeName.toLowerCase() : 'nothing';
  let s = '<' + el.localName;
  if (el.id) s += '#' + el.id;
  const cls = typeof el.className === 'string' ? el.className.trim().split(/\s+/).filter(Boolean).slice(0, 3) : [];
  if (cls.length) s += '.' + cls.join('.');
  s += '>';
  let label = el.getAttribute('aria-label') || el.getAttribute('alt') || el.getAttribute('placeholder') || '';
  if (!label) {
    const text = el.textContent || '';
    label = text.length > 2000 ? text.slice(0, 2000) : text;
  }
  if (!label && el.localName === 'input') label = el.value || '';
  label = label.replace(/\s+/g, ' ').trim();
  if (label) s += ' "' + (label.length > 40 ? label.slice(0, 40) + '…' : label) + '"';
  return s;
}`

// findScript finds the element for a text or selector target. It returns
// [element, {count, hidden, desc, error}].
const findScript = `function(kind, query) {
  const describe = ` + describeScript + `;
  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
  const visible = el => {
    if (!el.isConnected) return false;
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) return false;
    return el.checkVisibility ? el.checkVisibility({visibilityProperty: true}) : true;
  };
  const roots = [];
  const collect = root => {
    roots.push(root);
    const doc = root.ownerDocument || root;
    const walker = doc.createTreeWalker(root, 1);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      if (n.shadowRoot) collect(n.shadowRoot);
      if (n.localName === 'iframe' || n.localName === 'frame') {
        try { const d = n.contentDocument; if (d && d.documentElement) collect(d); } catch (e) {}
      }
    }
  };
  collect(document);
  let matches = [];
  if (kind === 'selector') {
    for (const root of roots) {
      try { matches.push(...root.querySelectorAll(query)); }
      catch (e) { return [null, {error: 'invalid CSS selector ' + JSON.stringify(query) + ': ' + e.message}]; }
    }
  } else {
    const needle = norm(query).toLowerCase();
    if (!needle) return [null, {error: 'text to find is empty'}];
    const skip = new Set(['script', 'style', 'noscript', 'template', 'head', 'title', 'option', 'optgroup']);
    const exact = [], partial = [], seen = new Set();
    const consider = (el, text) => {
      if (!el || seen.has(el) || skip.has(el.localName)) return;
      const t = norm(text).toLowerCase();
      if (!t.includes(needle)) return;
      seen.add(el);
      (t === needle ? exact : partial).push(el);
    };
    for (const root of roots) {
      const doc = root.ownerDocument || root;
      const walker = doc.createTreeWalker(root, 4);
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        if (n.data.length < needle.length || !norm(n.data).toLowerCase().includes(needle)) continue;
        const el = n.parentElement || (n.parentNode && n.parentNode.host);
        if (el) consider(el, el.textContent);
      }
      for (const el of root.querySelectorAll('[aria-label], img[alt], input[placeholder], textarea[placeholder], input[type=button], input[type=submit], input[type=reset], [title]')) {
        const label = el.getAttribute('aria-label') || el.getAttribute('alt') || el.getAttribute('placeholder') ||
          (/^(button|submit|reset)$/.test(el.type) ? el.value : '') || el.getAttribute('title');
        consider(el, label);
      }
    }
    if (!exact.length && !partial.length) {
      // Text split across elements, such as "Read <em>more</em>".
      for (const root of roots) {
        for (const el of root.querySelectorAll('*')) {
          if (skip.has(el.localName) || !norm(el.textContent).toLowerCase().includes(needle)) continue;
          if (![...el.children].some(c => norm(c.textContent).toLowerCase().includes(needle))) consider(el, el.textContent);
        }
      }
    }
    matches = exact.some(visible) ? exact : partial;
  }
  const shown = matches.filter(visible);
  const hidden = matches.length - shown.length;
  if (!shown.length) return [null, {count: 0, hidden}];
  return [shown[0], {count: shown.length, hidden, desc: describe(shown[0])}];
}`

// checkScript scrolls an element into view and says whether it can take
// the action, waiting two animation frames to see that it is not moving.
const checkScript = `async function(action) {
  const describe = ` + describeScript + `;
  let el = this.nodeType === 1 ? this : this.parentElement;
  if (!el || !el.isConnected) return {error: 'detached'};
  if ((action === 'type' || action === 'select' || action === 'files') && el.localName === 'label' && el.control) el = el.control;
  const desc = describe(el);
  if (action !== 'files') {
    // A person scrolls the page and boxes that scroll, not a box whose
    // overflow is hidden, which scrolling into view would scroll as well:
    // its offsets are put back, and the hit test finds the element cut off.
    const up = e => e.parentElement || (e.parentNode && e.parentNode.host);
    const kept = [];
    for (let a = up(el); a; a = up(a)) {
      if (a === document.body || a === document.documentElement) continue;
      const st = getComputedStyle(a);
      if (st.overflowX === 'hidden' || st.overflowY === 'hidden') kept.push([a, st.overflowX === 'hidden', a.scrollLeft, st.overflowY === 'hidden', a.scrollTop]);
    }
    if (el.scrollIntoViewIfNeeded) el.scrollIntoViewIfNeeded(true); else el.scrollIntoView({block: 'center', inline: 'center'});
    for (const [a, x, left, y, top] of kept) {
      if (x) a.scrollLeft = left;
      if (y) a.scrollTop = top;
    }
  }
  const box = () => { const r = el.getBoundingClientRect(); return [r.left, r.top, r.width, r.height].join(); };
  const before = box();
  for (let i = 0; i < 2; i++) await new Promise(r => { requestAnimationFrame(() => r()); setTimeout(r, 100); });
  if (!el.isConnected) return {error: 'detached', desc};
  const r = el.getBoundingClientRect();
  const visible = r.width > 0 && r.height > 0 && (!el.checkVisibility || el.checkVisibility({visibilityProperty: true}));
  if (action === 'files') return el.localName === 'input' && el.type === 'file' ? {ok: true, desc} : {error: 'not a file input (<input type=file>)', desc};
  if (action === 'select') {
    if (el.localName !== 'select') return {error: 'not a <select>; for a custom dropdown, click it and then its option', desc};
    if (el.disabled) return {error: 'disabled', desc};
    return {ok: true, desc};
  }
  if (!visible) return {error: 'hidden', desc};
  if (box() !== before) return {error: 'moving', desc};
  if (action !== 'hover' && action !== 'scroll' && getComputedStyle(el).pointerEvents === 'none') return {error: 'no-pointer', desc};
  if (action === 'click' || action === 'type') {
    if (el.matches(':disabled') || el.closest('[aria-disabled="true"]')) return {error: 'disabled', desc};
  }
  return {ok: true, desc};
}`

// hitScript judges the node the mouse would hit, given as hit, for the
// element this is: ok when it is the element, inside it, or one of its
// labels. Otherwise it names it and, when it holds the element (the click
// would land on a container the element is cut off from), what cuts it off.
const hitScript = `function(hit) {
  const describe = ` + describeScript + `;
  const target = this.nodeType === 1 ? this : this.parentElement;
  const n = hit && hit.nodeType !== 1 ? (hit.parentElement || hit.parentNode) : hit;
  const up = x => x.parentNode || x.host;
  const inside = (x, y) => { for (; x; x = up(x)) if (x === y) return true; return false; };
  if (!n || inside(n, target)) return {ok: true};
  if (target.labels) for (const l of target.labels) if (inside(n, l)) return {ok: true};
  if (n.localName === 'label' && n.control === target) return {ok: true};
  if (!inside(target, n)) return {cover: describe(n)};
  for (let a = up(target); a && a.nodeType === 1; a = up(a)) {
    const st = getComputedStyle(a);
    const clips = [...new Set([st.overflowX, st.overflowY].filter(o => o !== 'visible'))];
    if (clips.length) return {cover: describe(n), within: true, clip: describe(a).replace(/ ".*$/s, '') + ' (overflow: ' + clips.join(' ') + ')'};
  }
  return {cover: describe(n), within: true};
}`

func exceptionText(d *cdpruntime.ExceptionDetails) string {
	text := d.Text
	if d.Exception != nil && d.Exception.Description != "" {
		text = d.Exception.Description
	} else if d.Exception != nil && len(d.Exception.Value) > 0 {
		text = "Uncaught " + remoteText(d.Exception)
	}
	return clip(text, 2000)
}
