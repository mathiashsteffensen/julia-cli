package browser

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
)

// Viewport is a page's emulated screen.
type Viewport struct {
	Width, Height int     // CSS pixels
	Scale         float64 // device pixel ratio; 0 means 1
	// Mobile emulates a phone: the meta viewport is honoured, touch is
	// available, and the user agent is a mobile Chrome's.
	Mobile bool
}

// DefaultViewport is a laptop screen.
var DefaultViewport = Viewport{Width: 1280, Height: 800, Scale: 1}

// Viewport limits keep screenshots and layout within reason.
const (
	MinViewport = 120
	MaxViewport = 4096
)

func (v Viewport) check() error {
	if v.Width < MinViewport || v.Width > MaxViewport || v.Height < MinViewport || v.Height > MaxViewport {
		return fmt.Errorf("viewport must be between %d and %d CSS pixels on each side", MinViewport, MaxViewport)
	}
	if v.Scale != 0 && (v.Scale < 0.5 || v.Scale > 4) {
		return errors.New("device scale factor must be between 0.5 and 4")
	}
	return nil
}

func (v Viewport) scale() float64 {
	if v.Scale == 0 {
		return 1
	}
	return v.Scale
}

// Media is emulated CSS media. An empty field means the default (light,
// no-preference, screen) rather than the host's setting, so a page looks
// the same on every machine.
type Media struct {
	ColorScheme   string // prefers-color-scheme: "light" or "dark"
	ReducedMotion string // prefers-reduced-motion: "reduce" or "no-preference"
	Type          string // the media type: "screen" or "print"
}

func (m Media) check() error {
	switch {
	case m.ColorScheme != "" && m.ColorScheme != "light" && m.ColorScheme != "dark":
		return errors.New(`color scheme must be "light" or "dark"`)
	case m.ReducedMotion != "" && m.ReducedMotion != "reduce" && m.ReducedMotion != "no-preference":
		return errors.New(`reduced motion must be "reduce" or "no-preference"`)
	case m.Type != "" && m.Type != "screen" && m.Type != "print":
		return errors.New(`media type must be "screen" or "print"`)
	}
	return nil
}

// Dialog is a JavaScript dialog a page opened.
type Dialog struct {
	Type          string // alert, confirm, prompt or beforeunload
	Message       string
	DefaultPrompt string
	URL           string
}

func (d Dialog) String() string {
	return fmt.Sprintf("%s %q", d.Type, clip(d.Message, 300))
}

// DialogError reports a dialog left open for the caller to answer
// (DialogManual): the action that opened it, and every action until it is
// answered, returns one.
type DialogError struct{ Dialog Dialog }

func (e *DialogError) Error() string {
	return fmt.Sprintf("a %s dialog is open; answer it (accept or dismiss) before doing anything else on the page", e.Dialog)
}

type dialogAnswer struct {
	accept bool
	text   string
}

// Page is a tab. Its methods are safe for concurrent use, though actions on
// one page are best made one at a time, as a person would.
type Page struct {
	b         *Browser
	ctx       *Context
	id        string
	targetID  string
	opener    string
	openerDoc string // the document of the page that opened this one, then
	root      *session

	ready    chan struct{}
	readyErr error

	mu        sync.Mutex
	url       string
	title     string
	loader    string          // the main frame's current document
	loaders   []string        // its recent documents, oldest first, loader last
	lifecycle map[string]bool // lifecycle events of loader
	loading   bool
	starts    int // navigations started in the main frame
	sameDoc   int // navigations within its document: history entries, fragments
	children  map[string]*session
	frames    map[cdp.FrameID]*session // out-of-process iframes by frame id
	docs      map[cdp.FrameID]frameDoc // every frame's document
	inflight  map[string]inflight
	requests  int       // requests begun, which number inflight's
	netQuiet  time.Time // when inflight last changed
	viewport  Viewport
	media     Media
	dialog    *Dialog
	armed     *dialogAnswer
	crashed   string
	closed    string
	interrupt chan struct{} // closed when a dialog waits, the page crashes or closes
	changed   chan struct{} // closed and replaced on every state change
	logs      logBuffer
	refs      refTable
}

type inflight struct {
	url, method, kind, loader, session string
	seq                                int
}

// frameDoc is a frame's document and the frame that holds the frame.
type frameDoc struct {
	url    string
	parent cdp.FrameID
}

const maxInflight = 2000

func newPage(c *Context, info *target.Info) *Page {
	return &Page{
		b: c.b, ctx: c, targetID: string(info.TargetID),
		ready: make(chan struct{}), url: info.URL, title: info.Title,
		lifecycle: map[string]bool{}, children: map[string]*session{}, frames: map[cdp.FrameID]*session{}, docs: map[cdp.FrameID]frameDoc{},
		inflight: map[string]inflight{}, netQuiet: time.Now(),
		interrupt: make(chan struct{}), changed: make(chan struct{}),
	}
}

// ID is the page's short id, such as "p1".
func (p *Page) ID() string { return p.id }

// Opener is the id of the page that opened this one as a popup, if any.
func (p *Page) Opener() string { return p.opener }

// Document is the URL whose origin the page's document carries: its own, or,
// for a popup still at about:blank, which takes on its opener's origin, the
// document of the page that opened it. What a page there is given, its
// opener can read.
func (p *Page) Document() string { return p.ctx.documentOf(p) }

// URL is the page's current URL.
func (p *Page) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url
}

// Title is the page's current title.
func (p *Page) Title() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.title
}

