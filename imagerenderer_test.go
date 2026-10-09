package main

import (
	"strings"
	"testing"
)

func TestPlaceholderGridShape(t *testing.T) {
	diac := parseDiacritics()
	if len(diac) < 10 {
		t.Fatalf("expected diacritics embedded, got %d", len(diac))
	}
	grid := placeholderGrid(firstImageID, 3, 2, diac)
	lines := strings.Split(strings.TrimRight(grid, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(lines))
	}
	for i, line := range lines {
		if n := strings.Count(line, string(placeholderRune)); n != 3 {
			t.Errorf("row %d: got %d placeholder cells, want 3", i, n)
		}
	}
}

func TestCeilDiv(t *testing.T) {
	cases := [][3]int{{0, 18, 1}, {18, 18, 1}, {19, 18, 2}, {424, 18, 24}, {68, 39, 2}}
	for _, c := range cases {
		if got := ceilDiv(c[0], c[1]); got != c[2] {
			t.Errorf("ceilDiv(%d,%d)=%d, want %d", c[0], c[1], got, c[2])
		}
	}
}

func TestPlacementFitsWidth(t *testing.T) {
	r := &imageRenderer{diac: parseDiacritics(), cellW: 10, cellH: 20, nextID: firstImageID}
	a := &imageAsset{pxW: 2000, pxH: 1000, wantCols: 100, wantRows: 25, placements: map[int]*imagePlacement{}}

	p := r.placement(a, 200)
	if p.cols != 100 || p.rows != 25 {
		t.Errorf("uncapped: got %dx%d, want 100x25", p.cols, p.rows)
	}
	// Capped to 50 cols = 500px wide -> 250px tall -> 13 rows of 20px.
	p = r.placement(a, 50)
	if p.cols != 50 || p.rows != 13 {
		t.Errorf("capped: got %dx%d, want 50x13", p.cols, p.rows)
	}
	if again := r.placement(a, 50); again != p {
		t.Error("expected the same placement to be reused for the same size")
	}
	if len(a.placements) != 2 {
		t.Errorf("got %d placements, want 2", len(a.placements))
	}
}
