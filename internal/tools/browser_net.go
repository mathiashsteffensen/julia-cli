package tools

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"julia/internal/browser"

	"golang.org/x/sys/unix"
)

// The Browser's network policy. Chrome runs outside the OS sandbox (it
// sandboxes its own renderers), so this is where its reach is decided, at
// two layers that both default to refusal:
//
//   - every request a page, frame or worker makes is paused and judged by
//     policy before it is sent: documents, subresources, fetch and XHR,
//     workers and their requests, redirects and popups alike;
//   - every connection beyond loopback goes through a proxy whose dial opens
//     only approved hosts, which also stops what the first layer cannot see
//     (WebSockets, preconnects). Only Chrome may use it, and Chrome counts
//     what comes through it as the public internet, so its own Local Network
//     Access checks keep a document from there off loopback, WebSockets
//     included.
//
// What a page may reach without asking: the loopback servers the agent's
// own terminals listen on (the dev server a loopback Terminal runs), measured
// before each Browser action, files in the workspace and $JULIA_SCRATCH
// (served by JULIA itself, so Chrome never opens a path), and data: and
// blob: URLs a page makes. Other loopback servers are the machine's own
// services, which often trust any local client, and need approval like any
// origin. Every other origin waits for an approval that
// names it: navigate with network=true, or the allow action, reviewed once
// per origin in auto mode and asked of the user in ask mode, after which
// that origin's requests load for the rest of the turn.
//
// The threats are exfiltration and injection. A page, or a script the agent
// evaluates, can send whatever it reads to any origin it can reach, so a
// blocked origin stays blocked for subresources too: a CDN or font host
// needs the same one-time approval as a page, judged with the page that
// wants it in view. An approved origin is trusted as a destination: what the
// agent types, uploads or evaluates on a page outside loopback is reviewed
// again in auto mode, but further navigation within an approved origin is
// not. Page text is untrusted input, which the tool description and the
// system prompt say. A document from outside loopback, a frame's included,
// may not reach loopback servers except by top-level GET navigation: an
// approved site, or a widget a dev app embeds, must not get one fetch away
// from a dev server's API (Vite once served any file to any origin). DNS can be rebound, so an approved name must resolve to a public
// address; an IP address is dialed as approved.

// browserOrigin is the origin a URL belongs to for approval: its scheme
// (WebSockets counted as http and https), host and port, with default ports
// left out. ok is false for URLs without a network origin.
func browserOrigin(u *url.URL) (string, bool) {
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	case "http", "https":
	default:
		return "", false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", false
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(scheme == "http" && port == "80" || scheme == "https" && port == "443") {
		host += ":" + port
	}
	return scheme + "://" + host, true
}

// parseOrigin reads an origin as the allow action takes it: a URL whose
// path, if any, is ignored.
func parseOrigin(raw string) (string, *url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", nil, fmt.Errorf("%q is not an origin such as https://example.com", raw)
	}
	origin, ok := browserOrigin(u)
	if !ok {
		return "", nil, fmt.Errorf("%q is not an http or https origin", raw)
	}
	return origin, u, nil
}

// loopbackHost reports whether Chrome connects to host directly, bypassing
// the proxy: localhost, *.localhost and loopback addresses.
func loopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// external reports whether a URL is on the network beyond loopback: an
// http(s) or ws(s) URL whose host is not a loopback host, or a blob: URL a
// page there made, which carries its origin.
func external(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Scheme, "blob") {
		if u, err = url.Parse(u.Opaque); err != nil {
			return false
		}
	}
	_, ok := browserOrigin(u)
	return ok && !loopbackHost(u.Hostname())
}

// approve records origins as approved for the rest of the executor's life.
func (st *browserState) approve(origins ...string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, o := range origins {
		st.origins[o] = true
	}
}

func (st *browserState) approved(origin string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.origins[origin]
}

// approvedOrigins lists the approved origins, sorted.
func (st *browserState) approvedOrigins() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var list []string
	for o := range st.origins {
		list = append(list, o)
	}
	slices.Sort(list)
	return list
}

