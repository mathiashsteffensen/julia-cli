package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTypeDrivesAControlledInput(t *testing.T) {
	f := open(t)
	f.goTo(t, "/controlled")
	if _, err := f.p.Type(f.ctx, Target{Text: "Name"}, "ada lovelace", false); err != nil {
		t.Fatal(err)
	}
	// The page's input handler upper-cases state and writes it back, as a
	// React controlled input would; every key ran keydown.
	if got := f.eval(t, "out.textContent + '|' + fullname.value + '|' + keys.textContent"); got != `"ADA LOVELACE|ADA LOVELACE|12"` {
		t.Fatalf("state = %s", got)
	}
	// Type replaces what is there, and submit presses Enter.
	if _, err := f.p.Type(f.ctx, Target{Selector: "#search"}, "jarvis", true); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "submitted.textContent"); got != `"searched jarvis"` {
		t.Fatalf("submitted = %s", got)
	}
	if _, err := f.p.Type(f.ctx, Target{Text: "Notes"}, "line one\nline two ✓", false); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "notes.value"); got != `"line one\nline two ✓"` {
		t.Fatalf("textarea = %s", got)
	}
	if _, err := f.p.Type(f.ctx, Target{Text: "Rich text"}, "Final copy", false); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "rich.textContent"); got != `"Final copy"` {
		t.Fatalf("contenteditable = %s", got)
	}
	if _, err := f.p.Type(f.ctx, Target{Text: "Due"}, "2026-10-12", false); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "out.textContent"); got != `"due 2026-10-12"` {
		t.Fatalf("date = %s", got)
	}
	if _, err := f.p.Type(f.ctx, Target{Selector: "#search"}, "", false); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "search.value"); got != `""` {
		t.Fatalf("cleared field = %s", got)
	}
	_, err := f.p.Type(f.ctx, Target{Selector: "#out"}, "x", false)
	if err == nil || !strings.Contains(err.Error(), "not a text field") {
		t.Fatalf("typing into a paragraph: %v", err)
	}
}

func TestPressKeys(t *testing.T) {
	f := open(t)
	f.goTo(t, "/controlled")
	if _, err := f.p.Type(f.ctx, Target{Selector: "#search"}, "hello world", false); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		keys []string
		want string
	}{
		{[]string{"Backspace", "Backspace"}, `"hello wor"`},
		{[]string{"ArrowLeft", "shift+ArrowLeft", "shift+ArrowLeft", "X"}, `"hello Xr"`},
		{[]string{"meta+a", "Backspace"}, `""`},
		{[]string{"a", "b", "ctrl+a", "C"}, `"C"`},
		{[]string{"shift+1", "?", "space", "é"}, `"C!? é"`},
	} {
		if err := f.p.Press(f.ctx, step.keys...); err != nil {
			t.Fatal(err)
		}
		if got := f.eval(t, "search.value"); got != step.want {
			t.Fatalf("after %v value = %s, want %s", step.keys, got, step.want)
		}
	}
	if err := f.p.Press(f.ctx, "Tab"); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "document.activeElement.textContent"); got != `"Go"` {
		t.Fatalf("Tab moved focus to %s", got)
	}
	if err := f.p.Press(f.ctx, "Enter"); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "submitted.textContent"); got != `"searched C!? é"` {
		t.Fatalf("Enter on the button: %s", got)
	}
	if err := f.p.Press(f.ctx, "ctrl+bogus"); err == nil {
		t.Fatal("a bad key was accepted")
	}
}

