package browser

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
)

func TestSnapshotOfARealisticPage(t *testing.T) {
	f := open(t)
	f.goTo(t, "/app")
	snap := f.snapshot(t)
	t.Log("\n" + snap)
	for _, want := range []string{
		`Page ` + f.p.ID() + `: "Acme Checkout"`,
		"URL: " + f.site.url("/app"),
		"Viewport: 1280×800, scrolled to 0,0 of a 1280×",
		`- banner:`,
		`- link "Acme home" [ref=`,
		`[href=/]`,
		`- navigation "Main"`,
		`- link "Shop" [ref=`,
		`- main:`,
		`- heading "Checkout" [ref=`,
		`[level=1]`,
		`- textbox "Email" [ref=`,
		`[required]`,
		`- checkbox "Subscribe to news" [ref=`,
		`[checked]`,
		`- radio "Standard" [ref=`,
		`- radio "Express" [ref=`,
		`- combobox "Country" [ref=`,
		`: Sweden`,
		`- button "Pay $12.00" [ref=`,
		`[disabled]`,
		`- button "Order details" [ref=`, // <summary>
		`[collapsed]`,
		`- table "Totals"`,
		`- columnheader "Item"`,
		`- cell "$9.00"`,
		`- button "Shadow action" [ref=`, // an open shadow root
		`- textbox "Shadow field" [ref=`,
		`- iframe "Help" [ref=`,
		`- button "Chat with us" [ref=`, // a same-origin srcdoc iframe
		`- iframe "Payments" [ref=`,
		`- textbox "Card number" [ref=`, // a cross-site, out-of-process iframe
		`- button "Pay now" [ref=`,
		`- dialog "Cookies" [ref=`,
		`[modal]`,
		`- button "Accept all" [ref=`,
	} {
		if !strings.Contains(snap, want) {
			t.Errorf("snapshot lacks %q", want)
		}
	}
	for _, unwanted := range []string{"Secret hidden text", "Decorative flourish", "generic", "StaticText", "InlineTextBox", "Apples ×3"} {
		if strings.Contains(snap, unwanted) {
			t.Errorf("snapshot shows %q", unwanted)
		}
	}
	// Refs act: the collapsed details open when clicked by ref.
	if _, err := f.p.Click(f.ctx, Target{Ref: refOf(t, snap, `button "Order details"`)}); err != nil {
		t.Fatal(err)
	}
	after := f.snapshot(t)
	if !strings.Contains(after, `- listitem: Apples ×3`) || !strings.Contains(after, "[expanded]") {
		t.Fatalf("after opening details:\n%s", after)
	}
	// A focused field shows its state and value.
	if _, err := f.p.Type(f.ctx, Target{Ref: refOf(t, snap, `textbox "Email"`)}, "ada@example.com", false); err != nil {
		t.Fatal(err)
	}
	if after := f.snapshot(t); !strings.Contains(after, `[focused]`) || !strings.Contains(after, `: ada@example.com`) {
		t.Fatalf("typed field:\n%s", after)
	}

	// A subtree, by ref.
	sub, err := f.p.Snapshot(f.ctx, SnapshotOptions{Target: &Target{Ref: refOf(t, snap, `navigation "Main"`)}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sub, "Subtree: ") || !strings.Contains(sub, `link "About"`) || strings.Contains(sub, `heading "Checkout"`) {
		t.Fatalf("subtree:\n%s", sub)
	}
	// Capped, honestly.
	small, err := f.p.Snapshot(f.ctx, SnapshotOptions{MaxBytes: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if len(small) > 1200 || !strings.Contains(small, "more lines not shown") {
		t.Fatalf("capped snapshot (%d bytes):\n%s", len(small), small)
	}
}

// A tree over Chrome's message limit is read a part at a time, never shown
// as an empty page; a subtree of it is read alone.
func TestSnapshotOfAPageTooLargeToFetchWhole(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := openOn(t, launch(t, Options{messageLimit: 2 << 20}))
	f.goTo(t, "/big")
	snap := f.snapshot(t)
	for _, want := range []string{`heading "Inventory"`, `link "Item 0" [ref=`, `button "Act 0" [ref=`, "not shown"} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot lacks %q:\n%s", want, snap)
		}
	}
	if strings.Contains(snap, "shows nothing") || len(snap) > DefaultSnapshotBytes {
		t.Fatalf("snapshot of %d bytes:\n%s", len(snap), snap)
	}
	sub, err := f.p.Snapshot(f.ctx, SnapshotOptions{Target: &Target{Selector: "#row-2500"}})
	if err != nil || !strings.Contains(sub, `link "Item 2500"`) || !strings.Contains(sub, `button "Act 2500"`) {
		t.Fatalf("a subtree: %v\n%s", err, sub)
	}
}

// Cut to size, a long snapshot keeps the part the agent is working in: what
// is in view, which an action scrolls its target into.
func TestSnapshotWindow(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	f.goTo(t, "/big")
	window := func() string {
		t.Helper()
		snap, err := f.p.Snapshot(f.ctx, SnapshotOptions{MaxBytes: 8 << 10, Window: true})
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	f.eval(t, "document.querySelector('#row-2999 button').focus()")
	if snap := window(); !strings.Contains(snap, `button "Act 2999" [ref=`) || !strings.Contains(snap, "lines above not shown") || !strings.Contains(snap, "around what is in view") {
		t.Fatalf("around the focus:\n%s", snap)
	}
	f.eval(t, "document.activeElement.blur(); document.querySelector('#row-1500').scrollIntoView({block: 'center'})")
	if snap := window(); !strings.Contains(snap, `link "Item 1500"`) || !strings.Contains(snap, "around what is in view") {
		t.Fatalf("around what is in view:\n%s", snap)
	}
	f.eval(t, "scrollTo(0, 0)")
	if snap := window(); !strings.Contains(snap, `heading "Inventory"`) {
		t.Fatalf("at the top:\n%s", snap)
	}
}

// A modal dialog makes the rest of the page inert, which the header says, so
// that what left the tree is not read as removed.
func TestSnapshotNamesAModalDialog(t *testing.T) {
	f := open(t)
	f.goTo(t, "/settings-modal")
	if _, err := f.p.Click(f.ctx, Target{Text: "Settings"}); err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot(t)
	if !strings.Contains(snap, `Modal: dialog "Settings" is open; the rest of the page is inert behind it`) || strings.Contains(snap, "Groceries") {
		t.Fatalf("with the dialog open:\n%s", snap)
	}
	if _, err := f.p.Click(f.ctx, Target{Text: "Close"}); err != nil {
		t.Fatal(err)
	}
	if snap := f.snapshot(t); strings.Contains(snap, "Modal:") || !strings.Contains(snap, "Groceries") {
		t.Fatalf("once it closed:\n%s", snap)
	}
}

func TestSnapshotOfAnEmptyPage(t *testing.T) {
	f := open(t)
	if snap := f.snapshot(t); !strings.Contains(snap, "the page shows nothing") {
		t.Fatalf("blank page:\n%s", snap)
	}
}

// axNode builds a synthetic accessibility node for the renderer tests.
func axNode(id, role, name string, children ...string) *accessibility.Node {
	n := &accessibility.Node{NodeID: accessibility.NodeID(id), Role: axValue(role), BackendDOMNodeID: 100}
	if name != "" {
		n.Name = axValue(name)
	}
	for _, c := range children {
		n.ChildIDs = append(n.ChildIDs, accessibility.NodeID(c))
	}
	return n
}

func axValue(s string) *accessibility.Value {
	raw, _ := jsontext.AppendQuote(nil, s)
	return &accessibility.Value{Type: "string", Value: raw}
}

func TestRendererFormatsNodes(t *testing.T) {
	ignored := axNode("4", "generic", "", "5")
	ignored.Ignored = true
	checkbox := axNode("8", "checkbox", "Remember me")
	checkbox.Properties = []*accessibility.Property{{Name: "checked", Value: &accessibility.Value{Type: "tristate", Value: jsontext.Value(`"mixed"`)}}}
	heading := axNode("2", "heading", "Welcome", "3")
	heading.Properties = []*accessibility.Property{{Name: "level", Value: &accessibility.Value{Type: "integer", Value: jsontext.Value(`2`)}}}
	textbox := axNode("9", "textbox", "Search")
	textbox.Value = axValue("go")
	nodes := []*accessibility.Node{
		axNode("1", "RootWebArea", "Home", "2", "4", "6", "8", "9", "10"),
		heading,
		axNode("3", "StaticText", "Welcome"),
		ignored,
		axNode("5", "StaticText", "Hello"),
		axNode("6", "generic", "", "7", "11"),
		axNode("7", "strong", "", "12"),
		checkbox,
		textbox,
		axNode("10", "paragraph", "", "13"),
		axNode("11", "StaticText", "world"),
		axNode("12", "StaticText", "big"),
		axNode("13", "StaticText", "A paragraph of text."),
	}
	tree := &axTree{s: &session{id: "s"}, nodes: map[accessibility.NodeID]*accessibility.Node{}}
	for i, n := range nodes {
		n.BackendDOMNodeID = cdp.BackendNodeID(101 + i)
		tree.nodes[n.NodeID] = n
	}
	tree.root = nodes[0]
	r := &renderer{trees: axTrees{}, page: &Page{}}
	r.owners = map[ownerKey]*axTree{}
	r.children(tree, tree.root, 0)
	var refs refTable
	text := func(header string) string {
		return Tree{Header: header, Lines: r.layout(&refs), Anchor: -1}.Text(4096)
	}
	got := text("Page p1\n")
	want := `Page p1

- heading "Welcome" [ref=e1] [level=2]
- text: Hello big world
- checkbox "Remember me" [ref=e2] [checked=mixed]
- textbox "Search" [ref=e3]: go
- paragraph: A paragraph of text.
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// The same nodes keep their refs; a new document starts past them.
	r.lines = nil
	r.children(tree, tree.root, 0)
	if again := text("Page p1\n"); again != want {
		t.Fatalf("second render:\n%s", again)
	}
	refs.newDocument()
	r.lines = nil
	r.children(tree, tree.root, 0)
	if third := text(""); !strings.Contains(third, "[ref=e4]") {
		t.Fatalf("after a navigation:\n%s", third)
	}
}

// A tree too long to show whole is cut to a window: around its anchor, with
// what is left out above and below said, or from the top.
func TestTreeWindows(t *testing.T) {
	var lines []string
	for i := range 1000 {
		lines = append(lines, fmt.Sprintf("- listitem: row %03d", i))
	}
	header := "Page p1: \"Long\"\nURL: http://localhost/long\n"
	for _, tc := range []struct {
		name         string
		tree         Tree
		first, last  string
		above, below string
	}{
		{"whole", Tree{Header: header, Lines: lines[:10], Anchor: -1}, "row 000", "row 009", "", ""},
		{"top", Tree{Header: header, Lines: lines, Anchor: -1}, "row 000", "", "", "more lines not shown: the snapshot stops at 4 KiB"},
		{"around the focus", Tree{Header: header, Lines: lines, Anchor: 700, Around: "the focused element"}, "", "row 700", "lines above not shown", "lines below not shown; the snapshot stops at 4 KiB, so it shows the part around the focused element"},
		{"at the end", Tree{Header: header, Lines: lines, Anchor: 999, Around: "what is in view"}, "", "row 999", "lines above not shown", "the snapshot stops at 4 KiB, so it shows the part around what is in view"},
		{"partial", Tree{Header: header, Lines: lines[:10], Anchor: -1, Partial: true}, "row 000", "row 009", "", "the rest of the page is not shown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.tree.Text(4 << 10)
			if len(got) > 4<<10 {
				t.Fatalf("%d bytes, over 4 KiB", len(got))
			}
			for _, want := range []string{tc.first, tc.last, tc.above, tc.below} {
				if !strings.Contains(got, want) {
					t.Fatalf("lacks %q:\n%s", want, got)
				}
			}
			if tc.above == "" && strings.Contains(got, "above not shown") || tc.below == "" && strings.Contains(got, "not shown") {
				t.Fatalf("says something is left out:\n%s", got)
			}
			if !strings.HasPrefix(got, header+"\n") {
				t.Fatalf("header lost:\n%s", got)
			}
		})
	}
}
