package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jarvis/agents"
	"jarvis/internal/core"
)

func terminalFixture(t *testing.T, mode string, approve core.Approver) *Executor {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("sandboxed terminals require macOS")
	}
	e, _ := fixture(t, mode, approve)
	return e
}

func terminalCall(t *testing.T, e core.ToolExecutor, args map[string]any) core.ToolResult {
	t.Helper()
	b, _ := json.Marshal(args)
	return e.Execute(context.Background(), core.ToolCall{ID: "call", Name: "Terminal", Arguments: b})
}

// Arguments are checked before anything runs or is put to anyone for approval.
func TestTerminalArguments(t *testing.T) {
	e, _ := fixture(t, "ask", func(context.Context, core.Approval) (core.Decision, error) {
		t.Error("invalid arguments reached approval")
		return core.Decision{}, nil
	})
	click := map[string]any{"action": "click", "row": 1, "col": 1}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "open"}, "action must be"},
		{map[string]any{"action": "list", "bogus": 1}, "invalid arguments"},
		{map[string]any{"action": "start"}, "command is required"},
		{map[string]any{"action": "start", "command": "bash", "id": "t1"}, "id does not apply to start"},
		{map[string]any{"action": "start", "command": "bash", "cols": 5000}, "size must be"},
		{map[string]any{"action": "start", "command": "bash", "rows": 2}, "size must be"},
		{map[string]any{"action": "send", "text": "ls\n"}, "send needs the id"},
		{map[string]any{"action": "send", "id": "t1"}, "send needs text, keys or mouse"},
		{map[string]any{"action": "send", "id": "t1", "text": "x", "network": true}, "network does not apply to send"},
		{map[string]any{"action": "send", "id": "t1", "keys": []string{"shift+enter"}}, "alt+enter"},
		{map[string]any{"action": "send", "id": "t1", "keys": []string{"enter", "hyper+x"}}, "unknown modifier"},
		{map[string]any{"action": "send", "id": "t1", "text": "x", "mouse": click}, "on its own"},
		{map[string]any{"action": "send", "id": "t1", "keys": []string{"a"}, "paste": true}, "paste needs text"},
		{map[string]any{"action": "send", "id": "t1", "mouse": map[string]any{"action": "click", "row": 1, "col": 1, "button": "fourth"}}, "button must be"},
		{map[string]any{"action": "send", "id": "t1", "mouse": map[string]any{"action": "click", "row": 1}}, "needs a row and col"},
		{map[string]any{"action": "send", "id": "t1", "mouse": map[string]any{"action": "hover", "row": 1, "col": 1}}, "mouse action must be"},
		{map[string]any{"action": "snapshot", "id": "t1", "wait_for": "("}, "wait_for"},
		{map[string]any{"action": "snapshot", "id": "t1", "timeout": 301}, "timeout must be"},
		{map[string]any{"action": "snapshot", "id": "t1", "scrollback": 5000}, "scrollback must be"},
		{map[string]any{"action": "resize", "id": "t1", "cols": 80}, "resize needs cols and rows"},
		{map[string]any{"action": "stop", "id": "t1", "timeout": 3}, "timeout does not apply to stop"},
		{map[string]any{"action": "list", "mouse": click}, "mouse does not apply to list"},
		{map[string]any{"action": "snapshot", "id": "t9"}, `no terminal "t9"`},
	} {
		r := terminalCall(t, e, tc.args)
		if !r.IsError || !strings.Contains(r.Content, tc.want) {
			t.Errorf("%v: %+v, want an error mentioning %q", tc.args, r, tc.want)
		}
	}
}

