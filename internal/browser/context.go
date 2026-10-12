package browser

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
)

// Request is a request a page, frame or worker is about to make, as the
// policy sees it. Navigate also asks about the URL it is given before it
// tries it.
type Request struct {
	URL    *url.URL
	Method string
	// Type is how the response will be used: Document, Stylesheet, Image,
	// Media, Font, Script, XHR, Fetch, EventSource, WebSocket, Manifest,
	// Ping, Prefetch, Other.
	Type string
	// Navigation is true for a document loading in a page or frame.
	Navigation bool
	// Page is the page the request belongs to, or "" for a service worker or
	// shared worker, which serve every page of the context.
	Page string
	// MainFrame is true for a document loading in the page itself rather
	// than in a frame: a navigation the page or the agent started.
	MainFrame bool
	// From is whose request this is: the URL of the document of the frame
	// that makes it, a frame's own for a request from inside it (its
	// parent's, for an about:blank, srcdoc or data: frame, and its opener's,
	// for a popup still at about:blank), or a service or shared worker's
	// script. It is the document being left for a navigation, and "" when
	// Navigate asks.
	From string
}

// Decision is a policy's verdict on a Request. Reason is shown to the agent
// when the request is blocked.
type Decision struct {
	Allow  bool
	Reason string
}

// Policy decides whether a request may proceed. It runs on the connection's
// reader goroutine while the request waits, so it must be quick and must not
// call back into the browser. A panic blocks the request.
type Policy func(Request) Decision

// DialogPolicy says how JavaScript dialogs (alert, confirm, prompt,
// beforeunload) are answered. Whatever it says, an answer armed with
// Page.AnswerNextDialog is used first, and every dialog is reported as a Note.
type DialogPolicy int

const (
	// DialogAuto accepts alerts and beforeunload (the page may be left) and
	// dismisses confirm and prompt, the choice that changes nothing.
	DialogAuto DialogPolicy = iota
	// DialogAccept accepts every dialog; a prompt gets its default text.
	DialogAccept
	// DialogDismiss dismisses every dialog.
	DialogDismiss
	// DialogManual leaves a dialog open: the action that opened it returns
	// a *DialogError, and every other action fails fast with one until
	// Page.HandleDialog answers it.
	DialogManual
)

// ContextOptions configures a browser context.
type ContextOptions struct {
	// Policy decides every request of the context's pages and workers. Nil
	// allows all of them. URLs internal to the browser (chrome:, devtools:,
	// view-source: and the like) are refused whatever it says.
	Policy Policy
	// Viewport is the size new pages start with; zero means 1280×800 at 1×.
	Viewport Viewport
	// Dialogs says how dialogs are answered.
	Dialogs DialogPolicy
	// Downloads is the directory Chrome saves downloads to, each under a
	// name of its own. Empty refuses them.
	Downloads string
	// Keep, when set, is given each finished download from a goroutine of
	// its own: the file Chrome wrote and the name the page suggested. What it
	// returns is where the caller keeps the file, which the note reports: a
	// directory Chrome writes into may be one only the caller can trust.
	Keep func(path, name string) (string, error)
	// MaxDownload bounds each download in bytes: Chrome's is canceled once
	// it says it will grow, or has grown, past it. Zero means no bound.
	MaxDownload int64
	// LoopbackOnly sends every connection not addressed to a loopback host
	// (localhost, *.localhost, 127.0.0.0/8, [::1]) to the browser's proxy,
	// which opens only what Options.Dial allows: a network-level backstop for
	// what Policy cannot see, chiefly WebSockets and preconnects, which
	// Chrome opens without a request the Fetch domain can pause. Chrome
	// counts what arrives through that proxy as the public internet, so its
	// Local Network Access checks keep a page or frame from there off
	// loopback servers, WebSockets included.
	LoopbackOnly bool
	// Files, when set, serves every file: URL the policy allows, so Chrome
	// never reads the disk itself: the caller opens each file as it sees fit
	// (without following symlinks, say) at the moment it is read. path is the
	// URL's decoded path. An error fails the request; one wrapping
	// fs.ErrNotExist fails it as a missing file, any other is reported as a
	// blocked request.
	Files func(path string) (body []byte, mediaType string, err error)
}

