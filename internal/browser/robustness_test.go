package browser

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNavigationThatNeverSettles(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	// A server that never answers: bounded, and the page stays usable.
	ctx, cancel := context.WithTimeout(f.ctx, 1500*time.Millisecond)
	start := time.Now()
	err := f.p.Navigate(ctx, f.site.url("/slow"), WaitLoad)
	cancel()
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "got no response in time") {
		t.Fatalf("hanging server: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %v", took)
	}
	f.goTo(t, "/app")

	// A page whose image never loads: HTML parsed, load never fires.
	ctx, cancel = context.WithTimeout(f.ctx, 1500*time.Millisecond)
	err = f.p.Navigate(ctx, f.site.url("/stuck"), WaitLoad)
	cancel()
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "DOMContentLoaded") || !strings.Contains(err.Error(), "/slow?img") {
		t.Fatalf("stuck load: %v", err)
	}
	if snap := f.snapshot(t); !strings.Contains(snap, `heading "Almost there"`) {
		t.Fatalf("a page that never finished loading is not usable:\n%s", snap)
	}
	if err := f.p.Navigate(f.ctx, f.site.url("/stuck"), WaitDOMContentLoaded); err != nil {
		t.Fatalf("domcontentloaded: %v", err)
	}
	if err := f.p.Navigate(f.ctx, f.site.url("/stuck"), WaitCommit); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Long polling: networkidle is bounded and says what is still open.
	ctx, cancel = context.WithTimeout(f.ctx, 2*time.Second)
	err = f.p.Navigate(ctx, f.site.url("/poll"), WaitNetworkIdle)
	cancel()
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "/slow?poll") {
		t.Fatalf("networkidle under long polling: %v", err)
	}
	err = f.p.WaitFor(f.ctx, Wait{NetworkIdle: true}, time.Second)
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "1 request open") {
		t.Fatalf("WaitFor networkidle: %v", err)
	}
	if err := f.p.Navigate(f.ctx, f.site.url("/app"), WaitNetworkIdle); err != nil {
		t.Fatalf("networkidle on a quiet page: %v", err)
	}

	// Nothing listening.
	err = f.p.Navigate(f.ctx, "http://"+f.site.refused+"/", WaitLoad)
	if err == nil || !strings.Contains(err.Error(), "is the dev server running?") {
		t.Fatalf("connection refused: %v", err)
	}
}

