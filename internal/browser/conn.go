package browser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
)

// maxMessage bounds one message from Chrome, so a runaway page cannot make
// JARVIS buffer without limit. The largest legitimate messages are screenshots,
// which Screenshot keeps far below it.
const maxMessage = 128 << 20

// decodeOptions tolerate what JavaScript strings may hold (lone surrogates in a
// title or console message) rather than dropping the whole message.
var decodeOptions = json.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

// ProtocolError is an error Chrome returned for a command.
type ProtocolError struct {
	Method  string
	Code    int64
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%s: %s", e.Method, e.Message)
}

type wireError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitzero"`
}

type message struct {
	ID        int64          `json:"id,omitzero"`
	SessionID string         `json:"sessionId,omitzero"`
	Method    string         `json:"method,omitzero"`
	Params    jsontext.Value `json:"params,omitzero"`
	Result    jsontext.Value `json:"result,omitzero"`
	Error     *wireError     `json:"error,omitzero"`
}

type reply struct {
	result jsontext.Value
	err    error
}

type pendingCall struct {
	id      int64
	method  string
	session string
	reply   chan reply
}

// conn speaks the DevTools protocol over Chrome's --remote-debugging-pipe:
// NUL-terminated JSON messages, commands on one pipe and replies and events on
// the other. A pipe, unlike a debugging port, cannot be reached by any other
// process on the machine.
//
// Events are dispatched on the single reader goroutine in the order Chrome sent
// them. Handlers must not wait for replies; they use send, which only queues.
type conn struct {
	r io.Reader
	w io.WriteCloser

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]*pendingCall
	sessions map[string]*session
	err      error
	done     chan struct{}

	qmu   sync.Mutex
	queue [][]byte
	wake  chan struct{}

	// eof explains why Chrome closed the connection, if it can.
	eof func() error
	// limit bounds one message: maxMessage, or less in tests.
	limit int
}

func newConn(r io.Reader, w io.WriteCloser) *conn {
	c := &conn{
		r: r, w: w,
		pending:  map[int64]*pendingCall{},
		sessions: map[string]*session{},
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		limit:    maxMessage,
	}
	return c
}

func (c *conn) start() {
	go c.readLoop()
	go c.writeLoop()
}

// shutdown ends the connection with err, failing every waiting call.
func (c *conn) shutdown(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	pending := c.pending
	c.pending = map[int64]*pendingCall{}
	sessions := c.sessions
	c.sessions = map[string]*session{}
	close(c.done)
	c.mu.Unlock()
	for _, p := range pending {
		p.reply <- reply{err: err}
	}
	for _, s := range sessions {
		s.end(err)
	}
	c.w.Close()
}

func (c *conn) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// session is one flat-mode DevTools session: the browser itself (id "") or a
// target attached to it. It implements cdp.Session.
type session struct {
	c        *conn
	id       string
	targetID string
	kind     string   // page, iframe, worker, service_worker, shared_worker, browser
	parent   *session // the session that attached this one, nil for top-level targets
	url      string   // a service or shared worker's script: whose requests it makes
	// handle receives the session's events on the reader goroutine.
	handle func(s *session, method string, params jsontext.Value)

	mu   sync.Mutex
	subs map[string][]chan jsontext.Value
	err  error
	gone chan struct{}
}

func (c *conn) newSession(id, targetID, kind string, handle func(*session, string, jsontext.Value)) *session {
	s := &session{c: c, id: id, targetID: targetID, kind: kind, handle: handle, gone: make(chan struct{})}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		s.end(c.err)
		return s
	}
	c.sessions[id] = s
	c.mu.Unlock()
	return s
}

// detach forgets a session Chrome detached, failing its waiting calls.
func (c *conn) detach(id string, err error) {
	c.mu.Lock()
	s := c.sessions[id]
	delete(c.sessions, id)
	var failed []*pendingCall
	for callID, p := range c.pending {
		if p.session == id {
			failed = append(failed, p)
			delete(c.pending, callID)
		}
	}
	c.mu.Unlock()
	for _, p := range failed {
		p.reply <- reply{err: err}
	}
	if s != nil {
		s.end(err)
	}
}

func (s *session) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.err = err
	close(s.gone)
	for _, subs := range s.subs {
		for _, ch := range subs {
			close(ch)
		}
	}
	s.subs = nil
}

// ended reports whether the session has detached or the connection closed.
func (s *session) ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err != nil
}

// Call implements cdp.Session.
func (s *session) Call(ctx context.Context, method string, params, result any) error {
	raw, err := s.call(ctx, method, params)
	if err != nil {
		return err
	}
	if result == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, result, decodeOptions); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}

// Subscribe implements cdp.Session. A subscriber that falls more than 256
// events behind loses the newer ones rather than stalling every page.
func (s *session) Subscribe(method string) (<-chan jsontext.Value, func()) {
	ch := make(chan jsontext.Value, 256)
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	if s.subs == nil {
		s.subs = map[string][]chan jsontext.Value{}
	}
	s.subs[method] = append(s.subs[method], ch)
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			subs := s.subs[method]
			for i, sub := range subs {
				if sub == ch {
					s.subs[method] = append(subs[:i:i], subs[i+1:]...)
					close(ch)
					return
				}
			}
		})
	}
}

