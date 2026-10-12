package sockets

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// List returns the machine's sockets of the protocols named, tcp and udp,
// or of both when none is.
func List(ctx context.Context, protos ...string) ([]Socket, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if len(protos) == 0 {
		protos = []string{"tcp", "udp"}
	}
	var all []Socket
	for _, proto := range protos {
		out, err := exec.CommandContext(ctx, "/usr/sbin/netstat", "-anv", "-p", proto).Output()
		if err != nil {
			return nil, fmt.Errorf("netstat: %w", err)
		}
		socks, err := parse(string(out))
		if err != nil {
			return nil, err
		}
		for _, s := range socks {
			if strings.HasPrefix(s.Proto, proto) {
				all = append(all, s)
			}
		}
	}
	return all, nil
}
