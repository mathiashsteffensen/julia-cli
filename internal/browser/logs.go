package browser

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	cdpruntime "github.com/chromedp/cdproto/runtime"
)

// LogEntry is one thing the page reported: a console message, an uncaught
// exception, a failed or blocked request, or an HTTP error response.
type LogEntry struct {
	// Kind is console, exception, network, blocked or browser (Chrome's own
	// warnings, such as a Content Security Policy violation).
	Kind string
	// Level is error, warning, info, log or debug.
	Level string
	Text  string
	// Source is where it came from, as url:line:column, when known.
	Source string
	Time   time.Time
}

func (e LogEntry) String() string {
	s := e.Level + " " + e.Text
	if e.Source != "" {
		s += " (" + e.Source + ")"
	}
	return s
}

// Logs is what a page reported since the last read.
type Logs struct {
	Entries []LogEntry
	// Dropped counts entries lost because more arrived than are kept.
	Dropped int
}

const (
	maxLogEntries = 200
	maxLogText    = 2000
)

// logBuffer keeps the newest entries since the last read.
type logBuffer struct {
	entries []LogEntry
	dropped int
}

func (l *logBuffer) add(e LogEntry) {
	if len(l.entries) >= maxLogEntries {
		copy(l.entries, l.entries[1:])
		l.entries = l.entries[:len(l.entries)-1]
		l.dropped++
	}
	l.entries = append(l.entries, e)
}

// Logs returns the console messages, uncaught exceptions, failed and blocked
// requests and HTTP error responses since the last call, oldest first, and
// forgets them. At most 200 are kept between reads.
func (p *Page) Logs() Logs {
	p.mu.Lock()
	defer p.mu.Unlock()
	logs := Logs{Entries: p.logs.entries, Dropped: p.logs.dropped}
	p.logs = logBuffer{}
	return logs
}

func (p *Page) log(e LogEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Text = clip(strings.TrimSpace(e.Text), maxLogText)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs.add(e)
}

// onConsole records console messages, exceptions and Chrome's own warnings.
func (p *Page) onConsole(s *session, method string, params jsontext.Value) {
	switch method {
	case "Runtime.consoleAPICalled":
		var ev cdpruntime.EventConsoleAPICalled
		if !decode(params, &ev) {
			return
		}
		level := "log"
		switch ev.Type {
		case "error", "assert":
			level = "error"
		case "warning":
			level = "warning"
		case "info":
			level = "info"
		case "debug":
			level = "debug"
		case "endGroup", "clear", "profile", "profileEnd":
			return
		}
		text := consoleText(ev.Args)
		if ev.Type == "assert" {
			text = "Assertion failed: " + text
		}
		p.log(LogEntry{Kind: "console", Level: level, Text: text, Source: p.stackSource(ev.StackTrace)})
	case "Runtime.exceptionThrown":
		var ev cdpruntime.EventExceptionThrown
		if !decode(params, &ev) || ev.ExceptionDetails == nil {
			return
		}
		d := ev.ExceptionDetails
		text := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			text = "Uncaught " + d.Exception.Description
		} else if d.Exception != nil && len(d.Exception.Value) > 0 {
			text = "Uncaught " + remoteText(d.Exception)
		}
		source := p.stackSource(d.StackTrace)
		if source == "" && d.URL != "" {
			source = fmt.Sprintf("%s:%d:%d", p.short(d.URL), d.LineNumber+1, d.ColumnNumber+1)
		}
		p.log(LogEntry{Kind: "exception", Level: "error", Text: text, Source: source})
	case "Log.entryAdded":
		var ev struct {
			Entry struct {
				Source     string `json:"source"`
				Level      string `json:"level"`
				Text       string `json:"text"`
				URL        string `json:"url"`
				LineNumber int64  `json:"lineNumber"`
			} `json:"entry"`
		}
		if !decode(params, &ev) {
			return
		}
		e := ev.Entry
		// Network failures are reported from the network events, with the
		// request they belong to; verbose and info entries are noise.
		if e.Source == "network" || (e.Level != "error" && e.Level != "warning") {
			return
		}
		source := ""
		if e.URL != "" {
			source = p.short(e.URL)
			if e.LineNumber > 0 {
				source += ":" + strconv.FormatInt(e.LineNumber+1, 10)
			}
		}
		p.log(LogEntry{Kind: "browser", Level: e.Level, Text: e.Text, Source: source})
	}
}

func (p *Page) stackSource(st *cdpruntime.StackTrace) string {
	if st == nil || len(st.CallFrames) == 0 {
		return ""
	}
	f := st.CallFrames[0]
	if f.URL == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", p.short(f.URL), f.LineNumber+1, f.ColumnNumber+1)
}

// short drops the scheme and host of a URL on the page's own origin.
func (p *Page) short(raw string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shortLocked(raw)
}

// shortLocked is short, called with p.mu held.
func (p *Page) shortLocked(raw string) string {
	base := p.url
	u, err := url.Parse(raw)
	b, err2 := url.Parse(base)
	if err != nil || err2 != nil || u.Host == "" || u.Scheme != b.Scheme || u.Host != b.Host {
		return clip(raw, 200)
	}
	return clip(u.RequestURI(), 200)
}

