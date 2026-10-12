package terminal

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SendText types text as keystrokes: each character arrives as its own key
// and a newline as Enter (CR). Use Paste to deliver a block of text as one
// paste, and SendKeys for named and modified keys.
func (s *Session) SendText(text string) error {
	if err := s.writable(); err != nil {
		return err
	}
	return s.enqueue([]byte(typed(text)), true)
}

// Paste delivers text as a terminal paste: wrapped in bracketed-paste markers
// when the program enabled them (so newlines do not submit), typed otherwise.
func (s *Session) Paste(text string) error {
	if err := s.writable(); err != nil {
		return err
	}
	s.mu.Lock()
	bracketed := s.dec[2004]
	s.mu.Unlock()
	if !bracketed {
		return s.enqueue([]byte(typed(text)), true)
	}
	// An embedded end marker would let the rest of the paste run as keys.
	text = strings.ReplaceAll(text, "\x1b[201~", "")
	return s.enqueue([]byte("\x1b[200~"+text+"\x1b[201~"), true)
}

func typed(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\r"), "\n", "\r")
}

func (s *Session) writable() error {
	if s.isClosed() {
		return ErrClosed
	}
	if s.State().Exited {
		return ErrExited
	}
	return nil
}

// SendKeys presses keys in order. Nothing is sent unless every key parses.
//
// A key is a single character ("a", "A", "?", "é") or a name: enter, tab,
// backspace, esc, space, up, down, left, right, home, end, pgup, pgdown,
// insert, delete, f1–f12. Names are case-insensitive and accept common
// aliases (return, escape, pageup, del, and tmux's BSpace, NPage, PPage, BTab).
// Modifiers prefix the key with "+": ctrl+c, alt+x, shift+tab, ctrl+alt+left,
// alt++ (alt and the plus key); tmux's C-c, M-x and S-up work too.
//
// Keys use the legacy xterm encoding, honouring application cursor mode
// (DECCKM). Combinations it cannot express, such as shift+enter or
// ctrl+shift+a, are errors rather than silently sent as something else.
func (s *Session) SendKeys(keys ...string) error {
	if err := s.writable(); err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("terminal: no keys given")
	}
	s.mu.Lock()
	appCursor := s.dec[1]
	s.mu.Unlock()
	var out strings.Builder
	for _, spec := range keys {
		k, err := parseKey(spec)
		if err != nil {
			return err
		}
		seq, err := k.encode(appCursor)
		if err != nil {
			return err
		}
		out.WriteString(seq)
	}
	return s.enqueue([]byte(out.String()), true)
}