// The policy mirrors Bash: read-only mode never starts, types into or resizes
// a terminal; ask mode approves each start and send; auto mode runs offline
// terminals freely but reviews a network terminal's start and every send.
func TestTerminalPolicy(t *testing.T) {
	const command = "printf ready; exec cat"
	for _, mode := range []string{"auto", "ask", "read-only"} {
		for _, network := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "/network", false: "/offline"}[network], func(t *testing.T) {
				var asked, reviewed []core.Approval
				e := terminalFixture(t, mode, func(_ context.Context, a core.Approval) (core.Decision, error) {
					asked = append(asked, a)
					return core.Decision{Allow: true}, nil
				})
				e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
					reviewed = append(reviewed, a)
					return core.Decision{Allow: true, Reason: "fixture"}, nil
				}
				// Each send and start is put to: nobody, the reviewer, or a human.
				gate := map[string]string{"auto": "", "ask": "human", "read-only": "refused"}[mode]
				if mode == "auto" && network {
					gate = "reviewer"
				}
				check := func(action string, r core.ToolResult, gated bool) {
					t.Helper()
					want := ""
					if gated {
						want = gate
					}
					got := ""
					switch {
					case r.IsError && strings.Contains(r.Content, "read-only mode prohibits"):
						got = "refused"
					case r.IsError:
						t.Fatalf("%s failed: %s", action, r.Content)
					case len(asked) > 0:
						got = "human"
					case len(reviewed) > 0:
						got = "reviewer"
					}
					if got != want {
						t.Fatalf("%s went to %q, want %q (asked %d, reviewed %d)", action, got, want, len(asked), len(reviewed))
					}
					for _, a := range append(asked, reviewed...) {
						if a.Call.Name != "Terminal" || a.ExecutionMode != "sandboxed" || a.Network != network || !strings.Contains(string(a.Call.Arguments), `"command":"printf ready; exec cat"`) {
							t.Fatalf("%s approval lacks the terminal's command or capabilities: %+v %s", action, a, a.Call.Arguments)
						}
					}
					asked, reviewed = nil, nil
				}

				r := terminalCall(t, e, map[string]any{"action": "start", "command": command, "network": network})
				check("start", r, true)
				if mode == "read-only" {
					// Read-only runs no terminal, but one open already stays readable.
					e.config.Mode = "auto"
					if r = terminalCall(t, e, map[string]any{"action": "start", "command": command, "network": network}); r.IsError {
						t.Fatal(r.Content)
					}
					e.config.Mode = "read-only"
					asked, reviewed = nil, nil
				}
				if !strings.Contains(r.Content, "1\tready") {
					t.Fatalf("start screen: %s", r.Content)
				}

				r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "hello\n", "wait_for": "hello\\n.*hello"})
				check("send", r, true)
				r = terminalCall(t, e, map[string]any{"action": "resize", "id": "t1", "cols": 80, "rows": 20})
				check("resize", r, mode == "read-only")
				for _, action := range []string{"snapshot", "list", "stop"} {
					args := map[string]any{"action": action}
					if action != "list" {
						args["id"] = "t1"
					}
					check(action, terminalCall(t, e, args), false)
				}
			})
		}
	}
}

// A loopback terminal reaches only the servers the agent starts itself, so
// auto mode runs it, and everything sent to it, without review; ask mode
// still asks the user, saying what the terminal may do.
func TestTerminalLoopback(t *testing.T) {
	var asked, reviewed []core.Approval
	e := terminalFixture(t, "auto", func(_ context.Context, a core.Approval) (core.Decision, error) {
		asked = append(asked, a)
		return core.Decision{Allow: true}, nil
	})
	e.config.Review = func(_ context.Context, a core.Approval) (core.Decision, error) {
		reviewed = append(reviewed, a)
		return core.Decision{Allow: true}, nil
	}
	if r := terminalCall(t, e, map[string]any{"action": "start", "command": "bash", "network": true, "loopback": true}); !r.IsError || !strings.Contains(r.Content, "network already includes loopback") {
		t.Fatalf("network and loopback: %+v", r)
	}
	r := terminalCall(t, e, map[string]any{"action": "start", "command": "printf ready; exec cat", "loopback": true, "cols": 40, "rows": 5})
	if r.IsError || !strings.HasPrefix(r.Content, "t1 · printf ready; exec cat · loopback\n") {
		t.Fatalf("start: %+v", r)
	}
	if r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "hi\n"}); r.IsError {
		t.Fatal(r.Content)
	}
	if len(asked)+len(reviewed) > 0 {
		t.Fatalf("a loopback terminal was put to approval: %+v %+v", asked, reviewed)
	}
	e.config.Mode = "ask"
	if r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "again\n"}); r.IsError || len(asked) != 1 {
		t.Fatalf("ask mode: %+v", r)
	}
	if args := string(asked[0].Call.Arguments); !strings.Contains(args, `"loopback":true`) || !strings.Contains(args, `"network":false`) || asked[0].Network || !strings.Contains(asked[0].Reason, "listen on localhost and connect to the servers it") {
		t.Fatalf("approval: %s %+v", args, asked[0])
	}
	// Bash takes the same flag, under the same rules.
	e.config.Mode = "auto"
	asked = nil
	if r := invoke(e, "Bash", map[string]any{"command": "true", "loopback": true}); r.IsError || len(asked)+len(reviewed) > 0 {
		t.Fatalf("loopback Bash: %+v", r)
	}
	if r := invoke(e, "Bash", map[string]any{"command": "true", "loopback": true, "network": true}); !r.IsError || !strings.Contains(r.Content, "network already includes loopback") {
		t.Fatalf("Bash with both: %+v", r)
	}
	e.config.Mode = "ask"
	if r := invoke(e, "Bash", map[string]any{"command": "true", "loopback": true}); r.IsError || len(asked) != 1 || !strings.Contains(asked[0].Reason, "listen on localhost and connect to the servers it") {
		t.Fatalf("ask-mode loopback Bash: %+v %+v", r, asked)
	}
}

