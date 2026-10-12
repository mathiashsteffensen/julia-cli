// Package terminal runs a program on a pseudo-terminal behind a virtual
// terminal emulator, so an agent can drive a TUI with keys, text and mouse
// events and observe it as text or as a rendered image.
//
// Rows and columns are 1-based throughout, as in terminal protocols and in
// [Snapshot.Text]: row 1, column 1 is the top-left cell.
//
// The package carries no policy. The caller prepares the command, including
// any sandbox, and decides who may send what.
package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Size limits keep a session's memory bounded whatever the program does.
const (
	MaxCols         = 500
	MaxRows         = 200
	scrollbackLines = 1000
	maxPendingInput = 1 << 20
	maxTitle        = 256
	// drainGrace is how long an exited program's output may keep arriving
	// (a background child can hold the terminal open) before the session
	// reports the exit.
	drainGrace = 250 * time.Millisecond
	// hangupGrace is how long Close lets programs act on SIGHUP (an editor
	// removing its swap file, a shell passing it to its jobs) before it kills
	// whatever is left on the terminal.
	hangupGrace = 500 * time.Millisecond
	// maxWatchedLines bounds how many lines of one read are fed to the
	// emulator one by one while a Watch is open.
	maxWatchedLines = 64
	// syncTimeout bounds synchronized output (mode 2026), as real terminals
	// do, so a program that never ends a frame cannot stall WaitStable.
	syncTimeout = time.Second
)

var (
	ErrTimeout = errors.New("terminal: timed out")
	ErrExited  = errors.New("terminal: program has exited")
	ErrClosed  = errors.New("terminal: session is closed")
	ErrFrozen  = errors.New("terminal: the screen is frozen")
)

// State is the program's lifecycle state.
type State struct {
	Exited bool
	// Stopped means Close ended the program; Code and Signal then say how.
	Stopped bool
	Code    int    // exit status when the program exited normally
	Signal  string // terminating signal (e.g. "SIGKILL"), empty otherwise
}

func (s State) String() string {
	switch {
	case !s.Exited:
		return "running"
	case s.Stopped:
		return "stopped"
	case s.Signal != "":
		return "killed by " + s.Signal
	default:
		return fmt.Sprintf("exited with code %d", s.Code)
	}
}