func TestClickChecksWhatTheMouseWouldHit(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	f.goTo(t, "/covered")
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	_, err := f.p.Click(ctx, Target{Text: "Buy now"})
	if err == nil || !strings.Contains(err.Error(), `covered by <div.cookie-banner> "We use cookies"`) {
		t.Fatalf("click under a banner: %v", err)
	}
	if got := f.eval(t, "buy.textContent"); got != `"Buy now"` {
		t.Fatalf("the covered button was clicked: %s", got)
	}
	f.eval(t, "document.querySelector('.cookie-banner').remove()")
	if _, err := f.p.Click(f.ctx, Target{Text: "Buy now"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "buy.textContent"); got != `"Bought"` {
		t.Fatalf("after the banner went: %s", got)
	}
}

func TestClickWaitsForTheTargetAndExplainsFailures(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	f.goTo(t, "/app")
	// Appears after 300 ms: the click waits for it.
	f.eval(t, "setTimeout(() => { const b = document.createElement('button'); b.textContent = 'Late'; b.onclick = () => b.textContent = 'Late clicked'; document.body.prepend(b); }, 300)")
	if _, err := f.p.Click(f.ctx, Target{Text: "Late"}); err != nil {
		t.Fatal(err)
	}
	f.waitEval(t, "document.querySelector('button').textContent", `"Late clicked"`)
	// Each waits out TargetTimeout, on a page of its own, side by side.
	for _, tc := range []struct {
		target Target
		want   string
	}{
		{Target{Text: "Pay $12.00"}, "is disabled"},
		{Target{Text: "Secret hidden text"}, "hidden"},
		{Target{Text: "No such words"}, "nothing on the page matches"},
		{Target{Selector: "button["}, "invalid CSS selector"},
		{Target{Ref: "e999"}, "no ref e999"},
	} {
		t.Run(tc.target.String(), func(t *testing.T) {
			t.Parallel()
			f := open(t)
			f.goTo(t, "/app")
			start := time.Now()
			ctx, cancel := context.WithTimeout(f.ctx, 8*time.Second)
			_, err := f.p.Click(ctx, tc.target)
			cancel()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("click %s: %v, want %q", tc.target, err, tc.want)
			}
			if time.Since(start) > TargetTimeout+2*time.Second {
				t.Errorf("click %s took %v", tc.target, time.Since(start))
			}
		})
	}
}

// The covered check hit-tests where the mouse will be, on a scrolled page as
// on one at the top and at every size: a button below the fold is clicked,
// and the same button under a full-screen backdrop is not.
func TestHitTestsFollowTheScroll(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	for _, v := range []Viewport{DefaultViewport, {Width: 390, Height: 844, Scale: 1, Mobile: true}, {Width: 390, Height: 844, Scale: 1}, {Width: 768, Height: 1024, Scale: 1}} {
		if err := f.p.Resize(f.ctx, v); err != nil {
			t.Fatal(err)
		}
		f.goTo(t, "/settings")
		if _, err := f.p.Click(f.ctx, Target{Text: "Save settings"}); err != nil {
			t.Fatalf("%+v: %v", v, err)
		}
		// A phone shows a page without a meta viewport zoomed out, whole.
		if got := f.eval(t, "saved.textContent + ' ' + (scrollY > 0)"); got != `"Saved `+strconv.FormatBool(!v.Mobile)+`"` {
			t.Fatalf("%+v: after the click: %s", v, got)
		}
		f.goTo(t, "/settings-backdrop")
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
		_, err := f.p.Click(ctx, Target{Text: "Save settings"})
		cancel()
		if err == nil || !strings.Contains(err.Error(), "covered by <div#backdrop>") {
			t.Fatalf("%+v: click under a backdrop: %v", v, err)
		}
		if got := f.eval(t, "saved.textContent + '|' + (backdrop.dataset.clicked || '')"); got != `"|"` {
			t.Fatalf("%+v: under the backdrop, the click landed: %s", v, got)
		}
	}
	if err := f.p.Resize(f.ctx, DefaultViewport); err != nil {
		t.Fatal(err)
	}
	// A point names what is there now, however far the page has scrolled.
	f.goTo(t, "/settings")
	f.eval(t, "scrollTo(0, document.body.scrollHeight)")
	at := func(selector string) *Point {
		var pt []float64
		raw := f.eval(t, "(r => [r.left + r.width / 2, r.top + r.height / 2])(document.querySelector('"+selector+"').getBoundingClientRect())")
		if err := json.Unmarshal([]byte(raw), &pt); err != nil || len(pt) != 2 {
			t.Fatalf("%s: %s", selector, raw)
		}
		return &Point{pt[0], pt[1]}
	}
	if _, err := f.p.Type(f.ctx, Target{Point: at("#note")}, "hello", false); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "note.value"); got != `"hello"` {
		t.Fatalf("typed at a point: %s", got)
	}
	snap, err := f.p.Snapshot(f.ctx, SnapshotOptions{Target: &Target{Point: at("footer button")}})
	if err != nil || !strings.Contains(snap, `Subtree: <button> "Save settings"`) {
		t.Fatalf("snapshot at a point: %v\n%s", err, snap)
	}
	// A point is in the viewport: below it is a mistake, not a click.
	for _, pt := range []Point{{640, 1200}, {1300, 10}} {
		if _, err := f.p.Click(f.ctx, Target{Point: &pt}); err == nil || !strings.Contains(err.Error(), "outside the 1280×800 viewport") {
			t.Fatalf("click at %v: %v", pt, err)
		}
		if _, err := f.p.Hover(f.ctx, Target{Point: &pt}); err == nil {
			t.Fatalf("hover at %v succeeded", pt)
		}
	}
}

