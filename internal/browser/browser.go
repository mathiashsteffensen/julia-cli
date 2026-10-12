// Package browser drives a headless Chromium over the DevTools protocol for an
// agent doing frontend work: it opens pages in isolated contexts, acts on them
// as a person would (real mouse and key events), and observes them as an
// accessibility snapshot, screenshots, console output and network failures.
//
// The package carries no policy of its own. The caller decides which requests
// may leave the page (ContextOptions.Policy sees every one), how dialogs are
// answered, and which files may be uploaded; the package enforces what it is
// told and reports what happened as Notes and Logs.
//
// Chrome runs with a fresh throwaway profile, talks to JULIA over a pipe
// rather than a debugging port, and is killed with every helper process on
// Close, or by a watchdog when JULIA dies without closing it. It is not run
// under Seatbelt: it sandboxes its own renderers, and a nested Seatbelt
// profile breaks that sandbox.
package browser

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
)

var (
	// ErrClosed reports a browser, context or page that was closed.
	ErrClosed = errors.New("browser: closed")
	// ErrCrashed reports a page whose renderer crashed. Reload or Navigate
	// recovers it.
	ErrCrashed = errors.New("browser: the page crashed")
	// ErrTimeout reports a wait that ran out of time.
	ErrTimeout = errors.New("browser: timed out")
)

// Options configures Launch.
type Options struct {
	// Binary is the Chrome executable, usually from Find. Empty runs Find("").
	Binary string
	// DataDir is where the throwaway profile directory is created; empty uses
	// the system temporary directory. The profile is removed on Close.
	DataDir string
	// Args are extra Chrome switches, appended after the defaults. They come
	// from the caller, never from a page or a model.
	Args []string
	// Stderr receives Chrome's log output; nil discards it. The tail is kept
	// for launch errors either way.
	Stderr io.Writer
	// Dial carries the connections LoopbackOnly contexts make beyond
	// loopback. They all go to one proxy of the browser's, which asks Dial
	// to open each host:port a page reaches (a CONNECT for https and
	// WebSockets, a plain request for http) and refuses it when Dial fails;
	// nil refuses them all. It runs on the proxy's goroutines, so it may
	// block, resolving a name for instance.
	Dial func(ctx context.Context, address string) (net.Conn, error)

	messageLimit int // a smaller bound on one message from Chrome, for tests
}

// Browser is a running headless Chrome. Its methods are safe for concurrent
// use.
type Browser struct {
	cmd       *exec.Cmd
	pid       int
	profile   string
	conn      *conn
	root      *session
	product   string
	userAgent string
	stderr    *tailWriter
	exited    chan struct{}
	proxy     *proxy    // where LoopbackOnly contexts send what is beyond loopback
	watchdog  *exec.Cmd // kills Chrome if JULIA dies without closing it
	leash     *os.File  // the watchdog's pipe: it acts when this closes

	mu       sync.Mutex
	contexts map[cdp.BrowserContextID]*Context
	targets  map[target.ID]*Page
	adopted  chan struct{} // closed and replaced whenever a page is adopted
	nextPage int
	closing  bool

	closeOnce sync.Once
}