// Session is a program running on a pseudo-terminal. Its methods are safe for
// concurrent use; the final screen stays readable after the program exits and
// after Close.
type Session struct {
	cmd *exec.Cmd
	pty *os.File
	pid int
	tty int64 // the terminal device's number, which its processes report

	mu         sync.Mutex
	emu        *vt.Emulator
	frozen     *Snapshot // the screen when the emulator failed, nil while it works
	cols, rows int
	dec        map[int]bool // DEC private modes the program has set
	cursorOn   bool
	cursorKind vt.CursorStyle
	title      string
	syncSince  time.Time // start of a synchronized-output frame, zero if none
	lastOutput time.Time
	lastInput  time.Time
	drawn      bool        // the program has written something
	held       MouseButton // button pressed and not yet released, for drags
	state      State
	reaped     bool // the program has exited; state follows once output drains
	drained    bool // the reader saw end of file
	watches    []*Watch
	changed    chan struct{} // closed and replaced whenever anything changes

	inMu    sync.Mutex
	pending []byte // input and query replies waiting for the writer
	writing bool
	wake    chan struct{}

	done      chan struct{} // closed by Close
	exited    chan struct{} // closed once the exit is reported
	readDone  chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Start runs cmd on a new cols×rows pseudo-terminal. cmd must be unstarted
// with Stdin, Stdout and Stderr unset; they become the terminal.
//
// The program leads a new session with the terminal as its controlling tty
// (Setsid, Setctty). Other SysProcAttr fields the caller set are kept, except
// Setpgid, Pgid, Foreground and Noctty, which contradict a new session. Close
// hangs up and kills everything in that session or on the terminal.
//
// The environment (os.Environ when cmd.Env is nil) gets TERM=xterm-256color
// and, unless set, COLORTERM=truecolor. Variables that describe some other
// terminal are dropped: LINES and COLUMNS would pin ncurses to a stale size
// after Resize, and host-terminal identifiers (TERM_PROGRAM, TMUX, ...) would
// make programs speak dialects this emulator ignores.
func Start(cmd *exec.Cmd, cols, rows int) (*Session, error) {
	return StartFunc(cols, rows, func(string) (*exec.Cmd, error) { return cmd, nil })
}

// StartFunc is Start for a command that must name its terminal: build gets
// the new terminal's device path (/dev/ttys004 on macOS) before the command
// exists, so that, for example, a sandbox profile can grant that one device.
func StartFunc(cols, rows int, build func(device string) (*exec.Cmd, error)) (*Session, error) {
	if err := checkSize(cols, rows); err != nil {
		return nil, err
	}
	master, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("terminal: open pty: %w", err)
	}
	defer tty.Close()
	var device unix.Stat_t
	if err := unix.Fstat(int(tty.Fd()), &device); err != nil {
		master.Close()
		return nil, fmt.Errorf("terminal: pty: %w", err)
	}
	cmd, err := build(tty.Name())
	switch {
	case err != nil:
	case cmd.Process != nil:
		err = errors.New("terminal: command already started")
	case cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil:
		err = errors.New("terminal: command stdin, stdout and stderr must be unset")
	}
	if err != nil {
		master.Close()
		return nil, err
	}
	// A non-blocking master joins Go's poller, so Close interrupts a pending
	// Read even when an escaped grandchild still holds the terminal open. The
	// copy gets its own descriptor: two Files owning one would close it twice.
	fd, err := unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	master.Close()
	if err == nil {
		err = unix.SetNonblock(fd, true)
	}
	if err != nil {
		return nil, fmt.Errorf("terminal: pty: %w", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	if err := pty.Setsize(master, winsize(cols, rows)); err != nil {
		master.Close()
		return nil, fmt.Errorf("terminal: size pty: %w", err)
	}

	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.Env = terminalEnv(cmd.Env)
	attr := &syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		copied := *cmd.SysProcAttr
		attr = &copied
	}
	attr.Setsid, attr.Setctty, attr.Ctty = true, true, 0
	attr.Setpgid, attr.Pgid, attr.Foreground, attr.Noctty = false, 0, false, false
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, fmt.Errorf("terminal: start: %w", err)
	}

	s := &Session{
		cmd: cmd, pty: master, pid: cmd.Process.Pid, tty: int64(device.Rdev),
		cols: cols, rows: rows,
		dec:      map[int]bool{7: true, 25: true},
		cursorOn: true,
		changed:  make(chan struct{}),
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
		readDone: make(chan struct{}),
	}
	s.emu = newEmulator(s, cols, rows)
	s.wg.Add(4)
	go s.read()
	go s.replies()
	go s.write()
	go s.wait()
	return s, nil
}

func checkSize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > MaxCols || rows > MaxRows {
		return fmt.Errorf("terminal: size %dx%d outside 1x1 to %dx%d", cols, rows, MaxCols, MaxRows)
	}
	return nil
}

func winsize(cols, rows int) *pty.Winsize {
	return &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows), X: uint16(cols * cellWidth), Y: uint16(rows * cellHeight)}
}

func terminalEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+2)
	colorterm := false
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "TERM", "LINES", "COLUMNS", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID",
			"LC_TERMINAL", "LC_TERMINAL_VERSION", "TMUX", "TMUX_PANE", "STY", "ZELLIJ",
			"KITTY_WINDOW_ID", "GHOSTTY_RESOURCES_DIR", "ITERM_SESSION_ID", "WEZTERM_PANE",
			"VTE_VERSION", "WT_SESSION", "ALACRITTY_WINDOW_ID":
			continue
		case "COLORTERM":
			colorterm = true
		}
		out = append(out, kv)
	}
	out = append(out, "TERM=xterm-256color")
	if !colorterm {
		out = append(out, "COLORTERM=truecolor")
	}
	return out
}

