package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// This file handles LaTeX block math for image blocks (see imageblocks.go and
// imagerenderer.go): finding $$…$$ and \[…\] spans, and rendering them via the
// CodeCogs service.

var (
	// displayMathRe matches $$ … $$ block math (DOTALL: may span lines).
	displayMathRe = regexp.MustCompile(`(?s)\$\$(.+?)\$\$`)
	// bracketMathRe matches \[ … \] block math.
	bracketMathRe = regexp.MustCompile(`(?s)\\\[(.+?)\\\]`)
)

// replaceBlockMath swaps each math span in s for a sentinel, appending the
// formula to *blocks in encounter order.
func replaceBlockMath(s string, blocks *[]imageBlock) string {
	repl := func(re *regexp.Regexp, in string) string {
		return re.ReplaceAllStringFunc(in, func(m string) string {
			sub := re.FindStringSubmatch(m)
			idx := len(*blocks)
			*blocks = append(*blocks, imageBlock{kind: blockMath, src: strings.TrimSpace(sub[1])})
			return "\n\n" + imageSentinel(idx) + "\n\n"
		})
	}
	s = repl(displayMathRe, s)
	s = repl(bracketMathRe, s)
	return s
}

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

// buildMath renders a formula sized so its text matches the terminal font. It
// is rendered at imageSupersample× the on-screen size, so kitty packs the extra
// pixels into the cell box and downsamples (sharp on HiDPI).
func (r *imageRenderer) buildMath(latex string) (*imageAsset, error) {
	png, err := fetchFormulaPNG(r.ctx, r.client, latex, r.dpi*imageSupersample)
	if err != nil {
		return nil, err
	}
	cfg, err := decodePNGConfig(png)
	if err != nil {
		return nil, err
	}
	return newImageAsset(png, cfg,
		ceilDiv(cfg.Width, r.cellW*imageSupersample),
		ceilDiv(cfg.Height, r.cellH*imageSupersample)), nil
}

// fetchFormulaPNG renders latex to a PNG via the CodeCogs service. \fg{white}
// keeps it legible on dark terminals; spaces are %20-encoded because CodeCogs
// renders '+' (QueryEscape's encoding of space) as a literal plus sign.
func fetchFormulaPNG(ctx context.Context, client *http.Client, latex string, dpi int) ([]byte, error) {
	expr := fmt.Sprintf(`\dpi{%d}\fg{white}`, dpi) + latex
	enc := strings.ReplaceAll(url.QueryEscape(expr), "+", "%20")
	return fetchPNG(ctx, client, "https://latex.codecogs.com/png.image?"+enc)
}
