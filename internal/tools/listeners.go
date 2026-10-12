package tools

import (
	"context"
	"slices"

	"julia/internal/sockets"
)

// hostPorts lists the local ports the machine's own services listen on: every
// TCP or UDP listener on a loopback or wildcard address but those of this
// executor's terminals, whose servers the agent started itself. A loopback
// sandbox may connect to its own servers and to those, never to these: a
// local database or daemon runs outside the sandbox and often trusts any
// local client, which would carry a command beyond the sandbox unreviewed.
// The browser's proxy, which JULIA itself runs, is one of them.
func (e *Executor) hostPorts(ctx context.Context) ([]int, error) {
	_, host, err := e.loopbackListeners(ctx)
	return host, err
}

// terminalPorts lists the local ports only this executor's terminals listen
// on: the servers the agent started itself, which the browser may reach
// without approval. A port a service of the machine's shares is not one.
func (e *Executor) terminalPorts(ctx context.Context) ([]int, error) {
	own, host, err := e.loopbackListeners(ctx)
	return slices.DeleteFunc(own, func(port int) bool { return slices.Contains(host, port) }), err
}

// loopbackListeners splits the ports listened on at a loopback or wildcard
// address between this executor's terminals and everything else.
func (e *Executor) loopbackListeners(ctx context.Context) (own, host []int, err error) {
	socks, err := sockets.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	pids := map[int]bool{}
	for _, t := range e.terminals {
		pids[t.session.Pid()] = true
		members, _ := t.session.Members()
		for _, pid := range members {
			pids[pid] = true
		}
	}
	for _, s := range socks {
		if !s.Listening() || !s.Local.Loopback() {
			continue
		}
		list := &host
		if pids[s.PID] {
			list = &own
		}
		if !slices.Contains(*list, s.Local.Port) {
			*list = append(*list, s.Local.Port)
		}
	}
	slices.Sort(own)
	slices.Sort(host)
	return own, host, nil
}
