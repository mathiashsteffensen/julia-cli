package browser

import (
	"context"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// chromeOwns reports whether a connection to the proxy comes from Chrome or
// one of its helpers. The proxy listens on loopback, which a sandboxed command
// allowed onto loopback can reach too; without this check it could borrow the
// browser's approvals. The command sandbox runs on macOS only.
func chromeOwns(conn net.Conn, chrome int) bool {
	client, ok1 := conn.RemoteAddr().(*net.TCPAddr)
	server, ok2 := conn.LocalAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return false
	}
	if runtime.GOOS != "darwin" {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a",
		"-i4TCP@"+client.String(), "-sTCP:ESTABLISHED", "-Fpn").Output()
	if err != nil {
		return false
	}

	pid := clientPID(string(out), client.String(), server.String())
	return pid > 0 && descends(pid, chrome)
}

// lsof lists both ends of a loopback connection. Only the entry pointing
// from the client to the proxy identifies the process we need to check.
func clientPID(fields, client, server string) int {
	want := "n" + client + "->" + server
	var pid int
	for field := range strings.SplitSeq(fields, "\n") {
		if value, ok := strings.CutPrefix(field, "p"); ok {
			pid, _ = strconv.Atoi(value)
		}
		if field == want {
			return pid
		}
	}
	return 0
}

// descends reports whether pid is root or one of its descendants. A parent
// cannot be changed by the child, unlike a process group, which any process
// of the same session may join.
func descends(pid, root int) bool {
	parent := map[int]int{}
	for _, p := range processes() {
		parent[p.pid] = p.ppid
	}
	for hops := 0; hops < 16 && pid > 1; hops++ {
		if pid == root {
			return true
		}
		pid = parent[pid]
	}
	return false
}