// What a person or reviewer approves names the terminal's command beside the
// exact input, and a refusal sends nothing.
func TestTerminalApprovalShowsCommandAndInput(t *testing.T) {
	var approvals []core.Approval
	allow := true
	e := terminalFixture(t, "ask", func(_ context.Context, a core.Approval) (core.Decision, error) {
		approvals = append(approvals, a)
		return core.Decision{Allow: allow, Reason: "fixture"}, nil
	})
	if r := terminalCall(t, e, map[string]any{"action": "start", "command": "printf ready; exec cat", "cols": 40, "rows": 5}); r.IsError {
		t.Fatal(r.Content)
	}
	allow = false
	r := terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "secret-word\n", "keys": []string{"ctrl+d"}})
	if !r.IsError || !strings.Contains(r.Content, "approval denied") {
		t.Fatalf("denied send: %+v", r)
	}
	var request map[string]any
	if err := json.Unmarshal(approvals[1].Call.Arguments, &request); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"action": "send", "terminal": "t1", "command": "printf ready; exec cat", "network": false, "text": "secret-word\n", "keys": []any{"ctrl+d"}}
	if len(request) != len(want) {
		t.Fatalf("request %v, want %v", request, want)
	}
	for key, value := range want {
		if got, _ := json.Marshal(request[key]); string(got) != func() string { b, _ := json.Marshal(value); return string(b) }() {
			t.Fatalf("%s = %s in %v", key, got, request)
		}
	}
	if !strings.Contains(approvals[1].Reason, "terminal t1") {
		t.Fatalf("reason %q", approvals[1].Reason)
	}
	if r = terminalCall(t, e, map[string]any{"action": "snapshot", "id": "t1"}); strings.Contains(r.Content, "secret-word") {
		t.Fatalf("denied input reached the program: %s", r.Content)
	}
	if r = terminalCall(t, e, map[string]any{"action": "start", "command": "bash"}); !r.IsError || len(e.terminals) != 1 {
		t.Fatalf("denied start opened a terminal: %+v", r)
	}
}

