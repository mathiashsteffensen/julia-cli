package browser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"
)

// proxy is how LoopbackOnly contexts reach beyond loopback: an HTTP proxy on
// loopback that opens only what dial allows, and nothing when dial is nil.
// Chrome reaches loopback hosts directly and sends everything else here, so
// whatever the request policy cannot see (WebSockets, preconnects) still
// meets dial. Chrome is told that what arrives through it is the public
// internet, so a page from beyond loopback cannot reach loopback servers.
type proxy struct {
	l    net.Listener
	srv  *http.Server
	dial func(ctx context.Context, address string) (net.Conn, error)

	mu      sync.Mutex
	admit   func(net.Conn) bool   // whether a connection may use the proxy; nil admits none
	tunnels map[net.Conn]struct{} // hijacked connections, which srv no longer tracks
	closed  bool
}

func startProxy(dial func(ctx context.Context, address string) (net.Conn, error)) (*proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{l: l, tunnels: map[net.Conn]struct{}{}}
	p.dial = func(ctx context.Context, address string) (net.Conn, error) {
		if dial == nil {
			return nil, errors.New(address + " is beyond loopback, which this browser does not reach")
		}
		return dial(ctx, address)
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return p.dial(ctx, address)
		},
		MaxIdleConns:          32,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	forward := &httputil.ReverseProxy{
		// The request already names its destination; nothing is added.
		Rewrite:   func(*httputil.ProxyRequest) {},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, err.Error(), http.StatusBadGateway)
		},
	}
	p.srv = &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodConnect:
				p.tunnel(w, r)
			case r.URL.Scheme == "http" && r.URL.Host != "":
				forward.ServeHTTP(w, r)
			default:
				http.Error(w, "not a proxy request", http.StatusBadRequest)
			}
		}),
	}
	go p.srv.Serve(admitting{l, p})
	return p, nil
}

func (p *proxy) addr() string { return p.l.Addr().String() }

// admitting is the proxy's listener, which hangs up on a connection the
// proxy does not admit before reading a byte of it.
type admitting struct {
	net.Listener
	p *proxy
}

func (l admitting) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.p.mu.Lock()
		admit := l.p.admit
		l.p.mu.Unlock()
		if admit != nil && admit(conn) {
			return conn, nil
		}
		conn.Close()
	}
}

// admitAll lets every connection use the proxy, for tests that drive it
// without a browser.
func (p *proxy) admitAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.admit = func(net.Conn) bool { return true }
}

// tunnel answers a CONNECT, for https and WebSockets: it opens the connection
// through dial and then copies bytes both ways until either side ends.
func (p *proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	upstream, err := p.dial(ctx, r.Host)
	cancel()
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if !p.track(client, upstream) {
		return
	}
	defer p.untrack(client, upstream)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		// Bytes the client sent after its CONNECT may already be buffered.
		io.Copy(upstream, buffered)
		closeWrite(upstream)
		close(done)
	}()
	io.Copy(client, upstream)
	closeWrite(client)
	<-done
}

func closeWrite(c net.Conn) {
	if tcp, ok := c.(interface{ CloseWrite() error }); ok {
		tcp.CloseWrite()
	}
}

func (p *proxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for _, c := range conns {
			c.Close()
		}
		return false
	}
	for _, c := range conns {
		p.tunnels[c] = struct{}{}
	}
	return true
}

func (p *proxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		c.Close()
		delete(p.tunnels, c)
	}
}

// close stops the proxy and every connection through it. A nil proxy is
// already closed.
func (p *proxy) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	tunnels := p.tunnels
	p.tunnels = nil
	p.mu.Unlock()
	for c := range tunnels {
		c.Close()
	}
	if err := p.srv.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		p.l.Close()
	}
}
