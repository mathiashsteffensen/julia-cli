package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"julia/internal/core"
	"julia/internal/images"
	"julia/internal/terminal"
)

// Terminal runs programs on virtual terminals that outlive a call, so the
// agent can drive a TUI, REPL or dev server and look at it. Each runs as
// `bash -c` under the Bash sandbox, plus its own terminal device.
const (
	maxTerminals                     = 4
	defaultTerminalCols              = 100
	defaultTerminalRows              = 30
	maxTerminalCols, maxTerminalRows = 300, 100
	defaultTerminalWait              = 10 // seconds
	maxTerminalWait                  = 300
	maxTerminalScrollback            = 1000
	// terminalQuiet is how long the screen must rest to count as settled:
	// shorter than the ~530 ms caret blink of most TUIs, which never rests.
	terminalQuiet = 200 * time.Millisecond
)

var (
	terminalActions = []string{"start", "send", "snapshot", "resize", "stop", "list"}
	mouseActions    = []string{"click", "press", "release", "move", "scroll-up", "scroll-down", "scroll-left", "scroll-right"}
)

func terminalDefinition() core.ToolDefinition {
	sized := func(what string, def, most int) map[string]any {
		return integer(fmt.Sprintf("start, resize: %s in cells; default %d, at most %d", what, def, most))
	}
	mouse := schema(map[string]any{
		"action": map[string]any{"type": "string", "enum": mouseActions, "description": "click is press then release; a drag is press, move…, release"},
		"row":    integer("1-based screen row; with text, the row to search"),
		"col":    integer("1-based screen column"),
		"text":   str("Aim at the first character of this text's first occurrence on the screen (on row, if given) instead of at a column"),
		"button": map[string]any{"type": "string", "enum": []string{"left", "middle", "right"}, "description": "Default left"},
	}, "action")
	mouse["description"] = "send: one mouse event, instead of text and keys, aimed at row and col or at text. Only when the program turned mouse reporting on (the screen header says mouse on)."
	return core.ToolDefinition{
		Name: "Terminal",
		Description: fmt.Sprintf("Run a program in a virtual terminal and use it as a person would: drive a TUI, editor, REPL or interactive prompt with keys, or keep a dev server running while you work. start runs `bash -c command` in the workspace under the same OS sandbox and environment as Bash (no network unless loopback=true or network=true) and returns an id such as t1. Terminals persist across calls until stopped. stop, and the end of your turn, hang up the terminal and kill every process still on it, so never leave one running for the user; quit an editor with its own command first. A daemon that detaches from its terminal escapes, so run servers in the foreground, with loopback=true so they can listen on localhost (bind 127.0.0.1 rather than every interface, which would also reach the local network). At most %d are open at once; one whose program exited stays readable until stopped. "+
			"start, send and resize wait until the screen settles, or until wait_for matches, for up to timeout seconds; every response then shows the screen as numbered rows under a header with its size, cursor and state, which names the command a shell is still running (foreground job \"make\"). Rows and columns are 1-based; a wide character (CJK, most emoji) fills two columns but appears once in the text. Judge the text first, but TUIs often show the selected item and focused pane only by colour: a [highlighted: …] line under the rows lists cells set off by background colour or reverse video. image=true adds a rendered screenshot, which costs as much as ~1600 tokens of text: use it when the look itself matters (colours, alignment, layout). "+
			"Keys are single characters or names: enter, tab, backspace, esc, space, up, down, left, right, home, end, pgup, pgdown, insert, delete, f1–f12, with modifiers such as ctrl+c, alt+x, shift+tab. In ask mode every start and send needs approval; in auto mode an offline or loopback terminal needs none, and a terminal with network is reviewed at start and on every send.", maxTerminals),
		Parameters: schema(map[string]any{
			"action":     map[string]any{"type": "string", "enum": terminalActions, "description": "start a program; send it input; snapshot its screen; resize it; stop (hang up and forget) it; list open terminals"},
			"id":         str("The terminal, as start named it (t1, t2, …); required by send, snapshot, resize and stop"),
			"command":    str("start: the Bash command to run, such as `npm run dev`, `vim notes.md`, or `bash` for a shell"),
			"loopback":   map[string]any{"type": "boolean", "description": "start: let the program listen on localhost (127.0.0.1, ::1) and connect to the servers it or your other loopback terminals start there, for its whole life: a dev server, tests against it. Services already running on this machine (a database, the user's own dev server) and Unix sockets stay closed: they need network=true. No review in auto mode. Default false"},
			"network":    map[string]any{"type": "boolean", "description": "start: allow all network, loopback included, for this terminal's whole life; reviewed. Default false"},
			"cols":       sized("width", defaultTerminalCols, maxTerminalCols),
			"rows":       sized("height", defaultTerminalRows, maxTerminalRows),
			"text":       str("send: typed as keystrokes; a newline presses Enter"),
			"paste":      map[string]any{"type": "boolean", "description": "send: deliver text as one bracketed paste, so its newlines do not press Enter in programs that support it"},
			"keys":       map[string]any{"type": "array", "maxItems": 100, "items": map[string]any{"type": "string"}, "description": `send: keys pressed in order, after text, e.g. ["down", "enter"] or ["ctrl+c"]`},
			"mouse":      mouse,
			"wait_for":   str("start, send, snapshot, resize: RE2 regular expression to wait for instead of waiting for the screen to settle. It is matched against the visible rows with trailing spaces removed, so ^ and $ anchor at each row (a prompt row ending in $ matches \\$$). After send or resize only a match that was not on the screen before counts, but the program's echo of what you typed is new, so wait for text only the result can produce: send `make; echo DONE$?` and wait for DONE\\d. A snapshot without wait_for does not wait"),
			"timeout":    integer(fmt.Sprintf("Seconds to wait; default %d, at most %d", defaultTerminalWait, maxTerminalWait)),
			"scrollback": integer(fmt.Sprintf("Also show up to this many lines that scrolled off the top, at most %d", maxTerminalScrollback)),
			"image":      map[string]any{"type": "boolean", "description": "Also return a rendered screenshot of the screen"},
		}, "action"),
	}
}