// CheckKeys returns the error SendKeys would give keys, without a session,
// so a caller can refuse bad keys before acting on them (asking approval, say).
func CheckKeys(keys ...string) error {
	for _, spec := range keys {
		k, err := parseKey(spec)
		if err == nil {
			_, err = k.encode(false)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type key struct {
	spec             string
	name             string // a named key, or "" for char
	char             rune
	shift, alt, ctrl bool
}

var keyNames = map[string]string{
	"enter": "enter", "return": "enter", "ret": "enter",
	"tab": "tab", "btab": "btab", "backtab": "btab",
	"backspace": "backspace", "bspace": "backspace", "bs": "backspace",
	"esc": "esc", "escape": "esc",
	"space": "space", "spc": "space",
	"up": "up", "down": "down", "left": "left", "right": "right",
	"home": "home", "end": "end",
	"pgup": "pgup", "pageup": "pgup", "ppage": "pgup",
	"pgdown": "pgdown", "pgdn": "pgdown", "pagedown": "pgdown", "npage": "pgdown",
	"insert": "insert", "ins": "insert", "ic": "insert",
	"delete": "delete", "del": "delete", "dc": "delete",
}

func init() {
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("f%d", i)
		keyNames[name] = name
	}
}

func parseKey(spec string) (key, error) {
	k := key{spec: spec}
	rest := spec
	for len(rest) > 2 && rest[1] == '-' && strings.ContainsRune("CMS", rune(rest[0])) {
		k.setModifier(rest[:1])
		rest = rest[2:]
	}
	for {
		i := strings.IndexByte(rest, '+')
		if i <= 0 || i == len(rest)-1 {
			break
		}
		if !k.setModifier(strings.ToLower(rest[:i])) {
			return k, fmt.Errorf("terminal: key %q: unknown modifier %q (use ctrl, alt or shift)", spec, rest[:i])
		}
		rest = rest[i+1:]
	}
	if utf8.RuneCountInString(rest) == 1 {
		k.char, _ = utf8.DecodeRuneInString(rest)
		if k.char == utf8.RuneError || unicode.IsControl(k.char) {
			return k, fmt.Errorf("terminal: key %q is not a printable character", spec)
		}
		return k, nil
	}
	name, ok := keyNames[strings.ToLower(rest)]
	if !ok {
		return k, fmt.Errorf("terminal: unknown key %q: keys are single characters or names such as enter, tab, esc, up, pgdown, f5, with modifiers like ctrl+c; send text with SendText", spec)
	}
	if name == "btab" {
		name, k.shift = "tab", true
	}
	k.name = name
	return k, nil
}

func (k *key) setModifier(mod string) bool {
	switch mod {
	case "ctrl", "control", "C":
		k.ctrl = true
	case "alt", "opt", "option", "meta", "M":
		k.alt = true
	case "shift", "S":
		k.shift = true
	default:
		return false
	}
	return true
}

var (
	cursorFinals = map[string]byte{"up": 'A', "down": 'B', "right": 'C', "left": 'D', "home": 'H', "end": 'F'}
	tildeCodes   = map[string]int{"insert": 2, "delete": 3, "pgup": 5, "pgdown": 6, "f5": 15, "f6": 17, "f7": 18, "f8": 19, "f9": 20, "f10": 21, "f11": 23, "f12": 24}
	ss3Finals    = map[string]byte{"f1": 'P', "f2": 'Q', "f3": 'R', "f4": 'S'}
)

func (k key) encode(appCursor bool) (string, error) {
	mod := 1
	if k.shift {
		mod++
	}
	if k.alt {
		mod += 2
	}
	if k.ctrl {
		mod += 4
	}
	if final, ok := cursorFinals[k.name]; ok {
		switch {
		case mod > 1:
			return fmt.Sprintf("\x1b[1;%d%c", mod, final), nil
		case appCursor:
			return "\x1bO" + string(final), nil
		}
		return "\x1b[" + string(final), nil
	}
	if code, ok := tildeCodes[k.name]; ok {
		if mod > 1 {
			return fmt.Sprintf("\x1b[%d;%d~", code, mod), nil
		}
		return fmt.Sprintf("\x1b[%d~", code), nil
	}
	if final, ok := ss3Finals[k.name]; ok {
		if mod > 1 {
			return fmt.Sprintf("\x1b[1;%d%c", mod, final), nil
		}
		return "\x1bO" + string(final), nil
	}

	// The rest have only legacy forms: alt is an ESC prefix, and ctrl and
	// shift exist for a few keys alone.
	prefix := ""
	if k.alt {
		prefix = "\x1b"
	}
	unsupported := fmt.Errorf("terminal: key %q has no legacy terminal encoding", k.spec)
	switch k.name {
	case "enter":
		if k.shift || k.ctrl {
			return "", fmt.Errorf("%w; apps that take a newline usually bind alt+enter", unsupported)
		}
		return prefix + "\r", nil
	case "tab":
		switch {
		case k.ctrl:
			return "", unsupported
		case k.shift:
			return prefix + "\x1b[Z", nil
		}
		return prefix + "\t", nil
	case "backspace":
		switch {
		case k.shift:
			return "", unsupported
		case k.ctrl:
			return prefix + "\x08", nil
		}
		return prefix + "\x7f", nil
	case "esc":
		if k.shift || k.ctrl {
			return "", unsupported
		}
		return prefix + "\x1b", nil
	case "space":
		switch {
		case k.shift:
			return "", unsupported
		case k.ctrl:
			return prefix + "\x00", nil
		}
		return prefix + " ", nil
	}

	r := k.char
	letter := r < utf8.RuneSelf && unicode.IsLetter(r)
	if k.shift {
		if !letter || k.ctrl {
			return "", fmt.Errorf("%w; send the shifted character itself", unsupported)
		}
		r = unicode.ToUpper(r)
	}
	if k.ctrl {
		c, ok := controlByte(r)
		if !ok {
			return "", unsupported
		}
		return prefix + string(rune(c)), nil
	}
	return prefix + string(r), nil
}

// controlByte is what a terminal sends for ctrl and a character.
func controlByte(r rune) (byte, bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return byte(r - 'a' + 1), true
	case r >= 'A' && r <= 'Z':
		return byte(r - 'A' + 1), true
	}
	switch r {
	case '@', '2', ' ':
		return 0x00, true
	case '[', '3':
		return 0x1b, true
	case '\\', '4':
		return 0x1c, true
	case ']', '5':
		return 0x1d, true
	case '^', '6':
		return 0x1e, true
	case '_', '7', '/':
		return 0x1f, true
	case '?', '8':
		return 0x7f, true
	}
	return 0, false
}

// MouseAction is what a mouse event does.
type MouseAction string

const (
	MouseClick       MouseAction = "click" // press and release
	MousePress       MouseAction = "press"
	MouseRelease     MouseAction = "release"
	MouseMove        MouseAction = "move"
	MouseScrollUp    MouseAction = "scroll-up"
	MouseScrollDown  MouseAction = "scroll-down"
	MouseScrollLeft  MouseAction = "scroll-left"
	MouseScrollRight MouseAction = "scroll-right"
)

// MouseButton is a mouse button; empty means left.
type MouseButton string

const (
	MouseLeft   MouseButton = "left"
	MouseMiddle MouseButton = "middle"
	MouseRight  MouseButton = "right"
)

// MouseEvent is a mouse action at a 1-based cell.
type MouseEvent struct {
	Action           MouseAction
	Button           MouseButton // left when empty; ignored for scrolling
	Row, Col         int
	Shift, Alt, Ctrl bool
}

// Mouse sends a mouse event, encoded for the tracking mode and encoding the
// program enabled (SGR, SGR-pixels, urxvt, UTF-8 or X10). It fails when the
// program has not enabled mouse reporting, or does not report that action:
// moves need any-motion tracking, or drag tracking with a button pressed.
// A drag is press, move…, release.
func (s *Session) Mouse(ev MouseEvent) error {
	if err := s.writable(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, held, err := s.mouse(ev)
	if err != nil {
		return err
	}
	if err := s.enqueue([]byte(seq), true); err != nil {
		return err
	}
	s.held = held
	return nil
}

// CheckMouse returns the error Mouse would give ev now, without sending it,
// so a caller can refuse an event that cannot be delivered before acting on
// it (asking approval, say).
func (s *Session) CheckMouse(ev MouseEvent) error {
	if err := s.writable(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _, err := s.mouse(ev)
	return err
}

// mouse encodes ev and says which button it leaves held; s.mu must be held.
func (s *Session) mouse(ev MouseEvent) (string, MouseButton, error) {
	button := map[MouseButton]int{"": 0, MouseLeft: 0, MouseMiddle: 1, MouseRight: 2}
	wheel := map[MouseAction]int{MouseScrollUp: 64, MouseScrollDown: 65, MouseScrollLeft: 66, MouseScrollRight: 67}
	code, ok := button[ev.Button]
	if !ok {
		return "", "", fmt.Errorf("terminal: unknown mouse button %q (use left, middle or right)", ev.Button)
	}
	if ev.Button == "" {
		ev.Button = MouseLeft
	}
	tracking, encoding := s.mouseMode()
	if tracking == "" {
		return "", "", errors.New("terminal: the program has not enabled mouse reporting")
	}
	if ev.Row < 1 || ev.Col < 1 || ev.Row > s.rows || ev.Col > s.cols {
		return "", "", fmt.Errorf("terminal: mouse position row %d, col %d is outside the %d×%d screen", ev.Row, ev.Col, s.cols, s.rows)
	}
	mods := 0
	if tracking != "press" { // X10 compatibility mode reports no modifiers.
		if ev.Shift {
			mods |= 4
		}
		if ev.Alt {
			mods |= 8
		}
		if ev.Ctrl {
			mods |= 16
		}
	}
	type report struct {
		code    int
		release bool
	}
	var reports []report
	held := s.held
	switch ev.Action {
	case MouseClick:
		reports = append(reports, report{code | mods, false})
		if tracking != "press" {
			reports = append(reports, report{code | mods, true})
		}
		held = ""
	case MousePress:
		reports, held = append(reports, report{code | mods, false}), ev.Button
	case MouseRelease:
		if tracking != "press" {
			reports = append(reports, report{code | mods, true})
		}
		held = ""
	case MouseMove:
		switch {
		case tracking == "motion" && held == "":
			reports = append(reports, report{32 | 3 | mods, false})
		case tracking == "motion" || tracking == "drag" && held != "":
			reports = append(reports, report{32 | button[held] | mods, false})
		case tracking == "drag":
			return "", "", errors.New("terminal: the program reports motion only while a button is held; press first")
		default:
			return "", "", errors.New("terminal: the program does not track mouse motion")
		}
	default:
		w, ok := wheel[ev.Action]
		if !ok {
			return "", "", fmt.Errorf("terminal: unknown mouse action %q (use click, press, release, move, scroll-up, scroll-down, scroll-left or scroll-right)", ev.Action)
		}
		reports = append(reports, report{w | mods, false})
	}
	var out strings.Builder
	for _, r := range reports {
		seq, err := encodeMouse(encoding, r.code, r.release, ev.Col, ev.Row)
		if err != nil {
			return "", "", err
		}
		out.WriteString(seq)
	}
	return out.String(), held, nil
}

func encodeMouse(encoding string, code int, release bool, col, row int) (string, error) {
	switch encoding {
	case "sgr", "sgr-pixels":
		final := 'M'
		if release {
			final = 'm'
		}
		if encoding == "sgr-pixels" {
			col, row = (col-1)*cellWidth+cellWidth/2+1, (row-1)*cellHeight+cellHeight/2+1
		}
		return fmt.Sprintf("\x1b[<%d;%d;%d%c", code, col, row, final), nil
	}
	if release { // Pre-SGR encodings cannot say which button was released.
		code = 3 | code&^3
	}
	switch encoding {
	case "urxvt":
		return fmt.Sprintf("\x1b[%d;%d;%dM", 32+code, col, row), nil
	case "utf8":
		if col > 2015 || row > 2015 {
			return "", errors.New("terminal: position beyond the UTF-8 mouse encoding's range")
		}
		return "\x1b[M" + string([]rune{rune(32 + code), rune(32 + col), rune(32 + row)}), nil
	}
	if col > 223 || row > 223 {
		return "", errors.New("terminal: position beyond the X10 mouse encoding's 223-cell range")
	}
	return "\x1b[M" + string([]byte{byte(32 + code), byte(32 + col), byte(32 + row)}), nil
}
