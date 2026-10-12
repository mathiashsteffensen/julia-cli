package browser

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
)

// ActionTimeout bounds an input action, its wait for the target included,
// when ctx has no deadline.
const ActionTimeout = 30 * time.Second

// ClickOptions vary a click.
type ClickOptions struct {
	Button    string   // left (default), right or middle
	Count     int      // 1 (default), 2 for a double click
	Modifiers []string // keys held: ctrl, alt, shift, meta
}

// Click clicks the target's centre with real mouse events, after scrolling
// it into view and checking it is visible, enabled, still and not covered by
// something else. If the click starts a navigation, Click waits for it.
func (p *Page) Click(ctx context.Context, t Target) (string, error) {
	return p.ClickWith(ctx, t, ClickOptions{})
}

// DoubleClick double-clicks the target.
func (p *Page) DoubleClick(ctx context.Context, t Target) (string, error) {
	return p.ClickWith(ctx, t, ClickOptions{Count: 2})
}

// RightClick right-clicks the target, opening its context menu.
func (p *Page) RightClick(ctx context.Context, t Target) (string, error) {
	return p.ClickWith(ctx, t, ClickOptions{Button: "right"})
}

// ClickWith clicks the target as opts say. It returns a remark about how the
// target was chosen, if there is one worth making.
func (p *Page) ClickWith(ctx context.Context, t Target, opts ClickOptions) (string, error) {
	if err := t.check(); err != nil {
		return "", err
	}
	button := input.MouseButton(cmpOr(opts.Button, "left"))
	if button != input.MouseButtonLeft && button != input.MouseButtonRight && button != input.MouseButtonMiddle {
		return "", errors.New("button must be left, right or middle")
	}
	count := max(opts.Count, 1)
	if count > 3 {
		return "", errors.New("click at most 3 times at once")
	}
	modifiers, err := modifierMask(opts.Modifiers)
	if err != nil {
		return "", err
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return "", err
	}
	defer done()
	defer p.release()
	el, pt, err := p.actionable(ctx, t, "click")
	if err != nil {
		return "", fail(ctx, err)
	}
	before := p.snapshotNav()
	if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, pt, input.MouseButtonNone, 0, modifiers); err != nil {
		return "", fail(ctx, err)
	}
	for i := 1; i <= count; i++ {
		if err := p.mouse(ctx, input.DispatchMouseEventTypeMousePressed, pt, button, int64(i), modifiers); err != nil {
			return "", fail(ctx, err)
		}
		if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseReleased, pt, button, int64(i), modifiers); err != nil {
			return "", fail(ctx, err)
		}
	}
	return remark(el), fail(ctx, p.settle(ctx, before))
}

// Hover moves the mouse over the target.
func (p *Page) Hover(ctx context.Context, t Target) (string, error) {
	if err := t.check(); err != nil {
		return "", err
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return "", err
	}
	defer done()
	defer p.release()
	el, pt, err := p.actionable(ctx, t, "hover")
	if err != nil {
		return "", fail(ctx, err)
	}
	before := p.snapshotNav()
	if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, pt, input.MouseButtonNone, 0, 0); err != nil {
		return "", fail(ctx, err)
	}
	return remark(el), fail(ctx, p.settle(ctx, before))
}

func remark(el *element) string {
	if el == nil {
		return ""
	}
	return el.note
}

func modifierMask(names []string) (int64, error) {
	var mask int64
	for _, name := range names {
		switch strings.ToLower(name) {
		case "ctrl", "control":
			mask |= modCtrl
		case "alt", "option":
			mask |= modAlt
		case "shift":
			mask |= modShift
		case "meta", "cmd", "command":
			mask |= modMeta
		default:
			return 0, fmt.Errorf("unknown modifier %q (use ctrl, alt, shift or meta)", name)
		}
	}
	return mask, nil
}

