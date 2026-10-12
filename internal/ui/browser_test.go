package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"julia/internal/core"
)

func TestBrowserSummary(t *testing.T) {
	for _, tc := range []struct{ args, headline, want string }{
		{`{"action":"navigate","url":"http://localhost:5173/settings"}`, "", "navigate · localhost:5173/settings"},
		{`{"action":"navigate","url":"https://docs.example.com/api","network":true,"new_page":true}`, "", "navigate · docs.example.com/api · network · new page"},
		{`{"action":"click","target":"e12"}`, `Clicked button "Save" (e12).`, `click · button "Save"`},
		{`{"action":"click","target":"e12"}`, "", "click · e12"},
		{`{"action":"click","target":"text=Save","double":true}`, `Double-clicked text "Save".`, "double-click · text=Save"},
		{`{"action":"type","target":"e6","text":"hello","submit":true}`, `Typed "hello" into textbox "Email" (e6), then pressed Enter.`, `type · "hello" ↵ → textbox "Email"`},
		{`{"action":"type","target":"textbox \"Email\" (e6)","text":"a\nb"}`, "", `type · "a ↵ b" → textbox "Email"`},
		{`{"action":"press","keys":["ctrl+a","Backspace"],"page":"p2"}`, "", "press · ctrl+a Backspace · p2"},
		{`{"action":"select","target":"e11","values":["Sweden"]}`, `Selected "Sweden" in combobox "Country" (e11).`, `select · "Sweden" → combobox "Country"`},
		{`{"action":"upload","target":"css=#avatar","paths":["avatar.png"]}`, "", "upload · avatar.png → css=#avatar"},
		{`{"action":"drag","target":"e4","to":"e7"}`, `Dragged listitem "One" (e4) onto list "Done" (e7).`, `drag · listitem "One" → list "Done"`},
		{`{"action":"scroll"}`, "", "scroll · ↓ a screen"},
		{`{"action":"scroll","dy":-400,"dx":120,"target":"e3"}`, "", "scroll · → 120 ↑ 400 · e3"},
		{`{"action":"wait","text":"Saved"}`, "", `wait · "Saved"`},
		{`{"action":"wait","selector":".spinner","gone":true}`, "", "wait · .spinner · gone"},
		{`{"action":"wait","network_idle":true}`, "", "wait · network idle"},
		{`{"action":"eval","script":"document.title\n// more"}`, "", "eval · document.title"},
		{`{"action":"screenshot","full_page":true}`, "", "screenshot · full page"},
		{`{"action":"screenshot"}`, "", "screenshot"},
		{`{"action":"emulate","width":390,"height":844,"mobile":true,"scale":3,"color_scheme":"dark"}`, "", "emulate · 390×844 · mobile · @3x · dark"},
		{`{"action":"allow","origins":["https://fonts.googleapis.com","https://fonts.gstatic.com"]}`, "", "allow · https://fonts.googleapis.com, https://fonts.gstatic.com"},
		{`{"action":"close","page":"p2"}`, "", "close · p2"},
		{`{"action":"tabs"}`, "", "tabs"},
	} {
		got, ok := browserSummary(json.RawMessage(tc.args), tc.headline)
		if !ok || got != tc.want {
			t.Errorf("%s: %q, want %q", tc.args, got, tc.want)
		}
	}
	if got := toolSummary(core.ToolCall{Name: "Browser", Arguments: json.RawMessage(`{"action":"navigate","url":"file:///tmp/ws/dist/index.html"}`)}); got != "navigate · /tmp/ws/dist/index.html" {
		t.Errorf("toolSummary: %q", got)
	}
}

const browserResult = `Clicked button "Save" (e12).
Notes:
- blocked 3 requests to https://fonts.gstatic.com (GET /s/inter.woff2, …): https://fonts.gstatic.com is not approved for this turn; Browser allow asks for it
- dismissed a confirm dialog "Discard changes?"
Console errors:
- error Uncaught TypeError: x is undefined (/src/App.tsx:14:22)

Page p1: "Settings"
URL: http://localhost:5173/settings
Viewport: 1280×800, scrolled to 0,0 of a 1280×1432 page

Changes since the last snapshot of p1 (+ added, - removed; unchanged lines left out):
  - main:
    - form "Profile":
-     - button "Save" [ref=e12]
+     - button "Saving…" [ref=e12] [disabled]
+     - status: Saved`