// Note is something that happened that the agent should hear about: a popup,
// a dialog, a blocked request, a refused download.
type Note struct {
	Page string // the page it concerns, "" for the context as a whole
	Kind string
	Text string
}

// Note kinds.
const (
	NotePopup       = "popup"
	NoteDialog      = "dialog"
	NoteBlocked     = "blocked"
	NoteDownload    = "download"
	NoteAuth        = "auth"
	NoteCrash       = "crash"
	NoteClosed      = "closed"
	NoteFileChooser = "filechooser"
	NoteNavigated   = "navigated"
	NotePending     = "pending"
)

const (
	maxNotes = 100
	maxPages = 50
)

// Context is an isolated browser context, with its own cookies, storage and
// cache: an incognito profile. A subagent gets a context of its own.
type Context struct {
	b    *Browser
	id   cdp.BrowserContextID
	opts ContextOptions

	mu        sync.Mutex
	pages     []*Page
	notes     []noteEntry
	dropped   int
	downloads map[string]download
	last      *Page // the page acted on most recently, for worker output
	closed    bool
}

type noteEntry struct {
	Note
	key      string // blocked requests with the same key are counted, not repeated
	count    int
	origin   string
	reason   string
	examples []string
}

type download struct {
	page, name, url string
}

// NewContext creates an isolated browser context.
func (b *Browser) NewContext(ctx context.Context, opts ContextOptions) (*Context, error) {
	if err := b.Err(); err != nil {
		return nil, err
	}
	if opts.Viewport == (Viewport{}) {
		opts.Viewport = DefaultViewport
	}
	if err := opts.Viewport.check(); err != nil {
		return nil, err
	}
	if opts.LoopbackOnly && !b.checksLocalNetwork() {
		// Only Local Network Access keeps a page from beyond loopback off
		// loopback servers by WebSocket, which the policy cannot pause.
		return nil, fmt.Errorf("%s does not enforce Local Network Access; Chrome %d or later is required", b.product, minLocalNetworkMajor)
	}
	params := target.CreateBrowserContextParams{DisposeOnDetach: ptr(true)}
	if opts.LoopbackOnly {
		params.ProxyServer = "http://" + b.proxy.addr()
	}
	res, err := cdp.Call(ctx, b.root, target.CreateBrowserContext, params)
	if err != nil {
		return nil, err
	}
	c := &Context{b: b, id: res.BrowserContextID, opts: opts, downloads: map[string]download{}}
	behavior := browser.SetDownloadBehaviorParams{Behavior: browser.SetDownloadBehaviorBehaviorDeny, BrowserContextID: c.id, EventsEnabled: ptr(true)}
	if opts.Downloads != "" {
		dir, err := filepath.Abs(opts.Downloads)
		if err != nil {
			return nil, err
		}
		behavior.Behavior, behavior.DownloadPath = browser.SetDownloadBehaviorBehaviorAllowAndName, dir
	}
	if _, err := cdp.Call(ctx, b.root, browser.SetDownloadBehavior, behavior); err != nil {
		b.root.send(target.CommandDisposeBrowserContext, target.DisposeBrowserContextParams{BrowserContextID: c.id})
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return nil, ErrClosed
	}
	b.contexts[c.id] = c
	return c, nil
}

// NewPage opens a blank page in the context.
func (c *Context) NewPage(ctx context.Context) (*Page, error) {
	c.mu.Lock()
	closed, open := c.closed, 0
	for _, p := range c.pages {
		if !p.isClosed() {
			open++
		}
	}
	c.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if open >= maxPages {
		return nil, fmt.Errorf("browser: %d pages are open; close some first", open)
	}
	ctx, cancel := withDefault(ctx, 30*time.Second)
	defer cancel()
	// A window of its own, so pages of different contexts (subagents
	// working at once) never hide one another.
	res, err := cdp.Call(ctx, c.b.root, target.CreateTarget, target.CreateTargetParams{URL: "about:blank", BrowserContextID: c.id, NewWindow: ptr(true)})
	if err != nil {
		// The headless shell has no windows.
		res, err = cdp.Call(ctx, c.b.root, target.CreateTarget, target.CreateTargetParams{URL: "about:blank", BrowserContextID: c.id})
	}
	if err != nil {
		return nil, err
	}
	p, err := c.b.waitPage(ctx, res.TargetID)
	if err != nil {
		return nil, err
	}
	select {
	case <-p.ready:
	case <-ctx.Done():
		return nil, fmt.Errorf("new page: %w", context.Cause(ctx))
	}
	if p.readyErr != nil {
		return nil, p.readyErr
	}
	c.touch(p)
	return p, nil
}

