package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file renders LaTeX math as images in mods' output — both the interactive
// TUI and non-interactive streaming. glamour has no math support and exposes no
// extension hook, so we work around it: math is replaced before glamour runs
// (block math → line sentinels, inline math → width-matched filler runs) and the
// placeholders are swapped for kitty placeholder grids afterwards. The images
// come from the CodeCogs service, displayed via the kitty graphics protocol (see
// kittygraphics.go).

// mathSentinelMarker delimits a block-math placeholder token in the markdown
// stream. It is a private-use codepoint (U+F8FF) so it cannot collide with real
// content and passes through glamour/goldmark untouched.
const mathSentinelMarker = '\uF8FF'

// mathSentinel returns the unique token substituted for block-math formula i.
func mathSentinel(i int) string {
	return fmt.Sprintf("%cMODSMATH%d%c", mathSentinelMarker, i, mathSentinelMarker)
}

var (
	// displayMathRe matches $$ … $$ block math (DOTALL: may span lines).
	displayMathRe = regexp.MustCompile(`(?s)\$\$(.+?)\$\$`)
	// bracketMathRe matches \[ … \] block math.
	bracketMathRe = regexp.MustCompile(`(?s)\\\[(.+?)\\\]`)
	// inlineDollarRe matches $ … $ inline math. The content must begin and end
	// with a non-space (so currency like "$5 to $10" is not mistaken for math)
	// and contain no $ or newlines.
	inlineDollarRe = regexp.MustCompile(`\$([^\s$](?:[^$\n]*[^\s$])?)\$`)
	// inlineParenRe matches \( … \) inline math.
	inlineParenRe = regexp.MustCompile(`(?s)\\\((.+?)\\\)`)
	// sentinelRe recovers the formula index from a rendered block sentinel.
	sentinelRe = regexp.MustCompile(fmt.Sprintf("%cMODSMATH(\\d+)%c", mathSentinelMarker, mathSentinelMarker))
	// ansiRe matches SGR escape sequences (what glamour emits for styling).
	ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")
	// inlineTokenRe matches either an SGR escape or a run of inline filler runes,
	// so substituteInline can track the active text style while replacing fillers.
	inlineTokenRe = regexp.MustCompile(`\x1b\[[0-9;]*m|[\x{F0000}-\x{FFFFD}]+`)
)

// mapOutsideFences applies transform to every stretch of md that lies outside a
// fenced code block, leaving fenced code untouched so math substitution does not
// mangle code samples that contain $ or \[.
func mapOutsideFences(md string, transform func(string) string) string {
	var out, buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			out.WriteString(transform(buf.String()))
			buf.Reset()
		}
	}
	inFence := false
	fenceMarker := ""
	for line := range strings.SplitAfterSeq(md, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inFence:
			out.WriteString(line)
			if fenceMarker != "" && strings.HasPrefix(trimmed, fenceMarker) {
				inFence = false
			}
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flush()
			inFence = true
			fenceMarker = trimmed[:3]
			out.WriteString(line)
		default:
			buf.WriteString(line)
		}
	}
	flush()
	return out.String()
}

// extractBlockMath replaces each block-math span ($$…$$ and \[…\]) in md with a
// sentinel token on its own paragraph and returns the rewritten markdown plus
// the ordered list of extracted (trimmed) formulas. Inline math is left for
// prepareInline. Fenced code blocks are skipped.
func extractBlockMath(md string) (string, []string) {
	var formulas []string
	out := mapOutsideFences(md, func(s string) string {
		return replaceBlockMath(s, &formulas)
	})
	return out, formulas
}

// replaceBlockMath swaps each block-math span in s for a sentinel, appending the
// formula to *formulas in encounter order.
func replaceBlockMath(s string, formulas *[]string) string {
	repl := func(re *regexp.Regexp, in string) string {
		return re.ReplaceAllStringFunc(in, func(m string) string {
			sub := re.FindStringSubmatch(m)
			idx := len(*formulas)
			*formulas = append(*formulas, strings.TrimSpace(sub[1]))
			return "\n\n" + mathSentinel(idx) + "\n\n"
		})
	}
	s = repl(displayMathRe, s)
	s = repl(bracketMathRe, s)
	return s
}

