package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
)

// ScreenshotOptions say what to capture: the viewport (the default), the
// whole page, one element, or a rectangle of the viewport.
type ScreenshotOptions struct {
	FullPage bool
	Target   *Target
	Clip     *Rect // in viewport CSS pixels
	// MaxHeight is how much of a full page, in CSS pixels, is worth
	// capturing: a taller page is captured from the top down to there, as
	// shrinking it whole would leave its text unreadable. Zero means
	// MaxScreenshotHeight.
	MaxHeight float64
}

// Rect is a rectangle in CSS pixels.
type Rect struct{ X, Y, Width, Height float64 }

// Screenshot limits keep a capture, and the message carrying it, bounded.
const (
	MaxScreenshotHeight = 16384    // CSS pixels of a full-page capture
	maxScreenshotPixels = 40 << 20 // device pixels; larger captures are scaled down
)

// Screenshot captures the page as it is drawn, at the viewport's device
// scale. The note says when the capture was cut short or scaled down.
func (p *Page) Screenshot(ctx context.Context, opts ScreenshotOptions) (image.Image, string, error) {
	set := 0
	for _, ok := range []bool{opts.FullPage, opts.Target != nil, opts.Clip != nil} {
		if ok {
			set++
		}
	}
	if set > 1 {
		return nil, "", errors.New("capture the full page, an element or a clip, not several")
	}
	if opts.Target != nil {
		if err := opts.Target.check(); err != nil {
			return nil, "", err
		}
	}
	if c := opts.Clip; c != nil && (c.Width <= 0 || c.Height <= 0 || c.X < 0 || c.Y < 0 || math.IsNaN(c.X+c.Y+c.Width+c.Height)) {
		return nil, "", errors.New("a clip needs a positive width and height inside the viewport")
	}
	ctx, done, err := p.begin(ctx, 60*time.Second, false)
	if err != nil {
		return nil, "", err
	}
	defer done()
	defer p.release()
	var clip *Rect
	var note string
	if opts.Target != nil {
		el, _, err := p.actionable(ctx, *opts.Target, "scroll")
		if err != nil {
			return nil, "", fail(ctx, err)
		}
		if clip, err = p.elementBox(ctx, el); err != nil {
			return nil, "", fail(ctx, err)
		}
		note = remark(el)
	}
	m, err := cdp.Call(ctx, p.root, page.GetLayoutMetrics, cdp.Empty{})
	if err != nil || m.CSSVisualViewport == nil || m.CSSContentSize == nil {
		return nil, "", fail(ctx, fmt.Errorf("screenshot: page metrics: %w", err))
	}
	vv := m.CSSVisualViewport
	params := page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng, FromSurface: ptr(true)}
	switch {
	case opts.FullPage:
		w, h := math.Ceil(m.CSSContentSize.Width), math.Ceil(m.CSSContentSize.Height)
		most := float64(MaxScreenshotHeight)
		if opts.MaxHeight > 0 {
			most = min(most, opts.MaxHeight)
		}
		if h > most {
			note = fmt.Sprintf("the page is %d px tall, too tall to read whole; captured the top %d px: scroll and take viewport screenshots, or screenshot a target, to see the rest", int(h), int(most))
			h = most
		}
		clip = &Rect{0, 0, max(w, 1), max(h, 1)}
		params.CaptureBeyondViewport = ptr(true)
	case opts.Clip != nil:
		c := *opts.Clip
		clip = &Rect{c.X + vv.PageX, c.Y + vv.PageY, c.Width, c.Height}
	case clip != nil:
		clip.X += vv.PageX
		clip.Y += vv.PageY
		params.CaptureBeyondViewport = ptr(true)
	}
	scale := p.Viewport().scale()
	if clip != nil {
		params.Clip = &page.Viewport{X: clip.X, Y: clip.Y, Width: clip.Width, Height: clip.Height, Scale: 1}
		if px := clip.Width * clip.Height * scale * scale; px > maxScreenshotPixels {
			params.Clip.Scale = math.Sqrt(maxScreenshotPixels / px)
			note = strings.TrimPrefix(note+fmt.Sprintf("; scaled to %.0f%% to stay under %d megapixels", params.Clip.Scale*100, maxScreenshotPixels>>20), "; ")
		}
	}
	res, err := cdp.Call(ctx, p.root, page.CaptureScreenshot, params)
	if err != nil {
		return nil, "", fail(ctx, fmt.Errorf("screenshot: %w", err))
	}
	img, err := png.Decode(bytes.NewReader(res.Data))
	if err != nil {
		return nil, "", fmt.Errorf("screenshot: decode: %w", err)
	}
	return img, note, nil
}

// elementBox is an element's bounding box in the page's viewport.
func (p *Page) elementBox(ctx context.Context, el *element) (*Rect, error) {
	quads, err := cdp.Call(ctx, el.s, dom.GetContentQuads, dom.GetContentQuadsParams{ObjectID: el.object})
	if err != nil || len(quads.Quads) == 0 {
		return nil, &targetError{msg: el.desc + " has no box on screen", kind: "hidden"}
	}
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, q := range quads.Quads {
		for i := 0; i+1 < len(q); i += 2 {
			x0, x1 = min(x0, q[i]), max(x1, q[i])
			y0, y1 = min(y0, q[i+1]), max(y1, q[i+1])
		}
	}
	off, err := p.frameOffset(ctx, el.s)
	if err != nil {
		return nil, err
	}
	return &Rect{x0 + off.X, y0 + off.Y, max(x1-x0, 1), max(y1-y0, 1)}, nil
}

// Resize changes the page's emulated screen: its size, device scale and
// whether it is a phone.
func (p *Page) Resize(ctx context.Context, v Viewport) error {
	if err := v.check(); err != nil {
		return err
	}
	ctx, done, err := p.begin(ctx, 10*time.Second, false)
	if err != nil {
		return err
	}
	defer done()
	if err := p.root.pipeline(ctx, p.viewportCommands(v)); err != nil {
		return fail(ctx, err)
	}
	p.mu.Lock()
	p.viewport = v
	p.mu.Unlock()
	return fail(ctx, p.settle(ctx, p.snapshotNav()))
}

// EmulateMedia sets the page's emulated CSS media: color scheme, reduced
// motion and media type. Empty fields mean the defaults (light,
// no-preference, screen).
func (p *Page) EmulateMedia(ctx context.Context, m Media) error {
	if err := m.check(); err != nil {
		return err
	}
	ctx, done, err := p.begin(ctx, 10*time.Second, false)
	if err != nil {
		return err
	}
	defer done()
	for _, s := range p.frameSessions() {
		if _, err := s.call(ctx, "Emulation.setEmulatedMedia", mediaParams(m)); err != nil && s == p.root {
			return fail(ctx, err)
		}
	}
	p.mu.Lock()
	p.media = m
	p.mu.Unlock()
	return fail(ctx, p.settle(ctx, p.snapshotNav()))
}

// Media is the page's emulated CSS media.
func (p *Page) Media() Media {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.media
}