func browserEntry(args, content string) entry {
	return entry{role: "tool", done: true,
		call:   &core.ToolCall{Name: "Browser", Arguments: json.RawMessage(args)},
		result: &core.ToolResult{Content: content}}
}

// Collapsed, a page result is one row: the page, what changed and what needs
// attention; expanded, its notes and a card with the changed lines.
func TestBrowserPageCard(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(160, 40)
	item := browserEntry(`{"action":"click","target":"e12"}`, browserResult)
	collapsed := strings.Split(ansi.Strip(m.renderEntryContent(item)), "\n")
	if len(collapsed) != 2 || !strings.Contains(collapsed[0], `Browser  click · button "Save"`) ||
		!strings.Contains(collapsed[1], "p1 · Settings · localhost:5173/settings · 3 lines changed · 3 blocked · 1 error · dialog") {
		t.Fatalf("collapsed:\n%s", strings.Join(collapsed, "\n"))
	}
	// Narrower, the address gives way to what needs attention.
	m.resize(110, 40)
	if row := strings.Split(ansi.Strip(m.renderEntryContent(item)), "\n")[1]; !strings.HasSuffix(row, "p1 · Settings · 3 lines changed · 3 blocked · 1 error · dialog") {
		t.Fatalf("narrow collapsed: %q", row)
	}
	m.expandTools = true
	layout := m.layoutEntry(item)
	plain := ansi.Strip(layout.text)
	for _, want := range []string{
		`Clicked button "Save" (e12).`,
		"· blocked 3 requests to https://fonts.gstatic.com",
		`· dismissed a confirm dialog "Discard changes?"`,
		"! error Uncaught TypeError: x is undefined",
		"p1 · Settings", "localhost:5173/settings",
		"Viewport: 1280×800, scrolled to 0,0 of a 1280×1432 page",
		"changes since the last snapshot",
		`-     - button "Save" [ref=e12]`,
		`+     - status: Saved`,
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("expanded lacks %q:\n%s", want, plain)
		}
	}
	for _, row := range strings.Split(plain, "\n") {
		if ansi.StringWidth(row) > m.mainWidth {
			t.Errorf("row wider than %d: %q", m.mainWidth, row)
		}
	}
	// Double-clicking the card copies the snapshot lines.
	targets := toolTargets(item)
	if targets[1].label != "snapshot" || !strings.HasPrefix(targets[1].text, "  - main:\n") {
		t.Fatalf("targets: %+v", targets)
	}
	// A whole snapshot is drawn plain; an error or a list is not a page.
	whole := "Opened http://localhost:5173/ in p1.\n\nPage p1: \"Home\"\nURL: http://localhost:5173/\nViewport: 1280×800, scrolled to 0,0 of a 1280×800 page\n\n- heading \"Home\" [ref=e1] [level=1]\n- link \"Form\" [ref=e2] [href=/form]"
	page, ok := parseBrowserPage(whole)
	if !ok || page.diff || page.caption != "" || len(page.lines) != 2 || page.title != "Home" {
		t.Fatalf("whole snapshot: %+v", page)
	}
	if _, ok := parseBrowserPage("p1 · \"Home\" · http://localhost:5173/ · current"); ok {
		t.Fatal("tabs parsed as a page")
	}
	m.resize(160, 40)
	eval := "Ran the script in p1; it returned:\n42\n\nPage p1: \"Home\"\nURL: http://localhost:5173/\nViewport: 1280×800, scrolled to 0,0 of a 1280×800 page\n\nNo change since the last snapshot of p1."
	m.expandTools = false
	if row := ansi.Strip(m.renderEntryContent(browserEntry(`{"action":"eval","script":"6*7"}`, eval))); !strings.Contains(row, "p1 · Home · localhost:5173/ · no change · → 42") {
		t.Fatalf("eval collapsed: %s", row)
	}
}