// approvedAddress reports whether host:port belongs to an approved origin.
// The proxy sees only that much: a CONNECT for https and WebSockets, the
// host of a plain http request.
func (st *browserState) approvedAddress(host, port string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	st.mu.Lock()
	defer st.mu.Unlock()
	for o := range st.origins {
		u, _ := url.Parse(o)
		p := u.Port()
		if p == "" {
			p = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		if strings.Trim(u.Hostname(), "[]") == host && p == port {
			return true
		}
	}
	return false
}

// decide is the request policy. It runs on Chrome's connection while the
// request waits, so it does no I/O: files are confined by path here and
// opened, without following symlinks, when they are served.
func (st *browserState) decide(r browser.Request) browser.Decision {
	allow := browser.Decision{Allow: true}
	from := external(r.From)
	switch r.URL.Scheme {
	case "data", "blob":
		return allow
	case "file":
		if from {
			return browser.Decision{Reason: "a page from outside loopback may not open local files"}
		}
		if _, _, err := st.local(r.URL.Path); err != nil {
			return browser.Decision{Reason: err.Error()}
		}
		return allow
	}
	origin, ok := browserOrigin(r.URL)
	if !ok {
		return browser.Decision{Reason: r.URL.Scheme + ": URLs are not available"}
	}
	host := r.URL.Hostname()
	if loopbackHost(host) {
		if from && (!r.MainFrame || r.Method != "GET") {
			return browser.Decision{Reason: "a page from " + originOf(r.From) + " may not reach local servers"}
		}
		if st.terminalServer(r.URL) || st.approved(origin) {
			return allow
		}
		return browser.Decision{Reason: origin + " is not a server your terminals started and is not approved for this turn; Browser allow asks for it"}
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && ip.IsUnspecified() {
		return browser.Decision{Reason: host + " is not an address to browse; use localhost"}
	}
	if st.approved(origin) {
		return allow
	}
	return browser.Decision{Reason: origin + " is not approved for this turn; Browser allow asks for it"}
}

// measurePorts records the loopback ports the executor's terminals listen on.
// When they cannot be measured, none are: every loopback server then needs
// approval.
func (st *browserState) measurePorts(ports []int, err error) {
	if err != nil {
		ports = nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ports = append(ports, st.testPorts...)
}

// terminalServer reports whether u is a loopback URL whose port one of the
// executor's terminals listens on: a server the agent started itself.
func (st *browserState) terminalServer(u *url.URL) bool {
	if !loopbackHost(u.Hostname()) {
		return false
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "ws": "80", "https": "443", "wss": "443"}[strings.ToLower(u.Scheme)]
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Contains(st.ports, n)
}

// originOf names the origin of a URL that external accepted.
func originOf(raw string) string {
	u, _ := url.Parse(raw)
	if strings.EqualFold(u.Scheme, "blob") {
		u, _ = url.Parse(u.Opaque)
	}
	origin, _ := browserOrigin(u)
	return origin
}

// dial is the proxy's way out: it opens a connection only to an approved
// origin's host and port, and only to a public address unless the origin
// names the address itself, so an approved name cannot be rebound to a
// private one.
func (st *browserState) dial(ctx context.Context, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if !st.approvedAddress(host, port) {
		return nil, fmt.Errorf("%s is not approved for this turn", address)
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range resolved {
			if !publicAddress(ip) {
				return nil, fmt.Errorf("%s resolves to %s, a private address; approve the address itself if that is meant", host, ip.Unmap())
			}
		}
		addrs = resolved
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	err = fmt.Errorf("%s has no address", host)
	for _, ip := range addrs {
		var conn net.Conn
		if conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port)); err == nil {
			return conn, nil
		}
	}
	return nil, err
}

// connect opens an approved address: through dial, or in tests through
// testDial, which stands in for the internet once approval has been checked.
func (st *browserState) connect(ctx context.Context, address string) (net.Conn, error) {
	if st.testDial == nil {
		return st.dial(ctx, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if !st.approvedAddress(host, port) {
		return nil, fmt.Errorf("%s is not approved for this turn", address)
	}
	return st.testDial(ctx, address)
}

// specialUse are the global-unicast ranges IANA reserves for special use
// (RFC 6890 and its successors), which a network may route to its own hosts.
var specialUse = func() []netip.Prefix {
	var list []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.175.48.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "3fff::/20", "5f00::/16", "fec0::/10",
	} {
		list = append(list, netip.MustParsePrefix(p))
	}
	return list
}()

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range specialUse {
		if p.Contains(ip) {
			return false
		}
	}
	// Addresses that carry an IPv4 address are as public as that address.
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return publicAddress(netip.AddrFrom4([4]byte(b[12:])))
	case sixToFour.Contains(ip):
		return publicAddress(netip.AddrFrom4([4]byte(b[2:6])))
	}
	return true
}

// local confines a file: URL's path to the workspace or the scratch
// directory, by its spelling: no protected credential or metadata path, and
// nothing outside. root is the directory it is inside.
func (st *browserState) local(path string) (root, rel string, err error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", "", errors.New("not an absolute file path")
	}
	clean := filepath.Clean(path)
	for _, base := range st.roots {
		r, err := filepath.Rel(base.spelling, clean)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}
		if protected(r) {
			return "", "", errors.New("protected credential or metadata path is forbidden")
		}
		return base.dir, r, nil
	}
	return "", "", errors.New("outside the workspace and " + scratchVariable)
}