// A frame's own document does not see what covers the frame; the click is
// checked in each document around it too.
func TestClickChecksTheDocumentsAroundAFrame(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	f.goTo(t, "/frame-banner")
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	_, err := f.p.Click(ctx, Target{Text: "Pay now"})
	cancel()
	if err == nil || !strings.Contains(err.Error(), `covered by <div#banner> "We use cookies"`) {
		t.Fatalf("a cross-site frame under a banner: %v", err)
	}
	if got := f.eval(t, "banner.dataset.clicked || ''"); got != `""` {
		t.Fatal("the banner got the click")
	}
	f.eval(t, "banner.remove()")
	if _, err := f.p.Click(f.ctx, Target{Text: "Pay now"}); err != nil {
		t.Fatal(err)
	}
	if snap := f.snapshot(t); !strings.Contains(snap, `button "Paid"`) {
		t.Fatalf("after the banner went:\n%s", snap)
	}
}

// What its container cuts off cannot be clicked, as a person could not; the
// check neither clicks the container nor scrolls a box a person cannot.
func TestClickRefusesWhatIsCutOff(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	f.goTo(t, "/clip")
	for _, tc := range []struct{ text, want string }{
		{"Hidden action", "lies outside the visible box of <div#bar.bar> (overflow: hidden)"},
		{"Clipped action", "lies outside the visible box of <div#bar2.bar> (overflow: clip)"},
	} {
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
		_, err := f.p.Click(ctx, Target{Text: tc.text})
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("click %s: %v", tc.text, err)
		}
	}
	if got := f.eval(t, "out.textContent + '|' + bar.scrollLeft + '|' + bar3.scrollLeft"); got != `"|0|0"` {
		t.Fatalf("after the refused clicks: %s", got)
	}
	// Partly cut off: the part that shows is clicked.
	if _, err := f.p.Click(f.ctx, Target{Text: "Half action"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "out.textContent + '|' + bar3.scrollLeft"); got != `"half clicked|0"` {
		t.Fatalf("a half-hidden button: %s", got)
	}
	if _, err := f.p.Click(f.ctx, Target{Text: "Two"}); err != nil {
		t.Fatal(err)
	}
}

func TestTargetsReachShadowRootsAndFrames(t *testing.T) {
	f := open(t)
	f.goTo(t, "/app")
	if _, err := f.p.Click(f.ctx, Target{Text: "Shadow action"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "document.querySelector('my-widget').shadowRoot.querySelector('button').textContent"); got != `"Shadow done"` {
		t.Fatalf("shadow button: %s", got)
	}
	if _, err := f.p.Type(f.ctx, Target{Text: "Shadow field"}, "inside", false); err != nil {
		t.Fatal(err)
	}
	// A same-origin srcdoc iframe, found by text.
	if _, err := f.p.Click(f.ctx, Target{Text: "Chat with us"}); err != nil {
		t.Fatal(err)
	}
	// A cross-site iframe runs in its own process: found by text, typed
	// into and clicked with real events routed through the page.
	if _, err := f.p.Type(f.ctx, Target{Text: "Card number"}, "4242", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Click(f.ctx, Target{Text: "Pay now"}); err != nil {
		t.Fatal(err)
	}
	// Same site, another origin: the same process, out of the page's reach.
	if _, err := f.p.Click(f.ctx, Target{Text: "Helpful"}); err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot(t)
	for _, want := range []string{`textbox "Card number"`, ": 4242", `button "Paid"`, `textbox "Shadow field"`, ": inside", `iframe "Reviews"`, `button "Thanks!"`} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot lacks %q:\n%s", want, snap)
		}
	}
}