func TestTerminalLifecycle(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	for range maxTerminals {
		if r := terminalCall(t, e, map[string]any{"action": "start", "command": "printf ready; exec sleep 600", "cols": 40, "rows": 5}); r.IsError {
			t.Fatal(r.Content)
		}
	}
	r := terminalCall(t, e, map[string]any{"action": "start", "command": "true"})
	if !r.IsError || !strings.Contains(r.Content, "4 terminals are open") || !strings.Contains(r.Content, "t4 · printf ready; exec sleep 600 · running · 40×5") {
		t.Fatalf("fifth terminal: %+v", r)
	}
	if r = terminalCall(t, e, map[string]any{"action": "stop", "id": "t4"}); r.IsError || !strings.Contains(r.Content, " · stopped\n") {
		t.Fatalf("stop: %+v", r)
	}
	// Ids are never reused, and a program that exited says so at once and
	// stays readable.
	r = terminalCall(t, e, map[string]any{"action": "start", "command": "printf done; exit 3", "cols": 40, "rows": 5})
	if r.IsError || !strings.HasPrefix(r.Content, "t5 · ") || !strings.Contains(r.Content, "exited with code 3") {
		t.Fatalf("start after stop: %+v", r)
	}
	if r = terminalCall(t, e, map[string]any{"action": "snapshot", "id": "t5"}); r.IsError || !strings.Contains(r.Content, "exited with code 3") || !strings.Contains(r.Content, "1\tdone") {
		t.Fatalf("exited terminal: %+v", r)
	}
	if r = terminalCall(t, e, map[string]any{"action": "send", "id": "t5", "text": "x"}); !r.IsError || !strings.Contains(r.Content, "t5's program has exited with code 3") {
		t.Fatalf("send to an exited program: %+v", r)
	}
	if r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "mouse": map[string]any{"action": "click", "row": 1, "col": 1}}); !r.IsError || !strings.Contains(r.Content, "mouse reporting") {
		t.Fatalf("mouse without reporting: %+v", r)
	}
	if r = terminalCall(t, e, map[string]any{"action": "list"}); strings.Count(r.Content, "\n") != 3 || !strings.Contains(r.Content, "t5 · printf done; exit 3 · exited with code 3") {
		t.Fatalf("list: %s", r.Content)
	}
	// Close kills every program and removes its private HOME and TMPDIR.
	var pids []int
	var tmps []string
	for _, open := range e.terminals {
		pids, tmps = append(pids, open.session.Pid()), append(tmps, open.tmp)
	}
	e.Close()
	for i, pid := range pids {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("process %d survived Close: %v", pid, err)
		}
		if _, err := os.Stat(tmps[i]); !os.IsNotExist(err) {
			t.Errorf("%s survived Close: %v", tmps[i], err)
		}
	}
}

// A program that fails at once is reported as it ended, not as running.
func TestTerminalQuickExit(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	for i := range 6 {
		r := terminalCall(t, e, map[string]any{"action": "start", "command": "echo boom >&2; exit 1", "cols": 40, "rows": 5})
		if r.IsError || !strings.Contains(r.Content, "exited with code 1\n1\tboom") {
			t.Fatalf("start %d: %+v", i, r)
		}
		terminalCall(t, e, map[string]any{"action": "stop", "id": fmt.Sprintf("t%d", i+1)})
	}
}

// Jobs a shell runs in the background are killed with the terminal, and its
// private HOME and TMPDIR outlive them.
func TestTerminalCloseLeavesNoJobs(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	r := terminalCall(t, e, map[string]any{"action": "start", "command": "bash"})
	if r.IsError {
		t.Fatal(r.Content)
	}
	r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "sleep 1000 & nohup sleep 1001 >/dev/null 2>&1 & jobs -p\n", "wait_for": `^\d+\n\d+$`})
	pids := regexp.MustCompile(`(?m)^\d+\t(\d+)$`).FindAllStringSubmatch(r.Content, -1)
	if r.IsError || len(pids) != 2 {
		t.Fatalf("send: %+v", r)
	}
	e.Close()
	for _, m := range pids {
		pid, _ := strconv.Atoi(m[1])
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("job %d survived Close: %v", pid, err)
		}
	}
}

// stop hangs up first, as closing a terminal window does, so an editor left
// open removes its swap file instead of leaving it in the workspace.
func TestTerminalStopLetsAnEditorCleanUp(t *testing.T) {
	if _, err := exec.LookPath("vim"); err != nil {
		t.Skip("vim is not installed")
	}
	e := terminalFixture(t, "auto", nil)
	if r := terminalCall(t, e, map[string]any{"action": "start", "command": "vim -N -u NONE notes.md", "wait_for": "notes.md"}); r.IsError {
		t.Fatal(r.Content)
	}
	swap := filepath.Join(e.directory, ".notes.md.swp")
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(swap); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(swap); err != nil {
		t.Fatalf("vim made no swap file to clean up: %v", err)
	}
	if r := terminalCall(t, e, map[string]any{"action": "stop", "id": "t1"}); r.IsError || !strings.Contains(r.Content, " · stopped\n") {
		t.Fatalf("stop: %+v", r)
	}
	if _, err := os.Stat(swap); !os.IsNotExist(err) {
		t.Fatalf("the swap file outlived stop: %v", err)
	}
}

