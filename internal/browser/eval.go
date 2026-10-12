package browser

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdpruntime "github.com/chromedp/cdproto/runtime"
)

// MaxEvalResult caps the text Eval returns.
const MaxEvalResult = 16 << 10

// EvalTimeout bounds an evaluation when ctx has no deadline.
const EvalTimeout = 10 * time.Second

// Eval runs JavaScript in the page's main frame, as the DevTools console
// does: an expression or statements whose last value is the result, with
// top-level await and the console's helpers ($, $$, $x). A promise the last
// value is, such as an async function's, is awaited too. The result comes
// back as indented JSON, with DOM nodes, functions, maps, sets, errors and
// cycles described rather than lost, cut at MaxEvalResult. A thrown
// exception is the error. A script that runs past ctx is terminated.
func (p *Page) Eval(ctx context.Context, expression string) (string, error) {
	if strings.TrimSpace(expression) == "" {
		return "", errors.New("nothing to evaluate")
	}
	if len(expression) > 1<<20 {
		return "", errors.New("script is longer than 1 MiB")
	}
	ctx, done, err := p.begin(ctx, EvalTimeout, false)
	if err != nil {
		return "", err
	}
	defer done()
	defer p.release()
	before := p.snapshotNav()
	params := cdpruntime.EvaluateParams{
		Expression:            expression,
		ObjectGroup:           objectGroup,
		IncludeCommandLineAPI: ptr(true),
		UserGesture:           ptr(true),
		AwaitPromise:          ptr(true),
		ReplMode:              ptr(true),
	}
	limit := EvalTimeout
	if d, ok := ctx.Deadline(); ok {
		limit = max(time.Until(d)-50*time.Millisecond, 10*time.Millisecond)
	}
	// V8 terminates a script that runs past the limit, so an endless loop
	// cannot wedge the page.
	params.Timeout = cdpruntime.TimeDelta(limit.Milliseconds())
	timedOut := func() error {
		return failure(ErrTimeout, "the script did not finish in %s", limit.Round(100*time.Millisecond))
	}
	start := time.Now()
	res, err := cdp.Call(ctx, p.root, cdpruntime.Evaluate, params)
	if err != nil {
		if time.Since(start) >= limit || ctx.Err() != nil && context.Cause(ctx) == ctx.Err() {
			p.root.send(cdpruntime.CommandTerminateExecution, nil)
			return "", timedOut()
		}
		return "", fail(ctx, err)
	}
	// The console's REPL awaits a top-level await, not a promise the last
	// expression yields, as an async function's call or a .then chain does;
	// that is awaited here, as Playwright's evaluate would.
	if r := res.Result; res.ExceptionDetails == nil && r != nil && r.Subtype == "promise" && r.ObjectID != "" {
		settled, err := cdp.Call(ctx, p.root, cdpruntime.AwaitPromise, cdpruntime.AwaitPromiseParams{PromiseObjectID: r.ObjectID})
		if err != nil {
			if ctx.Err() != nil && context.Cause(ctx) == ctx.Err() {
				return "", failure(ErrTimeout, "the promise the script returned did not settle in %s", limit.Round(100*time.Millisecond))
			}
			return "", fail(ctx, err)
		}
		res.Result, res.ExceptionDetails = settled.Result, settled.ExceptionDetails
	}
	if d := res.ExceptionDetails; d != nil {
		msg := exceptionText(d)
		if strings.Contains(msg, "Execution was terminated") {
			return "", timedOut()
		}
		if d.LineNumber > 0 || d.ColumnNumber > 0 {
			msg += fmt.Sprintf(" (at %d:%d)", d.LineNumber+1, d.ColumnNumber+1)
		}
		return "", errors.New(msg)
	}
	out, err := p.serialize(ctx, res.Result)
	if err != nil {
		return "", fail(ctx, err)
	}
	return out, fail(ctx, p.settle(ctx, before))
}

func (p *Page) serialize(ctx context.Context, o *cdpruntime.RemoteObject) (string, error) {
	var text string
	switch {
	case o == nil || o.Type == "undefined":
		return "undefined", nil
	case o.UnserializableValue != "":
		return string(o.UnserializableValue), nil
	case o.ObjectID == "":
		// A primitive: re-encoded, so text is not \u-escaped.
		var v any
		if json.Unmarshal(o.Value, &v, decodeOptions) != nil {
			return clip(string(o.Value), MaxEvalResult), nil
		}
		out, err := json.Marshal(v, jsontext.AllowInvalidUTF8(true))
		if err != nil {
			return clip(string(o.Value), MaxEvalResult), nil
		}
		text = string(out)
	default:
		raw, err := p.callValue(ctx, p.root, o.ObjectID, serializeScript)
		if err != nil {
			return "", err
		}
		if json.Unmarshal(raw, &text, decodeOptions) != nil {
			text = string(raw)
		}
	}
	if len(text) > MaxEvalResult {
		text = fmt.Sprintf("%s\n… (cut at %d of %d bytes)", clip(text, MaxEvalResult), MaxEvalResult, len(text))
	}
	return text, nil
}