// consoleText formats console arguments as the console shows them, applying
// a leading format string's %s, %d, %i, %f, %o, %O and %c.
func consoleText(args []*cdpruntime.RemoteObject) string {
	var parts []string
	if len(args) > 0 && args[0].Type == "string" {
		format := remoteText(args[0])
		rest := args[1:]
		if strings.Contains(format, "%") {
			var b strings.Builder
			for i := 0; i < len(format); i++ {
				if format[i] != '%' || i+1 == len(format) {
					b.WriteByte(format[i])
					continue
				}
				verb := format[i+1]
				switch {
				case verb == '%':
					b.WriteByte('%')
				case strings.IndexByte("sdifoOc", verb) >= 0 && len(rest) > 0:
					if verb != 'c' { // %c styles the text; there is no style here
						b.WriteString(remoteText(rest[0]))
					}
					rest = rest[1:]
				default:
					b.WriteByte('%')
					b.WriteByte(verb)
				}
				i++
			}
			format = b.String()
		}
		parts = append(parts, format)
		args = rest
	}
	for _, a := range args {
		parts = append(parts, remoteText(a))
	}
	return strings.Join(parts, " ")
}

// remoteText renders a value the way the console prints it, briefly.
func remoteText(o *cdpruntime.RemoteObject) string {
	if o == nil {
		return ""
	}
	switch {
	case o.Type == "string":
		var s string
		if json.Unmarshal(o.Value, &s, decodeOptions) == nil {
			return s
		}
	case o.UnserializableValue != "":
		return string(o.UnserializableValue)
	case o.Type == "undefined":
		return "undefined"
	case o.Preview != nil:
		return previewText(o.Preview)
	case len(o.Value) > 0:
		return string(o.Value)
	}
	if o.Description != "" {
		return o.Description
	}
	return string(o.Type)
}

func previewText(pv *cdpruntime.ObjectPreview) string {
	var items []string
	array := pv.Subtype == "array" || pv.Subtype == "typedarray"
	for _, prop := range pv.Properties {
		value := prop.Value
		if prop.Type == "string" {
			value = strconv.Quote(value)
		} else if prop.Type == "object" && value == "" {
			value = "{…}"
		}
		if array {
			items = append(items, value)
		} else {
			items = append(items, prop.Name+": "+value)
		}
	}
	if pv.Overflow {
		items = append(items, "…")
	}
	switch {
	case array:
		return "[" + strings.Join(items, ", ") + "]"
	case pv.Subtype == "error" || pv.Subtype == "node" || pv.Subtype == "regexp" || pv.Subtype == "date" || pv.Type == "function":
		return pv.Description
	case len(items) == 0 && pv.Description != "Object":
		return pv.Description
	}
	prefix := ""
	if pv.Description != "" && pv.Description != "Object" {
		prefix = pv.Description + " "
	}
	return prefix + "{" + strings.Join(items, ", ") + "}"
}

// onNetwork tracks requests in flight, for networkidle, and records failed
// requests and HTTP error responses.
func (p *Page) onNetwork(s *session, method string, params jsontext.Value) {
	var ev struct {
		RequestID string `json:"requestId"`
		LoaderID  string `json:"loaderId"`
		Type      string `json:"type"`
		Request   *struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"request"`
		Response *struct {
			URL        string `json:"url"`
			Status     int    `json:"status"`
			StatusText string `json:"statusText"`
		} `json:"response"`
		ErrorText     string `json:"errorText"`
		Canceled      bool   `json:"canceled"`
		BlockedReason string `json:"blockedReason"`
		CORSError     *struct {
			CORSError string `json:"corsError"`
		} `json:"corsErrorStatus"`
	}
	if !decode(params, &ev) {
		return
	}
	// Keyed by request id alone: an iframe's navigation starts in its
	// parent's session and finishes in its own.
	key := ev.RequestID
	p.mu.Lock()
	defer p.mu.Unlock()
	switch method {
	case "Network.requestWillBeSent":
		if ev.Request == nil || ev.Type == "EventSource" || strings.HasPrefix(ev.Request.URL, "data:") {
			return // a stream never finishes; a data: URL never waits
		}
		if r, ok := p.inflight[key]; ok || len(p.inflight) < maxInflight {
			if !ok { // a redirect keeps its place
				p.requests++
				r.seq = p.requests
			}
			p.inflight[key] = inflight{url: ev.Request.URL, method: ev.Request.Method, kind: strings.ToLower(ev.Type), loader: ev.LoaderID, session: s.id, seq: r.seq}
			p.netQuiet = time.Now()
		}
	case "Network.responseReceived":
		if r := ev.Response; r != nil && r.Status >= 400 {
			req := p.inflight[key]
			if req.method == "" {
				req.method = "GET"
			}
			status := strconv.Itoa(r.Status)
			if r.StatusText != "" {
				status += " " + r.StatusText
			}
			p.logs.add(LogEntry{Kind: "network", Level: "error", Time: time.Now(),
				Text: fmt.Sprintf("%s %s %s (%s)", status, req.method, clip(r.URL, 300), strings.ToLower(ev.Type))})
		}
	case "Network.loadingFinished", "Network.loadingFailed":
		req, ok := p.inflight[key]
		delete(p.inflight, key)
		if ok {
			p.netQuiet = time.Now()
		}
		p.signalLocked()
		// A request the policy failed is logged as blocked already; Chrome
		// reports some of those as blocked by the "inspector", which is us.
		if method == "Network.loadingFinished" || ev.Canceled || ev.ErrorText == "net::ERR_BLOCKED_BY_CLIENT" || ev.ErrorText == "net::ERR_ABORTED" || ev.BlockedReason == "inspector" {
			return
		}
		why := ev.ErrorText
		if ev.CORSError != nil && ev.CORSError.CORSError != "" {
			why = "CORS error: " + ev.CORSError.CORSError
		} else if ev.BlockedReason != "" {
			why = "blocked by the browser: " + ev.BlockedReason
		}
		target := req.url
		if target == "" {
			target = "a request"
		}
		p.logs.add(LogEntry{Kind: "network", Level: "error", Time: time.Now(),
			Text: fmt.Sprintf("failed %s %s (%s): %s", cmpOr(req.method, "GET"), clip(target, 300), cmpOr(req.kind, strings.ToLower(ev.Type)), why)})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