func TestHistory(t *testing.T) {
	f := open(t)
	f.goTo(t, "/app")
	f.goTo(t, "/select")
	if err := f.p.Back(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.p.URL(), "/app") {
		t.Fatalf("back to %s", f.p.URL())
	}
	if err := f.p.Forward(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.p.URL(), "/select") {
		t.Fatalf("forward to %s", f.p.URL())
	}
	if err := f.p.Forward(f.ctx, ""); err == nil || !strings.Contains(err.Error(), "no later page") {
		t.Fatalf("forward past the end: %v", err)
	}
	f.eval(t, "history.pushState({}, '', '/select#two')")
	if err := f.p.Back(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.p.URL(), "/select") {
		t.Fatalf("back within the document to %s", f.p.URL())
	}
	if err := f.p.Reload(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	// An entry a modal pushed with the very same URL, so that Back closes it.
	f.goTo(t, "/modal-history")
	if _, err := f.p.Click(f.ctx, Target{Text: "Open modal"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	if err := f.p.Back(ctx, ""); err != nil {
		t.Fatalf("back to the same URL: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("back to the same URL took %v", took)
	}
	if got := f.eval(t, "m.hidden"); got != "true" || !strings.HasSuffix(f.p.URL(), "/modal-history") {
		t.Fatalf("after back: hidden %s at %s", got, f.p.URL())
	}
}

// A page that sends itself on before it has loaded, as a sign-in guard does,
// is followed to the document it lands on.
func TestNavigationFollowsThePageOn(t *testing.T) {
	f := open(t)
	for _, path := range []string{"/guard", "/replace"} {
		ctx, cancel := context.WithTimeout(f.ctx, 8*time.Second)
		start := time.Now()
		err := f.p.Navigate(ctx, f.site.url(path), WaitLoad)
		cancel()
		if err != nil || !strings.HasSuffix(f.p.URL(), "/login") {
			t.Fatalf("%s: %v, at %s", path, err, f.p.URL())
		}
		if took := time.Since(start); took > 4*time.Second {
			t.Fatalf("%s took %v", path, took)
		}
		if notes := noteText(f.c.Notes()); !strings.Contains(notes, "went on from "+f.site.url(path)+" to "+f.site.url("/login")) {
			t.Fatalf("%s notes:\n%s", path, notes)
		}
	}
	if snap := f.snapshot(t); !strings.Contains(snap, `heading "Sign in"`) {
		t.Fatalf("landed on:\n%s", snap)
	}
}

// What an action or a page starts fetching is waited for, briefly, so the
// snapshot shows what it brought, and the console its errors.
func TestActionsWaitForTheRequestsTheyStart(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	f := open(t)
	f.goTo(t, "/spa")
	if got := f.eval(t, "msg.textContent"); got != `"Orders: A-1, B-2"` {
		t.Fatalf("after navigate: %s", got)
	}
	f.goTo(t, "/save")
	var logs []string
	for _, e := range f.p.Logs().Entries {
		logs = append(logs, e.String())
	}
	if all := strings.Join(logs, "\n"); !strings.Contains(all, "404 Not Found GET "+f.site.url("/missing-items")) || !strings.Contains(all, "api failed") {
		t.Fatalf("the page's own fetch, logged under its navigation:\n%s", all)
	}
	if _, err := f.p.Click(f.ctx, Target{Text: "Save"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "out.textContent"); got != `"Saved!"` {
		t.Fatalf("right after the click: %s", got)
	}
	f.c.Notes()
	start := time.Now()
	if _, err := f.p.Click(f.ctx, Target{Text: "Sync"}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < requestSettle || took > requestSettle+2*time.Second {
		t.Fatalf("a request that never ends held the click %v", took)
	}
	if notes := noteText(f.c.Notes()); !strings.Contains(notes, "1 request is still open after 1.5s (GET /slow?save)") {
		t.Fatalf("notes:\n%s", notes)
	}
}

func TestCrashedPageRecovers(t *testing.T) {
	f := open(t)
	f.goTo(t, "/app")
	f.p.root.send("Page.crash", nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !f.p.Crashed() {
		time.Sleep(20 * time.Millisecond)
	}
	if !f.p.Crashed() {
		t.Fatal("the crash went unnoticed")
	}
	if _, err := f.p.Snapshot(f.ctx, SnapshotOptions{}); !errors.Is(err, ErrCrashed) || err.Error() != "page "+f.p.ID()+" crashed; reload it or navigate to recover" {
		t.Fatalf("snapshot of a crashed page: %v", err)
	}
	// Its minidump goes into the throwaway profile, not the crash database of
	// the user's own Chrome.
	var dumps []string
	for deadline := time.Now().Add(5 * time.Second); len(dumps) == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		dumps, _ = filepath.Glob(filepath.Join(f.b.ProfileDir(), "Crashpad", "*", "*.dmp"))
	}
	if len(dumps) == 0 {
		t.Fatalf("no minidump in the profile %s", f.b.ProfileDir())
	}
	if notes := noteText(f.c.Notes()); !strings.Contains(notes, "crashed") {
		t.Fatalf("notes:\n%s", notes)
	}
	if err := f.p.Reload(f.ctx, ""); err != nil {
		t.Fatalf("reload a crashed page: %v", err)
	}
	if snap := f.snapshot(t); !strings.Contains(snap, `heading "Checkout"`) {
		t.Fatalf("after recovery:\n%s", snap)
	}
}

// Errors about a page read as sentences, and still match their sentinels.
func TestPageErrorsReadAsSentences(t *testing.T) {
	f := open(t)
	f.goTo(t, "/app")
	if err := f.p.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	_, err := f.p.Snapshot(f.ctx, SnapshotOptions{})
	if !errors.Is(err, ErrClosed) || err.Error() != "page "+f.p.ID()+" was closed" {
		t.Fatalf("a closed page: %v", err)
	}
	g := open(t)
	g.c.Close()
	if _, err := g.p.Snapshot(g.ctx, SnapshotOptions{}); !errors.Is(err, ErrClosed) || err.Error() != "page "+g.p.ID()+" closed with its browser context" {
		t.Fatalf("a page of a closed context: %v", err)
	}
	h := open(t)
	err = h.p.WaitFor(h.ctx, Wait{Text: "never"}, 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) || strings.Contains(err.Error(), "browser:") {
		t.Fatalf("a wait that ran out: %v", err)
	}
}

func TestDialogsNeverHang(t *testing.T) {
	f := open(t)
	f.goTo(t, "/dialogs")
	for _, tc := range []struct{ button, want, note string }{
		{"Alert", `"alerted"`, `dismissed an alert dialog "Saved!"`},
		{"Delete", `"kept"`, `dismissed a confirm dialog "Delete 3 items?"`},
		{"Name", `"name=null"`, `dismissed a prompt dialog "Your name?"`},
	} {
		if _, err := f.p.Click(f.ctx, Target{Text: tc.button}); err != nil {
			t.Fatal(err)
		}
		if got := f.eval(t, "r.textContent"); got != tc.want {
			t.Fatalf("%s: %s, want %s", tc.button, got, tc.want)
		}
		if notes := noteText(f.c.Notes()); !strings.Contains(notes, tc.note) {
			t.Fatalf("%s notes:\n%s", tc.button, notes)
		}
	}
	// An answer armed before the action wins.
	f.p.AnswerNextDialog(true, "Ada")
	if _, err := f.p.Click(f.ctx, Target{Text: "Name"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "r.textContent"); got != `"name=Ada"` {
		t.Fatalf("armed prompt: %s", got)
	}
	f.p.AnswerNextDialog(true, "")
	if _, err := f.p.Click(f.ctx, Target{Text: "Delete"}); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "r.textContent"); got != `"deleted"` {
		t.Fatalf("armed confirm: %s", got)
	}
	// A dialog from Eval is answered too.
	if got, err := f.p.Eval(f.ctx, "confirm('sure?')"); err != nil || got != "false" {
		t.Fatalf("confirm from eval: %q, %v", got, err)
	}
}

func TestManualDialogsWaitForTheCaller(t *testing.T) {
	f := open(t, func(o *ContextOptions) { o.Dialogs = DialogManual })
	f.goTo(t, "/dialogs")
	start := time.Now()
	_, err := f.p.Click(f.ctx, Target{Text: "Delete"})
	var de *DialogError
	if !errors.As(err, &de) || de.Dialog.Type != "confirm" || de.Dialog.Message != "Delete 3 items?" {
		t.Fatalf("click that opens a confirm: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the click took %v", took)
	}
	// Everything else fails fast until it is answered.
	if _, err := f.p.Snapshot(f.ctx, SnapshotOptions{}); !errors.As(err, &de) {
		t.Fatalf("snapshot with a dialog open: %v", err)
	}
	if d := f.p.Dialog(); d == nil || d.Type != "confirm" {
		t.Fatalf("Dialog() = %v", d)
	}
	if err := f.p.HandleDialog(f.ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "r.textContent"); got != `"deleted"` {
		t.Fatalf("after accepting: %s", got)
	}
	if err := f.p.HandleDialog(f.ctx, true, ""); err == nil {
		t.Fatal("answered a dialog that is not open")
	}
	_, err = f.p.Click(f.ctx, Target{Text: "Name"})
	if !errors.As(err, &de) {
		t.Fatalf("prompt: %v", err)
	}
	if err := f.p.HandleDialog(f.ctx, true, "Grace"); err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, "r.textContent"); got != `"name=Grace"` {
		t.Fatalf("prompt answer: %s", got)
	}
}

func TestBeforeUnloadDoesNotBlockLeaving(t *testing.T) {
	f := open(t)
	f.goTo(t, "/unload")
	// beforeunload only asks once the page has been used.
	if _, err := f.p.Type(f.ctx, Target{Text: "Draft"}, "unsaved work", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Eval(f.ctx, "setTimeout(() => location.href = '/app', 0); 1"); err != nil {
		t.Fatal(err)
	}
	if err := f.p.WaitFor(f.ctx, Wait{URL: "/app"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	f.goTo(t, "/unload")
	if _, err := f.p.Type(f.ctx, Target{Text: "Draft"}, "more work", false); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Reload(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	f.goTo(t, "/app")
}

func TestStaleRefs(t *testing.T) {
	f := open(t)
	f.goTo(t, "/rerender")
	snap := f.snapshot(t)
	ref := refOf(t, snap, `button "Replace me"`)
	if _, err := f.p.Click(f.ctx, Target{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	// The click replaced the button: its ref is gone.
	_, err := f.p.Click(f.ctx, Target{Ref: ref})
	if err == nil || !strings.Contains(err.Error(), "no longer on the page; take a new snapshot") {
		t.Fatalf("ref of a removed element: %v", err)
	}
	again := refOf(t, f.snapshot(t), `button "Again"`)
	if again == ref {
		t.Fatal("a new element got an old ref")
	}
	// Refs survive another snapshot of the same document.
	if refOf(t, f.snapshot(t), `button "Again"`) != again {
		t.Fatal("a ref changed between snapshots")
	}
	f.goTo(t, "/app")
	_, err = f.p.Click(f.ctx, Target{Ref: again})
	if err == nil || !strings.Contains(err.Error(), "from before the page navigated") {
		t.Fatalf("ref from before a navigation: %v", err)
	}
}

func refOf(t *testing.T, snap, node string) string {
	t.Helper()
	for _, line := range strings.Split(snap, "\n") {
		if strings.Contains(line, node) {
			if i := strings.Index(line, "[ref="); i >= 0 {
				return line[i+5 : i+5+strings.IndexByte(line[i+5:], ']')]
			}
		}
	}
	t.Fatalf("no ref for %s in:\n%s", node, snap)
	return ""
}