// Launch starts Chrome headless with a fresh profile. ctx bounds the start-up
// only; the browser runs until Close.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	binary := opts.Binary
	if binary == "" {
		var err error
		if binary, err = Find(""); err != nil {
			return nil, err
		}
	}
	profile, err := os.MkdirTemp(opts.DataDir, "julia-browser-")
	if err != nil {
		return nil, fmt.Errorf("browser profile: %w", err)
	}
	// The proxy listens before Chrome starts, so Chrome can be told that
	// whatever arrives through it is the public internet.
	px, err := startProxy(opts.Dial)
	if err != nil {
		os.RemoveAll(profile)
		return nil, fmt.Errorf("browser proxy: %w", err)
	}
	// Chrome reads commands from fd 3 and writes replies and events to fd 4.
	toChrome, toChromeW, err := os.Pipe()
	if err != nil {
		px.close()
		os.RemoveAll(profile)
		return nil, err
	}
	fromChromeR, fromChrome, err := os.Pipe()
	if err != nil {
		toChrome.Close()
		toChromeW.Close()
		px.close()
		os.RemoveAll(profile)
		return nil, err
	}
	stderr := &tailWriter{limit: 16 << 10, forward: opts.Stderr}
	args := append(chromeArgs(binary, profile, opts.Args), "--ip-address-space-overrides="+px.addr()+"=public")
	cmd := exec.Command(binary, args...)
	cmd.Env = chromeEnv(os.Environ(), profile)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	// Its own process group, so Close can kill every helper with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{toChrome, toChromeW, fromChrome, fromChromeR} {
			f.Close()
		}
		px.close()
		os.RemoveAll(profile)
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	toChrome.Close()
	fromChrome.Close()

	b := &Browser{
		cmd:      cmd,
		pid:      cmd.Process.Pid,
		profile:  profile,
		stderr:   stderr,
		exited:   make(chan struct{}),
		proxy:    px,
		contexts: map[cdp.BrowserContextID]*Context{},
		targets:  map[target.ID]*Page{},
		adopted:  make(chan struct{}),
	}
	px.mu.Lock()
	px.admit = func(conn net.Conn) bool { return chromeOwns(conn, b.pid) }
	px.mu.Unlock()
	b.watchdog, b.leash = watch(b.pid, profile)
	b.conn = newConn(fromChromeR, toChromeW)
	if opts.messageLimit > 0 {
		b.conn.limit = opts.messageLimit
	}
	b.conn.eof = func() error {
		// The pipe closes as Chrome exits; its exit status says more.
		select {
		case <-b.exited:
		case <-time.After(time.Second):
		}
		return b.exitError(false)
	}
	b.root = b.conn.newSession("", "", "browser", b.onEvent)
	b.conn.start()
	go func() {
		cmd.Wait()
		fromChromeR.Close()
		// Chrome is gone: its process group may be reused, so the watchdog
		// must not act on it.
		b.unleash()
		// Err says why before Done says that it stopped.
		b.conn.shutdown(b.exitError(true))
		close(b.exited)
	}()

	if err := b.init(ctx); err != nil {
		b.Close()
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			err = fmt.Errorf("%w\n%s", err, lastLines(tail, 12))
		}
		return nil, fmt.Errorf("launch %s: %w", binary, err)
	}
	return b, nil
}

// watch starts the watchdog: a shell in a process group of its own, out of
// reach of the terminal's hangup, that waits on a pipe only JULIA writes to.
// When JULIA dies without closing the browser (a closed terminal window,
// SIGKILL, a panic), the pipe ends and the watchdog kills Chrome's process
// group and removes the profile. Without a shell the browser runs unwatched.
func watch(chrome int, profile string) (*exec.Cmd, *os.File) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil
	}
	defer r.Close()
	cmd := exec.Command("/bin/sh", "-c", `trap "" HUP INT TERM QUIT; read -r _; kill -9 -"$1" 2>/dev/null; sleep 1; rm -rf -- "$2"`, "julia-browser-watchdog", strconv.Itoa(chrome), profile)
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, nil
	}
	go cmd.Wait()
	return cmd, w
}

// unleash stops the watchdog without letting it act: Close or Chrome's own
// exit has made it unneeded.
func (b *Browser) unleash() {
	if b.watchdog != nil {
		b.watchdog.Process.Kill()
	}
	if b.leash != nil {
		b.leash.Close()
	}
}

func (b *Browser) init(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		// The first run of a freshly downloaded Chrome waits on Gatekeeper.
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	version, err := cdp.Call(ctx, b.root, browser.GetVersion, cdp.Empty{})
	if err != nil {
		return err
	}
	b.product = version.Product
	// Pages see a plain Chrome, as a person's browser is.
	b.userAgent = strings.Replace(version.UserAgent, "HeadlessChrome/", "Chrome/", 1)
	if _, err := cdp.Call(ctx, b.root, target.SetDiscoverTargets, target.SetDiscoverTargetsParams{Discover: true}); err != nil {
		return err
	}
	// Every new page, popup and worker starts paused until it is set up, so
	// none makes a request before the policy is in place.
	_, err = cdp.Call(ctx, b.root, target.SetAutoAttach, target.SetAutoAttachParams{AutoAttach: true, WaitForDebuggerOnStart: true, Flatten: ptr(true)})
	return err
}

// Version is Chrome's product string, such as "HeadlessChrome/151.0.7922.34".
func (b *Browser) Version() string { return b.product }

// PID is the browser process's id.
func (b *Browser) PID() int { return b.pid }

// Done is closed when the browser process has exited.
func (b *Browser) Done() <-chan struct{} { return b.exited }

// Err says why the browser stopped, or nil while it runs.
func (b *Browser) Err() error { return b.conn.closedErr() }

