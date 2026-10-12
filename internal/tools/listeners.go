package tools

import (
	"context"
	"slices"

	"jarvis/internal/sockets"
)

// hostPorts lists the local ports the machine's own services listen on: every
// TCP or UDP listener on a loopback or wildcard address but those of this
// executor's terminals, whose servers the agent started itself. A loopback
// sandbox may connect to its own servers and to those, never to these: a
// local database or daemon runs outside the sandbox and often trusts any
// local client, which would carry a command beyond the sandbox unreviewed.
// The browser's proxy, which JARVIS itself runs, is one of them.
func (e *Executor) hostPorts(ctx context.Context) ([]int, error) {
	socks, err := sockets.List(ctx)
	if err != nil {
		return nil, err
	}
	own := map[int]bool{}
	for _, t := range e.terminals {
		own[t.session.Pid()] = true
		members, _ := t.session.Members()
		for _, pid := range members {
			own[pid] = true
		}
	}
	var ports []int
	for _, s := range socks {
		if s.Listening() && s.Local.Loopback() && !own[s.PID] && !slices.Contains(ports, s.Local.Port) {
			ports = append(ports, s.Local.Port)
		}
	}
	slices.Sort(ports)
	return ports, nil
}
