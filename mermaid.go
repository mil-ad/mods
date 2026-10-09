package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// This file renders ```mermaid diagrams for image blocks (see imageblocks.go
// and imagerenderer.go) via the mermaid.ink service. Its PNGs have a
// transparent background, so only the theme needs to follow the terminal.

// mermaidTextPx is the height in pixels of a line of label text in a mermaid
// diagram at its natural size (16px font plus leading). Diagrams are scaled so
// this matches one terminal cell, putting labels at the size of surrounding
// text.
const mermaidTextPx = 20

// mermaidFontScale sizes diagrams relative to the terminal font; raise it for
// larger diagrams, lower it for smaller.
const mermaidFontScale = 1.0

// mermaid.ink rejects more than a couple of concurrent requests from one client
// with an immediate 503 (no Retry-After), so requests are queued through
// mermaidMaxConcurrent slots, and a 503/429 is retried up to mermaidRetries
// times with exponential backoff starting at mermaidRetryDelay.
const (
	mermaidMaxConcurrent = 2
	mermaidRetries       = 4
)

// mermaidRetryDelay is a variable so tests can shorten it.
var mermaidRetryDelay = time.Second

// fetchMermaidPNG fetches u from mermaid.ink, waiting for a free request slot
// and retrying when the service reports it is busy.
func (r *imageRenderer) fetchMermaidPNG(u string) ([]byte, error) {
	delay := mermaidRetryDelay
	for attempt := 0; ; attempt++ {
		select {
		case r.mermaidSlots <- struct{}{}:
		case <-r.ctx.Done():
			return nil, fmt.Errorf("render mermaid: %w", r.ctx.Err())
		}
		png, err := fetchPNG(r.ctx, r.client, u)
		<-r.mermaidSlots
		var se *httpStatusError
		busy := errors.As(err, &se) &&
			(se.code == http.StatusServiceUnavailable || se.code == http.StatusTooManyRequests)
		if !busy || attempt == mermaidRetries {
			if err != nil {
				return nil, fmt.Errorf("render mermaid: %w", err)
			}
			return png, nil
		}
		select {
		case <-time.After(delay):
		case <-r.ctx.Done():
			return nil, fmt.Errorf("render mermaid: %w", r.ctx.Err())
		}
		delay *= 2
	}
}

// buildMermaid renders a diagram sized so its labels match the terminal font,
// capped to the content width. mermaid.ink only scales when given an explicit
// width, and the natural width is unknown until rendered, so it takes two
// requests: one at natural size to measure, then one at imageSupersample× the
// on-screen width for sharpness.
func (r *imageRenderer) buildMermaid(src string) (*imageAsset, error) {
	natural, err := r.fetchMermaidPNG(mermaidURL(src, r.darkTheme, 0))
	if err != nil {
		return nil, err
	}
	cfg, err := decodePNGConfig(natural)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	maxPx := r.maxCols * r.cellW
	r.mu.Unlock()
	scale := float64(r.cellH) / mermaidTextPx * mermaidFontScale
	displayW := max(1, min(int(float64(cfg.Width)*scale), maxPx))
	displayH := max(1, displayW*cfg.Height/cfg.Width)
	cols, rows := ceilDiv(displayW, r.cellW), ceilDiv(displayH, r.cellH)

	target := displayW * imageSupersample
	if target <= cfg.Width {
		// Already at (or above) the needed resolution: a very wide diagram
		// or a tiny font.
		return newImageAsset(natural, cfg, cols, rows), nil
	}
	png, err := r.fetchMermaidPNG(mermaidURL(src, r.darkTheme, target))
	if err != nil {
		return nil, err
	}
	hiCfg, err := decodePNGConfig(png)
	if err != nil {
		return nil, err
	}
	return newImageAsset(png, hiCfg, cols, rows), nil
}

// mermaidURL builds a mermaid.ink PNG URL for src. width 0 means natural size.
func mermaidURL(src string, dark bool, width int) string {
	q := url.Values{"type": {"png"}}
	if dark {
		q.Set("theme", "dark")
	}
	if width > 0 {
		q.Set("width", fmt.Sprint(width))
	}
	return "https://mermaid.ink/img/" + base64.RawURLEncoding.EncodeToString([]byte(src)) + "?" + q.Encode()
}