// exitError says why the browser stopped; exited is whether Chrome's
// process has been waited for.
func (b *Browser) exitError(exited bool) error {
	b.mu.Lock()
	closing := b.closing
	b.mu.Unlock()
	if closing {
		return ErrClosed
	}
	msg := "browser: Chrome closed its connection unexpectedly"
	select {
	case <-b.exited:
		exited = true
	default:
	}
	if exited {
		msg = "browser: Chrome exited unexpectedly"
		if ps := b.cmd.ProcessState; ps != nil {
			msg += " (" + ps.String() + ")"
		}
	}
	if tail := strings.TrimSpace(b.stderr.String()); tail != "" {
		msg += "\n" + lastLines(tail, 6)
	}
	return errors.New(msg)
}

// Close shuts Chrome down, kills every process it started, and removes the
// profile. It is safe to call more than once.
func (b *Browser) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		contexts := b.contexts
		b.contexts = map[cdp.BrowserContextID]*Context{}
		b.mu.Unlock()
		for _, c := range contexts {
			c.markClosed()
		}
		// Note the helpers while Chrome is their parent: once it exits they
		// are orphans, found only by process group.
		pids := processTree(b.pid)
		b.root.send(browser.CommandClose, nil)
		select {
		case <-b.exited:
		case <-time.After(1500 * time.Millisecond):
		}
		syscall.Kill(-b.pid, syscall.SIGKILL)
		kill(append(pids, processTree(b.pid)...))
		select {
		case <-b.exited:
		case <-time.After(3 * time.Second):
		}
		b.unleash()
		b.conn.shutdown(ErrClosed)
		b.proxy.close()
		// A helper can still be writing as it dies.
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(b.profile); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return nil
}

// ProfileDir is the throwaway profile directory, removed on Close.
func (b *Browser) ProfileDir() string { return b.profile }

// onEvent handles the browser session's events on the reader goroutine.
func (b *Browser) onEvent(s *session, method string, params jsontext.Value) {
	switch method {
	case "Target.attachedToTarget":
		var ev target.EventAttachedToTarget
		if decode(params, &ev) && ev.TargetInfo != nil {
			b.attachTop(ev)
		}
	case "Target.detachedFromTarget":
		var ev target.EventDetachedFromTarget
		if decode(params, &ev) {
			b.conn.detach(string(ev.SessionID), ErrClosed)
		}
	case "Target.targetInfoChanged":
		var ev target.EventTargetInfoChanged
		if decode(params, &ev) && ev.TargetInfo != nil {
			if p := b.page(ev.TargetInfo.TargetID); p != nil {
				p.infoChanged(ev.TargetInfo)
			}
		}
	case "Target.targetDestroyed":
		var ev target.EventTargetDestroyed
		if decode(params, &ev) {
			if p := b.page(ev.TargetID); p != nil {
				p.destroyed()
			}
		}
	case "Target.targetCrashed":
		var ev target.EventTargetCrashed
		if decode(params, &ev) {
			if p := b.page(ev.TargetID); p != nil {
				p.crash(ev.Status)
			}
		}
	case "Browser.downloadWillBegin":
		var ev browser.EventDownloadWillBegin
		if decode(params, &ev) {
			if p := b.pageByFrame(ev.FrameID); p != nil {
				p.ctx.downloadBegan(p, ev)
			}
		}
	case "Browser.downloadProgress":
		var ev browser.EventDownloadProgress
		if decode(params, &ev) {
			b.mu.Lock()
			contexts := slices.Collect(maps.Values(b.contexts))
			b.mu.Unlock()
			for _, c := range contexts {
				c.downloadProgress(ev)
			}
		}
	}
}

// attachTop sets up a page, service worker or shared worker the browser
// session auto-attached, which is paused until set up.
func (b *Browser) attachTop(ev target.EventAttachedToTarget) {
	info := ev.TargetInfo
	b.mu.Lock()
	c := b.contexts[info.BrowserContextID]
	b.mu.Unlock()
	if c == nil {
		// Only pages JULIA opened, in contexts it made, may run.
		s := b.conn.newSession(string(ev.SessionID), string(info.TargetID), info.Type, nil)
		if info.Type == "page" {
			b.root.send(target.CommandCloseTarget, target.CloseTargetParams{TargetID: info.TargetID})
		} else {
			s.send("Runtime.runIfWaitingForDebugger", nil)
		}
		return
	}
	switch info.Type {
	case "page":
		c.adopt(ev)
	case "service_worker", "shared_worker", "worker":
		c.attachWorker(ev)
	default:
		s := b.conn.newSession(string(ev.SessionID), string(info.TargetID), info.Type, nil)
		s.send("Runtime.runIfWaitingForDebugger", nil)
	}
}

func (b *Browser) page(id target.ID) *Page {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.targets[id]
}

