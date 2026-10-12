package browser

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// leftovers lists the processes of a closed browser that are still alive:
// its descendants and process group, and anything naming its profile.
func leftovers(pid int, profile string) []string {
	var left []string
	for _, p := range processTree(pid) {
		left = append(left, "pid "+strconv.Itoa(p.pid))
	}
	out, _ := exec.Command("pgrep", "-f", profile).Output()
	for _, line := range strings.Fields(string(out)) {
		left = append(left, "pgrep "+line)
	}
	return left
}

func TestCloseKillsEveryProcessAndRemovesProfile(t *testing.T) {
	t.Parallel() // it mostly waits out timeouts, or on a browser of its own
	bin, err := Find(os.Getenv("JARVIS_BROWSER"))
	if err != nil {
		t.Skip("no Chrome to test with: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := Launch(ctx, Options{Binary: bin, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	site := newSite()
	defer site.close()
	c, err := b.NewContext(ctx, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Renderers for two sites, a GPU process, the network service: helpers
	// of every kind.
	for _, path := range []string{"/app", "/requests"} {
		p, err := c.NewPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Navigate(ctx, site.url(path), WaitLoad); err != nil {
			t.Fatal(err)
		}
	}
	running := processTree(b.PID())
	if len(running) < 4 {
		t.Fatalf("expected Chrome and its helpers, found %d processes", len(running))
	}
	if out, _ := exec.Command("pgrep", "-f", b.ProfileDir()).Output(); len(strings.Fields(string(out))) == 0 {
		t.Fatal("pgrep finds no process using the profile")
	}
	pid, profile := b.PID(), b.ProfileDir()
	start := time.Now()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Close took %v", took)
	}
	if left := leftovers(pid, profile); len(left) > 0 {
		t.Fatalf("processes left after Close: %v", left)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile %s still exists (err %v)", profile, err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-b.Done():
	default:
		t.Fatal("Done is not closed after Close")
	}
	if _, err := c.NewPage(ctx); err == nil {
		t.Fatal("NewPage on a closed browser succeeded")
	}
}

func TestUnexpectedExitIsReported(t *testing.T) {
	bin, err := Find(os.Getenv("JARVIS_BROWSER"))
	if err != nil {
		t.Skip("no Chrome to test with: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := Launch(ctx, Options{Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := b.NewContext(ctx, ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.NewPage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.cmd.Process.Kill()
	select {
	case <-b.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done not closed after Chrome died")
	}
	if err := b.Err(); err == nil || !strings.Contains(err.Error(), "unexpectedly") {
		t.Fatalf("Err = %v, want an unexpected exit", err)
	}
	if err := p.Navigate(ctx, "about:blank", WaitLoad); err == nil {
		t.Fatal("Navigate on a dead browser succeeded")
	}
}

// No browser outlives the process that launched it: when that dies without
// closing it (a terminal window closed under it, SIGKILL, a panic, a test
// timeout), the watchdog kills Chrome and removes the profile.
func TestWatchdogKillsAnOrphanedBrowser(t *testing.T) {
	if os.Getenv("JARVIS_WATCHDOG_CHILD") == "1" {
		b, err := Launch(context.Background(), Options{Binary: os.Getenv("JARVIS_WATCHDOG_BINARY")})
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(b.PID(), b.ProfileDir())
		time.Sleep(time.Minute) // until killed
		return
	}
	bin, err := Find(os.Getenv("JARVIS_BROWSER"))
	if err != nil {
		t.Skip("no Chrome to test with: " + err.Error())
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWatchdogKillsAnOrphanedBrowser$")
	cmd.Env = append(os.Environ(), "JARVIS_WATCHDOG_CHILD=1", "JARVIS_WATCHDOG_BINARY="+bin)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(out).ReadString('\n')
	var pid int
	var profile string
	if _, scanErr := fmt.Sscan(line, &pid, &profile); err != nil || scanErr != nil || pid <= 1 {
		t.Fatalf("the child said %q (%v)", line, err)
	}
	running := processTree(pid)
	cmd.Process.Kill()
	cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, statErr := os.Stat(profile)
		if syscall.Kill(pid, 0) == syscall.ESRCH && os.IsNotExist(statErr) && len(leftovers(pid, profile)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	kill(append(running, processTree(pid)...))
	os.RemoveAll(profile)
	t.Fatalf("Chrome %d or its profile outlived the process that launched it: %v", pid, leftovers(pid, profile))
}

func TestLaunchFailureShowsChromeOutput(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "chrome")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'cannot open display' >&2\nexit 3\n"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Launch(ctx, Options{Binary: fake, DataDir: dir})
	if err == nil || !strings.Contains(err.Error(), "cannot open display") {
		t.Fatalf("err = %v, want Chrome's output in it", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "jarvis-browser-") {
			t.Fatalf("profile %s left behind", e.Name())
		}
	}
}

func TestContextsAreIsolated(t *testing.T) {
	a := open(t)
	b := open(t)
	a.goTo(t, "/")
	b.goTo(t, "/")
	a.eval(t, "localStorage.setItem('who', 'a'); document.cookie = 'session=a'")
	if got := b.eval(t, "String(localStorage.getItem('who')) + ' ' + document.cookie"); got != `"null "` {
		t.Fatalf("context b sees %s", got)
	}
	if a.p.ID() == b.p.ID() {
		t.Fatal("pages share an id")
	}
	if _, err := a.c.Page(b.p.ID()); err == nil {
		t.Fatal("a context finds another context's page")
	}
}

func TestChromeArgsAndEnv(t *testing.T) {
	args := chromeArgs("/x/Google Chrome", "/tmp/p", []string{"--lang=de"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--headless=new", "--remote-debugging-pipe", "--user-data-dir=/tmp/p", "--no-first-run", "--disable-extensions", "--disable-sync", "--disable-background-networking", "--disable-component-update", "--use-mock-keychain", "--no-proxy-server", "--lang=de"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %s", want)
		}
	}
	if strings.Contains(joined, "--no-sandbox") || strings.Contains(joined, "remote-debugging-port") {
		t.Error("args weaken the sandbox or open a port")
	}
	if shell := chromeArgs("/x/chrome-headless-shell", "/p", nil); shell[0] != "--headless" {
		t.Errorf("headless shell gets %s", shell[0])
	}
	if proxied := strings.Join(chromeArgs("/x/chrome", "/p", []string{"--proxy-server=http://h:1"}), " "); strings.Contains(proxied, "--no-proxy-server") {
		t.Error("--no-proxy-server overrides the caller's proxy")
	}
	env := chromeEnv([]string{"HOME=/h", "PATH=/bin", "OPENAI_API_KEY=sk", "GITHUB_TOKEN=x", "LC_ALL=C", "AWS_SECRET_ACCESS_KEY=y"}, "/p")
	if got := strings.Join(env, " "); got != "HOME=/h PATH=/bin LC_ALL=C BREAKPAD_DUMP_LOCATION=/p/Crashpad" {
		t.Fatalf("env = %s", got)
	}
}

func TestReadMessageBoundsSize(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 300)
	stream := append([]byte(`{"id":7,"result":{}}`+"\x00"+`{"id":9,"result":"`), big...)
	stream = append(stream, []byte("\"}\x00{\"id\":10}\x00")...)
	br := bufio.NewReaderSize(bytes.NewReader(stream), 16)
	msg, err := readMessage(br, 100)
	if err != nil || string(msg) != `{"id":7,"result":{}}` {
		t.Fatalf("first = %q, %v", msg, err)
	}
	msg, err = readMessage(br, 100)
	if !errors.Is(err, errTooLarge) || !idPrefix.Match(msg) {
		t.Fatalf("second = %q, %v; want the start of a message too large", msg, err)
	}
	msg, err = readMessage(br, 100)
	if err != nil || string(msg) != `{"id":10}` {
		t.Fatalf("third = %q, %v", msg, err)
	}
}

func TestFind(t *testing.T) {
	home := t.TempDir()
	exe := func(path string) string {
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)
		return path
	}
	cache := filepath.Join(home, "Library", "Caches", "ms-playwright")
	shell := exe(filepath.Join(cache, "chromium_headless_shell-1300", "chrome-headless-shell-mac-arm64", "chrome-headless-shell"))
	old := exe(filepath.Join(cache, "chromium-1200", "chrome-mac-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"))
	newest := exe(filepath.Join(cache, "chromium-1234", "chrome-mac-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"))
	exe(filepath.Join(cache, "chromium-1234", "chrome-mac-x64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"))
	app := exe(filepath.Join(home, "Applications", "Chromium.app", "Contents", "MacOS", "Chromium"))
	apps := []string{filepath.Join(home, "System Applications"), filepath.Join(home, "Applications")}
	noLook := func(string) (string, error) { return "", errors.New("none") }
	for _, tc := range []struct {
		name string
		f    finder
		want string
		err  string
	}{
		{"explicit wins", finder{explicit: old, goos: "darwin", home: home, lookPath: noLook}, old, ""},
		{"explicit .app bundle", finder{explicit: filepath.Join(home, "Applications", "Chromium.app"), goos: "darwin", home: home, lookPath: noLook}, app, ""},
		{"explicit missing", finder{explicit: "/no/such/chrome", goos: "darwin", home: home, lookPath: noLook}, "", "not found"},
		{"installed app before caches", finder{goos: "darwin", goarch: "arm64", home: home, apps: apps, lookPath: noLook}, app, ""},
		{"newest full Chrome before a newer shell", finder{goos: "darwin", goarch: "arm64", home: home, apps: apps, playwright: cache, lookPath: noLook}, newest, ""},
		{"linux PATH last", finder{goos: "linux", home: t.TempDir(), lookPath: func(name string) (string, error) {
			if name == "chromium" {
				return "/usr/bin/chromium", nil
			}
			return "", errors.New("none")
		}}, "/usr/bin/chromium", ""},
		{"nothing found", finder{goos: "linux", home: t.TempDir(), lookPath: noLook}, "", "npx playwright install chromium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "newest full Chrome before a newer shell" {
				os.RemoveAll(filepath.Join(home, "Applications"))
				defer exe(app)
			}
			got, err := tc.f.find()
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	os.RemoveAll(filepath.Join(home, "Applications"))
	os.RemoveAll(filepath.Join(cache, "chromium-1200"))
	os.RemoveAll(filepath.Join(cache, "chromium-1234"))
	if got, err := (finder{goos: "darwin", home: home, apps: apps, lookPath: noLook}).find(); err != nil || got != shell {
		t.Fatalf("shell fallback: %q, %v", got, err)
	}
}