// Viewport is the page's emulated screen.
func (p *Page) Viewport() Viewport {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.viewport
}

// Dialog is the dialog waiting for an answer, if any (DialogManual).
func (p *Page) Dialog() *Dialog {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dialog == nil {
		return nil
	}
	d := *p.dialog
	return &d
}

// Crashed reports whether the page's renderer has crashed.
func (p *Page) Crashed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.crashed != ""
}

func (p *Page) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed != ""
}

// adopt takes on a page target the browser attached, paused, in this
// context: one NewPage asked for, or a popup a page opened.
func (c *Context) adopt(ev target.EventAttachedToTarget) {
	info := ev.TargetInfo
	if info.Subtype != "" {
		// A prerender or other hidden page: nothing the agent asked for.
		c.b.root.send(target.CommandCloseTarget, target.CloseTargetParams{TargetID: info.TargetID})
		return
	}
	p := newPage(c, info)
	p.root = c.b.conn.newSession(string(ev.SessionID), string(info.TargetID), "page", p.onEvent)
	p.viewport = c.opts.Viewport
	opener := c.b.page(info.OpenerID)
	if opener != nil && opener.ctx == c {
		p.opener, p.openerDoc = opener.id, c.documentOf(opener)
		opener.mu.Lock()
		p.viewport, p.media = opener.viewport, opener.media
		opener.mu.Unlock()
	}
	c.b.register(p)
	c.mu.Lock()
	closed := c.closed
	if len(c.pages) >= 2*maxPages {
		// Forget closed pages, but not so soon that their ids mean nothing.
		c.pages = slices.DeleteFunc(c.pages, (*Page).isClosed)
	}
	c.pages = append(c.pages, p)
	c.mu.Unlock()
	if closed {
		c.b.root.send(target.CommandCloseTarget, target.CloseTargetParams{TargetID: info.TargetID})
		return
	}
	if p.opener != "" {
		where := ""
		if info.URL != "" && info.URL != "about:blank" {
			where = " at " + clip(info.URL, 200)
		}
		c.note(p.opener, NotePopup, fmt.Sprintf("%s opened a new page %s%s", p.opener, p.id, where))
	}
	go p.setup()
}

func (p *Page) setup() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.prepare(ctx, p.root, true); err != nil {
		// A page the policy cannot see must not run.
		p.b.root.send(target.CommandCloseTarget, target.CloseTargetParams{TargetID: target.ID(p.targetID)})
		p.readyErr = fmt.Errorf("set up page: %w", err)
	}
	close(p.ready)
}

type command struct {
	method string
	params any
	must   bool // the session must not run without it
}

// prepare sets up a page or out-of-process frame session and lets the
// paused target run. Interception comes first and is awaited: the browser
// answers it while the target is paused, and nothing may load before the
// policy sees it. The renderer answers the rest only once the target runs,
// so they are queued ahead of runIfWaitingForDebugger, which Chrome then
// handles after them, and awaited after it.
func (p *Page) prepare(ctx context.Context, s *session, main bool) error {
	if err := s.pipeline(ctx, []command{fetchEnable, autoAttach}); err != nil {
		return err
	}
	p.mu.Lock()
	viewport, media := p.viewport, p.media
	p.mu.Unlock()
	cmds := []command{
		{"Page.enable", nil, true},
		{"Runtime.enable", nil, true},
		{"Network.enable", network.EnableParams{MaxTotalBufferSize: ptr[int64](1 << 20), MaxResourceBufferSize: ptr[int64](256 << 10), MaxPostDataSize: ptr[int64](1 << 10)}, false},
		{"Log.enable", nil, false},
		{"Inspector.enable", nil, false},
		{"Emulation.setEmulatedMedia", mediaParams(media), false},
	}
	if main {
		cmds = append(cmds,
			command{"Page.setLifecycleEventsEnabled", page.SetLifecycleEventsEnabledParams{Enabled: true}, true},
			command{"Page.setInterceptFileChooserDialog", page.SetInterceptFileChooserDialogParams{Enabled: true}, false},
			// Every page behaves as the focused one, as the page a person
			// works in is, though several share one headless window.
			command{"Emulation.setFocusEmulationEnabled", emulation.SetFocusEmulationEnabledParams{Enabled: true}, false},
		)
		cmds = append(cmds, p.viewportCommands(viewport)...)
	}
	return s.pipelineThenRun(ctx, cmds, true)
}

var (
	fetchEnable = command{"Fetch.enable", fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}, HandleAuthRequests: ptr(true)}, true}
	autoAttach  = command{"Target.setAutoAttach", target.SetAutoAttachParams{AutoAttach: true, WaitForDebuggerOnStart: true, Flatten: ptr(true)}, true}
)

// prepareWorker polices a worker, as prepare does a page: interception
// awaited, then what it reports, and then it runs. A dedicated worker has
// no Fetch domain of its own: its requests, its script included, pass
// through its page's, which intercepts them already. A service or shared
// worker serves no one page and must be intercepted itself.
func prepareWorker(ctx context.Context, s *session) error {
	err := s.pipeline(ctx, []command{fetchEnable})
	var perr *ProtocolError
	if err != nil && !(s.kind == "worker" && errors.As(err, &perr) && perr.Code == -32601) {
		return err
	}
	return s.pipelineThenRun(ctx, []command{
		autoAttach,
		{"Network.enable", network.EnableParams{MaxTotalBufferSize: ptr[int64](1 << 20), MaxResourceBufferSize: ptr[int64](256 << 10)}, false},
		{"Runtime.enable", nil, false},
	}, true)
}

