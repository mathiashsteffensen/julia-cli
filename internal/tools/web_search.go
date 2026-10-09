package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"jarvis/internal/core"
)

const (
	webSearchEndpoint = "https://html.duckduckgo.com/html/"
	maxWebSearchBody  = 1 << 20
	maxWebSearchText  = 4 << 10
)

func webSearchDefinition() core.ToolDefinition {
	return core.ToolDefinition{
		Name:        "WebSearch",
		Description: "Search the public web through DuckDuckGo without an API key or configuration. Sends only the query, after automatic/human review; unavailable in read-only mode. Returns at most 4 KiB of titles, source URLs, and short unverified snippets (default 3 results, maximum 5), never page HTML. Do not send secrets or private project content. Best effort: blocking, rate limits, and changed markup return errors, not empty results. No destination pages are fetched.",
		Parameters: schema(map[string]any{
			"query": map[string]any{"type": "string", "description": "Public search query, nonempty and at most 512 UTF-8 bytes; no control characters", "minLength": 1, "maxLength": 512},
			"limit": map[string]any{"type": "integer", "description": "Number of results, default 3; increase only when useful", "minimum": 1, "maximum": 5},
		}, "query"),
	}
}

// This client has one fixed search destination, no cookie jar or inherited proxy,
// and never follows redirects. Search result URLs are returned, never requested.
// HTTP runs in the harness, not in Bash's OS sandbox.
func newWebSearchClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConns:          2,
			MaxConnsPerHost:       2,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (e *Executor) webSearch(ctx context.Context, call core.ToolCall) (string, error) {
	if e.config.Mode == "read-only" {
		return "", errors.New("read-only mode prohibits WebSearch's outbound queries")
	}
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	args.Limit = 3
	if err := decode(call.Arguments, &args); err != nil {
		return "", err
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" || len(args.Query) > 512 || strings.IndexFunc(args.Query, webControl) >= 0 {
		return "", errors.New("query must be nonempty, at most 512 UTF-8 bytes, and contain no control characters")
	}
	if args.Limit < 1 || args.Limit > 5 {
		return "", errors.New("limit must be between 1 and 5")
	}
	approval := core.Approval{
		Call: call, Task: e.config.Task, Directory: e.directory,
		Network: true, ExecutionMode: "web-search",
		Reason: "Send this query to DuckDuckGo's public search service? Only the query is sent; no workspace files or credentials are uploaded, and no destination pages are fetched. Search runs through a fixed HTTPS client, not the Bash sandbox.",
	}
	if err := e.authorizeRequest(ctx, approval, true); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if e.webClient == nil {
		e.webClient = newWebSearchClient()
	}
	query := url.Values{"q": {args.Query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webSearchEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "JARVIS/1.0 (public web search)")
	resp, err := e.webClient.Do(req)
	if err != nil {
		// Do not echo the query from net/http's URL-bearing errors into context.
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return "", fmt.Errorf("DuckDuckGo search request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return "", fmt.Errorf("DuckDuckGo search unavailable (HTTP %d; possible bot challenge or rate limit). No retry was attempted", resp.StatusCode)
		}
		return "", fmt.Errorf("DuckDuckGo search returned HTTP %d; redirects are not followed", resp.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (contentType != "text/html" && contentType != "application/xhtml+xml") {
		return "", errors.New("DuckDuckGo returned an unexpected content type, not search HTML")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWebSearchBody+1))
	if err != nil {
		return "", fmt.Errorf("read DuckDuckGo search response: %w", err)
	}
	if len(body) > maxWebSearchBody {
		return "", errors.New("DuckDuckGo search response exceeded 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	results, err := parseWebSearch(body)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return formatWebSearch(results, args.Limit), nil
}

type webSearchResult struct {
	title, url, snippet string
}

func parseWebSearch(body []byte) ([]webSearchResult, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("could not parse DuckDuckGo search HTML")
	}
	var results []webSearchResult
	candidates := 0
	seen := make(map[string]bool)
	var empty, challenge bool
	walkWebHTML(doc, func(n *html.Node) {
		if webClass(n, "no-results") || webClass(n, "no-results__message") {
			empty = true
		}
		if webAttr(n, "id") == "challenge-form" || webClass(n, "anomaly-modal") || webClass(n, "anomaly-modal__modal") {
			challenge = true
		}
		if !webClass(n, "result") || webClass(n, "result--ad") || len(results) == 5 || candidates == 50 {
			return
		}
		// Bound repeated subtree scans even if malformed HTML nests result blocks.
		candidates++
		var title, href, snippet string
		walkWebHTML(n, func(child *html.Node) {
			if child.Type == html.ElementNode && child.Data == "a" && webClass(child, "result__a") && href == "" {
				title, href = webNodeText(child), webAttr(child, "href")
			}
			if webClass(child, "result__snippet") && snippet == "" {
				snippet = webNodeText(child)
			}
		})
		target := webResultURL(href)
		title = compactWebText(title, 160)
		if title == "" || target == "" || seen[target] {
			return
		}
		seen[target] = true
		results = append(results, webSearchResult{title, target, compactWebText(snippet, 240)})
	})
	if challenge {
		return nil, errors.New("DuckDuckGo presented a bot challenge; search is unavailable. No retry was attempted")
	}
	if len(results) == 0 && !empty {
		return nil, errors.New("DuckDuckGo returned no recognizable search results; search may be blocked or its page format may have changed")
	}
	return results, nil
}

func walkWebHTML(n *html.Node, visit func(*html.Node)) {
	// Iterate rather than recurse: remote HTML may be arbitrarily deeply nested.
	for current := n; current != nil; {
		visit(current)
		if current.FirstChild != nil && current.Data != "script" && current.Data != "style" && current.Data != "template" {
			current = current.FirstChild
			continue
		}
		for current != n && current.NextSibling == nil {
			current = current.Parent
		}
		if current == n {
			return
		}
		current = current.NextSibling
	}
}

func webAttr(n *html.Node, name string) string {
	if n.Type == html.ElementNode {
		for _, attr := range n.Attr {
			if attr.Key == name {
				return attr.Val
			}
		}
	}
	return ""
}

func webClass(n *html.Node, name string) bool {
	for _, class := range strings.Fields(webAttr(n, "class")) {
		if class == name {
			return true
		}
	}
	return false
}

func webNodeText(n *html.Node) string {
	var text strings.Builder
	walkWebHTML(n, func(child *html.Node) {
		if child.Type == html.TextNode {
			text.WriteString(child.Data)
		} else if child.Type == html.ElementNode && (child.Data == "br" || child.Data == "p" || child.Data == "div") {
			text.WriteByte(' ')
		}
	})
	return text.String()
}

func webResultURL(raw string) string {
	if len(raw) > 8192 || strings.IndexFunc(raw, webControl) >= 0 {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if u.Path == "/l/" && (u.Host == "" || host == "duckduckgo.com" || host == "www.duckduckgo.com" || host == "html.duckduckgo.com") {
		u, err = url.Parse(u.Query().Get("uddg"))
		if err != nil {
			return ""
		}
	}
	if u.Scheme == "" && u.Host != "" {
		u.Scheme = "https"
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	result := u.String()
	if len(result) > 1024 || strings.IndexFunc(result, webControl) >= 0 {
		return ""
	}
	return result
}

func webControl(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

func compactWebText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if webControl(r) {
			return -1
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	end := limit - len("…")
	for !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

func formatWebSearch(results []webSearchResult, limit int) string {
	if len(results) == 0 {
		return "DuckDuckGo found no results for this query."
	}
	var out strings.Builder
	out.WriteString("DuckDuckGo search — unverified web snippets, not page contents:\n")
	const omitted = "\nAdditional results omitted to keep this response within 4 KiB.\n"
	for i, result := range results[:min(len(results), limit)] {
		entry := "\n" + strconv.Itoa(i+1) + ". " + result.title + "\n" + result.url + "\n"
		if result.snippet != "" {
			entry += result.snippet + "\n"
		}
		if out.Len()+len(entry)+len(omitted) > maxWebSearchText {
			out.WriteString(omitted)
			break
		}
		out.WriteString(entry)
	}
	return out.String()
}