func (s *session) publish(method string, params jsontext.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs[method] {
		select {
		case ch <- params:
		default:
		}
	}
}

func (s *session) call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", method, context.Cause(ctx))
	}
	p, err := s.start(method, params)
	if err != nil {
		return nil, err
	}
	return s.await(ctx, p)
}

// start sends a command and registers for its reply without waiting.
func (s *session) start(method string, params any) (*pendingCall, error) {
	id := s.c.nextID.Add(1)
	data, err := encodeMessage(id, s.id, method, params)
	if err != nil {
		return nil, err
	}
	p := &pendingCall{id: id, method: method, session: s.id, reply: make(chan reply, 1)}
	s.c.mu.Lock()
	if s.c.err != nil {
		err := s.c.err
		s.c.mu.Unlock()
		return nil, err
	}
	s.mu.Lock()
	gone := s.err
	s.mu.Unlock()
	if gone != nil {
		s.c.mu.Unlock()
		return nil, gone
	}
	s.c.pending[id] = p
	s.c.mu.Unlock()
	s.c.enqueue(data)
	return p, nil
}

func (s *session) await(ctx context.Context, p *pendingCall) (jsontext.Value, error) {
	select {
	case r := <-p.reply:
		return r.result, r.err
	case <-ctx.Done():
		s.c.mu.Lock()
		delete(s.c.pending, p.id)
		s.c.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", p.method, context.Cause(ctx))
	}
}

// send queues a command without waiting for its reply, for event handlers
// on the reader goroutine. Chrome still runs commands in order per session.
func (s *session) send(method string, params any) {
	data, err := encodeMessage(s.c.nextID.Add(1), s.id, method, params)
	if err == nil {
		s.c.enqueue(data)
	}
}

func encodeMessage(id int64, sessionID, method string, params any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.WriteString(strconv.FormatInt(id, 10))
	buf.WriteString(`,"method":`)
	method2, _ := json.Marshal(method)
	buf.Write(method2)
	if sessionID != "" {
		buf.WriteString(`,"sessionId":`)
		sid, _ := json.Marshal(sessionID)
		buf.Write(sid)
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("%s: encode params: %w", method, err)
		}
		buf.WriteString(`,"params":`)
		buf.Write(raw)
	}
	buf.WriteString("}\x00")
	return buf.Bytes(), nil
}

func (c *conn) enqueue(data []byte) {
	c.qmu.Lock()
	c.queue = append(c.queue, data)
	c.qmu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *conn) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
		}
		c.qmu.Lock()
		batch := c.queue
		c.queue = nil
		c.qmu.Unlock()
		for _, data := range batch {
			if _, err := c.w.Write(data); err != nil {
				c.shutdown(fmt.Errorf("browser connection: %w", err))
				return
			}
		}
	}
}

var (
	errTooLarge = errors.New("message too large")
	idPrefix    = regexp.MustCompile(`^\{"id":(\d+)`)
)

func (c *conn) readLoop() {
	br := bufio.NewReaderSize(c.r, 1<<16)
	for {
		data, err := readMessage(br, c.limit)
		if errors.Is(err, errTooLarge) {
			// Fail the call it answers, if it is one, rather than leave it hanging.
			if m := idPrefix.FindSubmatch(data); m != nil {
				id, _ := strconv.ParseInt(string(m[1]), 10, 64)
				c.deliver(&message{ID: id, Error: &wireError{Message: fmt.Sprintf("reply larger than %d KiB", c.limit>>10)}})
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				err = errors.New("browser closed its connection")
				if c.eof != nil {
					err = c.eof()
				}
			}
			c.shutdown(err)
			return
		}
		var msg message
		if err := json.Unmarshal(data, &msg, decodeOptions); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.deliver(&msg)
			continue
		}
		if msg.Method == "" {
			continue
		}
		c.mu.Lock()
		s := c.sessions[msg.SessionID]
		c.mu.Unlock()
		if s == nil {
			continue
		}
		if s.handle != nil {
			s.handle(s, msg.Method, msg.Params)
		}
		s.publish(msg.Method, msg.Params)
	}
}

func (c *conn) deliver(msg *message) {
	c.mu.Lock()
	p := c.pending[msg.ID]
	delete(c.pending, msg.ID)
	c.mu.Unlock()
	if p == nil {
		return
	}
	if msg.Error != nil {
		text := msg.Error.Message
		if msg.Error.Data != "" {
			text += ": " + msg.Error.Data
		}
		p.reply <- reply{err: &ProtocolError{Method: p.method, Code: msg.Error.Code, Message: text}}
		return
	}
	p.reply <- reply{result: msg.Result}
}

// readMessage reads up to the next NUL. A message over limit is skipped; its
// first bytes are returned with errTooLarge so its id can still be found.
func readMessage(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	tooLarge := false
	for {
		chunk, err := br.ReadSlice(0)
		if !tooLarge {
			if len(buf)+len(chunk) > limit {
				tooLarge = true
				buf = append(buf, chunk[:min(len(chunk), 64)]...)
				buf = buf[:min(len(buf), 64)]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil && tooLarge:
			return buf, errTooLarge
		case err == nil:
			return buf[:len(buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}