func (p *Page) viewportCommands(v Viewport) []command {
	cmds := []command{
		{"Emulation.setDeviceMetricsOverride", emulation.SetDeviceMetricsOverrideParams{Width: int64(v.Width), Height: int64(v.Height), DeviceScaleFactor: v.scale(), Mobile: v.Mobile}, true},
		{"Emulation.setTouchEmulationEnabled", emulation.SetTouchEmulationEnabledParams{Enabled: v.Mobile, MaxTouchPoints: 5}, false},
	}
	if v.Mobile {
		cmds = append(cmds, command{"Emulation.setUserAgentOverride", emulation.SetUserAgentOverrideParams{UserAgent: p.b.mobileUserAgent(), Platform: "Linux armv8l"}, false})
	} else if p.b.userAgent != "" {
		cmds = append(cmds, command{"Emulation.setUserAgentOverride", emulation.SetUserAgentOverrideParams{UserAgent: p.b.userAgent}, false})
	}
	return cmds
}

func mediaParams(m Media) emulation.SetEmulatedMediaParams {
	return emulation.SetEmulatedMediaParams{
		Media: m.Type,
		Features: []*emulation.MediaFeature{
			{Name: "prefers-color-scheme", Value: cmpOr(m.ColorScheme, "light")},
			{Name: "prefers-reduced-motion", Value: cmpOr(m.ReducedMotion, "no-preference")},
		},
	}
}

// pipeline sends cmds in order without waiting between them, then waits for
// every reply. It fails if a command marked must fails.
func (s *session) pipeline(ctx context.Context, cmds []command) error {
	return s.pipelineThenRun(ctx, cmds, false)
}

// pipelineThenRun is pipeline that, if run, also lets a paused target run
// once cmds are queued and before their replies are awaited. Resuming a
// target that is not paused does nothing, so it is always sent: Chrome does
// not always say a worker is waiting.
func (s *session) pipelineThenRun(ctx context.Context, cmds []command, run bool) error {
	calls := make([]*pendingCall, len(cmds))
	for i, cmd := range cmds {
		call, err := s.start(cmd.method, cmd.params)
		if err != nil {
			return err
		}
		calls[i] = call
	}
	if run {
		s.send("Runtime.runIfWaitingForDebugger", nil)
	}
	var first error
	for i, call := range calls {
		if _, err := s.await(ctx, call); err != nil && cmds[i].must && first == nil {
			first = err
		}
	}
	return first
}

// onEvent handles the events of the page's session and of its frames' and
// workers' sessions, on the reader goroutine.
func (p *Page) onEvent(s *session, method string, params jsontext.Value) {
	main := s == p.root
	switch method {
	case "Fetch.requestPaused":
		p.ctx.intercept(s, p, params)
	case "Fetch.authRequired":
		p.ctx.authRequired(s, p, params)
	case "Target.attachedToTarget":
		var ev target.EventAttachedToTarget
		if decode(params, &ev) && ev.TargetInfo != nil {
			p.attachChild(s, ev)
		}
	case "Target.detachedFromTarget":
		var ev target.EventDetachedFromTarget
		if decode(params, &ev) {
			p.detachChild(string(ev.SessionID))
		}
	case "Page.lifecycleEvent":
		var ev page.EventLifecycleEvent
		if main && decode(params, &ev) && string(ev.FrameID) == p.targetID {
			p.mu.Lock()
			if ev.Name == "init" {
				p.loader, p.lifecycle = string(ev.LoaderID), map[string]bool{}
				p.loaders = append(p.loaders[max(0, len(p.loaders)-15):], p.loader)
				// Chrome does not always report the end of a request whose
				// document is gone; they would hold networkidle off forever.
				for key, r := range p.inflight {
					if r.session == s.id && r.loader != p.loader {
						delete(p.inflight, key)
					}
				}
			} else if string(ev.LoaderID) == p.loader {
				p.lifecycle[ev.Name] = true
			}
			p.signalLocked()
			p.mu.Unlock()
		}
	case "Page.frameNavigated":
		var ev page.EventFrameNavigated
		if !decode(params, &ev) || ev.Frame == nil {
			return
		}
		p.mu.Lock()
		p.setDocLocked(ev.Frame.ID, ev.Frame.ParentID, ev.Frame.URL+ev.Frame.URLFragment)
		if main && ev.Frame.ParentID == "" {
			p.url = ev.Frame.URL + ev.Frame.URLFragment
			p.refs.newDocument()
			p.signalLocked()
		}
		p.mu.Unlock()
	case "Page.frameAttached":
		var ev page.EventFrameAttached
		if decode(params, &ev) {
			p.mu.Lock()
			p.setDocLocked(ev.FrameID, ev.ParentFrameID, "")
			p.mu.Unlock()
		}
	case "Page.frameDetached":
		var ev page.EventFrameDetached
		if decode(params, &ev) && ev.Reason != page.FrameDetachedReasonSwap {
			p.mu.Lock()
			delete(p.docs, ev.FrameID)
			p.mu.Unlock()
		}
	case "Page.navigatedWithinDocument":
		var ev page.EventNavigatedWithinDocument
		if !decode(params, &ev) {
			return
		}
		p.mu.Lock()
		p.setDocLocked(ev.FrameID, "", ev.URL)
		if main && string(ev.FrameID) == p.targetID {
			p.url = ev.URL
			p.sameDoc++
			p.signalLocked()
		}
		p.mu.Unlock()
	case "Page.frameStartedLoading", "Page.frameStoppedLoading":
		var ev struct {
			FrameID string `json:"frameId"`
		}
		if main && decode(params, &ev) && ev.FrameID == p.targetID {
			p.mu.Lock()
			p.loading = method == "Page.frameStartedLoading"
			if p.loading {
				p.starts++
			}
			p.signalLocked()
			p.mu.Unlock()
		}
	case "Page.frameRequestedNavigation":
		var ev page.EventFrameRequestedNavigation
		if main && decode(params, &ev) && string(ev.FrameID) == p.targetID && ev.Disposition == "currentTab" {
			p.mu.Lock()
			p.starts++
			p.signalLocked()
			p.mu.Unlock()
		}
	case "Page.javascriptDialogOpening":
		var ev page.EventJavascriptDialogOpening
		if decode(params, &ev) {
			p.dialogOpened(s, ev)
		}
	case "Page.javascriptDialogClosed":
		p.mu.Lock()
		p.dialog = nil
		p.signalLocked()
		p.mu.Unlock()
	case "Page.fileChooserOpened":
		var ev page.EventFileChooserOpened
		if decode(params, &ev) {
			mode := "one file"
			if ev.Mode == page.FileChooserOpenedModeSelectMultiple {
				mode = "several files"
			}
			p.ctx.note(p.id, NoteFileChooser, fmt.Sprintf("a file chooser opened (for %s) and was cancelled; give the file input its files directly instead", mode))
		}
	case "Inspector.targetCrashed":
		p.crash("crashed")
	case "Runtime.consoleAPICalled", "Runtime.exceptionThrown", "Log.entryAdded":
		p.onConsole(s, method, params)
	case "Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished", "Network.loadingFailed":
		p.onNetwork(s, method, params)
	case "Network.webSocketCreated":
		p.ctx.webSocket(p, params)
	}
}