type terminalArgs struct {
	Action     string         `json:"action"`
	ID         string         `json:"id"`
	Command    string         `json:"command"`
	Network    bool           `json:"network"`
	Loopback   bool           `json:"loopback"`
	Cols       int            `json:"cols"`
	Rows       int            `json:"rows"`
	Text       string         `json:"text"`
	Paste      bool           `json:"paste"`
	Keys       []string       `json:"keys"`
	Mouse      *terminalMouse `json:"mouse"`
	WaitFor    string         `json:"wait_for"`
	Timeout    int            `json:"timeout"`
	Scrollback int            `json:"scrollback"`
	Image      bool           `json:"image"`

	pattern *regexp.Regexp
	reach   netAccess
}

type terminalMouse struct {
	Action string `json:"action"`
	Row    int    `json:"row"`
	Col    int    `json:"col"`
	Text   string `json:"text,omitempty"`
	Button string `json:"button,omitempty"`
}

func (m *terminalMouse) event() terminal.MouseEvent {
	return terminal.MouseEvent{Action: terminal.MouseAction(m.Action), Button: terminal.MouseButton(m.Button), Row: m.Row, Col: m.Col}
}

// check validates everything that does not depend on a running terminal, so
// nothing is approved that would then be refused. An argument meant for
// another action is a mistake worth naming rather than ignoring: network on a
// send does not give that terminal network.
func (a *terminalArgs) check() error {
	if !slices.Contains(terminalActions, a.Action) {
		return errors.New("action must be start, send, snapshot, resize, stop or list")
	}
	for _, arg := range []struct {
		name    string
		set     bool
		actions []string
	}{
		{"id", a.ID != "", []string{"send", "snapshot", "resize", "stop"}},
		{"command", a.Command != "", []string{"start"}},
		{"network", a.Network, []string{"start"}},
		{"loopback", a.Loopback, []string{"start"}},
		{"cols", a.Cols != 0, []string{"start", "resize"}},
		{"rows", a.Rows != 0, []string{"start", "resize"}},
		{"text", a.Text != "", []string{"send"}},
		{"paste", a.Paste, []string{"send"}},
		{"keys", len(a.Keys) > 0, []string{"send"}},
		{"mouse", a.Mouse != nil, []string{"send"}},
		{"wait_for", a.WaitFor != "", []string{"start", "send", "snapshot", "resize"}},
		{"timeout", a.Timeout != 0, []string{"start", "send", "snapshot", "resize"}},
		{"scrollback", a.Scrollback != 0, []string{"start", "send", "snapshot", "resize", "stop"}},
		{"image", a.Image, []string{"start", "send", "snapshot", "resize", "stop"}},
	} {
		if arg.set && !slices.Contains(arg.actions, a.Action) {
			return fmt.Errorf("%s does not apply to %s; it is for %s", arg.name, a.Action, strings.Join(arg.actions, ", "))
		}
	}
	if a.Action != "start" && a.Action != "list" && a.ID == "" {
		return fmt.Errorf("%s needs the id of a terminal", a.Action)
	}
	switch a.Action {
	case "start":
		if strings.TrimSpace(a.Command) == "" || len(a.Command) > 128*1024 {
			return errors.New("command is required and must be at most 128 KiB")
		}
		var err error
		if a.reach, err = access(a.Network, a.Loopback); err != nil {
			return err
		}
		a.Cols, a.Rows = cmp.Or(a.Cols, defaultTerminalCols), cmp.Or(a.Rows, defaultTerminalRows)
	case "resize":
		if a.Cols == 0 || a.Rows == 0 {
			return errors.New("resize needs cols and rows")
		}
	case "send":
		switch {
		case a.Text == "" && len(a.Keys) == 0 && a.Mouse == nil:
			return errors.New("send needs text, keys or mouse")
		case a.Mouse != nil && (a.Text != "" || len(a.Keys) > 0):
			return errors.New("send a mouse event on its own, without text or keys")
		case a.Paste && a.Text == "":
			return errors.New("paste needs text")
		case len(a.Text) > 64*1024 || !utf8.ValidString(a.Text):
			return errors.New("text must be UTF-8 and at most 64 KiB")
		case len(a.Keys) > 100:
			return errors.New("send at most 100 keys at once")
		}
		if err := terminal.CheckKeys(a.Keys...); err != nil {
			return errors.New(strings.TrimPrefix(err.Error(), "terminal: "))
		}
		if m := a.Mouse; m != nil {
			switch {
			case !slices.Contains(mouseActions, m.Action):
				return errors.New("mouse action must be " + strings.Join(mouseActions, ", "))
			case m.Text != "" && m.Col != 0:
				return errors.New("mouse takes text or a col to aim at, not both")
			case m.Text == "" && (m.Row < 1 || m.Col < 1), m.Row < 0:
				return errors.New("mouse needs a row and col, counted from 1, or text to aim at")
			case m.Button != "" && m.Button != "left" && m.Button != "middle" && m.Button != "right":
				return errors.New("mouse button must be left, middle or right")
			}
		}
	}
	if a.Cols != 0 && (a.Cols < 10 || a.Cols > maxTerminalCols) || a.Rows != 0 && (a.Rows < 3 || a.Rows > maxTerminalRows) {
		return fmt.Errorf("size must be between 10×3 and %d×%d", maxTerminalCols, maxTerminalRows)
	}
	a.Timeout = cmp.Or(a.Timeout, defaultTerminalWait)
	if a.Timeout < 1 || a.Timeout > maxTerminalWait {
		return fmt.Errorf("timeout must be between 1 and %d seconds", maxTerminalWait)
	}
	if a.Scrollback < 0 || a.Scrollback > maxTerminalScrollback {
		return fmt.Errorf("scrollback must be between 0 and %d lines", maxTerminalScrollback)
	}
	if a.WaitFor != "" {
		if len(a.WaitFor) > 4096 {
			return errors.New("wait_for must be at most 4 KiB")
		}
		if _, err := regexp.Compile(a.WaitFor); err != nil {
			return fmt.Errorf("wait_for: %w", err)
		}
		// Rows are what a person reads, so ^ and $ anchor at them.
		a.pattern = regexp.MustCompile("(?m)" + a.WaitFor)
	}
	return nil
}

