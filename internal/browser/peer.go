package browser

import (
	"context"
	"errors"
	"net"

	"julia/internal/sockets"
)

// chromeOwns reports whether a connection to the proxy comes from Chrome or
// one of its helpers. The proxy listens on loopback, which a sandboxed command
// allowed onto loopback can reach too; without this check it could borrow the
// browser's approvals and send what it likes to an approved origin. Where
// sockets cannot be listed, no sandbox runs beside the browser, and every
// local connection is Chrome's.
func chromeOwns(conn net.Conn, chrome int) bool {
	client, ok1 := conn.RemoteAddr().(*net.TCPAddr)
	server, ok2 := conn.LocalAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return false
	}
	socks, err := sockets.List(context.Background(), "tcp")
	if errors.Is(err, sockets.ErrUnsupported) {
		return true
	}
	if err != nil {
		return false
	}
	for _, s := range socks {
		if s.Local.Port == client.Port && s.Remote.Port == server.Port && s.Remote.Loopback() {
			return descends(s.PID, chrome)
		}
	}
	return false
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