// Pages lists the context's open pages in the order they opened.
func (c *Context) Pages() []*Page {
	c.mu.Lock()
	defer c.mu.Unlock()
	var open []*Page
	for _, p := range c.pages {
		if !p.isClosed() {
			open = append(open, p)
		}
	}
	return open
}

// Page finds an open page by id.
func (c *Context) Page(id string) (*Page, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pages {
		if p.id == id {
			if p.isClosed() {
				return nil, fmt.Errorf("page %s is closed", id)
			}
			return p, nil
		}
	}
	return nil, fmt.Errorf("no page %s", id)
}

// Notes returns what happened since the last call, oldest first, and forgets
// it. Repeated blocked requests to one origin are counted in a single note.
func (c *Context) Notes() []Note {
	c.mu.Lock()
	entries, dropped := c.notes, c.dropped
	c.notes, c.dropped = nil, 0
	c.mu.Unlock()
	notes := make([]Note, 0, len(entries)+1)
	for _, e := range entries {
		if e.count > 1 {
			more := ""
			if e.count > len(e.examples) {
				more = ", …"
			}
			e.Text = fmt.Sprintf("blocked %d requests to %s (%s%s): %s", e.count, e.origin, strings.Join(e.examples, ", "), more, e.reason)
		}
		notes = append(notes, e.Note)
	}
	if dropped > 0 {
		notes = append(notes, Note{Kind: "dropped", Text: fmt.Sprintf("%d more notes were dropped", dropped)})
	}
	return notes
}

// Close closes the context and all its pages.
func (c *Context) Close() error {
	if !c.markClosed() {
		return nil
	}
	c.b.mu.Lock()
	delete(c.b.contexts, c.id)
	c.b.mu.Unlock()
	if c.b.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cdp.Call(ctx, c.b.root, target.DisposeBrowserContext, target.DisposeBrowserContextParams{BrowserContextID: c.id})
	return err
}

func (c *Context) markClosed() bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.closed = true
	pages := c.pages
	c.mu.Unlock()
	for _, p := range pages {
		p.markClosed("closed with its browser context")
	}
	return true
}

func (c *Context) note(page, kind, text string) {
	c.addNote(noteEntry{Note: Note{Page: page, Kind: kind, Text: text}})
}

func (c *Context) addNote(e noteEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.key != "" {
		for i := range c.notes {
			if n := &c.notes[i]; n.key == e.key {
				n.count++
				if len(n.examples) < 3 && !slices.Contains(n.examples, e.examples[0]) {
					n.examples = append(n.examples, e.examples[0])
				}
				return
			}
		}
		e.count = 1
	}
	if len(c.notes) >= maxNotes {
		c.dropped++
		return
	}
	c.notes = append(c.notes, e)
}

func (c *Context) touch(p *Page) {
	c.mu.Lock()
	c.last = p
	c.mu.Unlock()
}

