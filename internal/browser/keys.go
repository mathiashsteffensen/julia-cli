package browser

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/chromedp/cdproto/input"
)

// keyDef is a key on a US keyboard as the DOM describes it.
type keyDef struct {
	key, code string
	keyCode   int64  // windowsVirtualKeyCode, the DOM's legacy keyCode
	text      string // what it types, if anything
	shifted   string // key and text with Shift held, for printable keys
	location  int64
}

// namedKeys are the non-printing keys, by canonical DOM name.
var namedKeys = map[string]keyDef{
	"Enter":      {key: "Enter", code: "Enter", keyCode: 13, text: "\r"},
	"Tab":        {key: "Tab", code: "Tab", keyCode: 9},
	"Backspace":  {key: "Backspace", code: "Backspace", keyCode: 8},
	"Delete":     {key: "Delete", code: "Delete", keyCode: 46},
	"Escape":     {key: "Escape", code: "Escape", keyCode: 27},
	"ArrowUp":    {key: "ArrowUp", code: "ArrowUp", keyCode: 38},
	"ArrowDown":  {key: "ArrowDown", code: "ArrowDown", keyCode: 40},
	"ArrowLeft":  {key: "ArrowLeft", code: "ArrowLeft", keyCode: 37},
	"ArrowRight": {key: "ArrowRight", code: "ArrowRight", keyCode: 39},
	"Home":       {key: "Home", code: "Home", keyCode: 36},
	"End":        {key: "End", code: "End", keyCode: 35},
	"PageUp":     {key: "PageUp", code: "PageUp", keyCode: 33},
	"PageDown":   {key: "PageDown", code: "PageDown", keyCode: 34},
	"Insert":     {key: "Insert", code: "Insert", keyCode: 45},
	"Shift":      {key: "Shift", code: "ShiftLeft", keyCode: 16, location: 1},
	"Control":    {key: "Control", code: "ControlLeft", keyCode: 17, location: 1},
	"Alt":        {key: "Alt", code: "AltLeft", keyCode: 18, location: 1},
	"Meta":       {key: "Meta", code: "MetaLeft", keyCode: 91, location: 1},
}

// keyAliases map the names the grammar accepts, lower-cased, to DOM names.
// They follow the Terminal tool's names where the two overlap.
var keyAliases = map[string]string{
	"enter": "Enter", "return": "Enter", "ret": "Enter",
	"tab": "Tab", "backspace": "Backspace", "bs": "Backspace", "bspace": "Backspace",
	"delete": "Delete", "del": "Delete",
	"escape": "Escape", "esc": "Escape",
	"up": "ArrowUp", "arrowup": "ArrowUp", "down": "ArrowDown", "arrowdown": "ArrowDown",
	"left": "ArrowLeft", "arrowleft": "ArrowLeft", "right": "ArrowRight", "arrowright": "ArrowRight",
	"home": "Home", "end": "End",
	"pageup": "PageUp", "pgup": "PageUp", "ppage": "PageUp",
	"pagedown": "PageDown", "pgdown": "PageDown", "pgdn": "PageDown", "npage": "PageDown",
	"insert": "Insert", "ins": "Insert",
	"space": " ", "spc": " ",
	"shift": "Shift", "control": "Control", "ctrl": "Control", "alt": "Alt", "option": "Alt",
	"meta": "Meta", "cmd": "Meta", "command": "Meta",
}

// printable are the US layout's typing keys: unshifted, then shifted.
var printable = map[rune]keyDef{}

func init() {
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("F%d", i)
		namedKeys[name] = keyDef{key: name, code: name, keyCode: int64(111 + i)}
		keyAliases[strings.ToLower(name)] = name
	}
	add := func(base, shifted rune, code string, keyCode int64) {
		def := keyDef{key: string(base), code: code, keyCode: keyCode, text: string(base), shifted: string(shifted)}
		printable[base] = def
		if shifted != 0 {
			printable[shifted] = def
		}
	}
	for r := 'a'; r <= 'z'; r++ {
		add(r, r-'a'+'A', "Key"+strings.ToUpper(string(r)), int64(r-'a'+'A'))
	}
	for i, shifted := range ")!@#$%^&*(" {
		add(rune('0'+i), shifted, fmt.Sprintf("Digit%d", i), int64('0'+i))
	}
	for _, k := range []struct {
		base, shifted rune
		code          string
		keyCode       int64
	}{
		{' ', 0, "Space", 32}, {'`', '~', "Backquote", 192}, {'-', '_', "Minus", 189}, {'=', '+', "Equal", 187},
		{'[', '{', "BracketLeft", 219}, {']', '}', "BracketRight", 221}, {'\\', '|', "Backslash", 220},
		{';', ':', "Semicolon", 186}, {'\'', '"', "Quote", 222}, {',', '<', "Comma", 188},
		{'.', '>', "Period", 190}, {'/', '?', "Slash", 191},
	} {
		add(k.base, k.shifted, k.code, k.keyCode)
	}
}