// A Browser permission request says what would happen, on which page, and
// which origins it would open up.
func TestBrowserApprovalView(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(110, 50)
	args := `{"action":"navigate","url":"https://docs.example.com/guide","origins":["https://docs.example.com"],"page":"p1","page_url":"http://localhost:5173/","page_title":"Home"}`
	m.pending = &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Browser", Arguments: json.RawMessage(args)}, ExecutionMode: "browser", Network: true, Reason: "Open https://docs.example.com/guide in the JULIA browser?"}}
	view := ansi.Strip(m.approvalView())
	for _, want := range []string{"Browser  navigate · docs.example.com/guide", "Page", `p1 · "Home" · http://localhost:5173/`, "Approves for the rest of this turn", "https://docs.example.com", "Reaches beyond this machine"} {
		if !strings.Contains(view, want) {
			t.Errorf("approval lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Network access requested") {
		t.Errorf("approval says network access as Bash would:\n%s", view)
	}
}

// A snapshot action's result has no headline: the page leads, or its notes
// do; neither is mistaken for a headline, and a note keeps its colour.
func TestBrowserSnapshotResults(t *testing.T) {
	plain := "Page p1: \"Home\"\nURL: http://localhost:5173/\nViewport: 1280×800, scrolled to 0,0 of a 1280×800 page\n\n- heading \"Home\" [ref=e1] [level=1]"
	page, ok := parseBrowserPage(plain)
	if !ok || page.headline != "" || page.id != "p1" || len(page.lines) != 1 {
		t.Fatalf("plain snapshot: %+v, %v", page, ok)
	}
	noted := "Notes:\n- p1 opened a new page p2\n- blocked 3 requests to https://cdn.example (GET /a.js, …): not approved\nConsole errors:\n- error boom\n\n" + plain
	page, ok = parseBrowserPage(noted)
	if !ok || page.headline != "" || len(page.details) != 0 || len(page.notes) != 2 || page.blocked() != 3 || len(page.errs) != 1 {
		t.Fatalf("snapshot with notes: %+v, %v", page, ok)
	}
	m := testModel(t, Config{})
	m.resize(120, 40)
	if row := strings.Split(ansi.Strip(m.renderEntryContent(browserEntry(`{"action":"snapshot"}`, noted))), "\n")[1]; !strings.Contains(row, "p1 · Home · localhost:5173/ · 3 blocked · 1 error") {
		t.Fatalf("collapsed snapshot: %q", row)
	}
	m.expandTools = true
	if plain := ansi.Strip(m.layoutEntry(browserEntry(`{"action":"snapshot"}`, noted)).text); strings.Contains(plain, "Notes:") || !strings.Contains(plain, "· p1 opened a new page p2") {
		t.Fatalf("expanded snapshot:\n%s", plain)
	}
}

// A screenshot taken with an action is a caption under the card, not a line
// of the tree, nor part of what copying the snapshot gives.
func TestBrowserScreenshotCaption(t *testing.T) {
	content := "Typed \"ada\" into textbox \"Email\" (e2).\n\nPage p1: \"Form\"\nURL: http://localhost:5173/form\nViewport: 1280×800, scrolled to 0,0 of a 1280×800 page\n\nChanges since the last snapshot of p1 (+ added, - removed; unchanged lines left out):\n- - textbox \"Email\" [ref=e2]\n+ - textbox \"Email\" [ref=e2]: ada\n\n[screenshot: 1280×800 CSS px]"
	page, ok := parseBrowserPage(content)
	if !ok || page.shot != "screenshot: 1280×800 CSS px" || len(page.lines) != 2 || strings.Contains(page.text(), "screenshot") {
		t.Fatalf("page: %+v", page)
	}
	m := testModel(t, Config{})
	m.resize(120, 40)
	m.expandTools = true
	rows := strings.Split(ansi.Strip(m.layoutEntry(browserEntry(`{"action":"type","target":"e2","text":"ada","image":true}`, content)).text), "\n")
	if last := strings.TrimSpace(strings.TrimPrefix(rows[len(rows)-1], "│")); last != "screenshot: 1280×800 CSS px" {
		t.Fatalf("last row %q of:\n%s", last, strings.Join(rows, "\n"))
	}
}

// Console, screenshot and tabs results are drawn as page rows are: what
// needs attention counted and in colour, addresses short, entries whole.
func TestBrowserOtherResults(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(100, 40)
	console := "Console of p1 since you last looked, oldest first:\n- error Browser allow asks for it before it can load the font that the page needs (/src/App.tsx:14:22)\n- error failed GET http://127.0.0.1:52402/api (fetch): net::ERR_CONNECTION_REFUSED\n- warning careful\n- log hello"
	for _, tc := range []struct {
		args, content string
		collapsed     string
		expanded      []string
	}{
		{`{"action":"console"}`, console, "p1 · 2 errors · 1 warning · 1 log", []string{"! Browser allow asks for it before it can load the font that the page needs", "  (/src/App.tsx:14:22)", "· warning careful", "· log hello"}},
		{`{"action":"console"}`, "p1 has logged nothing since you last looked.", "p1 · nothing logged", nil},
		{`{"action":"screenshot"}`, "Screenshot of the viewport in p1 \"Groceries\" · http://127.0.0.1:52402/\n[screenshot: 1280×800 CSS px]", "p1 · Groceries · 127.0.0.1:52402/ · viewport 1280×800", []string{"screenshot: 1280×800 CSS px"}},
		{`{"action":"screenshot","full_page":true}`, "Screenshot of the full page in p1 \"Long\" · http://127.0.0.1:52402/long\n[screenshot: 1280×4000 CSS px, sent as 640×2000; the page is 27103 px tall, too tall to read whole; captured the top 4000 px: scroll and take viewport screenshots, or screenshot a target, to see the rest]", "p1 · Long · 127.0.0.1:52402/long · full page 1280×4000", []string{"the page is 27103 px tall, too tall to"}},
		{`{"action":"tabs"}`, "p1 · \"Groceries\" · http://127.0.0.1:52402/ · current\np2 · \"\" · about:blank · opened by p1\np3 · \"Docs\" · https://docs.example.com/\nApproved this turn: https://docs.example.com", "3 pages · p1 current · 1 origin approved", []string{"p1 · Groceries · 127.0.0.1:52402/ · current", "p2 · about:blank · opened by p1", "approved this turn: https://docs.example.com"}},
		{`{"action":"close","page":"p2"}`, "Closed p2.\np1 · \"Home\" · http://localhost:5173/ · current", "Closed p2 · 1 page · p1 current", nil},
	} {
		m.expandTools = false
		item := browserEntry(tc.args, tc.content)
		rows := strings.Split(ansi.Strip(m.renderEntryContent(item)), "\n")
		if len(rows) < 2 || strings.TrimSpace(strings.TrimPrefix(rows[1], "│")) != tc.collapsed {
			t.Errorf("%s collapsed:\n%s\nwant %q", tc.args, strings.Join(rows, "\n"), tc.collapsed)
		}
		m.expandTools = true
		plain := ansi.Strip(m.layoutEntry(item).text)
		for _, want := range tc.expanded {
			if !strings.Contains(plain, want) {
				t.Errorf("%s expanded lacks %q:\n%s", tc.args, want, plain)
			}
		}
		for _, row := range strings.Split(plain, "\n") {
			if ansi.StringWidth(row) > m.mainWidth {
				t.Errorf("row wider than %d: %q", m.mainWidth, row)
			}
		}
	}
	// Errors are coral; the collapsed row says so in colour.
	if row := strings.Split(m.renderEntryContent(browserEntry(`{"action":"console"}`, console)), "\n")[1]; !strings.Contains(row, coralStyle.Render("2 errors")) {
		t.Errorf("errors are not coral: %q", row)
	}
}

// Approving an origin grants it for the rest of the turn, and the key says
// so, in the panel and the footer; the page shows once, in its own section.
func TestBrowserApprovalKeys(t *testing.T) {
	m := testModel(t, Config{})
	m.resize(120, 50)
	allow := `{"action":"allow","origins":["https://fonts.googleapis.com"],"page":"p1","page_url":"http://localhost:5173/","page_title":"Home"}`
	m.pending = &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Browser", Arguments: json.RawMessage(allow)}, ExecutionMode: "browser", Network: true, Reason: "Let the JULIA browser load pages and requests from https://fonts.googleapis.com for the rest of this turn, without asking again?"}}
	view := ansi.Strip(m.approvalView())
	if !strings.Contains(view, "y / Enter  Allow for this turn") || strings.Contains(view, "Allow once") || strings.Count(view, "localhost:5173") != 1 {
		t.Fatalf("allow panel:\n%s", view)
	}
	if footer := ansi.Strip(m.footer()); !strings.Contains(footer, "y / enter allow for this turn") {
		t.Fatalf("footer: %s", footer)
	}
	click := `{"action":"click","target":"button \"Save\" (e3)","page":"p2","page_url":"about:blank","opened_by":"http://external.test/phish"}`
	m.pending = &ApprovalRequest{Approval: core.Approval{Call: core.ToolCall{Name: "Browser", Arguments: json.RawMessage(click)}, ExecutionMode: "browser", Reason: "Click this element on p2 in the JULIA browser?"}}
	view = ansi.Strip(m.approvalView())
	if !strings.Contains(view, "y / Enter  Allow once") || !strings.Contains(view, "p2 · about:blank · opened by http://external.test/phish") || strings.Contains(view, `"opened_by"`) {
		t.Fatalf("click panel:\n%s", view)
	}
}