// evalObject evaluates an expression in a session and returns the remote id
// of the object it yields, or "" for null.
func (p *Page) evalObject(ctx context.Context, s *session, expression string) (cdpruntime.RemoteObjectID, error) {
	res, err := cdp.Call(ctx, s, cdpruntime.Evaluate, cdpruntime.EvaluateParams{Expression: expression, ObjectGroup: objectGroup})
	if err != nil {
		return "", err
	}
	if res.ExceptionDetails != nil {
		return "", errors.New(exceptionText(res.ExceptionDetails))
	}
	if res.Result == nil {
		return "", nil
	}
	return res.Result.ObjectID, nil
}

// serializeScript renders a value as indented JSON, describing what JSON
// cannot hold, and cuts it at max characters.
const serializeScript = `function() {
  const describe = ` + describeScript + `;
  const seen = new WeakSet();
  let budget = 5000;
  const walk = (v, depth) => {
    if (--budget < 0) return '…';
    if (v === undefined) return '[undefined]';
    if (v === null || typeof v === 'boolean' || typeof v === 'number' && isFinite(v)) return v;
    if (typeof v === 'number' || typeof v === 'bigint') return String(v) + (typeof v === 'bigint' ? 'n' : '');
    if (typeof v === 'string') return v.length > 2000 ? v.slice(0, 2000) + '…' : v;
    if (typeof v === 'symbol') return v.toString();
    if (typeof v === 'function') return '[function ' + (v.name || 'anonymous') + ']';
    if (typeof Node !== 'undefined' && v instanceof Node) return v.nodeType === 1 ? describe(v) : '[' + v.nodeName + ']';
    if (typeof Window !== 'undefined' && v instanceof Window) return '[Window ' + (v === window ? location.href : '') + ']';
    if (seen.has(v)) return '[circular]';
    if (depth > 6) return Array.isArray(v) ? '[Array(' + v.length + ')]' : '[Object]';
    seen.add(v);
    if (v instanceof Error) return v.name + ': ' + v.message;
    if (v instanceof Date) return isNaN(v) ? 'Invalid Date' : v.toISOString();
    if (v instanceof RegExp) return v.toString();
    if (v instanceof Promise) return '[Promise]';
    if (v instanceof Map) {
      const o = {};
      let i = 0;
      for (const [k, x] of v) { if (i++ >= 100) { o['…'] = (v.size - 100) + ' more'; break; } o[typeof k === 'string' ? k : String(k)] = walk(x, depth + 1); }
      return {['Map(' + v.size + ')']: o};
    }
    if (v instanceof Set) return {['Set(' + v.size + ')']: walk([...v].slice(0, 100), depth + 1)};
    if (ArrayBuffer.isView(v) && !(v instanceof DataView)) v = Array.from(v.slice(0, 100));
    if (Array.isArray(v) || (typeof NodeList !== 'undefined' && v instanceof NodeList) || (typeof HTMLCollection !== 'undefined' && v instanceof HTMLCollection)) {
      const a = Array.from(v).slice(0, 200).map(x => walk(x, depth + 1));
      if (v.length > 200) a.push('… ' + (v.length - 200) + ' more');
      return a;
    }
    const o = {};
    let i = 0;
    for (const k in v) {
      if (i++ >= 200) { o['…'] = 'more keys'; break; }
      let x;
      try { x = v[k]; } catch (e) { x = '[throws ' + e + ']'; }
      o[k] = walk(x, depth + 1);
    }
    return o;
  };
  const text = JSON.stringify(walk(this, 0), null, 2);
  return text === undefined ? String(this) : text.length > 1e6 ? text.slice(0, 1e6) : text;
}`

// mobileUserAgent is a phone's Chrome of the same major version.
func (b *Browser) mobileUserAgent() string {
	return "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + strconv.Itoa(b.major()) + ".0.0.0 Mobile Safari/537.36"
}

// major is Chrome's major version.
func (b *Browser) major() int {
	if _, version, ok := strings.Cut(b.product, "/"); ok {
		major, _, _ := strings.Cut(version, ".")
		if n, err := strconv.Atoi(major); err == nil {
			return n
		}
	}
	return 151
}

// checksLocalNetwork reports whether Chrome enforces Local Network Access, so
// that a page from the public internet, which is what Chrome takes the
// proxy's for, cannot reach loopback.
func (b *Browser) checksLocalNetwork() bool { return b.major() >= 142 }
