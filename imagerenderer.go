package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/png" // register PNG decoder for image.DecodeConfig
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file turns image blocks (see imageblocks.go) into kitty images: it
// fetches them from remote rendering services in the background, caches them
// for the session, transmits them to the terminal, and hands back the
// placeholder grids that display them (see kittygraphics.go).

// imageAsset is a rendered block cached for the lifetime of the session. The
// same PNG may be shown at several sizes (e.g. after a terminal resize), each
// as its own kitty image with its own placeholder grid.
type imageAsset struct {
	png        []byte
	pxW, pxH   int
	wantCols   int // preferred width in cells, before capping to the content width
	wantRows   int // preferred height in cells at wantCols
	placements map[int]*imagePlacement
}

// imagePlacement is an asset transmitted as a kitty image of a given cell size.
type imagePlacement struct {
	id          int
	cols, rows  int
	grid        string
	transmitted bool
}

// imageRendererOptions configures newImageRenderer.
type imageRendererOptions struct {
	math, mermaid bool   // which kinds of block to render
	darkTheme     bool   // style diagrams for a dark terminal background
	wordWrap      int    // glamour's wrap width; images are scaled down to fit
	failedNote    string // pre-styled line shown above a block that failed to render
}

// imageRenderer turns image blocks into transmitted kitty images and their
// placeholder grids, caching by source so the constantly re-rendering TUI never
// re-fetches or re-transmits.
//
// Threading: render runs on the bubbletea event-loop goroutine; uncached blocks
// are fetched on background goroutines. The fields under mu are shared with
// those goroutines; placements (and their transmitted flags) are only ever
// touched from render (event loop).
type imageRenderer struct {
	ctx          context.Context
	client       *http.Client
	out          io.Writer // terminal sink for image transmissions
	diac         []rune
	cellW, cellH int
	dpi          int // CodeCogs render DPI matching the terminal font
	math         bool
	mermaid      bool
	darkTheme    bool
	failedNote   string
	// mermaidSlots limits concurrent mermaid.ink requests (see fetchMermaidPNG).
	mermaidSlots chan struct{}
	notify       func() // called when an async fetch completes

	mu       sync.Mutex
	maxCols  int // content width in cells; images are scaled down to fit
	nextID   int
	cache    map[string]*imageAsset
	failed   map[string]bool      // blocks whose render errored; not retried
	inflight map[string]blockKind // blocks currently being fetched
	started  bool                 // a fetch started since the last takeStarted
}

// Image ids are encoded in the placeholder foreground color's low 24 bits (the
// most-significant byte would need a third diacritic, which we omit), so they
// must stay within [firstImageID, maxImageID]. The range holds far more images
// than any session needs; the counter wraps for safety.
const (
	firstImageID = 0x4D0000 // "M" in the high byte, distinctive
	maxImageID   = 0xFFFFFF
)

// imageSupersample is how many times the render resolution exceeds the
// on-screen size, for crisp output on HiDPI displays.
const imageSupersample = 2

// glamourMargin is the total horizontal margin (both sides) glamour's standard
// styles put around the document; images are capped to wordWrap minus this.
const glamourMargin = 4

// minImageCols keeps images legible when the terminal is very narrow.
const minImageCols = 10

func newImageRenderer(ctx context.Context, opts imageRendererOptions) *imageRenderer {
	cw, ch, err := cellPixels()
	if err != nil {
		cw, ch = 10, 20 // conservative fallback
	}
	// Transmit images straight to the controlling terminal. Non-interactive mode
	// renders to stderr and prints the final output to stdout (which may be
	// redirected), so prefer /dev/tty to reach the terminal regardless.
	var out io.Writer = os.Stdout
	if tty, terr := os.OpenFile("/dev/tty", os.O_WRONLY, 0); terr == nil {
		out = tty
	}
	r := &imageRenderer{
		ctx:          ctx,
		client:       &http.Client{Timeout: 15 * time.Second},
		out:          out,
		diac:         parseDiacritics(),
		cellW:        cw,
		cellH:        ch,
		dpi:          dpiForCell(ch),
		math:         opts.math,
		mermaid:      opts.mermaid,
		darkTheme:    opts.darkTheme,
		failedNote:   opts.failedNote,
		mermaidSlots: make(chan struct{}, mermaidMaxConcurrent),
		nextID:       firstImageID,
		cache:        map[string]*imageAsset{},
		failed:       map[string]bool{},
		inflight:     map[string]blockKind{},
	}
	r.setWordWrap(opts.wordWrap)
	return r
}

// setWordWrap updates the content width images are scaled down to fit.
func (r *imageRenderer) setWordWrap(wordWrap int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxCols = min(len(r.diac), max(minImageCols, wordWrap-glamourMargin))
}

// preprocess prepares md for glamour: image blocks become sentinels (for those
// with a ready image) and the returned grids map each sentinel's index to its
// placeholder grid, for substituteImages to swap in after rendering. Must be
// called on the event-loop goroutine (see render).
func (r *imageRenderer) preprocess(md string) (string, map[int]string) {
	processed, blocks := extractImageBlocks(md, r.math, r.mermaid)
	if len(blocks) == 0 {
		return processed, nil
	}
	grids := r.render(blocks)
	return restoreUnrendered(processed, blocks, grids, r.statusNote), grids
}

// statusNote returns the failure note for a block that failed to render, or ""
// otherwise. Blocks still rendering get no note; they are counted in the
// top-right status badge instead (see Mods.statusBadge).
func (r *imageRenderer) statusNote(b imageBlock) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed[cacheKey(b)] {
		return r.failedNote
	}
	return ""
}

