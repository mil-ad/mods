package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file handles the markdown side of "image blocks" — LaTeX block math and
// mermaid diagrams shown as kitty images. glamour supports neither and exposes
// no extension hook, so we work around it: each block is swapped for a
// sentinel before glamour runs (extractImageBlocks), and the sentinel is
// replaced with the image's placeholder grid afterwards (substituteImages).
// Fetching and displaying the images lives in imagerenderer.go, with the
// per-kind specifics in latex.go and mermaid.go.

// blockKind identifies what an image block contains and how to render it.
type blockKind int

const (
	blockMath blockKind = iota
	blockMermaid
)

// imageBlock is one renderable span extracted from the markdown.
type imageBlock struct {
	kind blockKind
	src  string // formula or diagram source
	// raw is the original markdown, put back in place of the sentinel when no
	// image is available (still fetching, or failed). Empty means the block is
	// dropped instead — used for math, whose raw form is noisy LaTeX.
	raw string
}

// imageSentinelMarker delimits an image placeholder token in the markdown
// stream. It is a private-use codepoint (U+F8FF) so it cannot collide with real
// content and passes through glamour/goldmark untouched.
const imageSentinelMarker = ''

// imageSentinel returns the unique token substituted for image block i.
func imageSentinel(i int) string {
	return fmt.Sprintf("%cMODSIMG%d%c", imageSentinelMarker, i, imageSentinelMarker)
}

var (
	// sentinelRe recovers the block index from a rendered sentinel line.
	sentinelRe = regexp.MustCompile(fmt.Sprintf("%cMODSIMG(\\d+)%c", imageSentinelMarker, imageSentinelMarker))
	// ansiRe matches SGR escape sequences (what glamour emits for styling).
	ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// extractImageBlocks replaces each image block in md with a sentinel token on
// its own paragraph and returns the rewritten markdown plus the ordered list of
// extracted blocks. With math set, block math ($$…$$ and \[…\]) is extracted;
// inline math ($…$, \(…\)) is deliberately left untouched. With mermaid set,
// closed ```mermaid fences are extracted; an unclosed fence (still streaming)
// is left as a code block. Other fenced code blocks are skipped so code samples
// containing $$ are not misinterpreted.
func extractImageBlocks(md string, math, mermaid bool) (string, []imageBlock) {
	var blocks []imageBlock
	// out is the rewritten markdown. prose collects lines outside fences (scanned
	// for math when flushed); fence collects the current fenced block verbatim,
	// and fenceSrc the same block without its fence lines.
	var out, prose, fence, fenceSrc strings.Builder

	flushProse := func() {
		if math {
			out.WriteString(replaceBlockMath(prose.String(), &blocks))
		} else {
			out.WriteString(prose.String())
		}
		prose.Reset()
	}

	inFence, isMermaid := false, false
	fenceMarker := ""
	for line := range strings.SplitAfterSeq(md, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case inFence && strings.HasPrefix(trimmed, fenceMarker):
			fence.WriteString(line)
			inFence = false
			if isMermaid {
				out.WriteString("\n" + imageSentinel(len(blocks)) + "\n\n")
				blocks = append(blocks, imageBlock{
					kind: blockMermaid,
					src:  strings.TrimSpace(fenceSrc.String()),
					raw:  fence.String(),
				})
			} else {
				out.WriteString(fence.String())
			}
			fence.Reset()
			fenceSrc.Reset()
		case inFence:
			fence.WriteString(line)
			fenceSrc.WriteString(line)
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flushProse()
			inFence = true
			fenceMarker = trimmed[:3]
			lang, _, _ := strings.Cut(strings.TrimSpace(strings.TrimLeft(trimmed, "`~")), " ")
			isMermaid = mermaid && strings.EqualFold(lang, "mermaid")
			fence.WriteString(line)
		default:
			prose.WriteString(line)
		}
	}
	flushProse()
	// An unclosed fence is passed through verbatim.
	out.WriteString(fence.String())
	return out.String(), blocks
}

// restoreUnrendered puts each block that has no grid back into the markdown in
// its original form (or drops it, when it has no raw form), so glamour renders
// it normally — e.g. a mermaid diagram that is still loading or failed to
// render stays visible as code.
//
// note, if non-nil, may supply a pre-styled status line for a restored block.
// The block's sentinel is then kept above it and the line is added to grids,
// so substituteImages swaps it in after glamour (which would otherwise restyle
// it).
func restoreUnrendered(md string, blocks []imageBlock, grids map[int]string, note func(imageBlock) string) string {
	for i, b := range blocks {
		if _, ok := grids[i]; ok {
			continue
		}
		raw := b.raw
		if raw != "" && note != nil {
			if n := note(b); n != "" {
				grids[i] = n
				raw = imageSentinel(i) + "\n\n" + raw
			}
		}
		md = strings.Replace(md, imageSentinel(i), raw, 1)
	}
	return md
}

// substituteImages replaces sentinel lines in the glamour-rendered output with
// the corresponding placeholder grids. The leading whitespace of the sentinel
// line is preserved as an indent prefix on every grid row so the image lines up
// with the surrounding text. A sentinel with no grid is dropped, so the raw
// marker never reaches the screen.
func substituteImages(rendered string, grids map[int]string) string {
	// Fast path: nothing to do when no sentinel is present.
	if !strings.ContainsRune(rendered, imageSentinelMarker) {
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