// signalLocked wakes everything waiting on the page's state, and keeps
// interrupt closed exactly while the page cannot act: a dialog waits for an
// answer, the renderer crashed, or the page closed. Called with p.mu held.
func (p *Page) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
	blocked := p.dialog != nil || p.crashed != "" || p.closed != ""
	select {
	case <-p.interrupt:
		if !blocked {
			p.interrupt = make(chan struct{})
		}
	default:
		if blocked {
			close(p.interrupt)
		}
	}
}

// setDocLocked records a frame's document, its parent, or both; an empty
// value keeps what is known. Called with p.mu held.
func (p *Page) setDocLocked(frame, parent cdp.FrameID, url string) {
	d, known := p.docs[frame]
	if !known && len(p.docs) >= 2000 {
		return // a page this full of frames counts their requests as its own
	}
	if parent != "" {
		d.parent = parent
	}
	if url != "" {
		d.url = url
	}
	p.docs[frame] = d
}

// frameDocument is the URL whose origin a frame's requests carry: its own
// document's or, for about:blank, about:srcdoc and data: documents, which
// take on the origin of the frame that made them, its parent's. The main
// frame's, and an unknown frame's, is the page's.
func (p *Page) frameDocument(frame cdp.FrameID) string {
	p.mu.Lock()
	for hops := 0; hops < 20 && frame != "" && string(frame) != p.targetID; hops++ {
		d, ok := p.docs[frame]
		if !ok {
			break
		}
		if d.url != "" && !strings.HasPrefix(d.url, "about:") && !strings.HasPrefix(d.url, "data:") {
			p.mu.Unlock()
			return d.url
		}
		frame = d.parent
	}
	p.mu.Unlock()
	return p.ctx.documentOf(p)
}

// attachChild sets up an out-of-process iframe or a worker of the page,
// which starts paused.
func (p *Page) attachChild(parent *session, ev target.EventAttachedToTarget) {
	info := ev.TargetInfo
	s := p.b.conn.newSession(string(ev.SessionID), string(info.TargetID), info.Type, p.onEvent)
	s.parent = parent
	p.mu.Lock()
	p.children[s.id] = s
	if info.Type == "iframe" {
		p.frames[cdp.FrameID(info.TargetID)] = s
	}
	p.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var err error
		switch info.Type {
		case "iframe":
			err = p.prepare(ctx, s, false)
		case "worker", "shared_worker", "service_worker":
			err = prepareWorker(ctx, s)
		default:
			// Worklets and the like fetch through their frame.
			err = s.pipelineThenRun(ctx, nil, true)
		}
		if err != nil && !s.ended() {
			// What the policy cannot see must not run.
			p.b.root.send(target.CommandCloseTarget, target.CloseTargetParams{TargetID: info.TargetID})
		}
	}()
}

func (p *Page) detachChild(id string) {
	p.b.conn.detach(id, errors.New("the frame or worker has gone"))
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.children[id]
	delete(p.children, id)
	if s != nil && s.kind == "iframe" {
		delete(p.frames, cdp.FrameID(s.targetID))
	}
	for key, r := range p.inflight {
		if r.session == id {
			delete(p.inflight, key)
		}
	}
	p.signalLocked()
}

func (p *Page) ownsFrame(frame cdp.FrameID) bool {
	if string(frame) == p.targetID {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.frames[frame]
	return ok
}

// infoChanged takes the title from the browser's view of the target. The URL
// comes from the page's own navigation events, which are ordered with the
// rest of its events.
func (p *Page) infoChanged(info *target.Info) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.title = info.Title
}