func TestHoverRightClickDoubleClick(t *testing.T) {
	f := open(t)
	f.goTo(t, "/hover")
	if _, err := f.p.Hover(f.ctx, Target{Text: "Info"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "tip.hidden"); got != "false" {
		t.Fatal("hover did not show the tooltip")
	}
	if _, err := f.p.RightClick(f.ctx, Target{Text: "Right-click here"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.DoubleClick(f.ctx, Target{Selector: "#dbl"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "menu.textContent + '|' + dbl.textContent"); got != `"menu opened|double clicked"` {
		t.Fatalf("got %s", got)
	}
	// A point clicks whatever is there.
	f.eval(t, "window.at = null; document.addEventListener('click', e => window.at = e.clientX + ',' + e.clientY)")
	if _, err := f.p.Click(f.ctx, Target{Point: &Point{300, 400}}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "at"); got != `"300,400"` {
		t.Fatalf("clicked at %s", got)
	}
}

func TestSelectOptions(t *testing.T) {
	f := open(t)
	f.goTo(t, "/select")
	got, err := f.p.Select(f.ctx, Target{Text: "Country"}, "Sweden")
	if err != nil || len(got) != 1 || got[0] != "Sweden" {
		t.Fatalf("select by label: %v, %v", got, err)
	}
	if out := f.eval(t, "r.textContent"); out != `"country se"` {
		t.Fatalf("change handler saw %s", out)
	}
	if _, err := f.p.Select(f.ctx, Target{Selector: "#c"}, "dk"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Select(f.ctx, Target{Text: "Toppings"}, "Cheese", "olives"); err != nil {
		t.Fatal(err)
	}
	if out := f.eval(t, "r.textContent"); out != `"Cheese+Olives"` {
		t.Fatalf("multiple: %s", out)
	}
	_, err = f.p.Select(f.ctx, Target{Selector: "#c"}, "Finland")
	if err == nil || !strings.Contains(err.Error(), `"Denmark", "Sweden", "Norway"`) {
		t.Fatalf("missing option: %v", err)
	}
	if _, err = f.p.Select(f.ctx, Target{Selector: "#c"}, "Norway"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled option: %v", err)
	}
	if _, err = f.p.Select(f.ctx, Target{Selector: "#r"}, "x"); err == nil || !strings.Contains(err.Error(), "not a <select>") {
		t.Fatalf("not a select: %v", err)
	}
}

func TestSetFiles(t *testing.T) {
	f := open(t)
	f.goTo(t, "/files")
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "photo.png")
	os.WriteFile(a, []byte("hello"), 0o644)
	os.WriteFile(b, []byte("12345678"), 0o644)
	if err := f.p.SetFiles(f.ctx, Target{Text: "Avatar"}, a); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "names.textContent"); got != `"a.txt:5"` {
		t.Fatalf("one file: %s", got)
	}
	if err := f.p.SetFiles(f.ctx, Target{Selector: "#many"}, a, b); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "names.textContent"); got != `"a.txt:5,photo.png:8"` {
		t.Fatalf("two files: %s", got)
	}
	if err := f.p.SetFiles(f.ctx, Target{Selector: "#one"}, a, b); err == nil || !strings.Contains(err.Error(), "takes one file") {
		t.Fatalf("two files into one: %v", err)
	}
	if err := f.p.SetFiles(f.ctx, Target{Selector: "#names"}, a); err == nil || !strings.Contains(err.Error(), "not a file input") {
		t.Fatalf("not a file input: %v", err)
	}
	if err := f.p.SetFiles(f.ctx, Target{Selector: "#one"}, "relative.txt"); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

func TestScroll(t *testing.T) {
	f := open(t)
	f.goTo(t, "/scroll")
	got, err := f.p.Scroll(f.ctx, nil, 0, 600)
	if err != nil || !strings.Contains(got, "page scrolled to 0,600") {
		t.Fatalf("page scroll: %q, %v", got, err)
	}
	// Again, from where the first left it.
	got, err = f.p.Scroll(f.ctx, nil, 0, 600)
	if err != nil || !strings.Contains(got, "page scrolled to 0,1200") {
		t.Fatalf("second page scroll: %q, %v", got, err)
	}
	f.eval(t, "scrollTo(0, 0)")
	got, err = f.p.Scroll(f.ctx, &Target{Selector: "#box"}, 0, 300)
	if err != nil || !strings.Contains(got, "<div#box> scrolled to 0,300") || strings.Contains(got, "page scrolled") {
		t.Fatalf("inner scroll: %q, %v", got, err)
	}
	got, err = f.p.Scroll(f.ctx, nil, 0, -500)
	if err != nil || !strings.Contains(got, "nothing scrolled") {
		t.Fatalf("scroll past the top: %q, %v", got, err)
	}
}

func TestDrag(t *testing.T) {
	f := open(t)
	f.goTo(t, "/drag")
	// HTML5 drag and drop.
	if _, err := f.p.Drag(f.ctx, Target{Text: "Card"}, Target{Text: "Drop here"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "zone.textContent"); got != `"dropped card-1"` {
		t.Fatalf("drop zone: %s", got)
	}
	// A pointer-driven slider, dragged to a point past its end.
	x, err := strconv.ParseFloat(f.eval(t, "track.getBoundingClientRect().right + 20"), 64)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := strconv.ParseFloat(f.eval(t, "track.getBoundingClientRect().top + 12"), 64)
	if _, err := f.p.Drag(f.ctx, Target{Selector: "#thumb"}, Target{Point: &Point{x, y}}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "value.textContent"); got != `"100"` {
		t.Fatalf("slider value %s", got)
	}
}

func TestWaitFor(t *testing.T) {
	f := open(t)
	f.goTo(t, "/later")
	if err := f.p.WaitFor(f.ctx, Wait{Text: "ready to go"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := f.p.WaitFor(f.ctx, Wait{Text: "Loading…", Gone: true}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := f.p.WaitFor(f.ctx, Wait{URL: "*/later/done"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := f.p.WaitFor(f.ctx, Wait{Selector: "#progress"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := f.p.WaitFor(f.ctx, Wait{Text: "never shows"}, 700*time.Millisecond)
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), `text "never shows"`) {
		t.Fatalf("timeout: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("timed out after %v", took)
	}
	if err := f.p.WaitFor(f.ctx, Wait{Text: "a", URL: "b"}, time.Second); err == nil {
		t.Fatal("two conditions accepted")
	}
}

func TestEval(t *testing.T) {
	f := open(t)
	f.goTo(t, "/app")
	for _, tc := range []struct{ js, want string }{
		{"1 + 1", "2"},
		{"'a' + 'b'", `"ab"`},
		{"undefined", "undefined"},
		{"NaN", "NaN"},
		{"10n ** 20n", "100000000000000000000n"},
		{"document.title", `"Acme Checkout"`},
		{"const n = 3; n * 2", "6"},
		{"await new Promise(r => setTimeout(() => r('later'), 50))", `"later"`},
		{"({a: 1, b: [true, null], c: undefined})", "{\n  \"a\": 1,\n  \"b\": [\n    true,\n    null\n  ],\n  \"c\": \"[undefined]\"\n}"},
		{"document.querySelector('h1')", `"<h1> \"Checkout\""`},
		{"$$('button').length", "2"},
		{"const o = {name: 'loop'}; o.self = o; o", "{\n  \"name\": \"loop\",\n  \"self\": \"[circular]\"\n}"},
		{"new Map([['k', 1]])", "{\n  \"Map(1)\": {\n    \"k\": 1\n  }\n}"},
		// A promise the script returns is awaited, as one awaited in it is.
		{"(async () => { const r = await fetch('/api/data'); return r.status })()", "200"},
		{"fetch('/api/data').then(r => r.status)", "200"},
		{"Promise.resolve(42)", "42"},
		{"await fetch('/api/data').then(r => r.status)", "200"},
	} {
		got, err := f.p.Eval(f.ctx, tc.js)
		if err != nil || got != tc.want {
			t.Errorf("Eval(%s) = %q, %v; want %q", tc.js, got, err, tc.want)
		}
	}
	_, err := f.p.Eval(f.ctx, "null.x")
	if err == nil || !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("exception: %v", err)
	}
	if _, err := f.p.Eval(f.ctx, "Promise.reject(new Error('nope'))"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("a rejected promise: %v", err)
	}
	big, err := f.p.Eval(f.ctx, "'x'.repeat(100000)")
	if err != nil || len(big) > MaxEvalResult+200 || !strings.Contains(big, "cut at") {
		t.Fatalf("big result: %d bytes, %v", len(big), err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	start := time.Now()
	_, err = f.p.Eval(ctx, "while (true) {}")
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 3*time.Second {
		t.Fatalf("endless loop: %v after %v", err, time.Since(start))
	}
	// The loop was terminated: the page still answers.
	if got := f.eval(t, "1"); got != "1" {
		t.Fatalf("after termination: %s", got)
	}
}