// newEmulator wires the emulator's callbacks into s; they run inside
// emu.Write, which is always called with s.mu held.
func newEmulator(s *Session, cols, rows int) *vt.Emulator {
	emu := vt.NewEmulator(cols, rows)
	emu.SetScrollbackSize(scrollbackLines)
	disableAltScrollback(emu)
	emu.SetDefaultForegroundColor(themeFg)
	emu.SetDefaultBackgroundColor(themeBg)
	emu.SetDefaultCursorColor(themeCursor)
	for i, c := range themePalette {
		emu.SetIndexedColor(i, c)
	}
	// Report synchronized output (2026) as supported, so frames arrive whole,
	// and grapheme clustering (2027) as on, which is how vt measures widths.
	emu.WriteString("\x1b[?2026l\x1b[?2027h")
	emu.SetCallbacks(vt.Callbacks{
		EnableMode:       func(m ansi.Mode) { s.setMode(m, true) },
		DisableMode:      func(m ansi.Mode) { s.setMode(m, false) },
		CursorVisibility: func(visible bool) { s.cursorOn = visible },
		CursorStyle:      func(style vt.CursorStyle, _ bool) { s.cursorKind = style },
	})
	// A full reset (RIS) shows a hidden cursor without vt reporting it.
	emu.RegisterEscHandler('c', func() bool {
		s.cursorOn, s.cursorKind = true, vt.CursorBlock
		return false
	})
	// vt takes a bottom (DECSTBM) or right (DECSLRM) margin past the screen as
	// given, then indexes out of range when lines or cells move; a program
	// still drawing for its size before a resize sends one. Clamp it to the
	// screen, as xterm does. params is the parser's own storage, which vt's
	// handler reads next.
	clamp := func(limit func() int) vt.CsiHandler {
		return func(params ansi.Params) bool {
			if margin, _, ok := params.Param(1, 0); ok && margin > limit() {
				params[1] = ansi.Param(limit())
			}
			return false
		}
	}
	emu.RegisterCsiHandler('r', clamp(emu.Height))
	emu.RegisterCsiHandler('s', clamp(emu.Width))
	registerQueries(s, emu)
	return emu
}

// screens reaches vt's main and alternate screens, which its API hides, or
// returns nil if vt's layout has changed.
func screens(emu *vt.Emulator) []*vt.Screen {
	field := reflect.ValueOf(emu).Elem().FieldByName("scrs")
	if !field.IsValid() || field.Kind() != reflect.Array || field.Len() != 2 || field.Index(1).Type() != reflect.TypeFor[vt.Screen]() {
		return nil
	}
	return []*vt.Screen{
		(*vt.Screen)(unsafe.Pointer(field.Index(0).UnsafeAddr())),
		(*vt.Screen)(unsafe.Pointer(field.Index(1).UnsafeAddr())),
	}
}

// disableAltScrollback drops the alternate screen's scrollback. vt keeps one
// of DefaultScrollbackSize lines that nothing can read or cap through its API,
// and a TUI that scrolls for hours would fill it.
func disableAltScrollback(emu *vt.Emulator) {
	if scrs := screens(emu); scrs != nil {
		scrs[1].SetScrollback(nil)
	}
}

// clampCursors keeps every cursor inside the screen after a resize, as xterm
// does. vt moves only the active screen's cursor; the other screen's, and
// those saved with DECSC, come back out of range, where tab stops and line
// edits index past the screen.
func clampCursors(emu *vt.Emulator) {
	cursor := reflect.TypeFor[vt.Cursor]()
	for _, scr := range screens(emu) {
		for _, name := range []string{"cur", "saved"} {
			field := reflect.ValueOf(scr).Elem().FieldByName(name)
			if !field.IsValid() || field.Type() != cursor {
				continue
			}
			c := (*vt.Cursor)(unsafe.Pointer(field.UnsafeAddr()))
			c.X, c.Y = min(max(c.X, 0), emu.Width()-1), min(max(c.Y, 0), emu.Height()-1)
		}
	}
}

// emulate runs f, which drives the emulator, with s.mu held. vt still panics
// on sequences no clamp foresees, and a program's output must never take
// JARVIS down. After a panic vt's state is unknown, so the screen freezes as
// it was; output is still drained, so the program never blocks writing.
func (s *Session) emulate(f func()) {
	if s.frozen != nil {
		return
	}
	defer func() {
		if recover() == nil {
			return
		}
		frozen := Snapshot{Cols: s.cols, Rows: s.rows, Lines: make([]string, s.rows)}
		func() {
			defer func() { recover() }()
			frozen = s.capture(scrollbackLines)
		}()
		frozen.Frozen = true
		s.frozen = &frozen
	}()
	f()
}

