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
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file renders LaTeX block math ($$…$$ and \[…\]) as inline images in the
// interactive TUI. glamour has no math support and exposes no extension hook, so
// we work around it: block math is swapped for a sentinel before glamour runs
// and the sentinel is replaced with a kitty placeholder grid afterwards. The
// images themselves come from the CodeCogs service and are displayed via the
// kitty graphics protocol (see kittygraphics.go).

// mathSentinelMarker delimits a math placeholder token in the markdown stream.
// It is a private-use codepoint (U+F8FF) so it cannot collide with real content
// and passes through glamour/goldmark untouched.
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
	// sentinelRe recovers the formula index from a rendered sentinel line.
	sentinelRe = regexp.MustCompile(fmt.Sprintf("%cMODSMATH(\\d+)%c", mathSentinelMarker, mathSentinelMarker))
	// ansiRe matches SGR escape sequences (what glamour emits for styling).
	ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// extractBlockMath replaces each block-math span ($$…$$ and \[…\]) in md with a
// sentinel token on its own paragraph and returns the rewritten markdown plus
// the ordered list of extracted (trimmed) formulas. Inline math ($…$, \(…\)) is
// deliberately left untouched. Fenced code blocks are skipped so code samples
// containing $$ are not misinterpreted.
func extractBlockMath(md string) (string, []string) {
	var formulas []string
	var out, buf strings.Builder

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		out.WriteString(replaceBlockMath(buf.String(), &formulas))
		buf.Reset()
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
	return out.String(), formulas
}

// replaceBlockMath swaps each math span in s for a sentinel, appending the
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

// substituteMath replaces sentinel lines in the glamour-rendered output with the
// corresponding placeholder grids. The leading whitespace of the sentinel line
// is preserved as an indent prefix on every grid row so the image lines up with
// the surrounding text. A sentinel with no grid (still fetching, or failed) is
// dropped, so the raw marker never reaches the screen.
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

// mathEntry is a rendered formula cached for the lifetime of the session.
type mathEntry struct {
	id          int
	cols, rows  int
	png         []byte
	grid        string
	transmitted bool
}

// mathRenderer turns LaTeX formulas into transmitted kitty images and their
// placeholder grids, caching by formula so the constantly re-rendering TUI never
// re-fetches or re-transmits.
//
// Threading: render runs on the bubbletea event-loop goroutine; uncached
// formulas are fetched on background goroutines. The cache, in-flight set, and
// id counter are guarded by mu; the transmitted flag is only ever touched from
// render (event loop).
type mathRenderer struct {
	ctx          context.Context
	diac         []rune
	cellW, cellH int
	dpi          int
	client       *http.Client

	mu       sync.Mutex
	nextID   int
	cache    map[string]*mathEntry
	inflight map[string]bool // formulas currently being fetched
	notify   func()          // called when an async fetch completes
}

// Image ids are encoded in the placeholder foreground color's low 24 bits (the
// most-significant byte would need a third diacritic, which we omit), so they
// must stay within [firstMathImageID, maxMathImageID]. The range holds far more
// formulas than any session needs; the counter wraps for safety.
const (
	firstMathImageID = 0x4D0000 // "M" in the high byte, distinctive
	maxMathImageID   = 0xFFFFFF
)

// mathSupersample is how many times the render resolution exceeds the on-screen
// size, for crisp output on HiDPI displays.
const mathSupersample = 2

// codecogsPxPerDPI is how many vertical pixels of cap-to-descender text CodeCogs
// renders per DPI, measured empirically (Xg height ≈ 0.15*dpi across DPIs).
const codecogsPxPerDPI = 0.15

// mathFontScale sizes the formula relative to the terminal font. 1.0 makes the
// formula's text height about one terminal cell (matching surrounding text);
// raise it for larger display math, lower it for smaller.
const mathFontScale = 1.0

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
	return &mathRenderer{
		ctx:      ctx,
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

// render returns a map from each formula's index to its placeholder grid.
// Cached formulas are transmitted (once) and returned immediately; uncached
// formulas trigger a background fetch and are omitted this round (their
// sentinels are dropped by substituteMath, leaving a brief gap) until the fetch
// completes and notify() drives a re-render. Must be called on the event-loop
// goroutine so transmits do not race the renderer.
func (r *mathRenderer) render(formulas []string) map[int]string {
	grids := make(map[int]string, len(formulas))
	for i, f := range formulas {
		r.mu.Lock()
		e := r.cache[cacheKey(f)]
		r.mu.Unlock()
		if e == nil {
			r.startFetch(f)
			continue
		}
		if !e.transmitted {
			// Write directly to the terminal, bypassing bubbletea's renderer
			// (which strips unknown graphics escapes). Virtual placements produce
			// no output and no cursor movement, so this does not disturb the
			// frame bubbletea is drawing.
			transmitImage(os.Stdout, e.id, e.cols, e.rows, e.png)
			e.transmitted = true
		}
		grids[i] = e.grid
	}
	return grids
}

// startFetch kicks off a background render for latex unless it is already cached
// or in flight. On success it caches the entry and calls notify() so the caller
// can re-render with the now-available image.
func (r *mathRenderer) startFetch(latex string) {
	key := cacheKey(latex)
	r.mu.Lock()
	if r.inflight[key] || r.cache[key] != nil {
		r.mu.Unlock()
		return
	}
	r.inflight[key] = true
	r.mu.Unlock()

	go func() {
		e, err := r.build(latex)
		r.mu.Lock()
		delete(r.inflight, key)
		if err == nil {
			r.cache[key] = e
		}
		r.mu.Unlock()
		if err == nil && r.notify != nil {
			r.notify()
		}
	}()
}

// build fetches and sizes a formula into a mathEntry. It performs network I/O
// and is meant to run on a background goroutine. The formula is rendered at
// dpi*supersample for crispness, then the on-screen grid is sized as if rendered
// at dpi, so kitty packs the extra pixels into the same cell box and downsamples
// (sharp on HiDPI).
func (r *mathRenderer) build(latex string) (*mathEntry, error) {
	png, err := fetchFormulaPNG(r.ctx, r.client, latex, r.dpi*mathSupersample)
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(strings.NewReader(string(png)))
	if err != nil {
		return nil, fmt.Errorf("decode formula png: %w", err)
	}
	cols := ceilDiv(cfg.Width, r.cellW*mathSupersample)
	rows := ceilDiv(cfg.Height, r.cellH*mathSupersample)
	if cols > len(r.diac) || rows > len(r.diac) {
		return nil, fmt.Errorf("formula too large: %dx%d cells", cols, rows)
	}
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	if r.nextID > maxMathImageID {
		r.nextID = firstMathImageID
	}
	r.mu.Unlock()
	return &mathEntry{
		id:   id,
		cols: cols,
		rows: rows,
		png:  png,
		grid: placeholderGrid(id, cols, rows, r.diac),
	}, nil
}

// cacheKey is a stable key for a formula. Cell size and DPI are not folded in
// because the cache lives on a single renderer instance whose sizing is fixed
// for its lifetime.
func cacheKey(latex string) string {
	h := sha256.Sum256([]byte(latex))
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
// renders '+' (QueryEscape's encoding of space) as a literal plus sign.
func fetchFormulaPNG(ctx context.Context, client *http.Client, latex string, dpi int) ([]byte, error) {
	expr := fmt.Sprintf(`\dpi{%d}\fg{white}`, dpi) + latex
	enc := strings.ReplaceAll(url.QueryEscape(expr), "+", "%20")
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