// wait_for after input wants new output: not text that was already on the
// screen, while ^ and $ anchor at rows, as a prompt pattern needs.
func TestTerminalWaitForNewOutput(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	if r := terminalCall(t, e, map[string]any{"action": "start", "command": "bash", "wait_for": `\$$`}); r.IsError || strings.Contains(r.Content, "wait_for") {
		t.Fatalf("start: %+v", r)
	}
	if r := terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "echo build finished\n", "wait_for": `^build finished$`}); r.IsError || strings.Contains(r.Content, "wait_for") {
		t.Fatalf("first build: %+v", r)
	}
	begun := time.Now()
	r := terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "sleep 0.4; echo build finished\n", "wait_for": `^build finished$`})
	if r.IsError || strings.Contains(r.Content, "wait_for") || time.Since(begun) < 350*time.Millisecond || strings.Count(r.Content, "\tbuild finished") != 2 {
		t.Fatalf("second build after %s: %+v", time.Since(begun), r)
	}
	if r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "cd /\n", "wait_for": `\$$`, "timeout": 2}); r.IsError || strings.Contains(r.Content, "wait_for") {
		t.Fatalf("prompt: %+v", r)
	}
	r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "true\n", "wait_for": "build finished", "timeout": 1})
	if r.IsError || !strings.Contains(r.Content, "wait_for found no new match within 1s; the match on the screen was there before.") {
		t.Fatalf("old match: %+v", r)
	}
}

// A mouse event that cannot be delivered is refused before anyone is asked,
// and one aimed at text lands on it, wide characters and all.
func TestTerminalMouseIsCheckedBeforeApproval(t *testing.T) {
	asked := 0
	e := terminalFixture(t, "ask", func(_ context.Context, a core.Approval) (core.Decision, error) {
		asked++
		return core.Decision{Allow: true}, nil
	})
	if r := terminalCall(t, e, map[string]any{"action": "start", "command": `printf '\e[?1002h\e[?1006h📦 Packages  [Install]\n'; exec cat`, "cols": 40, "rows": 5, "wait_for": "Install"}); r.IsError {
		t.Fatal(r.Content)
	}
	for mouse, want := range map[string]string{
		`{"action":"click","row":50,"col":1}`:         "outside the 40×5 screen",
		`{"action":"move","row":1,"col":1}`:           "press first",
		`{"action":"click","text":"Remove"}`:          `"Remove" is not on the screen of t1`,
		`{"action":"click","text":"Install","row":2}`: `"Install" is not on row 2 of t1`,
	} {
		asked = 0
		r := e.Execute(context.Background(), core.ToolCall{Name: "Terminal", Arguments: json.RawMessage(`{"action":"send","id":"t1","mouse":` + mouse + `}`)})
		if !r.IsError || !strings.Contains(r.Content, want) || asked != 0 {
			t.Errorf("%s: %+v after %d approvals; want an error mentioning %q", mouse, r, asked, want)
		}
	}
	r := terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "mouse": map[string]any{"action": "click", "text": "[Install]"}, "wait_for": `14;1m`})
	if r.IsError || asked != 1 || !strings.Contains(r.Content, "^[[<0;14;1M^[[<0;14;1m") {
		t.Fatalf("click on text: %+v", r)
	}
}

// A subagent sees only its own terminals, and they stop when it returns.
func TestTerminalSubagentScope(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	start := map[string]any{"action": "start", "command": "printf ready; exec sleep 600", "cols": 40, "rows": 5}
	if r := terminalCall(t, e, start); r.IsError {
		t.Fatal(r.Content)
	}
	var child *openTerminal
	executor := WithSubagents(e, agents.Builtin(), func(ctx context.Context, _ agents.Role, _ string, tools core.ToolExecutor) (string, error) {
		if r := terminalCall(t, tools, map[string]any{"action": "snapshot", "id": "t1"}); !r.IsError {
			return "", errors.New("the child reached the parent's terminal")
		}
		if r := terminalCall(t, tools, start); r.IsError || !strings.HasPrefix(r.Content, "t2 · ") {
			return "", errors.New("child start: " + r.Content)
		}
		child = e.terminals[1]
		r := terminalCall(t, tools, map[string]any{"action": "list"})
		if !strings.HasPrefix(r.Content, "t2 · ") || !strings.HasSuffix(r.Content, "1 more belongs to another agent.") {
			return "", errors.New("child list: " + r.Content)
		}
		return "ok", nil
	})
	if r := executor.Execute(context.Background(), core.ToolCall{Name: "Subagent", Arguments: json.RawMessage(`{"tasks":[{"role":"delegate","task":"drive a terminal"}]}`)}); r.IsError || strings.Contains(r.Content, `"error"`) {
		t.Fatal(r.Content)
	}
	if len(e.terminals) != 1 || e.terminals[0].id != "t1" || child == nil || !child.session.State().Exited {
		t.Fatalf("after the subagent: %d open, child %+v", len(e.terminals), child)
	}
}