// openTerminal is a terminal the executor keeps between calls. scope is the
// agent that started it: 0 for the main agent, or a subagent's.
type openTerminal struct {
	id, command string
	reach       netAccess
	scope       int
	session     *terminal.Session
	tmp         string // the program's private HOME and TMPDIR
}

// label names the terminal in results: its id and command, on one line.
func (t *openTerminal) label() string {
	command := strings.Join(strings.Fields(t.command), " ")
	if utf8.RuneCountInString(command) > 60 {
		command = string([]rune(command)[:59]) + "…"
	}
	label := t.id + " · " + command
	switch t.reach {
	case online:
		label += " · network"
	case loopbackOnly:
		label += " · loopback"
	}
	return label
}

// close hangs up the program and removes its HOME and TMPDIR once nothing
// is left running that could still use them.
func (t *openTerminal) close() {
	t.session.Close()
	os.RemoveAll(t.tmp)
}

func (e *Executor) terminal(ctx context.Context, call core.ToolCall, scope int) (core.ToolResult, error) {
	var a terminalArgs
	if err := decode(call.Arguments, &a); err != nil {
		return core.ToolResult{}, err
	}
	if err := a.check(); err != nil {
		return core.ToolResult{}, err
	}
	if e.config.Mode == "read-only" && (a.Action == "start" || a.Action == "send" || a.Action == "resize") {
		return core.ToolResult{}, errors.New("read-only mode prohibits starting, typing into or resizing terminals")
	}
	switch a.Action {
	case "list":
		return core.ToolResult{Content: e.listTerminals(scope)}, nil
	case "start":
		return e.startTerminal(ctx, call, a, scope)
	}
	t := e.findTerminal(a.ID, scope)
	if t == nil {
		return core.ToolResult{}, fmt.Errorf("there is no terminal %q (stopped ones are forgotten). Open now:\n%s", a.ID, e.listTerminals(scope))
	}
	note := ""
	// A wait after input begins before it, so what was already on the
	// screen cannot answer for the output still to come.
	var watch *terminal.Watch
	switch a.Action {
	case "send":
		// Refuse what cannot be delivered before anyone is asked to approve it.
		if state := t.session.State(); state.Exited {
			return core.ToolResult{}, fmt.Errorf("%s's program has %s; read it with snapshot, or stop it", t.id, state)
		}
		if m := a.Mouse; m != nil {
			snap := t.session.Snapshot(0)
			if snap.Mouse == "" {
				return core.ToolResult{}, fmt.Errorf("%s's program has not turned on mouse reporting; use keys", t.id)
			}
			if m.Text != "" {
				at, ok := snap.Find(m.Text, m.Row)
				if !ok {
					where := "the screen"
					if m.Row != 0 {
						where = fmt.Sprintf("row %d", m.Row)
					}
					return core.ToolResult{}, fmt.Errorf("%q is not on %s of %s", m.Text, where, t.id)
				}
				m.Row, m.Col = at.Row, at.Col
			}
			if err := t.session.CheckMouse(m.event()); err != nil {
				return core.ToolResult{}, fmt.Errorf("%s: %w", t.id, err)
			}
		}
		if e.config.Mode != "auto" || t.reach == online {
			if err := e.authorizeTerminal(ctx, call, a, t); err != nil {
				return core.ToolResult{}, err
			}
		}
		if a.pattern != nil {
			watch = t.session.Watch(a.pattern)
			defer watch.Stop()
		}
		if err := sendTerminal(t.session, a); err != nil {
			return core.ToolResult{}, fmt.Errorf("%s: %w", t.id, err)
		}
	case "resize":
		if a.pattern != nil {
			watch = t.session.Watch(a.pattern)
			defer watch.Stop()
		}
		if err := t.session.Resize(a.Cols, a.Rows); err != nil {
			return core.ToolResult{}, fmt.Errorf("%s: %w", t.id, err)
		}
	case "stop":
		// The screen outlives the program, so the result shows how it ended.
		e.terminals = slices.DeleteFunc(e.terminals, func(open *openTerminal) bool { return open == t })
		t.close()
		return terminalResult(t, t.session.Snapshot(a.Scrollback), "", a.Image), nil
	}
	if a.Action != "snapshot" || a.pattern != nil {
		var err error
		if note, err = waitTerminal(ctx, t.session, a, watch); err != nil {
			return core.ToolResult{}, err
		}
	}
	return terminalResult(t, t.session.Snapshot(a.Scrollback), note, a.Image), nil
}