// dialogOpened answers a dialog at once, unless the policy leaves it to the
// caller; it must never block the page while the agent is waiting.
func (p *Page) dialogOpened(s *session, ev page.EventJavascriptDialogOpening) {
	d := Dialog{Type: string(ev.Type), Message: ev.Message, DefaultPrompt: ev.DefaultPrompt, URL: ev.URL}
	p.mu.Lock()
	answer := p.armed
	p.armed = nil
	if answer == nil {
		switch p.ctx.opts.Dialogs {
		case DialogManual:
			p.dialog = &d
			p.signalLocked()
			p.mu.Unlock()
			p.ctx.note(p.id, NoteDialog, fmt.Sprintf("a %s dialog is open, waiting for an answer", d))
			return
		case DialogAccept:
			answer = &dialogAnswer{accept: true, text: d.DefaultPrompt}
		case DialogDismiss:
			answer = &dialogAnswer{}
		default:
			answer = &dialogAnswer{accept: d.Type == "alert" || d.Type == "beforeunload"}
		}
	}
	p.mu.Unlock()
	params := page.HandleJavaScriptDialogParams{Accept: answer.accept}
	if answer.accept && d.Type == "prompt" {
		params.PromptText = answer.text
	}
	p.root.send(page.CommandHandleJavaScriptDialog, params)
	p.ctx.note(p.id, NoteDialog, fmt.Sprintf("%s dialog %s", describeAnswer(d, *answer), clip(fmt.Sprintf("%q", d.Message), 300)))
}

func describeAnswer(d Dialog, a dialogAnswer) string {
	switch {
	case d.Type == "alert":
		return "dismissed an alert"
	case d.Type == "beforeunload" && a.accept:
		return "left the page at a beforeunload"
	case d.Type == "beforeunload":
		return "stayed on the page at a beforeunload"
	case d.Type == "prompt" && a.accept:
		return fmt.Sprintf("answered %q to a prompt", clip(a.text, 100))
	case a.accept:
		return "accepted a " + d.Type
	default:
		return "dismissed a " + d.Type
	}
}

// AnswerNextDialog arms the answer for the next dialog the page opens,
// whatever the context's DialogPolicy; text is a prompt's answer. Arm it
// before the action that will open the dialog.
func (p *Page) AnswerNextDialog(accept bool, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = &dialogAnswer{accept: accept, text: text}
}

// HandleDialog answers the dialog waiting for one (DialogManual).
func (p *Page) HandleDialog(ctx context.Context, accept bool, text string) error {
	p.mu.Lock()
	d := p.dialog
	p.mu.Unlock()
	if d == nil {
		return errors.New("no dialog is open")
	}
	ctx, cancel := withDefault(ctx, 10*time.Second)
	defer cancel()
	params := page.HandleJavaScriptDialogParams{Accept: accept}
	if accept && d.Type == "prompt" {
		params.PromptText = text
	}
	if _, err := cdp.Call(ctx, p.root, page.HandleJavaScriptDialog, params); err != nil {
		return err
	}
	p.mu.Lock()
	if p.dialog == d {
		p.dialog = nil
	}
	p.signalLocked()
	p.mu.Unlock()
	p.ctx.note(p.id, NoteDialog, fmt.Sprintf("%s dialog %q", describeAnswer(*d, dialogAnswer{accept, text}), clip(d.Message, 300)))
	return p.settle(ctx, p.snapshotNav())
}

func (p *Page) crash(status string) {
	p.mu.Lock()
	if p.crashed != "" || p.closed != "" {
		p.mu.Unlock()
		return
	}
	p.crashed = status
	p.signalLocked()
	p.mu.Unlock()
	p.ctx.note(p.id, NoteCrash, fmt.Sprintf("page %s crashed (its renderer %s); reload it or navigate to recover", p.id, status))
}

func (p *Page) destroyed() {
	if p.markClosed("was closed") {
		p.ctx.note(p.id, NoteClosed, fmt.Sprintf("page %s closed", p.id))
	}
	p.b.forget(p)
}

func (p *Page) markClosed(reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed != "" {
		return false
	}
	p.closed = reason
	p.signalLocked()
	return true
}

// usable says why the page cannot act now: closed, crashed (unless the
// action recovers from that), or held by an open dialog.
func (p *Page) usable(recovers bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.closed != "":
		return failure(ErrClosed, "page %s %s", p.id, p.closed)
	case p.dialog != nil:
		return &DialogError{Dialog: *p.dialog}
	case p.crashed != "" && !recovers:
		return failure(ErrCrashed, "page %s crashed; reload it or navigate to recover", p.id)
	}
	return nil
}

// interruption is why an interrupted action stopped.
func (p *Page) interruption() error {
	if err := p.usable(false); err != nil {
		return err
	}
	return errors.New("interrupted")
}

// pageError is a sentence about a page or an action that errors.Is still
// matches with the sentinel it stands for, without the sentinel's own words
// in the middle of it.
type pageError struct {
	msg  string
	kind error
}

func (e *pageError) Error() string { return e.msg }
func (e *pageError) Unwrap() error { return e.kind }

func failure(kind error, format string, args ...any) error {
	return &pageError{msg: fmt.Sprintf(format, args...), kind: kind}
}