// Modifier bits, as Input.dispatchKeyEvent and dispatchMouseEvent take them.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

// chord is one parsed key press: a key and the modifiers held for it.
type chord struct {
	spec      string
	def       keyDef
	char      string // the character typed, for a printable key
	modifiers int64
}

// parseKey reads one key of the grammar Press documents.
func parseKey(spec string) (chord, error) {
	k := chord{spec: spec}
	rest := spec
	for len(rest) > 2 && rest[1] == '-' && strings.ContainsRune("CMS", rune(rest[0])) {
		k.modifiers |= map[byte]int64{'C': modCtrl, 'M': modAlt, 'S': modShift}[rest[0]]
		rest = rest[2:]
	}
	for {
		i := strings.IndexByte(rest, '+')
		if i <= 0 || i == len(rest)-1 {
			break
		}
		switch strings.ToLower(rest[:i]) {
		case "ctrl", "control":
			k.modifiers |= modCtrl
		case "alt", "option", "opt":
			k.modifiers |= modAlt
		case "shift":
			k.modifiers |= modShift
		case "meta", "cmd", "command", "super", "win":
			k.modifiers |= modMeta
		default:
			return k, fmt.Errorf("key %q: unknown modifier %q (use ctrl, alt, shift or meta)", spec, rest[:i])
		}
		rest = rest[i+1:]
	}
	if rest == "" {
		return k, fmt.Errorf("key %q is empty", spec)
	}
	if utf8.RuneCountInString(rest) == 1 {
		r, _ := utf8.DecodeRuneInString(rest)
		if r < ' ' || r == 0x7f {
			return k, fmt.Errorf("key %q is a control character; name it (Enter, Tab, Backspace …)", spec)
		}
		def, ok := printable[r]
		if ok && def.shifted == string(r) && r != ' ' {
			k.modifiers |= modShift
		}
		if ok && k.modifiers&modShift != 0 && def.shifted != "" {
			k.char = def.shifted
		} else if ok {
			k.char = def.text
		} else {
			k.char = rest
		}
		k.def = def
		return k, nil
	}
	name, ok := keyAliases[strings.ToLower(rest)]
	if !ok {
		if _, named := namedKeys[rest]; named {
			name, ok = rest, true
		}
	}
	if !ok {
		return k, fmt.Errorf("unknown key %q: keys are single characters or names such as Enter, Tab, Escape, ArrowDown, PageUp, F5, with modifiers like ctrl+a or shift+Tab", spec)
	}
	if name == " " {
		k.def, k.char = printable[' '], " "
		return k, nil
	}
	k.def = namedKeys[name]
	return k, nil
}

// CheckKeys returns the error Press would give keys, so a caller can refuse
// bad keys before acting on them.
func CheckKeys(keys ...string) error {
	if len(keys) == 0 {
		return fmt.Errorf("no keys given")
	}
	for _, spec := range keys {
		if _, err := parseKey(spec); err != nil {
			return err
		}
	}
	return nil
}

// editingCommands are the editor commands a shortcut runs. Headless Chrome on
// macOS has no native key bindings to run them, so they are sent with the key.
var editingCommands = map[string]string{
	"ctrl+a": "selectAll", "meta+a": "selectAll",
	"ctrl+z": "undo", "meta+z": "undo",
	"ctrl+y": "redo", "ctrl+shift+z": "redo", "meta+shift+z": "redo",
	"ctrl+x": "cut", "meta+x": "cut",
	"ctrl+c": "copy", "meta+c": "copy",
	"ctrl+v": "paste", "meta+v": "paste",
}