func (e *Executor) startTerminal(ctx context.Context, call core.ToolCall, a terminalArgs, scope int) (core.ToolResult, error) {
	if runtime.GOOS != "darwin" && e.config.Mode != "yolo" {
		return core.ToolResult{}, errors.New("Terminal requires macOS Seatbelt in this version; other platforms fail closed")
	}
	if len(e.terminals) >= maxTerminals {
		return core.ToolResult{}, fmt.Errorf("%d terminals are open, the most at once; stop one before starting another. Open now:\n%s", maxTerminals, e.listTerminals(scope))
	}
	// An offline or loopback terminal reaches no further than offline or
	// loopback Bash, which auto mode runs freely. Network is granted for the
	// terminal's whole life, so it is reviewed here and again for everything
	// typed into it.
	if e.config.Mode != "auto" || a.reach == online {
		if err := e.authorizeTerminal(ctx, call, a, nil); err != nil {
			return core.ToolResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	var sh shellCommand
	session, err := terminal.StartFunc(a.Cols, a.Rows, func(device string) (*exec.Cmd, error) {
		var err error
		if sh, err = e.shell(ctx, a.Command, a.reach, false, device); err != nil {
			return nil, err
		}
		cmd := exec.Command(sh.path, sh.args...)
		// The terminal package sets TERM; macOS's bash need not advertise zsh.
		cmd.Dir, cmd.Env = e.directory, append(sh.env, "BASH_SILENCE_DEPRECATION_WARNING=1")
		return cmd, nil
	})
	if err != nil {
		if sh.tmp != "" {
			os.RemoveAll(sh.tmp)
		}
		return core.ToolResult{}, fmt.Errorf("start sandboxed terminal (no fallback): %w", err)
	}
	e.terminalCount++
	t := &openTerminal{id: fmt.Sprintf("t%d", e.terminalCount), command: a.Command, reach: a.reach, scope: scope, session: session, tmp: sh.tmp}
	e.terminals = append(e.terminals, t)
	note, err := waitTerminal(ctx, session, a, nil)
	if err != nil {
		return core.ToolResult{}, err
	}
	return terminalResult(t, session.Snapshot(a.Scrollback), note, a.Image), nil
}

// authorizeTerminal asks for a start, or for input to t. The request shows the
// terminal's command beside exactly what is sent, so input to a shell can be
// judged as the command it is.
func (e *Executor) authorizeTerminal(ctx context.Context, call core.ToolCall, a terminalArgs, t *openTerminal) error {
	request := struct {
		Action   string         `json:"action"`
		Terminal string         `json:"terminal,omitempty"`
		Command  string         `json:"command"`
		Network  bool           `json:"network"`
		Loopback bool           `json:"loopback,omitempty"`
		Cols     int            `json:"cols,omitempty"`
		Rows     int            `json:"rows,omitempty"`
		Text     string         `json:"text,omitempty"`
		Paste    bool           `json:"paste,omitempty"`
		Keys     []string       `json:"keys,omitempty"`
		Mouse    *terminalMouse `json:"mouse,omitempty"`
	}{Action: a.Action, Command: a.Command, Network: a.reach == online, Loopback: a.reach == loopbackOnly, Cols: a.Cols, Rows: a.Rows}
	reach := map[netAccess]string{offline: "the workspace OS sandbox", loopbackOnly: "the workspace OS sandbox, able to listen on localhost and connect to the servers it or other terminals start there", online: "the OS sandbox with network access"}
	reason := "Start this command in a terminal in " + reach[a.reach] + "? It keeps running until stopped or the turn ends."
	if a.reach == online {
		reason += " Everything sent to it is reviewed too."
	}
	if t != nil {
		request.Terminal, request.Command, request.Network, request.Loopback = t.id, t.command, t.reach == online, t.reach == loopbackOnly
		request.Text, request.Paste, request.Keys, request.Mouse = a.Text, a.Paste, a.Keys, a.Mouse
		request.Cols, request.Rows = 0, 0
		reason = "Send this input to terminal " + t.id + ", which runs the command shown in " + reach[t.reach] + "? The program receives it as typed input: a shell runs it as a command."
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		return err
	}
	approval := core.Approval{Call: core.ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments}, Task: e.config.Task, Directory: e.directory, Network: request.Network, ExecutionMode: "sandboxed", Reason: reason}
	return e.authorizeRequest(ctx, approval, true)
}

func sendTerminal(s *terminal.Session, a terminalArgs) error {
	if m := a.Mouse; m != nil {
		return s.Mouse(m.event())
	}
	if a.Text != "" {
		send := s.SendText
		if a.Paste {
			send = s.Paste
		}
		if err := send(a.Text); err != nil {
			return err
		}
	}
	if len(a.Keys) > 0 {
		return s.SendKeys(a.Keys...)
	}
	return nil
}

// waitTerminal waits as a asks, on watch when input was sent. A wait that
// runs out is no failure: the screen as it is still answers the call, with a
// note saying it did not settle.
func waitTerminal(ctx context.Context, s *terminal.Session, a terminalArgs, watch *terminal.Watch) (string, error) {
	timeout := time.Duration(a.Timeout) * time.Second
	var err error
	switch {
	case watch != nil:
		err = watch.Wait(ctx, timeout)
	case a.pattern != nil:
		err = s.WaitFor(ctx, a.pattern, timeout)
	default:
		err = s.WaitStable(ctx, terminalQuiet, timeout)
	}
	switch {
	case err == nil, errors.Is(err, terminal.ErrFrozen): // terminalResult explains a frozen screen.
		return "", nil
	case errors.Is(err, terminal.ErrExited):
		return "The program exited without wait_for matching.", nil
	case !errors.Is(err, terminal.ErrTimeout):
		return "", err
	case watch != nil && a.pattern.MatchString(strings.Join(s.Snapshot(0).Lines, "\n")):
		return fmt.Sprintf("wait_for found no new match within %ds; the match on the screen was there before.", a.Timeout), nil
	case a.pattern != nil:
		return fmt.Sprintf("wait_for did not match within %ds.", a.Timeout), nil
	case slices.IndexFunc(s.Snapshot(0).Lines, func(line string) bool { return line != "" }) < 0:
		return fmt.Sprintf("Nothing was drawn within %ds.", a.Timeout), nil
	}
	return fmt.Sprintf("The screen was still changing after %ds.", a.Timeout), nil
}

// terminalResult shows t's screen to the model: its label, any note, then the
// snapshot. Scrollback gives way first when the output limit is reached,
// because the screen is what the call asked about.
func terminalResult(t *openTerminal, snap terminal.Snapshot, note string, image bool) core.ToolResult {
	head := t.label()
	if note != "" {
		head += "\n" + note
	}
	if snap.Frozen {
		head += "\nThe terminal emulator failed on this program's output, so the screen stopped changing there. Stop the terminal; its output is still drained."
	}
	text := snap.Text()
	for excess := len(head) + 1 + len(text) - maxOutput; excess > 0 && len(snap.Scrollback) > 0; excess = len(head) + 1 + len(text) - maxOutput {
		drop := 0
		for freed := 0; freed < excess && drop < len(snap.Scrollback); drop++ {
			freed += len(snap.Scrollback[drop]) + 7 // its number, tab and newline
		}
		snap.Scrollback = snap.Scrollback[drop:]
		text = snap.Text()
	}
	result := core.ToolResult{Content: bounded(head + "\n" + text)}
	if image {
		if img, err := images.Encode(snap.Render(terminal.RenderOptions{})); err != nil {
			result.Content += "\n[screenshot unavailable: " + err.Error() + "]"
		} else {
			result.Images = []core.Image{img}
		}
		if missing := snap.Missing(); len(missing) > 0 {
			if len(missing) > 8 {
				missing = append(missing[:8], "…")
			}
			result.Content += "\n[screenshot: no font here has " + strings.Join(missing, " ") + ", so they are drawn as boxes; the text above is exact]"
		}
	}
	return result
}

func (e *Executor) findTerminal(id string, scope int) *openTerminal {
	for _, t := range e.terminals {
		if t.id == id && t.scope == scope {
			return t
		}
	}
	return nil
}

// listTerminals describes the terminals an agent may use; those of other
// agents (the main agent's, or another subagent's) are only counted.
func (e *Executor) listTerminals(scope int) string {
	var rows []string
	others := 0
	for _, t := range e.terminals {
		if t.scope != scope {
			others++
			continue
		}
		cols, lines := t.session.Size()
		rows = append(rows, fmt.Sprintf("%s · %s · %d×%d", t.label(), t.session.State(), cols, lines))
	}
	if len(rows) == 0 {
		rows = append(rows, "No terminals are open.")
	}
	switch {
	case others == 1:
		rows = append(rows, "1 more belongs to another agent.")
	case others > 1:
		rows = append(rows, fmt.Sprintf("%d more belong to other agents.", others))
	}
	return strings.Join(rows, "\n")
}

// closeTerminals stops the terminals of scope, or all of them when scope < 0,
// at once: each may give its programs a moment to act on the hangup.
func (e *Executor) closeTerminals(scope int) {
	var closing sync.WaitGroup
	e.terminals = slices.DeleteFunc(e.terminals, func(t *openTerminal) bool {
		if scope >= 0 && t.scope != scope {
			return false
		}
		closing.Go(t.close)
		return true
	})
	closing.Wait()
}

// scopedExecutor is how a subagent uses the executor: the same workspace,
// lock and approvals, but terminals and browser pages of its own, closed
// when it returns.
type scopedExecutor struct {
	executor *Executor
	scope    int
}

func (s scopedExecutor) Definitions() []core.ToolDefinition { return s.executor.Definitions() }
func (s scopedExecutor) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	return s.executor.execute(ctx, call, s.scope)
}

// subagentScope gives one subagent its own view of the executor, and the
// function that stops its terminals and closes its browser context once it
// has finished.
func (e *Executor) subagentScope() (core.ToolExecutor, func()) {
	e.mu.Lock()
	e.scopes++
	scope := e.scopes
	e.mu.Unlock()
	return scopedExecutor{e, scope}, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.closeTerminals(scope)
		e.closeBrowser(scope)
	}
}