func (s *Session) setMode(m ansi.Mode, on bool) {
	mode, ok := m.(ansi.DECMode)
	if !ok {
		return
	}
	s.dec[int(mode)] = on
	if mode == 2026 {
		s.syncSince = time.Time{}
		if on {
			s.syncSince = time.Now()
		}
	}
}

// notify wakes every waiter; s.mu must be held.
func (s *Session) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// framing reports a synchronized-output frame in progress, whose screen is
// half drawn; s.mu must be held.
func (s *Session) framing() bool {
	return !s.syncSince.IsZero() && time.Since(s.syncSince) < syncTimeout
}

// lines is the visible screen's text; s.mu must be held.
func (s *Session) lines() []string {
	if s.frozen != nil {
		return s.frozen.Lines
	}
	return screenLines(s.emu, s.cols, s.rows)
}

func (s *Session) read() {
	defer s.wg.Done()
	defer close(s.readDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		s.mu.Lock()
		if n > 0 {
			s.feed(buf[:n])
			s.lastOutput, s.drawn = time.Now(), true
		}
		if err != nil {
			s.drained = true
		}
		s.notify()
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// feed writes program output to the emulator; s.mu must be held. While a
// Watch is open, a read goes in a line at a time and is watched after each,
// so a match that one read both removes and redraws, as a shell does when it
// echoes a command, runs it and prints a new prompt, is seen to come back.
func (s *Session) feed(p []byte) {
	for range maxWatchedLines {
		i := bytes.IndexByte(p, '\n')
		if len(s.watches) == 0 || i < 0 || i == len(p)-1 {
			break
		}
		s.emulate(func() { s.emu.Write(p[:i+1]) })
		s.observe()
		p = p[i+1:]
	}
	s.emulate(func() { s.emu.Write(p) })
	s.observe()
}

// replies forwards the emulator's answers to terminal queries. It never takes
// s.mu: emu.Write blocks under that lock until this goroutine reads.
func (s *Session) replies() {
	defer s.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, err := s.emu.Read(buf)
		if n > 0 {
			s.enqueue(buf[:n], false)
		}
		if err != nil {
			return
		}
	}
}

// enqueue queues bytes for the program. When a program has stopped reading,
// input is refused with an error, while query replies, which have no caller to
// tell, are dropped.
func (s *Session) enqueue(b []byte, input bool) error {
	s.inMu.Lock()
	if len(s.pending)+len(b) > maxPendingInput {
		s.inMu.Unlock()
		if input {
			return errors.New("terminal: input backlog is full; the program is not reading its input")
		}
		return nil
	}
	s.pending = append(s.pending, b...)
	s.inMu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// write is the PTY's only writer, which keeps replies from splitting input.
func (s *Session) write() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		for {
			s.inMu.Lock()
			chunk := s.pending
			s.pending = nil
			s.writing = len(chunk) > 0
			s.inMu.Unlock()
			if len(chunk) == 0 {
				break
			}
			_, err := s.pty.Write(chunk)
			// lastInput moves before writing clears; WaitStable reads them in
			// the opposite order, so it never sees neither.
			s.mu.Lock()
			s.lastInput = time.Now()
			s.notify()
			s.mu.Unlock()
			s.inMu.Lock()
			s.writing = false
			if err != nil { // The program is gone; discard what is left.
				s.pending = nil
			}
			s.inMu.Unlock()
		}
	}
}

func (s *Session) inputBusy() bool {
	s.inMu.Lock()
	defer s.inMu.Unlock()
	return len(s.pending) > 0 || s.writing
}

func (s *Session) wait() {
	defer s.wg.Done()
	s.cmd.Wait()
	s.mu.Lock()
	s.reaped = true
	s.notify()
	s.mu.Unlock()
	state := State{Exited: true, Stopped: s.isClosed(), Code: -1} // -1 if Wait itself failed
	if ps := s.cmd.ProcessState; ps != nil {
		state.Code = ps.ExitCode()
		if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			state.Code, state.Signal = 0, unix.SignalName(status.Signal())
			if state.Signal == "" {
				state.Signal = status.Signal().String()
			}
		}
	}
	timer := time.NewTimer(drainGrace)
	select {
	case <-s.readDone:
	case <-timer.C:
	case <-s.done:
	}
	timer.Stop()
	s.mu.Lock()
	s.state = state
	s.notify()
	s.mu.Unlock()
	close(s.exited)
}