// substituteMath replaces block sentinel lines in the glamour-rendered output
// with the corresponding placeholder grids. The leading whitespace of the
// sentinel line is preserved as an indent prefix on every grid row so the image
// lines up with the surrounding text. A sentinel with no grid (still fetching,
// or failed) is dropped, so the raw marker never reaches the screen.
func substituteMath(rendered string, grids map[int]string) string {
	// Fast path: nothing to do when no sentinel is present.
	if !strings.ContainsRune(rendered, mathSentinelMarker) {
		return rendered
	}
	lines := strings.Split(rendered, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		loc := sentinelRe.FindStringSubmatchIndex(line)
		if loc == nil {
			out = append(out, line)
			continue
		}
		idx, _ := strconv.Atoi(line[loc[2]:loc[3]])
		grid, ok := grids[idx]
		if !ok {
			continue // drop the sentinel line entirely
		}
		prefix := visibleIndent(line)
		for _, gl := range strings.Split(strings.TrimRight(grid, "\n"), "\n") {
			out = append(out, prefix+gl)
		}
	}
	return strings.Join(out, "\n")
}

// visibleIndent returns the leading-space indent of a rendered line, ignoring
// any ANSI escape sequences that precede or interleave with the spaces (glamour
// emits styling codes before the indent).
func visibleIndent(line string) string {
	stripped := ansiRe.ReplaceAllString(line, "")
	n := len(stripped) - len(strings.TrimLeft(stripped, " "))
	return strings.Repeat(" ", n)
}

// inlineRep records a filler token planted in the markdown and the single-row
// placeholder grid that should replace it after glamour has wrapped. The filler
// is a run of identical private-use runes whose length equals the image's cell
// width, so glamour wraps it as one word of the correct width.
type inlineRep struct {
	filler string
	grid   string
}

// substituteInline swaps each inline filler run for its placeholder grid,
// restoring the surrounding text style afterwards. The grid sets and resets the
// foreground colour for its cells, which would otherwise drop glamour's colour
// for the following text, so the active SGR is re-emitted after each grid.
func substituteInline(rendered string, reps []inlineRep) string {
	if len(reps) == 0 {
		return rendered
	}
	grids := make(map[string]string, len(reps))
	for _, rep := range reps {
		grids[rep.filler] = rep.grid
	}
	activeSGR := ""
	return inlineTokenRe.ReplaceAllStringFunc(rendered, func(tok string) string {
		if strings.HasPrefix(tok, "\x1b") {
			activeSGR = tok
			return tok
		}
		if grid, ok := grids[tok]; ok {
			return grid + activeSGR
		}
		return tok
	})
}

// mathMode distinguishes block (own line, multi-row) from inline (one cell tall,
// flows with text) rendering. The same formula renders differently in each — and
// inline uses CodeCogs' compact \inline style — so it is cached separately per
// mode.
type mathMode int

const (
	modeBlock mathMode = iota
	modeInline
)

// mathEntry is a rendered formula cached for the lifetime of the session. ok is
// false when the formula could not be rendered usably (too large for the
// placeholder grid), in which case callers fall back to text.
type mathEntry struct {
	id          int
	cols, rows  int
	png         []byte
	grid        string
	ok          bool
	transmitted bool
}

// mathRenderer turns LaTeX formulas into transmitted kitty images and their
// placeholder grids, caching by formula so the constantly re-rendering output
// never re-fetches or re-transmits.
//
// Threading: render/prepareInline run on the bubbletea event-loop goroutine;
// uncached formulas are fetched on background goroutines. The cache, in-flight
// set, and id counter are guarded by mu; the transmitted flag is only ever
// touched from the event loop.
type mathRenderer struct {
	ctx          context.Context
	diac         []rune
	cellW, cellH int
	dpi          int
	client       *http.Client
	out          io.Writer // terminal sink for image transmissions

	mu       sync.Mutex
	nextID   int
	cache    map[string]*mathEntry
	inflight map[string]bool // formulas currently being fetched
	notify   func()          // called when an async fetch completes
}

// Image ids are encoded in the placeholder foreground colour's low 24 bits (the
// most-significant byte would need a third diacritic, which we omit), so they
// must stay within [firstMathImageID, maxMathImageID]. The range holds far more
// formulas than any session needs; the counter wraps for safety.
const (
	firstMathImageID = 0x4D0000 // "M" in the high byte, distinctive
	maxMathImageID   = 0xFFFFFF
)

// inlineFillerBase is the start of a private-use range used for inline filler
// runes (one distinct rune per inline image in a render, its run length equal to
// the image's cell width). inlineFillerCount caps inline images per render.
const (
	inlineFillerBase  = 0xF0000 // supplementary private-use area
	inlineFillerCount = 4096
)

// mathSupersample is how many times the render resolution exceeds the on-screen
// size, for crisp output on HiDPI displays.
const mathSupersample = 2

// codecogsPxPerDPI is how many vertical pixels of cap-to-descender text CodeCogs
// renders per DPI, measured empirically (Xg height ≈ 0.15*dpi across DPIs).
const codecogsPxPerDPI = 0.15

// mathFontScale sizes display (block) math relative to the terminal font. 1.0
// makes the cap-to-descender about one cell tall; raise for larger display math.
const mathFontScale = 1.0