func (c *Context) lastPage() *Page {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// decide applies the built-in refusals, then the policy.
func (c *Context) decide(r Request) (d Decision) {
	if reason := refused(r.URL); reason != "" {
		return Decision{Reason: reason}
	}
	if r.URL.Scheme == "about" || c.opts.Policy == nil {
		return Decision{Allow: true}
	}
	defer func() {
		if recover() != nil {
			d = Decision{Reason: "the request policy failed"}
		}
	}()
	d = c.opts.Policy(r)
	if !d.Allow && d.Reason == "" {
		d.Reason = "blocked by policy"
	}
	return d
}

// refused says why a URL is never loaded, whatever the policy: the browser's
// own pages and schemes that run code or reach outside the web.
func refused(u *url.URL) string {
	switch u.Scheme {
	case "http", "https", "ws", "wss", "file", "data", "blob":
		return ""
	case "about":
		if u.Opaque == "blank" || u.Opaque == "srcdoc" {
			return ""
		}
		return "only about:blank is available"
	case "javascript":
		return "javascript: URLs are not loaded; evaluate script instead"
	case "":
		return "not an absolute URL"
	default:
		return u.Scheme + ": URLs are not available"
	}
}

// intercept answers a request paused by the Fetch domain on any session: the
// policy decides, and a blocked request fails as blocked-by-client and is
// recorded where the agent will see it.
func (c *Context) intercept(s *session, p *Page, params jsontext.Value) {
	var ev fetch.EventRequestPaused
	if !decode(params, &ev) || ev.Request == nil {
		return
	}
	raw := ev.Request.URL + ev.Request.URLFragment
	r := Request{Method: ev.Request.Method, Type: string(ev.ResourceType), Navigation: ev.ResourceType == "Document", From: s.url}
	if p != nil {
		r.Page = p.id
		r.MainFrame = r.Navigation && s == p.root && string(ev.FrameID) == p.targetID
		if r.MainFrame {
			r.From = c.documentOf(p)
		} else {
			r.From = p.frameDocument(ev.FrameID)
		}
	}
	u, err := url.Parse(raw)
	d := Decision{Reason: "malformed URL"}
	if err == nil {
		r.URL = u
		d = c.decide(r)
	}
	if d.Allow && u.Scheme == "file" && c.opts.Files != nil {
		// Reading may take a while; the reader goroutine must not wait.
		go c.serveFile(s, p, ev.RequestID, r)
		return
	}
	if d.Allow {
		s.send(fetch.CommandContinueRequest, fetch.ContinueRequestParams{RequestID: ev.RequestID})
		return
	}
	s.send(fetch.CommandFailRequest, fetch.FailRequestParams{RequestID: ev.RequestID, ErrorReason: "BlockedByClient"})
	c.blocked(p, r.Method, raw, r.Type, d.Reason, s.kind)
}

var escapeHTML = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;")

// documentOf is the URL whose origin p's requests carry: its own, or for a
// popup still at about:blank, which took on its opener's origin as it
// opened, the opener's document then.
func (c *Context) documentOf(p *Page) string {
	from := p.URL()
	if p.opener != "" && p.openerDoc != "" && (from == "" || strings.HasPrefix(from, "about:")) {
		return p.openerDoc
	}
	for hops := 0; hops < 5 && p.opener != "" && (from == "" || strings.HasPrefix(from, "about:")); hops++ {
		opener, err := c.Page(p.opener)
		if err != nil {
			break
		}
		p, from = opener, opener.URL()
	}
	return from
}

// serveFile answers an allowed file: request from ContextOptions.Files.
func (c *Context) serveFile(s *session, p *Page, id fetch.RequestID, r Request) {
	body, media, err := c.opts.Files(r.URL.Path)
	status := int64(200)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A 404 is logged like any other, and a page shows what is missing.
		status, media, body = 404, "text/html; charset=utf-8", nil
		if r.Type == "Document" {
			body = fmt.Appendf(nil, "<!doctype html><title>No such file</title><h1>No such file</h1><p>%s</p>", escapeHTML.Replace(r.URL.Path))
		}
	case err != nil:
		s.send(fetch.CommandFailRequest, fetch.FailRequestParams{RequestID: id, ErrorReason: network.ErrorReasonBlockedByClient})
		c.blocked(p, r.Method, r.URL.String(), r.Type, err.Error(), s.kind)
		return
	}
	headers := []*fetch.HeaderEntry{{Name: "Content-Type", Value: cmpOr(media, "application/octet-stream")}, {Name: "Cache-Control", Value: "no-store"}}
	s.send(fetch.CommandFulfillRequest, fetch.FulfillRequestParams{RequestID: id, ResponseCode: status, ResponseHeaders: headers, Body: body})
}