// Type replaces the content of a text field, text area or editable element
// with text, typed key by key with real key events, so the page's keydown,
// input and keyup handlers (and React's onChange) all run. A newline presses
// Enter. Text over 500 characters, and characters a US keyboard lacks, are
// inserted as text input instead. Date, time, color and range inputs get
// their value set and input and change events fired. submit presses Enter
// afterwards.
func (p *Page) Type(ctx context.Context, t Target, text string, submit bool) (string, error) {
	if err := t.check(); err != nil {
		return "", err
	}
	if len(text) > 1<<20 {
		return "", errors.New("text is longer than 1 MiB")
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return "", err
	}
	defer done()
	defer p.release()
	el, pt, err := p.actionable(ctx, t, "type")
	if err != nil {
		return "", fail(ctx, err)
	}
	before := p.snapshotNav()
	if t.Point != nil {
		// A point names where to click; whatever takes focus gets the text.
		if err := p.clickAt(ctx, pt); err != nil {
			return "", fail(ctx, err)
		}
		if el, err = p.focused(ctx); err != nil {
			return "", fail(ctx, err)
		}
	}
	raw, err := p.callValue(ctx, el.s, el.object, focusScript, text)
	if err != nil {
		return "", fail(ctx, err)
	}
	var res struct {
		OK    bool   `json:"ok"`
		Set   bool   `json:"set"`
		Error string `json:"error"`
		Desc  string `json:"desc"`
	}
	json.Unmarshal(raw, &res, decodeOptions)
	if !res.OK {
		el.desc = cmpOr(res.Desc, el.desc)
		return "", checkError(t, el, res.Error)
	}
	if !res.Set {
		if text == "" {
			err = p.press(ctx, chord{def: namedKeys["Backspace"]})
		} else {
			err = p.typeText(ctx, text)
		}
		if err != nil {
			return "", fail(ctx, err)
		}
	}
	if submit {
		if err := p.press(ctx, chord{def: namedKeys["Enter"]}); err != nil {
			return "", fail(ctx, err)
		}
	}
	return remark(el), fail(ctx, p.settle(ctx, before))
}

func (p *Page) clickAt(ctx context.Context, pt Point) error {
	for _, typ := range []input.DispatchMouseEventType{input.DispatchMouseEventTypeMouseMoved, input.DispatchMouseEventTypeMousePressed, input.DispatchMouseEventTypeMouseReleased} {
		button := input.MouseButtonLeft
		if typ == input.DispatchMouseEventTypeMouseMoved {
			button = input.MouseButtonNone
		}
		if err := p.mouse(ctx, typ, pt, button, 1, 0); err != nil {
			return err
		}
	}
	return nil
}

// focused is the element that has focus, in whichever frame.
func (p *Page) focused(ctx context.Context) (*element, error) {
	for _, s := range p.frameSessions() {
		res, err := p.evalObject(ctx, s, `(() => { let a = document.activeElement; while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement; return a && a !== document.body ? a : null; })()`)
		if err == nil && res != "" {
			return &element{s: s, object: res}, nil
		}
	}
	return nil, &targetError{msg: "nothing took focus at that point; aim at a text field", kind: "invalid"}
}

// Press presses keys in order, each with real key events (keydown, keypress,
// keyup) sent to whatever has focus. Nothing is sent unless every key
// parses. A key is
//
//   - a single character: "a", "A", "?", "é" (one not on a US keyboard is
//     inserted as text, without key events)
//   - a name, in any case: Enter, Tab, Escape (esc), Backspace, Delete,
//     ArrowUp, ArrowDown, ArrowLeft, ArrowRight (up, down, left, right),
//     Home, End, PageUp, PageDown (pgup, pgdown), Insert, Space, F1–F12, or a
//     modifier alone: Shift, Control, Alt, Meta
//   - modifiers joined with "+": ctrl+a, shift+Tab, ctrl+shift+ArrowLeft,
//     alt++ (alt and the plus key), with ctrl (control), alt (option), shift
//     and meta (cmd: the Command key on a Mac). The Terminal tool's C-x,
//     M-x (alt) and S-x work too.
//
// ctrl or meta with a, z, y, x, c and v select all, undo, redo, cut, copy
// and paste on every platform, as an agent expects whatever the host.
func (p *Page) Press(ctx context.Context, keys ...string) error {
	if len(keys) > 100 {
		return errors.New("press at most 100 keys at once")
	}
	chords := make([]chord, len(keys))
	for i, spec := range keys {
		k, err := parseKey(spec)
		if err != nil {
			return err
		}
		chords[i] = k
	}
	if len(chords) == 0 {
		return errors.New("no keys given")
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return err
	}
	defer done()
	before := p.snapshotNav()
	for _, k := range chords {
		if err := p.press(ctx, k); err != nil {
			return fail(ctx, err)
		}
	}
	return fail(ctx, p.settle(ctx, before))
}

