// Package sockets lists the machine's TCP and UDP sockets with the process
// that owns each, as netstat shows them: enough to tell which local ports
// belong to the machine's own services, and which process is on the other end
// of a loopback connection.
package sockets

import (
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Endpoint is one end of a socket. Host is "*" for any address and Port 0
// for any port.
type Endpoint struct {
	Host string
	Port int
}

// Loopback reports whether the endpoint is a loopback address, or any address,
// which loopback reaches too.
func (e Endpoint) Loopback() bool {
	if e.Host == "*" {
		return true
	}
	ip, err := netip.ParseAddr(e.Host)
	return err == nil && (ip.Unmap().IsLoopback() || ip.IsUnspecified())
}

// Socket is one socket and the process that last used it.
type Socket struct {
	Proto  string // tcp4, tcp6, tcp46, udp4, udp6, udp46
	Local  Endpoint
	Remote Endpoint
	State  string // LISTEN, ESTABLISHED, …; empty for UDP
	PID    int
}

// Listening reports whether the socket waits for others: a TCP listener, or
// a UDP socket bound but not connected.
func (s Socket) Listening() bool {
	if strings.HasPrefix(s.Proto, "udp") {
		return s.Remote.Host == "*" && s.Local.Port != 0
	}
	return s.State == "LISTEN"
}

// ErrUnsupported reports a platform where sockets cannot be listed.
var ErrUnsupported = errors.ErrUnsupported

// row is a line of `netstat -anv` on macOS 13 and later: the process that
// owns a socket is its name and pid joined by a colon, and the name may hold
// spaces.
var row = regexp.MustCompile(`^((?:tcp|udp)[46]*)\s+\d+\s+\d+\s+(\S+)\s+(\S+)\s+(?:([A-Z][A-Z_0-9]*)\s+)?\d+\s+\d+\s+\d+\s+\d+\s+(.*?):(\d+)\s+[0-9a-f]+\s`)

// parse reads netstat's output. A table it cannot read at all is an error:
// guessing would let the caller miss a socket it must know about.
func parse(out string) ([]Socket, error) {
	lines := strings.Split(out, "\n")
	header := false
	for _, line := range lines {
		if strings.HasPrefix(line, "Proto ") {
			header = true
			if !strings.Contains(line, "process:pid") {
				return nil, errors.New("netstat does not name the process of each socket here")
			}
		}
	}
	if !header {
		return nil, errors.New("netstat printed no socket table")
	}
	var socks []Socket
	for _, line := range lines {
		if !strings.HasPrefix(line, "tcp") && !strings.HasPrefix(line, "udp") {
			continue
		}
		m := row.FindStringSubmatch(line)
		if m == nil {
			return nil, errors.New("unrecognised netstat row: " + line)
		}
		pid, _ := strconv.Atoi(m[6])
		socks = append(socks, Socket{Proto: m[1], Local: endpoint(m[2]), Remote: endpoint(m[3]), State: m[4], PID: pid})
	}
	return socks, nil
}

// endpoint reads netstat's address.port, as in 127.0.0.1.5432, ::1.5432 or
// *.8080.
func endpoint(s string) Endpoint {
	i := strings.LastIndexByte(s, '.')
	if i < 0 {
		return Endpoint{Host: s}
	}
	host, port := s[:i], s[i+1:]
	n, _ := strconv.Atoi(port) // "*" is any port: 0
	return Endpoint{Host: host, Port: n}
}