func (c *Context) blocked(p *Page, method, raw, kind, reason, from string) {
	page := ""
	if p != nil {
		page = p.id
		p.log(LogEntry{Kind: "blocked", Level: "error", Text: fmt.Sprintf("blocked %s %s (%s): %s", method, clip(raw, 300), strings.ToLower(kind), reason)})
	}
	origin, example := raw, raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		origin, example = u.Scheme+"://"+u.Host, u.RequestURI()
	}
	where := ""
	if from == "service_worker" || from == "shared_worker" || from == "worker" {
		where = " from a " + strings.ReplaceAll(from, "_", " ")
	}
	c.addNote(noteEntry{
		Note:     Note{Page: page, Kind: NoteBlocked, Text: fmt.Sprintf("blocked %s %s%s (%s): %s", method, clip(raw, 300), where, strings.ToLower(kind), reason)},
		key:      page + "\x00" + origin + "\x00" + reason,
		origin:   clip(origin, 200),
		reason:   reason,
		examples: []string{method + " " + clip(example, 80)},
	})
}

// webSocket reports a WebSocket the policy refuses. Chrome opens WebSockets
// without a request the Fetch domain can pause, so only LoopbackOnly stops
// one: the proxy refuses it beyond loopback, and Chrome's Local Network
// Access checks refuse a page that came through the proxy a loopback one.
// A loopback page's WebSocket to loopback is beyond both, so that one is
// reported as opened.
func (c *Context) webSocket(p *Page, params jsontext.Value) {
	var ev network.EventWebSocketCreated
	if !decode(params, &ev) {
		return
	}
	u, err := url.Parse(ev.URL)
	if err != nil {
		return
	}
	from := c.documentOf(p)
	d := c.decide(Request{URL: u, Method: "GET", Type: "WebSocket", Page: p.id, From: from})
	if d.Allow {
		return
	}
	if c.opts.LoopbackOnly && (!isLoopback(u.Hostname()) || public(from)) {
		c.blocked(p, "GET", ev.URL, "WebSocket", d.Reason, "page")
		return
	}
	text := fmt.Sprintf("a WebSocket to %s opened although the policy refuses it (%s): WebSockets cannot be intercepted", clip(ev.URL, 300), d.Reason)
	p.log(LogEntry{Kind: "blocked", Level: "warning", Text: text})
	c.note(p.id, NoteBlocked, text)
}

// public reports whether a document came through the proxy, which Chrome
// counts as the public internet: an http(s) document, or a blob: one made
// there, not on a loopback host.
func public(doc string) bool {
	u, err := url.Parse(doc)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Scheme, "blob") {
		if u, err = url.Parse(u.Opaque); err != nil {
			return false
		}
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Hostname() != "" && !isLoopback(u.Hostname())
}

// isLoopback reports whether Chrome connects to host directly even when a
// proxy is set: its implicit bypass rules.
func isLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// authRequired refuses an HTTP authentication challenge at once: the agent
// has no credentials to give, and an unanswered challenge would hang.
func (c *Context) authRequired(s *session, p *Page, params jsontext.Value) {
	var ev fetch.EventAuthRequired
	if !decode(params, &ev) {
		return
	}
	s.send(fetch.CommandContinueWithAuth, fetch.ContinueWithAuthParams{RequestID: ev.RequestID, AuthChallengeResponse: &fetch.AuthChallengeResponse{Response: "CancelAuth"}})
	page, where := "", ""
	if p != nil {
		page = p.id
	}
	if ev.Request != nil {
		where = " from " + clip(ev.Request.URL, 200)
	}
	realm := ""
	if ev.AuthChallenge != nil && ev.AuthChallenge.Realm != "" {
		realm = fmt.Sprintf(" (realm %q)", ev.AuthChallenge.Realm)
	}
	c.note(page, NoteAuth, fmt.Sprintf("refused an HTTP authentication challenge%s%s: the browser cannot sign in", where, realm))
}

// attachWorker polices a service worker or shared worker of the context.
func (c *Context) attachWorker(ev target.EventAttachedToTarget) {
	s := c.b.conn.newSession(string(ev.SessionID), string(ev.TargetInfo.TargetID), ev.TargetInfo.Type, c.onWorkerEvent)
	s.url = ev.TargetInfo.URL
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := prepareWorker(ctx, s); err != nil {
			// A worker the policy cannot see must not run.
			c.b.root.send(target.CommandCloseTarget, target.CloseTargetParams{TargetID: target.ID(s.targetID)})
		}
	}()
}