// Select chooses options of a <select> by value or visible label, firing
// input and change events, and returns the labels now selected. A custom
// dropdown is not a <select>: click it and its option instead.
func (p *Page) Select(ctx context.Context, t Target, values ...string) ([]string, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, errors.New("give at least one option to select")
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return nil, err
	}
	defer done()
	defer p.release()
	el, _, err := p.actionable(ctx, t, "select")
	if err != nil {
		return nil, fail(ctx, err)
	}
	before := p.snapshotNav()
	raw, err := p.callValue(ctx, el.s, el.object, selectScript, values)
	if err != nil {
		return nil, fail(ctx, err)
	}
	var res struct {
		Error    string   `json:"error"`
		Selected []string `json:"selected"`
	}
	json.Unmarshal(raw, &res, decodeOptions)
	if res.Error != "" {
		return nil, fmt.Errorf("%s: %s", el.desc, res.Error)
	}
	return res.Selected, fail(ctx, p.settle(ctx, before))
}

// SetFiles gives a file input the files at paths, firing input and change
// events, as choosing them in a file dialog does. The caller confines paths;
// they must be absolute.
func (p *Page) SetFiles(ctx context.Context, t Target, paths ...string) error {
	if err := t.check(); err != nil {
		return err
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("file path %q is not absolute", path)
		}
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return err
	}
	defer done()
	defer p.release()
	el, _, err := p.actionable(ctx, t, "files")
	if err != nil {
		return fail(ctx, err)
	}
	raw, err := p.callValue(ctx, el.s, el.object, "function() { const el = this.localName === 'label' && this.control ? this.control : this; return el.multiple; }")
	if err != nil {
		return fail(ctx, err)
	}
	if string(raw) != "true" && len(paths) > 1 {
		return fmt.Errorf("%s takes one file, not %d", el.desc, len(paths))
	}
	before := p.snapshotNav()
	files := paths
	if files == nil {
		files = []string{}
	}
	if _, err := cdp.Call(ctx, el.s, dom.SetFileInputFiles, dom.SetFileInputFilesParams{Files: files, ObjectID: el.object}); err != nil {
		return fail(ctx, err)
	}
	return fail(ctx, p.settle(ctx, before))
}

// Scroll turns the mouse wheel over the target (the page when t is nil) by
// dx, dy CSS pixels and says what scrolled and where it ended up.
func (p *Page) Scroll(ctx context.Context, t *Target, dx, dy float64) (string, error) {
	if math.IsNaN(dx) || math.IsNaN(dy) || math.Abs(dx) > 1e6 || math.Abs(dy) > 1e6 {
		return "", errors.New("scroll by a finite distance")
	}
	if t != nil {
		if err := t.check(); err != nil {
			return "", err
		}
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return "", err
	}
	defer done()
	defer p.release()
	v := p.Viewport()
	pt := Point{float64(v.Width) / 2, float64(v.Height) / 2}
	if t != nil {
		if _, pt, err = p.actionable(ctx, *t, "scroll"); err != nil {
			return "", fail(ctx, err)
		}
	}
	at, err := p.elementAt(ctx, pt)
	if err != nil {
		return "", fail(ctx, err)
	}
	before, err := p.callValue(ctx, at.s, at.object, scrollStateScript)
	if err != nil {
		return "", fail(ctx, err)
	}
	// Both deltas must be sent, even zero, which the typed params omit.
	wheel := map[string]any{"type": "mouseWheel", "x": pt.X, "y": pt.Y, "deltaX": dx, "deltaY": dy}
	if _, err := p.root.call(ctx, input.CommandDispatchMouseEvent, wheel); err != nil {
		return "", fail(ctx, err)
	}
	// Wheel scrolling is animated: wait until it rests.
	var after []byte
	last := ""
	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			return "", fail(ctx, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
		now, err := p.callValue(ctx, at.s, at.object, scrollStateScript)
		if err != nil {
			return "", fail(ctx, err)
		}
		if string(now) == last && string(now) != string(before) || i >= 6 && string(now) == last {
			after = now
			break
		}
		last, after = string(now), now
	}
	return describeScroll(before, after), nil
}