// begin starts an action: it checks the page is usable and returns a
// context that ends when the page is interrupted (a dialog waits for an
// answer, the renderer crashes, the page closes), bounded by limit unless
// ctx has a deadline.
func (p *Page) begin(ctx context.Context, limit time.Duration, recovers bool) (context.Context, context.CancelFunc, error) {
	<-p.ready
	if recovers {
		p.clearCrash()
	}
	if err := p.usable(recovers); err != nil {
		return nil, nil, err
	}
	p.ctx.touch(p)
	// A tab behind another gets no animation frames and no paint: bring the
	// page acted on to the front, as a person switching tabs does.
	front, unfront := context.WithTimeout(ctx, 2*time.Second)
	p.root.call(front, page.CommandBringToFront, nil)
	unfront()
	ctx, cancelLimit := withDefault(ctx, limit)
	ctx, cancel := context.WithCancelCause(ctx)
	p.mu.Lock()
	interrupt := p.interrupt
	p.mu.Unlock()
	go func() {
		select {
		case <-interrupt:
			cancel(p.interruption())
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel(nil)
		cancelLimit()
	}, nil
}

// fail turns a failure inside an action into the error to report: why the
// page was interrupted, if it was.
func fail(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return err
}

// waitUntil waits for cond, checked under p.mu after every change of state
// and at least every 100 ms.
func (p *Page) waitUntil(ctx context.Context, cond func() bool) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		p.mu.Lock()
		ok, changed := cond(), p.changed
		p.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-tick.C:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// WaitUntil names the point at which a navigation counts as done.
type WaitUntil string

const (
	WaitCommit           WaitUntil = "commit"           // the new document is committed
	WaitDOMContentLoaded WaitUntil = "domcontentloaded" // its HTML is parsed
	WaitLoad             WaitUntil = "load"             // it and its subresources loaded
	WaitNetworkIdle      WaitUntil = "networkidle"      // loaded, and no request for 500 ms
)

// NavigationTimeout bounds a navigation when ctx has no deadline.
const NavigationTimeout = 30 * time.Second

// networkQuiet is how long the network must rest to count as idle.
const networkQuiet = 500 * time.Millisecond

type navState struct {
	loader   string
	starts   int
	sameDoc  int
	requests int
}

func (p *Page) snapshotNav() navState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return navState{p.loader, p.starts, p.sameDoc, p.requests}
}

// reached reports whether a document newer than before has reached until.
// Called with p.mu held.
func (p *Page) reached(before string, until WaitUntil) bool {
	if p.loader == before || p.loader == "" {
		return false
	}
	switch until {
	case WaitCommit:
		return true
	case WaitDOMContentLoaded:
		return p.lifecycle["DOMContentLoaded"]
	case WaitNetworkIdle:
		return p.lifecycle["load"] && p.idleLocked()
	default:
		return p.lifecycle["load"]
	}
}

func (p *Page) idleLocked() bool {
	return len(p.inflight) == 0 && time.Since(p.netQuiet) >= networkQuiet
}

// Navigate loads url and waits until the new document reaches until (load
// when empty), and then, briefly, for what it began fetching as it loaded.
// A page that sends itself on (a sign-in guard's location.assign, a dev
// server's reload) is followed to the document it lands on. A URL the policy
// refuses fails before anything is sent. If the document does not settle in
// time, the error says how far it got; the page is still usable.
func (p *Page) Navigate(ctx context.Context, rawURL string, until WaitUntil) error {
	until, err := checkWaitUntil(until)
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("navigate: %q is not an absolute URL (include http:// or file://)", rawURL)
	}
	if d := p.ctx.decide(Request{URL: u, Method: "GET", Type: "Document", Navigation: true, MainFrame: true, Page: p.id}); !d.Allow {
		p.ctx.blocked(p, "GET", u.String(), "Document", d.Reason, "page")
		return fmt.Errorf("navigation to %s blocked: %s", clip(u.String(), 300), d.Reason)
	}
	ctx, done, err := p.begin(ctx, NavigationTimeout, true)
	if err != nil {
		return err
	}
	defer done()
	before := p.snapshotNav()
	res, err := cdp.Call(ctx, p.root, page.Navigate, page.NavigateParams{URL: u.String()})
	if err != nil {
		if ctx.Err() != nil {
			p.root.send(page.CommandStopLoading, nil)
		}
		return p.navError(ctx, u.String(), err)
	}
	if res.ErrorText != "" {
		if res.ErrorText != "net::ERR_ABORTED" {
			// Chrome commits an error page; let it, so it cannot land
			// in the middle of whatever comes next.
			wait, cancel := context.WithTimeout(ctx, time.Second)
			p.waitUntil(wait, func() bool { return p.loader != before.loader && p.lifecycle["load"] })
			cancel()
		}
		return p.navFailed(u.String(), res.ErrorText)
	}
	if res.LoaderID == "" {
		return nil // same document: only the fragment changed
	}
	if err := p.waitNavigation(ctx, before, string(res.LoaderID), until); err != nil {
		return err
	}
	p.mu.Lock()
	moved, now := p.loader != string(res.LoaderID), p.url
	p.mu.Unlock()
	if moved {
		p.ctx.note(p.id, NoteNavigated, fmt.Sprintf("page %s went on from %s to %s", p.id, clip(u.String(), 200), clip(now, 300)))
	}
	p.loaded(ctx, before, until)
	return nil
}

// loaded waits, after a navigation reached load, for what the new document
// began fetching, as settle does after an action. Earlier points are asked
// for so as not to wait, and networkidle has waited already.
func (p *Page) loaded(ctx context.Context, before navState, until WaitUntil) {
	if until == WaitLoad {
		p.quiet(ctx, before.requests)
	}
}

func checkWaitUntil(until WaitUntil) (WaitUntil, error) {
	switch until {
	case "":
		return WaitLoad, nil
	case WaitCommit, WaitDOMContentLoaded, WaitLoad, WaitNetworkIdle:
		return until, nil
	}
	return "", fmt.Errorf("wait until must be commit, domcontentloaded, load or networkidle, not %q", until)
}