func (c *Context) onWorkerEvent(s *session, method string, params jsontext.Value) {
	switch method {
	case "Fetch.requestPaused":
		c.intercept(s, nil, params)
	case "Fetch.authRequired":
		c.authRequired(s, nil, params)
	case "Runtime.consoleAPICalled", "Runtime.exceptionThrown":
		if p := c.lastPage(); p != nil {
			p.onConsole(s, method, params)
		}
	}
}

func (c *Context) downloadBegan(p *Page, ev browser.EventDownloadWillBegin) {
	c.mu.Lock()
	tracked := len(c.downloads) < 100
	if tracked {
		c.downloads[ev.GUID] = download{page: p.id, name: ev.SuggestedFilename, url: ev.URL}
	}
	c.mu.Unlock()
	switch {
	case c.opts.Downloads == "":
		c.note(p.id, NoteDownload, fmt.Sprintf("refused a download of %s from %s: downloads are disabled", quoteName(ev.SuggestedFilename), clip(ev.URL, 200)))
	case !tracked:
		// An untracked download could not be bounded.
		c.cancelDownload(ev.GUID)
		c.note(p.id, NoteDownload, fmt.Sprintf("canceled a download of %s from %s: too many downloads are in progress", quoteName(ev.SuggestedFilename), clip(ev.URL, 200)))
	}
}

func (c *Context) cancelDownload(guid string) {
	c.b.root.send(browser.CommandCancelDownload, browser.CancelDownloadParams{GUID: guid, BrowserContextID: c.id})
}

func (c *Context) downloadProgress(ev browser.EventDownloadProgress) {
	c.mu.Lock()
	d, ok := c.downloads[ev.GUID]
	if ok && ev.State != browser.DownloadProgressStateInProgress {
		delete(c.downloads, ev.GUID)
	}
	c.mu.Unlock()
	if !ok || c.opts.Downloads == "" {
		return
	}
	limit := float64(c.opts.MaxDownload)
	if limit > 0 && (ev.TotalBytes > limit || ev.ReceivedBytes > limit) {
		c.mu.Lock()
		delete(c.downloads, ev.GUID)
		c.mu.Unlock()
		if ev.State == browser.DownloadProgressStateInProgress {
			c.cancelDownload(ev.GUID)
		} else if ev.State == browser.DownloadProgressStateCompleted {
			path := ev.FilePath
			if path == "" {
				path = filepath.Join(c.opts.Downloads, ev.GUID)
			}
			os.Remove(path)
		}
		c.note(d.page, NoteDownload, fmt.Sprintf("canceled the download of %s: it is larger than %d MiB", quoteName(d.name), c.opts.MaxDownload>>20))
		return
	}
	switch ev.State {
	case browser.DownloadProgressStateCompleted:
		path := ev.FilePath
		if path == "" {
			path = filepath.Join(c.opts.Downloads, ev.GUID) // AllowAndName names it by its GUID
		}
		size := int64(ev.ReceivedBytes)
		if c.opts.Keep == nil {
			c.note(d.page, NoteDownload, fmt.Sprintf("downloaded %s (%d bytes) to %s", quoteName(d.name), size, path))
			return
		}
		// Copying may take a while; the reader goroutine must not wait.
		go func() {
			kept, err := c.opts.Keep(path, d.name)
			if err != nil {
				c.note(d.page, NoteDownload, fmt.Sprintf("downloaded %s (%d bytes) but could not keep it: %v", quoteName(d.name), size, err))
				return
			}
			c.note(d.page, NoteDownload, fmt.Sprintf("downloaded %s (%d bytes) to %s", quoteName(d.name), size, kept))
		}()
	case browser.DownloadProgressStateCanceled:
		c.note(d.page, NoteDownload, fmt.Sprintf("the download of %s was canceled", quoteName(d.name)))
	}
}

func quoteName(name string) string {
	if name == "" {
		return "a file"
	}
	return fmt.Sprintf("%q", name)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// withDefault bounds ctx by d unless it already has a deadline.
func withDefault(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