func TestTerminalImage(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	r := terminalCall(t, e, map[string]any{"action": "start", "command": "printf 'hello ✓ ✅'", "cols": 40, "rows": 5, "image": true})
	if r.IsError || len(r.Images) != 1 || r.Images[0].MediaType != "image/png" || r.Images[0].Width < 400 || len(r.Images[0].Data) == 0 {
		t.Fatalf("result %+v", r.Record())
	}
	// A character drawn as a box is named, so the screenshot is not believed.
	if !strings.HasSuffix(r.Content, "\n[screenshot: no font here has ✅, so they are drawn as boxes; the text above is exact]") {
		t.Fatalf("no note of the emoji drawn as a box:\n%s", r.Content)
	}
	if r = terminalCall(t, e, map[string]any{"action": "snapshot", "id": "t1"}); len(r.Images) != 0 {
		t.Fatal("an image without image=true")
	}
}

// A wait holds the executor, so cancellation must end it at once.
func TestTerminalWaitCancels(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	if r := terminalCall(t, e, map[string]any{"action": "start", "command": "printf ready; exec cat"}); r.IsError {
		t.Fatal(r.Content)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	begun := time.Now()
	r := e.Execute(ctx, core.ToolCall{Name: "Terminal", Arguments: json.RawMessage(`{"action":"send","id":"t1","text":"x","wait_for":"never","timeout":300}`)})
	if !r.IsError || !strings.Contains(r.Content, "deadline") || time.Since(begun) > 3*time.Second {
		t.Fatalf("%+v after %s", r, time.Since(begun))
	}
	if r = terminalCall(t, e, map[string]any{"action": "snapshot", "id": "t1", "wait_for": "never", "timeout": 1}); r.IsError || !strings.Contains(r.Content, "wait_for did not match within 1s.") {
		t.Fatalf("timed-out wait: %+v", r)
	}
}

// The output limit takes history before the screen the call asked about.
func TestTerminalScrollbackYieldsToScreen(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	r := terminalCall(t, e, map[string]any{"action": "start", "command": "for i in $(seq 1 1500); do printf 'line %04d %s\\n' $i $(printf '%090d' 0); done; printf end", "scrollback": 1000, "wait_for": "end"})
	if r.IsError || len(r.Content) > maxOutput || !strings.Contains(r.Content, "\tend") || !strings.Contains(r.Content, "scrollback, last ") || strings.Contains(r.Content, "last 1000 lines") {
		t.Fatalf("%d bytes:\n%s", len(r.Content), r.Content[:min(len(r.Content), 400)])
	}
}

// Bash and Terminal build their commands in one place: a terminal's profile is
// Bash's plus its own device, and their environments are the same.
func TestShellSharedByBashAndTerminal(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	bash, err := e.shell(context.Background(), "echo hi", online, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(bash.tmp)
	term, err := e.shell(context.Background(), "echo hi", online, false, "/dev/ttys042")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(term.tmp)
	same := func(s string) string { return strings.ReplaceAll(s, term.tmp, bash.tmp) }
	rules, _ := terminalProfile("/dev/ttys042")
	if len(term.args) != len(bash.args) || term.args[0] != "-p" || same(term.args[1]) != bash.args[1]+rules || strings.Join(term.args[2:], " ") != "/bin/bash --noprofile --norc -c echo hi" {
		t.Fatalf("terminal args differ beyond the device:\n%q\n%q", bash.args, term.args)
	}
	if same(strings.Join(term.env, "\n")) != strings.Join(bash.env, "\n") {
		t.Fatalf("environments differ:\n%q\n%q", bash.env, term.env)
	}
	if !strings.HasPrefix(bash.args[1], seatbeltProfile(e.directory, bash.tmp, e.scratch, online, nil)[:200]) || !strings.Contains(bash.args[1], "(allow network*)") || strings.Contains(bash.args[1], "file-ioctl") {
		t.Fatal("Bash's profile changed")
	}
	var keys []string
	for _, kv := range bash.env {
		key, _, _ := strings.Cut(kv, "=")
		keys = append(keys, key)
	}
	if got := strings.Join(keys[:22], " "); got != "PATH HOME TMPDIR TMP TEMP JARVIS_SCRATCH LANG LC_ALL TERM GIT_CONFIG_NOSYSTEM GIT_CONFIG_GLOBAL GIT_TERMINAL_PROMPT GIT_OPTIONAL_LOCKS GIT_CONFIG_COUNT GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0 XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME GOCACHE GOMODCACHE GOPATH" {
		t.Fatalf("Bash's environment changed: %s", got)
	}
}

func TestTerminalProfileGrantsOnlyItsDevice(t *testing.T) {
	rules, err := terminalProfile("/dev/ttys042")
	if err != nil || rules != "(allow file-ioctl file-read-metadata (literal \"/dev/ttys042\"))\n(allow file-read* file-write* file-ioctl (literal \"/dev/tty\"))\n(allow file-read-data (literal \"/dev\"))\n" {
		t.Fatalf("%q %v", rules, err)
	}
	for _, device := range []string{"/dev/disk0", "/tmp/ttys1", "/dev/pts/../disk0", ""} {
		if _, err := terminalProfile(device); err == nil {
			t.Errorf("granted %q", device)
		}
	}
}

// A terminal program reads and writes its terminal through what it inherited
// and /dev/tty, never by the device's name, which a later, unrelated terminal
// may reuse.
func TestTerminalDeviceIsNotOpenedByName(t *testing.T) {
	e := terminalFixture(t, "auto", nil)
	r := terminalCall(t, e, map[string]any{"action": "start", "cols": 60, "rows": 8, "wait_for": "done",
		"command": `T=$(tty); echo "$T"; stty raw -echo && stty -raw echo && echo ioctl-ok; ` +
			`(exec 3<"$T") 2>/dev/null || echo read-refused; (exec 3>"$T") 2>/dev/null || echo write-refused; ` +
			`echo hi >/dev/tty && echo dev-tty-ok; echo done`})
	if r.IsError || !regexp.MustCompile(`(?m)^1\t/dev/ttys\d+\n2\tioctl-ok\n3\tread-refused\n4\twrite-refused\n5\thi\n6\tdev-tty-ok$`).MatchString(r.Content) {
		t.Fatalf("%+v", r)
	}
}

// Run with JARVIS_SANDBOX_TEST=1: builds JARVIS and drives its demo in a
// sandboxed terminal, the hardest case: raw mode, the alternate screen, mouse
// and paste modes, and terminal queries, all through Seatbelt.
func TestSandboxIntegrationTerminalDrivesJarvis(t *testing.T) {
	if os.Getenv("JARVIS_SANDBOX_TEST") != "1" {
		t.Skip("set JARVIS_SANDBOX_TEST=1 for host sandbox integration")
	}
	e := terminalFixture(t, "auto", nil)
	if out, err := exec.Command("go", "build", "-o", filepath.Join(e.directory, "jarvis"), "jarvis/cmd/jarvis").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	r := terminalCall(t, e, map[string]any{"action": "start", "command": "./jarvis --demo", "cols": 120, "rows": 40, "wait_for": "What shall we work on", "timeout": 30})
	if r.IsError || !strings.Contains(r.Content, "alt screen · mouse on · running") || !strings.Contains(r.Content, "✦ JARVIS") {
		t.Fatalf("demo did not draw:\n%s", r.Content)
	}
	r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "text": "hello", "wait_for": "› hello"})
	if r.IsError {
		t.Fatalf("typing:\n%s", r.Content)
	}
	r = terminalCall(t, e, map[string]any{"action": "send", "id": "t1", "keys": []string{"ctrl+p"}, "wait_for": "CHOOSE A MODEL", "image": true})
	if r.IsError || len(r.Images) != 1 {
		t.Fatalf("keys:\n%s", r.Content)
	}
	if path := os.Getenv("JARVIS_TERMINAL_SCREENSHOT"); path != "" {
		os.WriteFile(path, r.Images[0].Data, 0o644)
	}
}