// fileRoot is a directory file: URLs may reach, under one of its spellings:
// a scratch directory under /var is also /private/var on macOS.
type fileRoot struct{ dir, spelling string }

// maxServedFile bounds a file served to the browser, and maxUpload the files
// given to one file input.
const (
	maxServedFile = 32 << 20
	maxUpload     = 32 << 20
)

// maxDownload bounds one download, and maxDownloads what the downloads of a
// session may keep in all.
const (
	maxDownload  = 256 << 20
	maxDownloads = 1 << 30
)

// serveFile reads a file: URL's file for the browser, refusing a symlink at
// any component, as ViewImage does, so a link swapped in after the policy
// looked still cannot lead outside.
func (st *browserState) serveFile(path string) ([]byte, string, error) {
	root, rel, err := st.local(path)
	if err != nil {
		return nil, "", err
	}
	var f *os.File
	if rel == "." {
		f, err = os.Open(root)
	} else {
		f, err = openBeneath(root, rel)
	}
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) {
			err = fs.ErrNotExist
		}
		return nil, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if info.IsDir() {
		return directoryListing(f, path), "text/html; charset=utf-8", nil
	}
	if !info.Mode().IsRegular() {
		return nil, "", errors.New("not a regular file")
	}
	if info.Size() > maxServedFile {
		return nil, "", fmt.Errorf("larger than the %d MiB a file may be", maxServedFile>>20)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxServedFile+1))
	if err != nil {
		return nil, "", err
	}
	media := mime.TypeByExtension(filepath.Ext(path))
	if media == "" {
		media = http.DetectContentType(data)
	}
	return data, media, nil
}