func (k chord) commands() []string {
	if k.def.code == "" || !strings.HasPrefix(k.def.code, "Key") {
		return nil
	}
	var name []string
	if k.modifiers&modCtrl != 0 {
		name = append(name, "ctrl")
	}
	if k.modifiers&modMeta != 0 {
		name = append(name, "meta")
	}
	if k.modifiers&modShift != 0 {
		name = append(name, "shift")
	}
	if k.modifiers&modAlt != 0 || len(name) == 0 {
		return nil
	}
	cmd := editingCommands[strings.Join(name, "+")+"+"+strings.ToLower(k.def.key)]
	if cmd == "" {
		return nil
	}
	return []string{cmd}
}

var modifierKeys = []struct {
	bit  int64
	name string
}{{modShift, "Shift"}, {modCtrl, "Control"}, {modAlt, "Alt"}, {modMeta, "Meta"}}

// press sends one chord: modifier downs, the key's down and up, and the
// modifier ups, as a keyboard does. A character not on the US layout is
// inserted as text.
func (p *Page) press(ctx context.Context, k chord) error {
	if k.def.code == "" {
		return p.insertText(ctx, k.char)
	}
	held := int64(0)
	for _, m := range modifierKeys {
		if k.modifiers&m.bit != 0 && k.def.key != m.name {
			held |= m.bit
			if err := p.keyEvent(ctx, input.DispatchKeyEventTypeKeyDown, namedKeys[m.name], held, "", nil); err != nil {
				return err
			}
		}
	}
	text := k.char
	if k.def.key == "Enter" {
		text = "\r"
	}
	if k.modifiers&(modCtrl|modMeta|modAlt) != 0 {
		text = "" // a shortcut types nothing
	}
	def := k.def
	if k.char != "" && k.modifiers&modShift != 0 && def.shifted != "" {
		def.key = def.shifted
	}
	typ := input.DispatchKeyEventTypeKeyDown
	if text == "" {
		typ = input.DispatchKeyEventTypeRawKeyDown
	}
	err := p.keyEvent(ctx, typ, def, k.modifiers, text, k.commands())
	if err == nil {
		err = p.keyEvent(ctx, input.DispatchKeyEventTypeKeyUp, def, k.modifiers, "", nil)
	}
	for i := len(modifierKeys) - 1; i >= 0; i-- {
		m := modifierKeys[i]
		if held&m.bit != 0 {
			held &^= m.bit
			if e := p.keyEvent(ctx, input.DispatchKeyEventTypeKeyUp, namedKeys[m.name], held, "", nil); err == nil {
				err = e
			}
		}
	}
	return err
}

func (p *Page) keyEvent(ctx context.Context, typ input.DispatchKeyEventType, def keyDef, modifiers int64, text string, commands []string) error {
	params := input.DispatchKeyEventParams{
		Type:      typ,
		Modifiers: modifiers,
		Key:       def.key,
		Code:      def.code,
		// No nativeVirtualKeyCode: on macOS it is a Mac key code (49 is
		// Space), and a wrong one can wedge the renderer's input.
		WindowsVirtualKeyCode: def.keyCode,
		Location:              def.location,
		Text:                  text,
		UnmodifiedText:        text,
		Commands:              commands,
	}
	_, err := p.root.call(ctx, input.CommandDispatchKeyEvent, params)
	return err
}

func (p *Page) insertText(ctx context.Context, text string) error {
	_, err := p.root.call(ctx, input.CommandInsertText, input.InsertTextParams{Text: text})
	return err
}

// typeText types text key by key, as a person does, so keydown, keypress,
// input and keyup handlers all run; a newline presses Enter. Characters off
// the US layout, and text over maxKeyedText runes, go in as inserted text.
func (p *Page) typeText(ctx context.Context, text string) error {
	if utf8.RuneCountInString(text) > maxKeyedText {
		return p.insertText(ctx, text)
	}
	var pending strings.Builder
	flush := func() error {
		if pending.Len() == 0 {
			return nil
		}
		defer pending.Reset()
		return p.insertText(ctx, pending.String())
	}
	for _, r := range strings.ReplaceAll(text, "\r\n", "\n") {
		var k chord
		switch def, ok := printable[r]; {
		case r == '\n' || r == '\r':
			k = chord{def: namedKeys["Enter"]}
		case r == '\t':
			k = chord{def: namedKeys["Tab"]}
		case ok:
			k = chord{def: def, char: string(r)}
			if def.shifted == string(r) && r != ' ' {
				k.modifiers = modShift
			}
		default:
			pending.WriteRune(r)
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		if err := p.press(ctx, k); err != nil {
			return err
		}
	}
	return flush()
}

// maxKeyedText is the longest text typed key by key; longer text is
// inserted in one input event.
const maxKeyedText = 500
