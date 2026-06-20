package main

import (
	"strings"
	"testing"
)

func TestExtractBlockMath(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantFormulas []string
		// substrings that must survive in the processed output
		wantContains []string
		// substrings that must NOT appear (i.e. were extracted)
		wantAbsent []string
	}{
		{
			name:         "display math",
			in:           "Here:\n\n$$a^2 + b^2 = c^2$$\n\ndone.",
			wantFormulas: []string{"a^2 + b^2 = c^2"},
			wantContains: []string{"Here:", "done.", mathSentinel(0)},
			wantAbsent:   []string{"a^2 + b^2"},
		},
		{
			name:         "bracket math multiline",
			in:           "x\n\n\\[\n\\int_0^1 x\\,dx\n\\]\n\ny",
			wantFormulas: []string{"\\int_0^1 x\\,dx"},
			wantContains: []string{mathSentinel(0)},
		},
		{
			name:         "inline math untouched",
			in:           "cost is $5 and the value $x$ stays inline",
			wantFormulas: nil,
			wantContains: []string{"$x$", "$5"},
		},
		{
			name:         "two display blocks",
			in:           "$$E=mc^2$$ and $$F=ma$$",
			wantFormulas: []string{"E=mc^2", "F=ma"},
			wantContains: []string{mathSentinel(0), mathSentinel(1)},
		},
		{
			name:         "dollar inside fenced code is ignored",
			in:           "```sh\necho $$ is the pid\n```\n",
			wantFormulas: nil,
			wantContains: []string{"echo $$ is the pid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, formulas := extractBlockMath(tt.in)
			if len(formulas) != len(tt.wantFormulas) {
				t.Fatalf("got %d formulas %q, want %d %q", len(formulas), formulas, len(tt.wantFormulas), tt.wantFormulas)
			}
			for i, want := range tt.wantFormulas {
				if formulas[i] != want {
					t.Errorf("formula[%d] = %q, want %q", i, formulas[i], want)
				}
			}
			for _, s := range tt.wantContains {
				if !strings.Contains(got, s) {
					t.Errorf("output missing %q\n--- output ---\n%s", s, got)
				}
			}
			for _, s := range tt.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("output should not contain %q\n--- output ---\n%s", s, got)
				}
			}
		})
	}
}

func TestSubstituteMath(t *testing.T) {
	rendered := "intro line\n  " + mathSentinel(0) + "\noutro line"
	grids := map[int]string{0: "ROW1\nROW2\n"}
	got := substituteMath(rendered, grids)

	want := "intro line\n  ROW1\n  ROW2\noutro line"
	if got != want {
		t.Fatalf("substituteMath:\n got %q\nwant %q", got, want)
	}
}

func TestSubstituteMathDropsMissing(t *testing.T) {
	rendered := "a\n" + mathSentinel(0) + "\nb"
	got := substituteMath(rendered, map[int]string{0: "X"}) // present
	if !strings.Contains(got, "X") {
		t.Errorf("expected grid substituted, got %q", got)
	}
	// A sentinel with no grid is dropped entirely.
	got2 := substituteMath("a\n"+mathSentinel(5)+"\nb", map[int]string{0: "X"})
	if strings.Contains(got2, "MODSMATH") {
		t.Errorf("unmatched sentinel should be dropped, got %q", got2)
	}
	if got2 != "a\nb" {
		t.Errorf("got %q, want %q", got2, "a\nb")
	}
}

func TestPlaceholderGridShape(t *testing.T) {
	diac := parseDiacritics()
	if len(diac) < 10 {
		t.Fatalf("expected diacritics embedded, got %d", len(diac))
	}
	grid := placeholderGrid(firstMathImageID, 3, 2, diac)
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