// directoryListing is the page a file: URL to a directory shows: its
// entries, without symlinks or protected names.
func directoryListing(dir *os.File, path string) []byte {
	entries, _ := dir.ReadDir(2001)
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html><meta charset=utf-8><title>%s</title><h1>%s</h1><ul>", html.EscapeString(filepath.Base(path)), html.EscapeString(path))
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for i, e := range entries {
		if i == 2000 {
			b.WriteString("<li>…</li>")
			break
		}
		if e.Type()&os.ModeSymlink != 0 || protected(e.Name()) {
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		href := (&url.URL{Scheme: "file", Path: filepath.Join(path, e.Name())}).String()
		if e.IsDir() {
			href += "/"
		}
		fmt.Fprintf(&b, `<li><a href="%s">%s</a></li>`, html.EscapeString(href), html.EscapeString(name))
	}
	b.WriteString("</ul>")
	return []byte(b.String())
}

// uploadCopies copies the files a file input is given into a private
// directory, so the page reads exactly what was approved: the sandbox can
// change the workspace and scratch directory later, but not this copy.
func (e *Executor) uploadCopies(paths []string) ([]string, error) {
	st := e.browsing
	if st.uploads == "" {
		dir, err := os.MkdirTemp("", "julia-uploads-")
		if err != nil {
			return nil, err
		}
		st.uploads = dir
	}
	var copies []string
	total := int64(0)
	for _, path := range paths {
		f, name, err := e.openImage(path) // the same confinement as ViewImage
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("%s is not a regular file", name)
		}
		if err == nil && total+info.Size() > maxUpload {
			err = fmt.Errorf("the files given at once may total at most %d MiB", maxUpload>>20)
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		total += info.Size()
		dir, err := os.MkdirTemp(st.uploads, "file-")
		if err != nil {
			f.Close()
			return nil, err
		}
		copy := filepath.Join(dir, filepath.Base(name))
		out, err := os.OpenFile(copy, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = io.Copy(out, io.LimitReader(f, maxUpload))
			err = errors.Join(err, out.Close())
		}
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", name, err)
		}
		copies = append(copies, copy)
	}
	return copies, nil
}

// keepDownload copies a finished download from the private directory Chrome
// saves into to $JULIA_SCRATCH/downloads, under the name the page
// suggested. The sandbox can write the scratch directory, so nothing in it
// is followed: Chrome runs outside the sandbox, and a symlink planted for
// downloads, or for the scratch directory itself, would otherwise have it
// write wherever the user can. Each copy is bounded by maxDownload, and all
// of them together by maxDownloads.
func (st *browserState) keepDownload(scratch string) func(path, name string) (string, error) {
	return func(path, name string) (string, error) {
		defer os.Remove(path)
		src, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer src.Close()
		info, err := src.Stat()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Size() > maxDownload {
			return "", fmt.Errorf("a download may be at most %d MiB", maxDownload>>20)
		}
		st.mu.Lock()
		over := st.kept+info.Size() > maxDownloads
		if !over {
			st.kept += info.Size()
		}
		st.mu.Unlock()
		if over {
			return "", fmt.Errorf("this session's downloads may keep at most %d MiB in all", maxDownloads>>20)
		}
		root, err := unix.Open(scratch, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", scratchVariable, err)
		}
		defer unix.Close(root)
		if err := unix.Mkdirat(root, "downloads", 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("create %s/downloads: %w", scratchVariable, err)
		}
		dir, err := unix.Openat(root, "downloads", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return "", fmt.Errorf("%s/downloads is not a plain directory (%w)", scratchVariable, err)
		}
		defer unix.Close(dir)
		base := downloadName(name)
		ext := filepath.Ext(base)
		for i := 0; i < 100; i++ {
			candidate := base
			if i > 0 {
				candidate = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(base, ext), i, ext)
			}
			fd, err := unix.Openat(dir, candidate, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			if err != nil {
				return "", err
			}
			dst := os.NewFile(uintptr(fd), candidate)
			n, err := io.Copy(dst, io.LimitReader(src, info.Size()+1))
			if err == nil && n > info.Size() {
				err = fmt.Errorf("the download grew while it was copied")
			}
			if err = errors.Join(err, dst.Close()); err != nil {
				unix.Unlinkat(dir, candidate, 0)
				return "", err
			}
			return scratchVariable + "/downloads/" + candidate, nil
		}
		return "", errors.New("too many downloads of that name")
	}
}

// downloadName makes a page's suggested file name safe to create: one plain
// name in the directory, never a hidden one.
func downloadName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < ' ' {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "download"
	}
	if len(name) > 200 {
		name = name[len(name)-200:]
	}
	return name
}