// waitNavigation waits for a document newer than before to reach until.
// When loader names the document asked for, that one must have committed
// first: a document after it, which the page went on to by itself, counts
// as well.
func (p *Page) waitNavigation(ctx context.Context, before navState, loader string, until WaitUntil) error {
	err := p.waitUntil(ctx, func() bool {
		return (loader == "" || slices.Contains(p.loaders, loader)) && p.reached(before.loader, until)
	})
	if err == nil {
		return nil
	}
	if cause := fail(ctx, err); !errors.Is(cause, context.DeadlineExceeded) && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return p.unsettled(before, loader, until)
}

// unsettled describes a navigation that ran out of time: how far it got and
// what it is still waiting for.
func (p *Page) unsettled(before navState, loader string, until WaitUntil) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	stage := "it had not committed a new document"
	switch {
	case p.loader == before.loader || p.loader == "":
	case loader != "" && !slices.Contains(p.loaders, loader):
		stage = "another navigation replaced it, to " + clip(p.url, 200)
	case p.lifecycle["load"]:
		stage = "it had loaded"
	case p.lifecycle["DOMContentLoaded"]:
		stage = "its HTML had been parsed (DOMContentLoaded) but not every resource had loaded"
	default:
		stage = "it had committed but not finished parsing"
	}
	var waiting []string
	for _, r := range p.inflight {
		if len(waiting) == 3 {
			waiting = append(waiting, "…")
			break
		}
		waiting = append(waiting, clip(r.url, 120))
	}
	msg := fmt.Sprintf("page %s did not reach %s in time: %s", p.id, until, stage)
	if len(p.inflight) > 0 {
		msg += fmt.Sprintf("; %s still open (%s)", plural(len(p.inflight), "request was", "requests were"), strings.Join(waiting, ", "))
	}
	return failure(ErrTimeout, "%s", msg)
}

func (p *Page) navError(ctx context.Context, target string, err error) error {
	err = fail(ctx, err)
	if errors.Is(err, context.DeadlineExceeded) {
		return failure(ErrTimeout, "navigation to %s got no response in time (is the server hanging?)", clip(target, 300))
	}
	return err
}

// navFailed explains Chrome's network error for a navigation.
func (p *Page) navFailed(target, code string) error {
	why := code
	switch code {
	case "net::ERR_BLOCKED_BY_CLIENT":
		why = "blocked by the request policy"
	case "net::ERR_CONNECTION_REFUSED":
		why = "connection refused: nothing is listening there (is the dev server running?)"
	case "net::ERR_TOO_MANY_REDIRECTS":
		why = "too many redirects (a redirect loop)"
	case "net::ERR_ABORTED":
		why = "aborted (the response was a download, an empty 204, or the navigation was replaced)"
	case "net::ERR_UNSAFE_PORT":
		why = "Chrome refuses that port as unsafe (it belongs to a well-known service); use another port"
	case "net::ERR_NAME_NOT_RESOLVED":
		why = "the host name does not resolve"
	case "net::ERR_FILE_NOT_FOUND":
		why = "no such file"
	case "net::ERR_INVALID_AUTH_CREDENTIALS":
		why = "the server wants a sign-in, which the browser cannot give"
	}
	return fmt.Errorf("navigation to %s failed: %s", clip(target, 300), why)
}

func (p *Page) clearCrash() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.crashed != "" {
		p.crashed = ""
		p.signalLocked()
	}
}

// Reload reloads the page and waits as Navigate does. It recovers a crashed
// page.
func (p *Page) Reload(ctx context.Context, until WaitUntil) error {
	until, err := checkWaitUntil(until)
	if err != nil {
		return err
	}
	ctx, done, err := p.begin(ctx, NavigationTimeout, true)
	if err != nil {
		return err
	}
	defer done()
	before := p.snapshotNav()
	if _, err := cdp.Call(ctx, p.root, page.Reload, page.ReloadParams{}); err != nil {
		return p.navError(ctx, p.URL(), err)
	}
	if err := p.waitNavigation(ctx, before, "", until); err != nil {
		return err
	}
	p.loaded(ctx, before, until)
	return nil
}

// Back goes back one entry in the page's history.
func (p *Page) Back(ctx context.Context, until WaitUntil) error { return p.history(ctx, -1, until) }

// Forward goes forward one entry in the page's history.
func (p *Page) Forward(ctx context.Context, until WaitUntil) error { return p.history(ctx, 1, until) }