// Pid is the program's process ID, which is also its process group ID.
func (s *Session) Pid() int { return s.pid }

// Members lists the live processes on the terminal: the program, the jobs a
// shell runs, and what they started, such as a dev server's workers.
func (s *Session) Members() ([]int, error) { return s.members() }

// State reports whether the program is still running.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Size returns the terminal's columns and rows.
func (s *Session) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// Resize changes the terminal size; the program receives SIGWINCH.
func (s *Session) Resize(cols, rows int) error {
	if err := checkSize(cols, rows); err != nil {
		return err
	}
	if s.isClosed() {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := pty.Setsize(s.pty, winsize(cols, rows)); err != nil {
		return fmt.Errorf("terminal: resize: %w", err)
	}
	s.resize(cols, rows)
	s.lastInput = time.Now() // The program redraws, as after a key.
	s.notify()
	return nil
}

// resize resizes the emulator; s.mu must be held.
func (s *Session) resize(cols, rows int) {
	s.emulate(func() {
		s.emu.Resize(cols, rows)
		clampCursors(s.emu)
	})
	if s.frozen == nil {
		s.cols, s.rows = cols, rows
	}
	s.observe()
}

// hangUp sends SIGHUP (and SIGCONT, so a stopped job sees it) to everything
// on the terminal, so editors drop their swap files and shells pass it on,
// waits up to hangupGrace for them to exit, then kills what is left until
// nothing remains.
func (s *Session) hangUp() {
	members, err := s.members()
	if err != nil { // The program's process group is all there is to reach.
		syscall.Kill(-s.pid, syscall.SIGHUP)
		if _, err := s.WaitExit(context.Background(), hangupGrace); err != nil {
			syscall.Kill(-s.pid, syscall.SIGKILL)
		}
		return
	}
	signal := func(sig syscall.Signal) {
		for _, pid := range members {
			syscall.Kill(pid, sig)
		}
	}
	signal(syscall.SIGHUP)
	signal(syscall.SIGCONT)
	deadline := time.Now().Add(hangupGrace)
	for len(members) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		members, _ = s.members()
	}
	// A process that forks as fast as it is killed loses the race eventually;
	// one that is not ours to kill (a setuid program) is left after a while.
	for range 100 {
		if len(members) == 0 {
			break
		}
		signal(syscall.SIGKILL)
		time.Sleep(time.Millisecond)
		members, _ = s.members()
	}
	if err := syscall.Kill(-s.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.cmd.Process.Kill()
	}
}

func (s *Session) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Close hangs up the terminal, as closing a real one does, and then kills
// whatever is still on it, so nothing the program started outlives the
// session: not a shell's background jobs, which lead process groups of their
// own, nor nohup'd ones. A program that detached into a session of its own
// is beyond reach. Close is idempotent and returns once those processes are
// gone and the session's goroutines have stopped.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.hangUp()
		s.pty.Close()
		if pipe, ok := s.emu.InputPipe().(*io.PipeWriter); ok {
			pipe.CloseWithError(io.EOF)
		}
	})
	s.wg.Wait()
	return nil
}

// WaitStable waits until the screen has been quiet for quiet: no output since
// the later of the last output and the last input or resize, no input queued and
// no synchronized frame open. A program that has not drawn anything yet is
// not stable, so a slow starter is not mistaken for a blank screen. It
// returns once the program has exited and its output is drained, with the
// exit recorded, and ErrTimeout when the screen did not settle in time.
func (s *Session) WaitStable(ctx context.Context, quiet, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		busy := s.inputBusy()
		s.mu.Lock()
		changed, drained, drawn := s.changed, s.drained, s.drawn
		until := later(s.lastOutput, s.lastInput).Add(quiet)
		if !s.syncSince.IsZero() {
			until = later(until, s.syncSince.Add(syncTimeout))
		}
		s.mu.Unlock()
		now := time.Now()
		if drained || (drawn && !busy && !now.Before(until)) {
			s.settleExit(ctx)
			return nil
		}
		if !now.Before(deadline) {
			return ErrTimeout
		}
		if err := s.sleep(ctx, changed, earlier(until, deadline).Sub(now)); err != nil {
			return err
		}
	}
}