// inlineFontScale sizes inline math relative to display math. Below 1 so inline
// formulas sit a touch shorter — closer to the surrounding text size than to the
// full cell height used for display equations.
const inlineFontScale = 0.9

// dpiForCell derives the render DPI so the formula's cap-to-descender text spans
// one cell height: cellH = codecogsPxPerDPI * dpi.
func dpiForCell(cellH int) int {
	return max(120, int(float64(cellH)/codecogsPxPerDPI*mathFontScale))
}

func newMathRenderer(ctx context.Context) *mathRenderer {
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
	return &mathRenderer{
		ctx:      ctx,
		out:      out,
		diac:     parseDiacritics(),
		cellW:    cw,
		cellH:    ch,
		dpi:      dpiForCell(ch),
		nextID:   firstMathImageID,
		cache:    map[string]*mathEntry{},
		inflight: map[string]bool{},
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// render returns a map from each block formula's index to its placeholder grid.
// Cached formulas are transmitted (once) and returned immediately; uncached
// formulas trigger a background fetch and are omitted this round (their sentinels
// are dropped by substituteMath) until the fetch completes and notify() drives a
// re-render. Must be called on the event-loop goroutine.
func (r *mathRenderer) render(formulas []string) map[int]string {
	grids := make(map[int]string, len(formulas))
	for i, f := range formulas {
		if e := r.lookup(f, modeBlock); e != nil && e.ok {
			r.transmitOnce(e)
			grids[i] = e.grid
		}
	}
	return grids
}

// prepareInline rewrites inline math ($…$ and \(…\)) in md into width-matched
// filler runs and returns the rewritten markdown plus the substitutions to apply
// after glamour wraps. Formulas not yet cached are left as text (a fetch is
// started) until the fetch completes and notify() drives a re-render. Must be
// called on the event-loop goroutine (it transmits cached images).
func (r *mathRenderer) prepareInline(md string) (string, []inlineRep) {
	var reps []inlineRep
	out := mapOutsideFences(md, func(s string) string {
		return r.replaceInline(s, &reps)
	})
	return out, reps
}

func (r *mathRenderer) replaceInline(s string, reps *[]inlineRep) string {
	repl := func(re *regexp.Regexp) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			formula := strings.TrimSpace(re.FindStringSubmatch(m)[1])
			e := r.lookup(formula, modeInline)
			if e == nil || !e.ok || len(*reps) >= inlineFillerCount {
				return m // fetching, failed, or out of filler runes: keep as text
			}
			r.transmitOnce(e)
			filler := strings.Repeat(string(rune(inlineFillerBase+len(*reps))), e.cols)
			*reps = append(*reps, inlineRep{filler: filler, grid: e.grid})
			return filler
		})
	}
	repl(inlineDollarRe)
	repl(inlineParenRe)
	return s
}

// lookup returns the cached entry for a formula in the given mode, or nil if it
// is not ready yet (in which case a background fetch is started).
func (r *mathRenderer) lookup(latex string, mode mathMode) *mathEntry {
	key := cacheKey(latex, mode)
	r.mu.Lock()
	e := r.cache[key]
	r.mu.Unlock()
	if e == nil {
		r.startFetch(latex, mode)
		return nil
	}
	return e
}

// transmitOnce sends an entry's image to the terminal the first time it is used.
// Written directly to the terminal, bypassing bubbletea's renderer (which strips
// graphics escapes); virtual placements move no cursor, so the frame is
// undisturbed. Must run on the event-loop goroutine.
func (r *mathRenderer) transmitOnce(e *mathEntry) {
	if e.ok && !e.transmitted {
		transmitImage(r.out, e.id, e.cols, e.rows, e.png)
		e.transmitted = true
	}
}

// startFetch kicks off a background render for latex unless it is already cached
// or in flight. It always calls notify() on completion (success or failure) so a
// caller waiting for outstanding work — e.g. before quitting at end of stream —
// is never left hanging on a fetch that errored.
func (r *mathRenderer) startFetch(latex string, mode mathMode) {
	key := cacheKey(latex, mode)
	r.mu.Lock()
	if r.inflight[key] || r.cache[key] != nil {
		r.mu.Unlock()
		return
	}
	r.inflight[key] = true
	r.mu.Unlock()

	go func() {
		e, err := r.build(latex, mode)
		r.mu.Lock()
		delete(r.inflight, key)
		if err == nil {
			r.cache[key] = e
		}
		r.mu.Unlock()
		if r.notify != nil {
			r.notify()
		}
	}()
}

// pending reports whether any formula fetches are still in flight.
func (r *mathRenderer) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inflight) > 0
}