func (p *Page) history(ctx context.Context, delta int, until WaitUntil) error {
	until, err := checkWaitUntil(until)
	if err != nil {
		return err
	}
	ctx, done, err := p.begin(ctx, NavigationTimeout, false)
	if err != nil {
		return err
	}
	defer done()
	h, err := cdp.Call(ctx, p.root, page.GetNavigationHistory, cdp.Empty{})
	if err != nil {
		return fail(ctx, err)
	}
	i := int(h.CurrentIndex) + delta
	if i < 0 || i >= len(h.Entries) {
		if delta < 0 {
			return errors.New("there is no earlier page in this page's history")
		}
		return errors.New("there is no later page in this page's history")
	}
	entry := h.Entries[i]
	u, err := url.Parse(entry.URL)
	if err != nil {
		return err
	}
	if d := p.ctx.decide(Request{URL: u, Method: "GET", Type: "Document", Navigation: true, MainFrame: true, Page: p.id}); !d.Allow {
		return fmt.Errorf("navigation to %s blocked: %s", clip(entry.URL, 300), d.Reason)
	}
	before := p.snapshotNav()
	if _, err := cdp.Call(ctx, p.root, page.NavigateToHistoryEntry, page.NavigateToHistoryEntryParams{EntryID: entry.ID}); err != nil {
		return fail(ctx, err)
	}
	// An entry in the same document, such as one a modal pushed with the
	// very same URL, is a navigation within it: counted, whatever the URL.
	err = p.waitUntil(ctx, func() bool {
		return p.reached(before.loader, until) || (p.loader == before.loader && p.sameDoc != before.sameDoc && !p.loading)
	})
	if err != nil {
		if cause := fail(ctx, err); !errors.Is(cause, context.DeadlineExceeded) {
			return cause
		}
		return p.unsettled(before, "", until)
	}
	p.loaded(ctx, before, until)
	return nil
}

// settleTimeout bounds how long an input action waits for a navigation it
// started.
const settleTimeout = 10 * time.Second

// requestSettle bounds how long an action waits for the requests it began,
// and a navigation for those its document began as it loaded: long enough
// for a dev server's API, short enough not to stall on a long poll.
const requestSettle = 1500 * time.Millisecond

// settle lets the page react to an input action: two animation frames for
// the DOM to update; if the action started a navigation in the main frame,
// until the new document has loaded; and then, briefly, until the requests
// the action began have finished and the page has drawn what they changed.
func (p *Page) settle(ctx context.Context, before navState) error {
	if p.usable(false) != nil {
		return nil
	}
	p.drawn(ctx)
	p.mu.Lock()
	navigating := p.starts != before.starts || p.loader != before.loader
	p.mu.Unlock()
	if navigating {
		nav, cancel := context.WithTimeout(ctx, settleTimeout)
		err := p.waitUntil(nav, func() bool {
			return p.reached(before.loader, WaitLoad) || (!p.loading && p.loader == before.loader && p.starts != before.starts)
		})
		cancel()
		if err != nil {
			if cause := fail(ctx, err); cause != nil && ctx.Err() != nil {
				return cause
			}
			return nil // still loading; the agent can wait for it
		}
		p.mu.Lock()
		moved, now := p.loader != before.loader, p.url
		p.mu.Unlock()
		if moved {
			p.ctx.note(p.id, NoteNavigated, fmt.Sprintf("page %s navigated to %s", p.id, clip(now, 300)))
		}
	}
	p.quiet(ctx, before.requests)
	return nil
}

// drawn waits for two animation frames, so the DOM has caught up with what
// just happened.
func (p *Page) drawn(ctx context.Context) {
	frames, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	p.root.call(frames, "Runtime.evaluate", map[string]any{"expression": twoFrames, "awaitPromise": true})
}

// quiet waits, for at most requestSettle, until the requests begun after the
// one numbered since have finished, letting the page draw what each brought;
// a fetch's result often changes the page only once it is back. Requests
// still open then are noted, since what they change will show only later.
func (p *Page) quiet(ctx context.Context, since int) {
	if p.usable(false) != nil {
		return
	}
	wait, cancel := context.WithTimeout(ctx, requestSettle)
	defer cancel()
	for {
		if p.waitUntil(wait, func() bool { return len(p.pendingLocked(since)) == 0 }) != nil {
			break
		}
		p.drawn(wait)
		p.mu.Lock()
		more := len(p.pendingLocked(since))
		p.mu.Unlock()
		if more == 0 {
			return
		}
	}
	if ctx.Err() != nil || p.usable(false) != nil {
		return
	}
	p.mu.Lock()
	open := p.pendingLocked(since)
	var named []string
	for _, r := range open[:min(len(open), 3)] {
		named = append(named, r.method+" "+p.shortLocked(r.url))
	}
	p.mu.Unlock()
	if len(open) == 0 {
		return
	}
	if len(open) > 3 {
		named = append(named, "…")
	}
	them := "it brings"
	if len(open) > 1 {
		them = "they bring"
	}
	p.ctx.note(p.id, NotePending, fmt.Sprintf("%s still open after %s (%s); wait for network_idle, or for text %s, to see the page with it", plural(len(open), "request is", "requests are"), requestSettle, strings.Join(named, ", "), them))
}

// pendingLocked lists the requests in flight that began after the one
// numbered since, oldest first. Called with p.mu held.
func (p *Page) pendingLocked(since int) []inflight {
	var open []inflight
	for _, r := range p.inflight {
		if r.seq > since {
			open = append(open, r)
		}
	}
	slices.SortFunc(open, func(a, b inflight) int { return a.seq - b.seq })
	return open
}

// twoFrames waits for two animation frames, or 100 ms each if frames stop
// (a page that is not drawn gets none).
const twoFrames = `new Promise(done => {
  const frame = () => new Promise(r => { requestAnimationFrame(() => r()); setTimeout(r, 100); });
  frame().then(frame).then(() => done());
})`

// Close closes the page. beforeunload handlers do not run.
func (p *Page) Close(ctx context.Context) error {
	if p.isClosed() {
		return nil
	}
	ctx, cancel := withDefault(ctx, 10*time.Second)
	defer cancel()
	_, err := cdp.Call(ctx, p.b.root, target.CloseTarget, target.CloseTargetParams{TargetID: target.ID(p.targetID)})
	p.markClosed("was closed")
	p.b.forget(p)
	return err
}

// plural counts things: "1 request", "3 requests".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