func (b *Browser) pageByFrame(frame cdp.FrameID) *Page {
	b.mu.Lock()
	pages := slices.Collect(maps.Values(b.targets))
	b.mu.Unlock()
	for _, p := range pages {
		if p.ownsFrame(frame) {
			return p
		}
	}
	return nil
}

// register records a page and wakes NewPage calls waiting for it.
func (b *Browser) register(p *Page) {
	b.mu.Lock()
	b.nextPage++
	p.id = fmt.Sprintf("p%d", b.nextPage)
	b.targets[target.ID(p.targetID)] = p
	close(b.adopted)
	b.adopted = make(chan struct{})
	b.mu.Unlock()
}

func (b *Browser) forget(p *Page) {
	b.mu.Lock()
	delete(b.targets, target.ID(p.targetID))
	b.mu.Unlock()
}

// waitPage waits for the target to be adopted as a page.
func (b *Browser) waitPage(ctx context.Context, id target.ID) (*Page, error) {
	for {
		b.mu.Lock()
		p, adopted := b.targets[id], b.adopted
		b.mu.Unlock()
		if p != nil {
			return p, nil
		}
		select {
		case <-adopted:
		case <-ctx.Done():
			return nil, fmt.Errorf("new page: %w", context.Cause(ctx))
		case <-b.conn.done:
			return nil, b.Err()
		}
	}
}

// chromeArgs are the switches for a quiet, isolated, automation-friendly
// Chrome: no first-run UI, extensions, sync, background networking, component
// updates, crash reporting or keychain prompts, and nothing that would open
// unless asked.
func chromeArgs(binary, profile string, extra []string) []string {
	headless := "--headless=new"
	if isHeadlessShell(binary) {
		headless = "--headless"
	}
	args := []string{
		headless,
		"--remote-debugging-pipe",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--no-startup-window",
		"--disable-extensions",
		"--disable-component-extensions-with-background-pages",
		"--disable-default-apps",
		"--disable-sync",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-client-side-phishing-detection",
		"--disable-domain-reliability",
		"--disable-breakpad",
		"--disable-crash-reporter",
		"--disable-field-trial-config",
		"--metrics-recording-only",
		"--no-pings",
		"--no-service-autorun",
		"--password-store=basic",
		"--use-mock-keychain",
		"--disable-search-engine-choice-screen",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-back-forward-cache",
		"--disable-hang-monitor",
		"--disable-ipc-flooding-protection",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--deny-permission-prompts",
		"--dns-prefetch-disable",
		"--disable-features=Translate,MediaRouter,OptimizationHints,AutofillServerCommunication,CertificateTransparencyComponentUpdater,InterestFeedContentSuggestions,PrivacySandboxSettings4,DialMediaRouteProvider,GlobalMediaControls,NetworkPrediction,Prerender2,LensOverlay,HttpsUpgrades",
		"--force-color-profile=srgb",
		"--hide-scrollbars",
		"--mute-audio",
		"--enable-unsafe-swiftshader",
		"--window-size=1280,800",
		// Desktop pointer media features in headless, as a mouse user has.
		"--blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4",
	}
	proxied := slices.ContainsFunc(extra, func(a string) bool {
		return strings.HasPrefix(a, "--proxy-server") || strings.HasPrefix(a, "--proxy-pac-url")
	})
	if !proxied {
		// No system proxy or PAC script fetched behind the policy's back.
		args = append(args, "--no-proxy-server")
	}
	return append(args, extra...)
}

// chromeEnv passes Chrome only what a browser needs from the environment:
// none of the agent's API keys or tokens. A crashed renderer's minidump goes
// into the throwaway profile, not the crash database of the user's Chrome.
func chromeEnv(environ []string, profile string) []string {
	keep := []string{"HOME", "PATH", "TMPDIR", "USER", "LOGNAME", "SHELL", "LANG", "LANGUAGE", "TZ",
		"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "FONTCONFIG_FILE", "FONTCONFIG_PATH"}
	var env []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keep, name) || strings.HasPrefix(name, "LC_") {
			env = append(env, kv)
		}
	}
	return append(env, "BREAKPAD_DUMP_LOCATION="+filepath.Join(profile, "Crashpad"))
}

// tailWriter keeps the last limit bytes written to it and forwards them.
type tailWriter struct {
	mu      sync.Mutex
	buf     []byte
	limit   int
	forward io.Writer
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.limit; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	if w.forward != nil {
		w.forward.Write(p)
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(bytes.ToValidUTF8(w.buf, []byte("�")))
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func decode(raw jsontext.Value, v any) bool {
	return json.Unmarshal(raw, v, decodeOptions) == nil
}

func ptr[T any](v T) *T { return &v }