// build fetches and sizes a formula into a mathEntry. It performs network I/O
// and is meant to run on a background goroutine. The formula is rendered at
// dpi*supersample for crispness. Block math keeps its natural cell footprint;
// inline math uses CodeCogs' compact \inline style and is normalized to exactly
// one cell tall: short formulas are padded up to one cell (so they render at text
// size rather than being upscaled to fill it) and taller ones scale down to fit.
func (r *mathRenderer) build(latex string, mode mathMode) (*mathEntry, error) {
	dpi := r.dpi * mathSupersample
	if mode == modeInline {
		// Inline math renders a touch shorter than display math.
		dpi = int(float64(r.dpi)*inlineFontScale) * mathSupersample
	}
	imgPNG, err := fetchFormulaPNG(r.ctx, r.client, latex, dpi, mode == modeInline)
	if err != nil {
		return nil, err
	}
	w, h, err := pngSize(imgPNG)
	if err != nil {
		return nil, err
	}

	var cols, rows int
	if mode == modeInline {
		// Normalize height to one cell. Pad shorter formulas up to one cell so
		// kitty does not upscale them; the column count then follows the aspect
		// ratio at that height.
		targetH := r.cellH * mathSupersample
		if h < targetH {
			imgPNG, w, h, err = padPNGHeight(imgPNG, targetH)
			if err != nil {
				return nil, err
			}
		}
		rows = 1
		cols = ceilDiv(w*r.cellH, h*r.cellW)
	} else {
		cols = ceilDiv(w, r.cellW*mathSupersample)
		rows = ceilDiv(h, r.cellH*mathSupersample)
	}
	if cols < 1 || cols > len(r.diac) || rows > len(r.diac) {
		return &mathEntry{ok: false}, nil
	}

	id := r.nextImageID()
	grid := placeholderGrid(id, cols, rows, r.diac)
	if mode == modeInline {
		grid = placeholderRow(id, cols, r.diac)
	}
	return &mathEntry{id: id, cols: cols, rows: rows, png: imgPNG, grid: grid, ok: true}, nil
}

// nextImageID allocates the next wrapping image id under the lock.
func (r *mathRenderer) nextImageID() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID
	r.nextID++
	if r.nextID > maxMathImageID {
		r.nextID = firstMathImageID
	}
	return id
}

// pngSize returns the pixel dimensions of a PNG without fully decoding it.
func pngSize(data []byte) (int, int, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("decode formula png: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

// inlineBaselineGap is the fraction of a cell left empty below a padded inline
// formula, approximating the terminal font's descender space so the formula's
// baseline sits roughly on the text baseline instead of floating centered.
const inlineBaselineGap = 0.1

// padPNGHeight places the image in a transparent canvas of height targetH,
// bottom-aligned with a small descender gap so its baseline lines up with the
// surrounding text. Returns the re-encoded PNG and its dimensions. Used to
// normalize short inline formulas to one cell tall without upscaling glyphs.
func padPNGHeight(data []byte, targetH int) ([]byte, int, int, error) {
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode formula png: %w", err)
	}
	b := src.Bounds()
	w := b.Dx()
	dst := image.NewRGBA(image.Rect(0, 0, w, targetH))
	offset := targetH - b.Dy() - int(float64(targetH)*inlineBaselineGap)
	if offset < 0 {
		offset = 0
	}
	draw.Draw(dst, image.Rect(0, offset, w, offset+b.Dy()), src, b.Min, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, 0, 0, fmt.Errorf("encode padded png: %w", err)
	}
	return buf.Bytes(), w, targetH, nil
}

// cacheKey is a stable key for a formula in a given mode. Cell size and DPI are
// not folded in because the cache lives on a single renderer instance whose
// sizing is fixed for its lifetime.
func cacheKey(latex string, mode mathMode) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s", mode, latex))
	return hex.EncodeToString(h[:8])
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return 1
	}
	return max(1, (a+b-1)/b)
}

// fetchFormulaPNG renders latex to a PNG via the CodeCogs service. \fg{white}
// keeps it legible on dark terminals; spaces are %20-encoded because CodeCogs
// renders '+' (QueryEscape's encoding of space) as a literal plus sign. When
// inline is set, \inline selects CodeCogs' compact text style so fractions and
// sums lay out at roughly one line tall instead of stacked.
func fetchFormulaPNG(ctx context.Context, client *http.Client, latex string, dpi int, inline bool) ([]byte, error) {
	directives := fmt.Sprintf(`\dpi{%d}\fg{white}`, dpi)
	if inline {
		directives = `\inline` + directives
	}
	enc := strings.ReplaceAll(url.QueryEscape(directives+latex), "+", "%20")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://latex.codecogs.com/png.image?"+enc, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch formula: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codecogs status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read formula: %w", err)
	}
	return data, nil
}
