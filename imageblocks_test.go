package main

import (
	"strings"
	"testing"
)

func TestExtractImageBlocks(t *testing.T) {
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
			wantContains: []string{"Here:", "done.", imageSentinel(0)},
			wantAbsent:   []string{"a^2 + b^2"},
		},
		{
			name:         "bracket math multiline",
			in:           "x\n\n\\[\n\\int_0^1 x\\,dx\n\\]\n\ny",
			wantFormulas: []string{"\\int_0^1 x\\,dx"},
			wantContains: []string{imageSentinel(0)},
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
			wantContains: []string{imageSentinel(0), imageSentinel(1)},
		},
		{
			name:         "dollar inside fenced code is ignored",
			in:           "```sh\necho $$ is the pid\n```\n",
			wantFormulas: nil,
			wantContains: []string{"echo $$ is the pid"},
		},
		{
			name:         "mermaid fence",
			in:           "See:\n\n```mermaid\ngraph TD\n  A --> B\n```\n\nafter",
			wantFormulas: []string{"graph TD\n  A --> B"},
			wantContains: []string{"See:", "after", imageSentinel(0)},
			wantAbsent:   []string{"A --> B", "```"},
		},
		{
			name:         "unclosed mermaid fence is left as code",
			in:           "```mermaid\ngraph TD\n  A --> B\n",
			wantFormulas: nil,
			wantContains: []string{"```mermaid", "A --> B"},
		},
		{
			name:         "math and mermaid keep encounter order",
			in:           "$$x$$\n\n```Mermaid\ngraph LR\n```\n\n$$y$$",
			wantFormulas: []string{"x", "graph LR", "y"},
			wantContains: []string{imageSentinel(0), imageSentinel(1), imageSentinel(2)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, blocks := extractImageBlocks(tt.in, true, true)
			if len(blocks) != len(tt.wantFormulas) {
				t.Fatalf("got %d blocks %+v, want %d %q", len(blocks), blocks, len(tt.wantFormulas), tt.wantFormulas)
			}
			for i, want := range tt.wantFormulas {
				if blocks[i].src != want {
					t.Errorf("block[%d] = %q, want %q", i, blocks[i].src, want)
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

func TestExtractImageBlocksDisabled(t *testing.T) {
	in := "$$x$$\n\n```mermaid\ngraph TD\n```\n"
	got, blocks := extractImageBlocks(in, false, false)
	if len(blocks) != 0 || got != in {
		t.Fatalf("expected passthrough, got %q with %d blocks", got, len(blocks))
	}
	_, blocks = extractImageBlocks(in, false, true)
	if len(blocks) != 1 || blocks[0].kind != blockMermaid {
		t.Fatalf("expected only the mermaid block, got %+v", blocks)
	}
}

func TestRestoreUnrendered(t *testing.T) {
	in := "$$x$$\n\n```mermaid\ngraph TD\n```\n\n$$y$$"
	md, blocks := extractImageBlocks(in, true, true)
	// Only the second formula has an image: the mermaid block falls back to
	// its code fence, and the first formula (no raw form) is dropped.
	got := restoreUnrendered(md, blocks, map[int]string{2: "IMG"}, nil)
	for _, want := range []string{"```mermaid\ngraph TD\n```", imageSentinel(2)} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, absent := range []string{imageSentinel(0), imageSentinel(1), "x"} {
		if strings.Contains(got, absent) {
			t.Errorf("unexpected %q in %q", absent, got)
		}
	}
}

func TestRestoreUnrenderedNote(t *testing.T) {
	md, blocks := extractImageBlocks("$$x$$\n\n```mermaid\ngraph TD\n```\n", true, true)
	grids := map[int]string{}
	got := restoreUnrendered(md, blocks, grids, func(imageBlock) string { return "Loading" })
	// The mermaid block keeps its sentinel above the fence, with the note
	// queued as its grid; math has no raw form, so it gets no note.
	if !strings.Contains(got, imageSentinel(1)+"\n\n```mermaid") {
		t.Errorf("expected sentinel above the mermaid fence, got %q", got)
	}
	if len(grids) != 1 || grids[1] != "Loading" {
		t.Errorf("expected only the mermaid note queued, got %q", grids)
	}
	if strings.Contains(got, imageSentinel(0)) {
		t.Errorf("math sentinel should be dropped, got %q", got)
	}
}

func TestSubstituteImages(t *testing.T) {
	rendered := "intro line\n  " + imageSentinel(0) + "\noutro line"
	grids := map[int]string{0: "ROW1\nROW2\n"}
	got := substituteImages(rendered, grids)

	want := "intro line\n  ROW1\n  ROW2\noutro line"
	if got != want {
		t.Fatalf("substituteImages:\n got %q\nwant %q", got, want)
	}
}

func TestSubstituteImagesDropsMissing(t *testing.T) {
	rendered := "a\n" + imageSentinel(0) + "\nb"
	got := substituteImages(rendered, map[int]string{0: "X"}) // present
	if !strings.Contains(got, "X") {
		t.Errorf("expected grid substituted, got %q", got)
	}
	// A sentinel with no grid is dropped entirely.
	got2 := substituteImages("a\n"+imageSentinel(5)+"\nb", map[int]string{0: "X"})
	if strings.Contains(got2, "MODSIMG") {
		t.Errorf("unmatched sentinel should be dropped, got %q", got2)
	}
	if got2 != "a\nb" {
		t.Errorf("got %q, want %q", got2, "a\nb")
	}
}
