package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// The test binary doubles as the program under test: TERMINAL_HELPER picks
// a mode that reports what it receives from the terminal.
func TestMain(m *testing.M) {
	if mode := os.Getenv("TERMINAL_HELPER"); mode != "" {
		helper(mode, os.Getenv("TERMINAL_HELPER_SETUP"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(mode, setup string) {
	switch mode {
	case "input": // Show every byte received, quoted, on row 2.
		term.MakeRaw(os.Stdin.Fd())
		fmt.Print(setup + "ready\r\n")
		var all []byte
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			all = append(all, buf[:n]...)
			fmt.Printf("\x1b[2;1H\x1b[2Kgot %s", strconv.Quote(string(all)))
		}
	case "query": // Ask the terminal about itself, with DA1 as the sentinel.
		term.MakeRaw(os.Stdin.Fd())
		fmt.Print("\x1b[6n\x1b]11;?\x07\x1b[>q\x1b[16t\x1b[18t\x1b]4;1;?\x07\x1b[?996n\x1b[?2026$p\x1b[c")
		var all []byte
		buf := make([]byte, 1024)
		for !regexp.MustCompile(`\x1b\[\?[0-9;]*c`).Match(all) {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			all = append(all, buf[:n]...)
		}
		fmt.Printf("\x1b[2J\x1b[Hreplies %s\r\n", strconv.Quote(string(all)))
		time.Sleep(time.Minute)
	case "winch": // Print the window size at start and on every SIGWINCH.
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		for {
			ws, _ := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ)
			fmt.Printf("size %dx%d\r\n", ws.Col, ws.Row)
			<-winch
		}
	}
}

func startHelper(t *testing.T, mode, setup string, cols, rows int) *Session {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TERMINAL_HELPER="+mode, "TERMINAL_HELPER_SETUP="+setup)
	return start(t, cmd, cols, rows)
}

func startBash(t *testing.T, script string, cols, rows int) *Session {
	t.Helper()
	return start(t, exec.Command("/bin/bash", "--noprofile", "--norc", "-c", script), cols, rows)
}

func start(t *testing.T, cmd *exec.Cmd, cols, rows int) *Session {
	t.Helper()
	s, err := Start(cmd, cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func waitFor(t *testing.T, s *Session, pattern string) {
	t.Helper()
	if err := s.WaitFor(context.Background(), regexp.MustCompile(pattern), 5*time.Second); err != nil {
		t.Fatalf("waiting for %q: %v\n%s", pattern, err, s.Snapshot(0).Text())
	}
}

func TestSnapshotOfStyledOutput(t *testing.T) {
	s := startBash(t, `printf '\e]2;build; logs\a\e[1;31mred\e[m 界 ok   \n\e[?25l'; sleep 30`, 40, 6)
	waitFor(t, s, "ok")
	if err := s.WaitStable(context.Background(), 100*time.Millisecond, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot(0)
	if snap.Lines[0] != "red 界 ok" || snap.Title != "build; logs" || snap.CursorVisible || snap.State.Exited {
		t.Fatalf("snapshot = %+v", snap)
	}
	want := "screen 40×6 · cursor hidden · running · title \"build; logs\"\n1\tred 界 ok\n[rows 2–6 blank]"
	if got := snap.Text(); got != want {
		t.Fatalf("Text() =\n%s\nwant\n%s", got, want)
	}
	if pos, ok := snap.Find("ok", 0); !ok || pos != (Position{Row: 1, Col: 8}) {
		t.Fatalf("Find(ok) = %v %v; the wide character counts two columns", pos, ok)
	}
}

func TestEnvironmentDescribesThisTerminal(t *testing.T) {
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", `echo "$TERM/$COLORTERM/${LINES-unset}/${TERM_PROGRAM-unset}/$(stty size)"; sleep 30`)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TERM=dumb", "LINES=99", "TERM_PROGRAM=ghostty"}
	s := start(t, cmd, 77, 11)
	waitFor(t, s, `xterm-256color/truecolor/unset/unset/11 77`)
}

func TestTerminalEnv(t *testing.T) {
	got := terminalEnv([]string{"A=1", "TERM=dumb", "COLORTERM=24bit", "COLUMNS=80", "TMUX=/tmp/x"})
	want := []string{"A=1", "COLORTERM=24bit", "TERM=xterm-256color"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terminalEnv = %q, want %q", got, want)
	}
}

func TestStartKeepsCallerProcessAttributes(t *testing.T) {
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", `ps -o pid=,pgid=,tpgid= -p $$; sleep 30`)
	attr := &syscall.SysProcAttr{Setpgid: true}
	cmd.SysProcAttr = attr
	s := start(t, cmd, 80, 5)
	pid := strconv.Itoa(s.Pid())
	// The program leads its own process group, in the foreground of its
	// controlling terminal, so Close can kill everything it starts.
	waitFor(t, s, `(?m)^\s*`+pid+`\s+`+pid+`\s+`+pid+`$`)
	if !attr.Setpgid || attr.Setsid {
		t.Fatal("Start modified the caller's SysProcAttr")
	}

	busy := exec.Command("/bin/true")
	busy.Stdout = os.Stdout
	if _, err := Start(busy, 80, 24); err == nil {
		t.Fatal("Start accepted a command with stdout already set")
	}
	if _, err := Start(exec.Command("/bin/true"), 0, 24); err == nil {
		t.Fatal("Start accepted zero columns")
	}
}

// A command built for its terminal runs on the device it was told about.
func TestStartFuncNamesTheDevice(t *testing.T) {
	var device string
	s, err := StartFunc(60, 5, func(name string) (*exec.Cmd, error) {
		device = name
		return exec.Command("/bin/sh", "-c", "tty; sleep 30"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if !strings.HasPrefix(device, "/dev/") {
		t.Fatalf("device %q", device)
	}
	waitFor(t, s, regexp.QuoteMeta(device))
	refused := errors.New("refused")
	if _, err := StartFunc(60, 5, func(string) (*exec.Cmd, error) { return nil, refused }); !errors.Is(err, refused) {
		t.Fatalf("build error = %v", err)
	}
}

func TestQueryReplies(t *testing.T) {
	s := startHelper(t, "query", "", 200, 24)
	waitFor(t, s, "replies")
	text := strings.Join(s.Snapshot(0).Lines, "")
	for _, want := range []string{
		`\x1b[1;1R`,                         // DSR cursor position
		`\x1b]11;rgb:1d1d/1f1f/2121\a`,      // OSC 11 dark background
		`\x1bP>|julia-vt\x1b\\`,             // XTVERSION
		`\x1b[6;30;15t`,                     // cell size
		`\x1b[8;24;200t`,                    // size in cells
		`\x1b]4;1;rgb:cccc/6666/6666\x1b\\`, // palette red
		`\x1b[?997;1n`,                      // dark colour scheme
		`\x1b[?2026;2$y`,                    // synchronized output supported
		`\x1b[?62;`,                         // DA1
	} {
		if !strings.Contains(text, want) {
			t.Errorf("replies lack %s\n%s", want, text)
		}
	}
}

func TestKeysHonourCursorMode(t *testing.T) {
	s := startHelper(t, "input", "", 80, 4)
	waitFor(t, s, "ready")
	if err := s.SendKeys("up", "ctrl+c", "alt+x", "shift+tab", "f5", "enter"); err != nil {
		t.Fatal(err)
	}
	if err := s.SendText("hi\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, regexp.QuoteMeta(`got "\x1b[A\x03\x1bx\x1b[Z\x1b[15~\rhi\r"`))

	app := startHelper(t, "input", "\x1b[?1h", 80, 4)
	waitFor(t, app, "ready")
	app.SendKeys("up", "home", "ctrl+right")
	waitFor(t, app, regexp.QuoteMeta(`got "\x1bOA\x1bOH\x1b[1;5C"`))
	if snap := app.Snapshot(0); !snap.AppCursorKeys {
		t.Fatal("snapshot does not report application cursor keys")
	}
}

func TestPasteBracketsWhenEnabled(t *testing.T) {
	s := startHelper(t, "input", "\x1b[?2004h", 80, 4)
	waitFor(t, s, "ready")
	s.Paste("one\ntwo\x1b[201~")
	waitFor(t, s, regexp.QuoteMeta(`got "\x1b[200~one\ntwo\x1b[201~"`))

	plain := startHelper(t, "input", "", 80, 4)
	waitFor(t, plain, "ready")
	plain.Paste("one\ntwo")
	waitFor(t, plain, regexp.QuoteMeta(`got "one\rtwo"`))
}

func TestMouseFollowsTheProgramsMode(t *testing.T) {
	off := startHelper(t, "input", "", 80, 10)
	waitFor(t, off, "ready")
	if err := off.Mouse(MouseEvent{Action: MouseClick, Row: 1, Col: 1}); err == nil {
		t.Fatal("mouse event sent to a program without mouse reporting")
	}

	s := startHelper(t, "input", "\x1b[?1002h\x1b[?1006h", 120, 10)
	waitFor(t, s, "ready")
	if snap := s.Snapshot(0); snap.Mouse != "drag" || snap.MouseEncoding != "sgr" {
		t.Fatalf("mouse mode = %q/%q", snap.Mouse, snap.MouseEncoding)
	}
	// CheckMouse refuses what Mouse would, without sending anything.
	for _, ev := range []MouseEvent{{Action: MouseMove, Row: 3, Col: 5}, {Action: MouseClick, Row: 11, Col: 1}, {Action: MouseClick, Row: 1, Col: 121}} {
		if err := s.CheckMouse(ev); err == nil {
			t.Fatalf("CheckMouse accepted %+v", ev)
		}
		if err := s.Mouse(ev); err == nil {
			t.Fatalf("Mouse sent %+v", ev)
		}
	}
	if err := s.CheckMouse(MouseEvent{Action: MouseClick, Row: 3, Col: 5}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []MouseEvent{
		{Action: MouseClick, Row: 3, Col: 5},
		{Action: MousePress, Button: MouseRight, Row: 3, Col: 5, Ctrl: true},
		{Action: MouseMove, Row: 4, Col: 6},
		{Action: MouseRelease, Button: MouseRight, Row: 4, Col: 6},
		{Action: MouseScrollDown, Row: 1, Col: 80},
	} {
		if err := s.Mouse(ev); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, s, regexp.QuoteMeta(`got "\x1b[<0;5;3M\x1b[<0;5;3m\x1b[<18;5;3M\x1b[<34;6;4M\x1b[<2;6;4m\x1b[<65;80;1M"`))
	if err := s.Mouse(MouseEvent{Action: MouseClick, Row: 11, Col: 1}); err == nil {
		t.Fatal("mouse event outside the screen accepted")
	}
}

func TestResizeSignalsTheProgram(t *testing.T) {
	s := startHelper(t, "winch", "", 80, 24)
	waitFor(t, s, "size 80x24")
	if err := s.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "size 100x30")
	if snap := s.Snapshot(0); snap.Cols != 100 || snap.Rows != 30 || len(snap.Lines) != 30 {
		t.Fatalf("snapshot size %dx%d with %d lines", snap.Cols, snap.Rows, len(snap.Lines))
	}
	if err := s.Resize(MaxCols+1, 30); err == nil {
		t.Fatal("oversized resize accepted")
	}
}

func TestExitStateAndFinalScreen(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   State
	}{
		{"echo bye; exit 3", State{Exited: true, Code: 3}},
		{"echo bye; kill -TERM $$", State{Exited: true, Signal: "SIGTERM"}},
	} {
		s := startBash(t, tc.script, 40, 5)
		state, err := s.WaitExit(context.Background(), 5*time.Second)
		if err != nil || state != tc.want {
			t.Fatalf("%s: WaitExit = %+v, %v; want %+v", tc.script, state, err, tc.want)
		}
		s.Close()
		snap := s.Snapshot(0)
		if snap.Lines[0] != "bye" || snap.State != tc.want {
			t.Fatalf("%s: final snapshot %+v", tc.script, snap)
		}
		if !strings.Contains(snap.Text(), tc.want.String()) {
			t.Fatalf("header lacks %q:\n%s", tc.want, snap.Text())
		}
		if err := s.SendText("x"); !errors.Is(err, ErrClosed) {
			t.Fatalf("SendText after Close = %v", err)
		}
	}
}

func TestWaitSemantics(t *testing.T) {
	ctx := context.Background()
	s := startBash(t, `for i in 1 2 3 4 5; do echo $i; sleep 0.1; done; sleep 30`, 20, 8)
	if err := s.WaitStable(ctx, 300*time.Millisecond, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(0).Lines[4]; got != "5" {
		t.Fatalf("WaitStable returned before output settled: row 5 = %q", got)
	}

	busy := startBash(t, `while :; do echo x; sleep 0.05; done`, 20, 8)
	if err := busy.WaitStable(ctx, 300*time.Millisecond, 400*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("WaitStable on a busy screen = %v", err)
	}
	cancelled, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := busy.WaitFor(cancelled, regexp.MustCompile("never"), 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitFor after cancellation = %v", err)
	}

	done := startBash(t, `echo finished`, 20, 8)
	if err := done.WaitFor(ctx, regexp.MustCompile("never"), 5*time.Second); !errors.Is(err, ErrExited) {
		t.Fatalf("WaitFor on an exited program = %v", err)
	}
}

// A wait that ends because the program finished reports how it finished,
// not that it is still running: the exit is recorded a moment after its
// output ends.
func TestWaitsReportAQuickExit(t *testing.T) {
	ctx := context.Background()
	for range 20 {
		s := startBash(t, `printf hi; exit 3`, 20, 3)
		if err := s.WaitStable(ctx, 200*time.Millisecond, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if state := s.Snapshot(0).State; state != (State{Exited: true, Code: 3}) {
			t.Fatalf("after WaitStable: %+v", state)
		}
		s = startBash(t, `printf hi; exit 4`, 20, 3)
		if err := s.WaitFor(ctx, regexp.MustCompile("never"), 5*time.Second); !errors.Is(err, ErrExited) {
			t.Fatal(err)
		}
		if state := s.State(); state != (State{Exited: true, Code: 4}) {
			t.Fatalf("after WaitFor: %+v", state)
		}
	}
}

// interactiveShell is bash as a person would run it, with job control.
func interactiveShell(t *testing.T) *Session {
	t.Helper()
	shell := exec.Command("/bin/bash", "--noprofile", "--norc", "-i", "+H")
	shell.Env = []string{"PATH=/usr/bin:/bin", "PS1=$ ", "HOME=" + t.TempDir(), "BASH_SILENCE_DEPRECATION_WARNING=1"}
	s := start(t, shell, 80, 12)
	waitFor(t, s, `(?m)^\$$`)
	return s
}

// A watch is answered only by output that came after it began: not by the
// last command's result still on the screen, but by a prompt drawn again,
// even when one read echoes the command, runs it and redraws the prompt.
func TestWatchWantsNewOutput(t *testing.T) {
	ctx := context.Background()
	s := interactiveShell(t)
	s.SendText("echo tests passed\n")
	waitFor(t, s, `(?m)^tests passed$`)

	w := s.Watch(regexp.MustCompile(`passed|FAILED`))
	begun := time.Now()
	s.SendText(`sleep 0.3; echo tests FAIL""ED` + "\n")
	if err := w.Wait(ctx, 5*time.Second); err != nil || time.Since(begun) < 250*time.Millisecond {
		t.Fatalf("Wait = %v after %s, before the command finished\n%s", err, time.Since(begun), s.Snapshot(0).Text())
	}
	waitFor(t, s, `tests FAILED`)

	prompt := regexp.MustCompile(`(?m)\$$`)
	for _, command := range []string{"cd /", "echo hi", "true", "ls / >/dev/null"} {
		for range 5 {
			w := s.Watch(prompt)
			s.SendText(command + "\n")
			if err := w.Wait(ctx, 2*time.Second); err != nil {
				t.Fatalf("%s: prompt not seen again: %v\n%s", command, err, s.Snapshot(0).Text())
			}
		}
	}

	w = s.Watch(regexp.MustCompile(`tests FAILED`))
	s.SendText("true\n")
	if err := w.Wait(ctx, 500*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("old output answered a watch: %v", err)
	}
}

// While a shell runs a command, the snapshot names it.
func TestSnapshotNamesTheForegroundJob(t *testing.T) {
	s := interactiveShell(t)
	if snap := s.Snapshot(0); snap.Foreground != "" {
		t.Fatalf("idle shell has foreground %q", snap.Foreground)
	}
	s.SendText("sleep 0.5\n")
	deadline := time.Now().Add(2 * time.Second)
	for s.Snapshot(0).Foreground == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if snap := s.Snapshot(0); snap.Foreground != "sleep" || !strings.Contains(snap.Text(), ` · running · foreground job "sleep"`) {
		t.Fatalf("foreground %q:\n%s", snap.Foreground, snap.Text())
	}
	waitFor(t, s, `(?m)^\$$`)
	for s.Snapshot(0).Foreground != "" && time.Now().Before(deadline.Add(time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	if snap := s.Snapshot(0); snap.Foreground != "" {
		t.Fatalf("finished job still in the foreground: %q", snap.Foreground)
	}
}

func TestScrollback(t *testing.T) {
	s := startBash(t, `seq 1 100; sleep 30`, 20, 10)
	waitFor(t, s, `(?m)^100$`)
	snap := s.Snapshot(3)
	first, _ := strconv.Atoi(snap.Lines[0])
	want := []string{strconv.Itoa(first - 3), strconv.Itoa(first - 2), strconv.Itoa(first - 1)}
	if !reflect.DeepEqual(snap.Scrollback, want) {
		t.Fatalf("scrollback = %q, want %q above row 1 (%q)", snap.Scrollback, want, snap.Lines[0])
	}
	if text := snap.Text(); !strings.Contains(text, "-1\t"+want[2]+"\nscreen:\n1\t"+snap.Lines[0]) {
		t.Fatalf("Text() scrollback section:\n%s", text)
	}
}

func TestCloseKillsTheProcessGroupWithoutLeaks(t *testing.T) {
	baseline := runtime.NumGoroutine()
	var children []int
	for range 3 {
		s := startBash(t, `sleep 60 & echo child $!; wait`, 40, 5)
		waitFor(t, s, `child \d+`)
		pid, _ := strconv.Atoi(strings.TrimPrefix(s.Snapshot(0).Lines[0], "child "))
		children = append(children, pid)
		s.Close()
		s.Close() // idempotent
		if state := s.State(); !state.Stopped || state.String() != "stopped" {
			t.Fatalf("state after Close = %+v", state)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, pid := range children {
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("background child %d survived Close", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d after Close, %d before\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Close hangs up first, so a program can clean up as it would when its
// terminal window closes, then kills what an interactive shell left behind:
// jobs in process groups of their own, and nohup'd ones that ignore SIGHUP.
// Each process reports its pid once its signal handling is in place.
func TestCloseHangsUpAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	shell := exec.Command("/bin/bash", "--noprofile", "--norc", "-i", "+H")
	shell.Env = []string{"PATH=/usr/bin:/bin", "PS1=$ ", "HOME=" + dir, "BASH_SILENCE_DEPRECATION_WARNING=1"}
	s := start(t, shell, 100, 20)
	s.SendText(`sleep 600 & echo "job $!"; ` +
		`nohup sh -c 'echo "nohup $$" >` + dir + `/nohup; exec sleep 601' >/dev/null 2>&1 & ` +
		`sh -c 'trap "echo hup >` + dir + `/hup; exit" HUP; echo "trap $$"; while :; do sleep 0.05; done' &` + "\n")
	waitFor(t, s, `trap \d+`)
	var pids []int
	for i := 0; len(pids) < 3 && i < 100; i++ {
		data, _ := os.ReadFile(dir + "/nohup")
		pids = nil
		for _, m := range regexp.MustCompile(`(?:job|nohup|trap) (\d+)`).FindAllStringSubmatch(strings.Join(s.Snapshot(0).Lines, "\n")+string(data), -1) {
			pid, _ := strconv.Atoi(m[1])
			pids = append(pids, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pids) != 3 {
		t.Fatalf("pids %v from\n%s", pids, s.Snapshot(0).Text())
	}
	if pgid, _ := syscall.Getpgid(pids[0]); pgid == s.Pid() {
		t.Fatal("the shell has no job control; this test proves nothing")
	}
	s.Close()
	for _, pid := range pids {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("process %d survived Close: %v", pid, err)
		}
	}
	if data, err := os.ReadFile(dir + "/hup"); err != nil || string(data) != "hup\n" {
		t.Errorf("the job was not hung up before it was killed: %q %v", data, err)
	}
	if state := s.State(); state.String() != "stopped" {
		t.Errorf("state %+v", state)
	}
}

// TestJuliaDemo drives JULIA's own offline preview, a Bubble Tea v2 program
// that probes its terminal at startup, when JULIA_TERMINAL_DEMO=1.
func TestJuliaDemo(t *testing.T) {
	if os.Getenv("JULIA_TERMINAL_DEMO") == "" {
		t.Skip("set JULIA_TERMINAL_DEMO=1 to build and drive julia --demo")
	}
	bin := t.TempDir() + "/julia"
	if out, err := exec.Command("go", "build", "-o", bin, "julia/cmd/julia").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	s := start(t, exec.Command(bin, "--demo"), 120, 40)
	waitFor(t, s, "What shall we work on")
	s.SendText("hello")
	s.Paste("one\ntwo") // A bracketed paste adds a line instead of sending.
	waitFor(t, s, `› helloone\s*│\n\s*│\s+two`)
	snap := s.Snapshot(0)
	if !snap.AltScreen || snap.Mouse == "" || !snap.BracketedPaste || snap.Title == "" {
		t.Fatalf("modes: %+v", snap)
	}
	if path := os.Getenv("TERMINAL_RENDER_SAMPLE"); path != "" {
		data, _ := snap.PNG(RenderOptions{})
		os.WriteFile(path, data, 0o644)
	}
}

// emulator is a session without a program, fed output directly.
func emulator(t *testing.T, cols, rows int) *Session {
	s := &Session{cols: cols, rows: rows, dec: map[int]bool{}, cursorOn: true, changed: make(chan struct{})}
	s.emu = newEmulator(s, cols, rows)
	go func() { // Query replies go nowhere.
		buf := make([]byte, 1024)
		for {
			if _, err := s.emu.Read(buf); err != nil {
				return
			}
		}
	}()
	// As Close does: vt's own Close races with a pending Read.
	t.Cleanup(func() { s.emu.InputPipe().(*io.PipeWriter).CloseWithError(io.EOF) })
	return s
}

// Output that once panicked vt, and with it JULIA, is clamped to the screen
// as xterm does, including what a program drawing for its old size sends.
func TestHostileOutputIsClamped(t *testing.T) {
	for _, tc := range []struct {
		before string
		resize [2]int
		after  string
	}{
		{after: "\x1b[1;100r\x1b[2L"},
		{after: "\x1b[1;100r\x1b[L"},
		{after: "\x1b[1;100r\x1b[2M"},
		{after: "\x1b[5;100r\x1b[2S"},
		{after: "\x1b[5;100r\x1b[2T"},
		{after: "\x1b[?69h\x1b[1;300s\x1b[5@"},
		{before: "\x1b[1;70H\x1b7", resize: [2]int{20, 5}, after: "\x1b8\x1bH"},
		{before: "\x1b[1;70H\x1b7", resize: [2]int{20, 5}, after: "\x1b8\x1b[g"},
		{before: "\x1b[?1049h\x1b[24;80H", resize: [2]int{20, 5}, after: "\x1b[?1049l\x1bH\x1b[L"},
	} {
		s := emulator(t, 80, 24)
		s.feed([]byte(tc.before))
		if tc.resize[0] > 0 {
			s.resize(tc.resize[0], tc.resize[1])
		}
		s.feed([]byte(tc.after + "\x1b[Hstill here"))
		if snap := s.Snapshot(0); snap.Frozen || !strings.Contains(snap.Lines[0], "still here") {
			t.Errorf("%q, resize %v, %q: %s", tc.before, tc.resize, tc.after, snap.Text())
		}
	}
}

// Random escape sequences and resizes never get past the clamps.
func TestRandomOutputNeverFreezesTheScreen(t *testing.T) {
	const finals = "@ABCDEFGHIJKLMPSTXZ`abdefghlmnrsuxz"
	param := func(r *rand.Rand) string {
		return []string{"", strconv.Itoa(r.IntN(5)), strconv.Itoa(r.IntN(40)), strconv.Itoa(r.IntN(300))}[r.IntN(4)]
	}
	for seed := range uint64(300) {
		r := rand.New(rand.NewPCG(seed, 1))
		s := emulator(t, 80, 24)
		var trace []string
		for range 60 {
			var seq string
			switch r.IntN(12) {
			case 0:
				cols, rows := 1+r.IntN(120), 1+r.IntN(40)
				trace = append(trace, fmt.Sprintf("resize %dx%d", cols, rows))
				s.resize(cols, rows)
				continue
			case 1:
				seq = "\x1b" + string("78DEHMc=>"[r.IntN(9)])
			case 2:
				seq = "\x1b[?" + []string{"6", "69", "1049", "47", "1047", "7", "2026"}[r.IntN(7)] + string("hl"[r.IntN(2)])
			case 3:
				seq = strings.Repeat("x", r.IntN(120)) + "\r\n界🚀é́\t\b"
			default:
				seq = "\x1b[" + param(r) + ";" + param(r) + string(finals[r.IntN(len(finals))])
			}
			trace = append(trace, seq)
			s.feed([]byte(seq))
			if s.frozen != nil {
				t.Fatalf("seed %d froze the screen after %q", seed, trace)
			}
		}
	}
}

// A panic the clamps did not foresee freezes the screen instead of the
// process: the snapshot keeps the last screen and says so, and output keeps
// draining.
func TestEmulatorPanicFreezesTheScreen(t *testing.T) {
	s := emulator(t, 40, 5)
	s.emu.RegisterCsiHandler('y', func(ansi.Params) bool { panic("vt bug") })
	s.feed([]byte("before\r\n"))
	s.feed([]byte("\x1b[y"))
	s.feed([]byte("after"))
	s.resize(20, 3)
	snap := s.Snapshot(0)
	if !snap.Frozen || snap.Lines[0] != "before" || snap.Cols != 40 || !strings.HasPrefix(snap.Text(), "screen 40×5 · cursor row 2, col 1 · frozen · running\n1\tbefore") {
		t.Fatalf("frozen snapshot:\n%s", snap.Text())
	}
	if err := s.WaitFor(context.Background(), regexp.MustCompile("after"), time.Second); !errors.Is(err, ErrFrozen) {
		t.Fatalf("WaitFor on a frozen screen = %v", err)
	}
}

// The same output from a real program leaves JULIA and the program running.
func TestHostileProgramKeepsRunning(t *testing.T) {
	s := startBash(t, `printf '\e[1;100r\e[2L\e[Hdrawn'; sleep 30`, 80, 24)
	waitFor(t, s, "drawn")
	if snap := s.Snapshot(0); snap.Frozen || snap.State.Exited {
		t.Fatalf("snapshot %s", snap.Text())
	}
}