func describeScroll(before, after []byte) string {
	type state struct {
		Name string  `json:"name"`
		X    float64 `json:"x"`
		Y    float64 `json:"y"`
		MaxX float64 `json:"maxX"`
		MaxY float64 `json:"maxY"`
	}
	var b, a []state
	json.Unmarshal(before, &b, decodeOptions)
	json.Unmarshal(after, &a, decodeOptions)
	var moved []string
	for i := range min(len(a), len(b)) {
		if a[i].X != b[i].X || a[i].Y != b[i].Y {
			moved = append(moved, fmt.Sprintf("%s scrolled to %d,%d (of %d,%d)", a[i].Name, int(a[i].X), int(a[i].Y), int(a[i].MaxX), int(a[i].MaxY)))
		}
	}
	if len(moved) == 0 {
		if len(a) > 0 {
			return fmt.Sprintf("nothing scrolled: the page is at %d,%d (of %d,%d)", int(a[len(a)-1].X), int(a[len(a)-1].Y), int(a[len(a)-1].MaxX), int(a[len(a)-1].MaxY))
		}
		return "nothing scrolled"
	}
	return strings.Join(moved, "; ")
}

// Drag drags from one target to another with the mouse: press, move in
// steps, release. HTML5 drag and drop (draggable elements) is carried
// through too.
func (p *Page) Drag(ctx context.Context, from, to Target) (string, error) {
	if err := from.check(); err != nil {
		return "", err
	}
	if err := to.check(); err != nil {
		return "", err
	}
	ctx, done, err := p.begin(ctx, ActionTimeout, false)
	if err != nil {
		return "", err
	}
	defer done()
	defer p.release()
	el, start, err := p.actionable(ctx, from, "hover")
	if err != nil {
		return "", fail(ctx, err)
	}
	_, end, err := p.actionable(ctx, to, "hover")
	if err != nil {
		return "", fail(ctx, err)
	}
	// Reaching the drop target may have scrolled the source away.
	if _, start, err = p.actionable(ctx, from, "hover"); err != nil {
		return "", fail(ctx, err)
	}
	before := p.snapshotNav()
	intercepted, stop := p.root.Subscribe("Input.dragIntercepted")
	defer stop()
	p.root.call(ctx, "Input.setInterceptDrags", map[string]bool{"enabled": true})
	defer p.root.send("Input.setInterceptDrags", map[string]bool{"enabled": false})
	if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, start, input.MouseButtonNone, 0, 0); err != nil {
		return "", fail(ctx, err)
	}
	if err := p.mouse(ctx, input.DispatchMouseEventTypeMousePressed, start, input.MouseButtonLeft, 1, 0); err != nil {
		return "", fail(ctx, err)
	}
	const steps = 8
	var data *input.DragData
	for i := 1; i <= steps; i++ {
		f := float64(i) / steps
		pt := Point{start.X + (end.X-start.X)*f, start.Y + (end.Y-start.Y)*f}
		if data != nil {
			if err := p.drag(ctx, input.DispatchDragEventTypeDragOver, pt, data); err != nil {
				return "", fail(ctx, err)
			}
			continue
		}
		if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseMoved, pt, input.MouseButtonLeft, 0, 0); err != nil {
			return "", fail(ctx, err)
		}
		select {
		case raw, ok := <-intercepted:
			var ev struct {
				Data *input.DragData `json:"data"`
			}
			if ok && decode(raw, &ev) && ev.Data != nil {
				data = ev.Data
				if err := p.drag(ctx, input.DispatchDragEventTypeDragEnter, pt, data); err != nil {
					return "", fail(ctx, err)
				}
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if data != nil {
		if err := p.drag(ctx, input.DispatchDragEventTypeDrop, end, data); err != nil {
			return "", fail(ctx, err)
		}
	}
	if err := p.mouse(ctx, input.DispatchMouseEventTypeMouseReleased, end, input.MouseButtonLeft, 1, 0); err != nil {
		return "", fail(ctx, err)
	}
	return remark(el), fail(ctx, p.settle(ctx, before))
}

func (p *Page) drag(ctx context.Context, typ input.DispatchDragEventType, pt Point, data *input.DragData) error {
	_, err := cdp.Call(ctx, p.root, input.DispatchDragEvent, input.DispatchDragEventParams{Type: typ, X: pt.X, Y: pt.Y, Data: data})
	return err
}

// Wait is a condition WaitFor waits for. Set one of Text, Selector, URL
// or NetworkIdle.
type Wait struct {
	// Text is visible text, matched as a Target's text is.
	Text string
	// Selector is a CSS selector some visible element must match.
	Selector string
	// URL is a substring of the page's URL, or a glob when it holds a *
	// (which matches any run of characters).
	URL string
	// NetworkIdle waits until no request has been open for 500 ms.
	NetworkIdle bool
	// Gone waits for Text or Selector to disappear instead.
	Gone bool
}

// WaitFor waits until the condition holds, polling, for at most timeout
// (default 10 s). It fails with ErrTimeout, saying what it last saw.
func (p *Page) WaitFor(ctx context.Context, w Wait, timeout time.Duration) error {
	set := 0
	for _, ok := range []bool{w.Text != "", w.Selector != "", w.URL != "", w.NetworkIdle} {
		if ok {
			set++
		}
	}
	if set != 1 {
		return errors.New("wait for one thing: text, a selector, a URL or network idle")
	}
	if w.Gone && w.Text == "" && w.Selector == "" {
		return errors.New("gone applies to text or a selector")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	var pattern *regexp.Regexp
	if strings.Contains(w.URL, "*") {
		pattern = regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(w.URL), `\*`, ".*") + "$")
	}
	ctx, done, err := p.begin(ctx, timeout, false)
	if err != nil {
		return err
	}
	defer done()
	defer p.release()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	check := func() (bool, error) {
		switch {
		case w.URL != "":
			u := p.URL()
			return pattern != nil && pattern.MatchString(u) || pattern == nil && strings.Contains(u, w.URL), nil
		case w.NetworkIdle:
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.idleLocked(), nil
		}
		kind, query := "text", w.Text
		if w.Selector != "" {
			kind, query = "selector", w.Selector
		}
		found := false
		for _, sc := range p.scopes(ctx) {
			_, info, err := p.find(ctx, sc, kind, query)
			if err != nil {
				var te *targetError
				if errors.As(err, &te) {
					return false, err
				}
				return false, nil
			}
			if info.Count > 0 {
				found = true
				break
			}
		}
		p.release()
		return found != w.Gone, nil
	}
	for {
		ok, err := check()
		if err != nil {
			return fail(ctx, err)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			if cause := fail(ctx, ctx.Err()); !errors.Is(cause, context.DeadlineExceeded) && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return p.waitTimeout(w, timeout)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (p *Page) waitTimeout(w Wait, timeout time.Duration) error {
	var what string
	switch {
	case w.Text != "" && w.Gone:
		what = fmt.Sprintf("text %q to disappear", w.Text)
	case w.Text != "":
		what = fmt.Sprintf("text %q", w.Text)
	case w.Selector != "" && w.Gone:
		what = fmt.Sprintf("selector %q to stop matching", w.Selector)
	case w.Selector != "":
		what = fmt.Sprintf("a visible match for %q", w.Selector)
	case w.URL != "":
		what = fmt.Sprintf("the URL to match %q (it is %s)", w.URL, clip(p.URL(), 300))
	default:
		p.mu.Lock()
		var open []string
		for _, r := range p.inflight {
			open = append(open, clip(r.url, 120))
		}
		n := len(open)
		p.mu.Unlock()
		slices.Sort(open)
		if len(open) > 3 {
			open = append(open[:3], "…")
		}
		what = fmt.Sprintf("the network to go idle (%s open: %s)", plural(n, "request", "requests"), strings.Join(open, ", "))
	}
	return failure(ErrTimeout, "waited %s for %s", timeout.Round(100*time.Millisecond), what)
}

// focusScript focuses an editable element and selects its content, so typing
// replaces it. Inputs that take no typing (date, color, range) get the value
// set directly, through the native setter so React notices.
const focusScript = `function(text) {
  const describe = ` + describeScript + `;
  let el = this.nodeType === 1 ? this : this.parentElement;
  if (el.localName === 'label' && el.control) el = el.control;
  const desc = describe(el);
  const settable = ['date', 'time', 'datetime-local', 'month', 'week', 'color', 'range'];
  const noText = ['button', 'submit', 'reset', 'checkbox', 'radio', 'file', 'image', 'hidden'];
  const editable = e => e.localName === 'textarea' || (e.localName === 'input' && !noText.includes(e.type)) || e.isContentEditable;
  if (!editable(el)) {
    const inner = el.querySelector && el.querySelector('input:not([type=hidden]), textarea, [contenteditable=""], [contenteditable="true"]');
    if (!inner) return {error: 'not-editable', desc};
    el = inner;
    if (!editable(el)) return {error: 'not-editable', desc};
  }
  if (el.readOnly) return {error: 'readonly', desc};
  if (el.disabled) return {error: 'disabled', desc};
  el.focus();
  if (el.localName === 'input' && settable.includes(el.type)) {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    setter.call(el, text);
    el.dispatchEvent(new Event('input', {bubbles: true}));
    el.dispatchEvent(new Event('change', {bubbles: true}));
    return {ok: true, set: true, desc};
  }
  if (el.localName === 'input' || el.localName === 'textarea') {
    el.select();
  } else {
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }
  return {ok: true, desc};
}`

// selectScript picks a <select>'s options by value or label.
const selectScript = `function(values) {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
  const el = this.localName === 'label' && this.control ? this.control : this;
  const options = [...el.options];
  const picked = [];
  for (const v of values) {
    const o = options.find(o => o.value === v) || options.find(o => norm(o.label) === norm(v)) ||
      options.find(o => norm(o.label).toLowerCase() === norm(v).toLowerCase());
    if (!o) return {error: 'it has no option ' + JSON.stringify(v) + '; its options are ' + options.slice(0, 40).map(o => JSON.stringify(norm(o.label))).join(', ') + (options.length > 40 ? ', …' : '')};
    if (o.disabled) return {error: 'option ' + JSON.stringify(norm(o.label)) + ' is disabled'};
    picked.push(o);
  }
  if (!el.multiple && picked.length > 1) return {error: 'it takes one option, not ' + picked.length};
  el.focus();
  for (const o of options) o.selected = picked.includes(o);
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return {selected: picked.map(o => norm(o.label))};
}`

// scrollStateScript lists the scroll positions of an element's scrollable
// ancestors and of the page, innermost first.
const scrollStateScript = `function() {
  const out = [];
  const describe = ` + describeScript + `;
  for (let e = this.nodeType === 1 ? this : this.parentElement; e && e !== document.documentElement && e !== document.body; e = e.parentElement || (e.parentNode && e.parentNode.host)) {
    const st = getComputedStyle(e);
    if (/(auto|scroll|overlay)/.test(st.overflowY + st.overflowX) && (e.scrollHeight > e.clientHeight || e.scrollWidth > e.clientWidth)) {
      out.push({name: describe(e).replace(/ ".*$/, ''), x: e.scrollLeft, y: e.scrollTop, maxX: e.scrollWidth - e.clientWidth, maxY: e.scrollHeight - e.clientHeight});
    }
  }
  const s = document.scrollingElement || document.documentElement;
  out.push({name: 'page', x: scrollX, y: scrollY, maxX: s.scrollWidth - innerWidth, maxY: s.scrollHeight - innerHeight});
  return out;
}`