// WaitFor waits until the visible screen, as rows joined by newlines with
// trailing spaces trimmed, matches pattern; compile it with (?m) for ^ and $
// to anchor at rows. It returns ErrExited when the program has exited and
// drained without the screen matching, and ErrFrozen when the screen can no
// longer change. Text already on the screen counts; see Watch for a match
// that only new output can give.
func (s *Session) WaitFor(ctx context.Context, pattern *regexp.Regexp, timeout time.Duration) error {
	return s.until(ctx, timeout, func() bool {
		return pattern.MatchString(strings.Join(s.lines(), "\n"))
	})
}

// until waits for done, called with s.mu held and never mid-frame, as
// WaitFor describes.
func (s *Session) until(ctx context.Context, timeout time.Duration, done func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		changed, drained, frozen, framing := s.changed, s.drained, s.frozen != nil, s.framing()
		matched := !framing && done()
		s.mu.Unlock()
		switch {
		case matched:
			s.settleExit(ctx)
			return nil
		case frozen:
			return ErrFrozen
		case drained && !framing:
			s.settleExit(ctx)
			return ErrExited
		}
		now := time.Now()
		if !now.Before(deadline) {
			return ErrTimeout
		}
		wait := deadline.Sub(now)
		if framing {
			wait = min(wait, syncTimeout)
		}
		if err := s.sleep(ctx, changed, wait); err != nil {
			return err
		}
	}
}

// settleExit gives a program whose output has ended, or that has exited, the
// moment it takes for its exit to be recorded (at most drainGrace after it
// exits), so a caller that saw it finish does not report it running.
func (s *Session) settleExit(ctx context.Context) {
	s.mu.Lock()
	ending := (s.reaped || s.drained) && !s.state.Exited
	s.mu.Unlock()
	if !ending {
		return
	}
	timer := time.NewTimer(drainGrace + 50*time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.exited:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// A Watch waits for a pattern to appear anew: for the screen to hold more
// matches than it has at any moment since the watch began. Begin one before
// sending input, so text already on the screen, such as the last command's
// result, cannot answer for output still to come, while a prompt that is
// overwritten and drawn again counts as new. Input the program echoes is new
// output too.
type Watch struct {
	s       *Session
	pattern *regexp.Regexp
	fewest  int
	matched bool
}

// Watch begins watching for pattern, compiled as for WaitFor.
func (s *Session) Watch(pattern *regexp.Regexp) *Watch {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &Watch{s: s, pattern: pattern}
	w.fewest = w.count(s.lines())
	s.watches = append(s.watches, w)
	return w
}

func (w *Watch) count(lines []string) int {
	return len(w.pattern.FindAllStringIndex(strings.Join(lines, "\n"), -1))
}

// observe moves each watch on to the screen as it now stands; s.mu is held.
func (s *Session) observe() {
	if len(s.watches) == 0 || s.framing() {
		return
	}
	lines := s.lines()
	for _, w := range s.watches {
		if w.matched {
			continue
		}
		if n := w.count(lines); n > w.fewest {
			w.matched = true
		} else {
			w.fewest = n
		}
	}
}

// Wait waits for the watch to match, with WaitFor's results, and ends it.
func (w *Watch) Wait(ctx context.Context, timeout time.Duration) error {
	defer w.Stop()
	return w.s.until(ctx, timeout, func() bool { return w.matched })
}

// Stop ends the watch.
func (w *Watch) Stop() {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.s.watches = slices.DeleteFunc(w.s.watches, func(open *Watch) bool { return open == w })
}

// WaitExit waits for the program to exit and its output to drain.
func (s *Session) WaitExit(ctx context.Context, timeout time.Duration) (State, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.exited:
		return s.State(), nil
	case <-timer.C:
		return s.State(), ErrTimeout
	case <-ctx.Done():
		return s.State(), ctx.Err()
	}
}

func (s *Session) sleep(ctx context.Context, changed <-chan struct{}, d time.Duration) error {
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	select {
	case <-changed:
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