// render returns a map from each block's index to its placeholder grid. Cached
// blocks are transmitted (once per size) and returned immediately; uncached
// blocks trigger a background fetch and are omitted this round until the fetch
// completes and notify() drives a re-render. Must be called on the event-loop
// goroutine so transmits do not race the renderer.
func (r *imageRenderer) render(blocks []imageBlock) map[int]string {
	grids := make(map[int]string, len(blocks))
	r.mu.Lock()
	maxCols := r.maxCols
	r.mu.Unlock()
	for i, b := range blocks {
		key := cacheKey(b)
		r.mu.Lock()
		a := r.cache[key]
		r.mu.Unlock()
		if a == nil {
			r.startFetch(b)
			continue
		}
		p := r.placement(a, maxCols)
		if !p.transmitted {
			// Write directly to the terminal, bypassing bubbletea's renderer
			// (which strips unknown graphics escapes). Virtual placements produce
			// no output and no cursor movement, so this does not disturb the
			// frame bubbletea is drawing.
			transmitImage(r.out, p.id, p.cols, p.rows, a.png)
			p.transmitted = true
		}
		grids[i] = p.grid
	}
	return grids
}

// placement returns the asset's placement sized to fit maxCols, creating it on
// first use. kitty fits the image into the cell box preserving aspect ratio.
func (r *imageRenderer) placement(a *imageAsset, maxCols int) *imagePlacement {
	cols, rows := a.wantCols, a.wantRows
	if cols > maxCols {
		cols = maxCols
		rows = ceilDiv(cols*r.cellW*a.pxH, a.pxW*r.cellH)
	}
	if rows > len(r.diac) {
		rows = len(r.diac)
		cols = max(1, min(cols, rows*r.cellH*a.pxW/(a.pxH*r.cellW)))
	}
	if p := a.placements[cols]; p != nil {
		return p
	}
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	if r.nextID > maxImageID {
		r.nextID = firstImageID
	}
	r.mu.Unlock()
	p := &imagePlacement{id: id, cols: cols, rows: rows, grid: placeholderGrid(id, cols, rows, r.diac)}
	a.placements[cols] = p
	return p
}

// startFetch kicks off a background render for b unless it is already cached,
// failed or in flight. It always calls notify() on completion (success or
// failure) so a caller waiting for outstanding work — e.g. before quitting at
// EOF — is never left hanging on a fetch that errored.
func (r *imageRenderer) startFetch(b imageBlock) {
	key := cacheKey(b)
	r.mu.Lock()
	if _, ok := r.inflight[key]; ok || r.failed[key] || r.cache[key] != nil {
		r.mu.Unlock()
		return
	}
	r.inflight[key] = b.kind
	r.started = true
	r.mu.Unlock()

	go func() {
		a, err := r.build(b)
		r.mu.Lock()
		delete(r.inflight, key)
		if err == nil {
			r.cache[key] = a
		} else {
			r.failed[key] = true
		}
		r.mu.Unlock()
		if r.notify != nil {
			r.notify()
		}
	}()
}

// pending reports whether any image fetches are still in flight.
func (r *imageRenderer) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inflight) > 0
}

// pendingDiagrams counts mermaid diagrams still being rendered. Formulas are
// left out: they render near-instantly, so reporting them would only flicker.
func (r *imageRenderer) pendingDiagrams() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, k := range r.inflight {
		if k == blockMermaid {
			n++
		}
	}
	return n
}

// takeStarted reports whether a fetch has started since the last call, and
// resets the flag.
func (r *imageRenderer) takeStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.started
	r.started = false
	return s
}

// build renders b into an imageAsset. It performs network I/O and is meant to
// run on a background goroutine.
func (r *imageRenderer) build(b imageBlock) (*imageAsset, error) {
	if b.kind == blockMermaid {
		return r.buildMermaid(b.src)
	}
	return r.buildMath(b.src)
}

// newImageAsset wraps a decoded PNG to be shown in a cols×rows cell box.
func newImageAsset(png []byte, cfg image.Config, cols, rows int) *imageAsset {
	return &imageAsset{
		png:        png,
		pxW:        cfg.Width,
		pxH:        cfg.Height,
		wantCols:   cols,
		wantRows:   rows,
		placements: map[int]*imagePlacement{},
	}
}

// httpStatusError is a non-200 response from a rendering service.
type httpStatusError struct {
	host string
	code int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: status %d", e.host, e.code)
}

// fetchPNG GETs a rendered image from a rendering service.
func fetchPNG(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{host: req.URL.Host, code: resp.StatusCode}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	return data, nil
}

func decodePNGConfig(png []byte) (image.Config, error) {
	cfg, _, err := image.DecodeConfig(strings.NewReader(string(png)))
	if err != nil {
		return cfg, fmt.Errorf("decode png: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return cfg, fmt.Errorf("empty image: %dx%d", cfg.Width, cfg.Height)
	}
	return cfg, nil
}

// cacheKey is a stable key for a block. Sizing is not folded in: the cell size
// is fixed for the renderer's lifetime, and content-width changes are handled
// by an asset's placements rather than by re-fetching.
func cacheKey(b imageBlock) string {
	h := sha256.Sum256([]byte(strconv.Itoa(int(b.kind)) + "\x00" + b.src))
	return hex.EncodeToString(h[:8])
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return 1
	}
	return max(1, (a+b-1)/b)
}
