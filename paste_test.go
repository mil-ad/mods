package main

import (
	"strings"
	"testing"
)

func TestPasteLineCount(t *testing.T) {
	cases := map[string]int{
		"":            1,
		"one":         1,
		"a\nb":        2,
		"a\nb\n":      2, // trailing newline ignored
		"a\nb\nc\nd":  4,
		"a\nb\nc\nd\n": 4,
	}
	for in, want := range cases {
		if got := pasteLineCount(in); got != want {
			t.Errorf("pasteLineCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestRegisterAndExpand(t *testing.T) {
	m := &Mods{}
	text := strings.Repeat("line\n", 160)
	marker := m.registerPaste(text)
	if marker != "[Pasted text +160 lines]" {
		t.Fatalf("marker = %q", marker)
	}
	// Round-trip: a typed prefix + marker + suffix expands to full text.
	input := "look at this " + marker + " thanks"
	got := m.expandPastes(input)
	want := "look at this " + text + " thanks"
	if got != want {
		t.Fatalf("expandPastes mismatch")
	}
}

func TestRegisterCollision(t *testing.T) {
	m := &Mods{}
	a := strings.Repeat("x\n", 10)
	b := strings.Repeat("y\n", 10) // same line count, different text
	ma := m.registerPaste(a)
	mb := m.registerPaste(b)
	if ma == mb {
		t.Fatalf("collision not disambiguated: %q == %q", ma, mb)
	}
	if m.expandPastes(ma) != a || m.expandPastes(mb) != b {
		t.Fatalf("collided markers expand wrong")
	}
}

func TestInsertPasteThreshold(t *testing.T) {
	m := &Mods{interactive: true}
	m.textarea = newInteractiveTextarea()

	// 3 lines: inserted verbatim, no marker registered.
	m.insertPaste("a\nb\nc")
	if len(m.pastes) != 0 {
		t.Fatalf("3-line paste should not be collapsed, got %d markers", len(m.pastes))
	}
	if m.textarea.Value() != "a\nb\nc" {
		t.Fatalf("verbatim paste value = %q", m.textarea.Value())
	}

	m.textarea.Reset()
	m.clearPastes()

	// 4 lines: collapsed to a marker.
	m.insertPaste("a\nb\nc\nd")
	if len(m.pastes) != 1 {
		t.Fatalf("4-line paste should be collapsed, got %d markers", len(m.pastes))
	}
	if m.textarea.Value() != "[Pasted text +4 lines]" {
		t.Fatalf("collapsed value = %q", m.textarea.Value())
	}
}

func TestDeletePasteMarkerBackward(t *testing.T) {
	m := &Mods{interactive: true}
	m.textarea = newInteractiveTextarea()
	m.textarea.SetWidth(80)

	m.textarea.InsertString("before ")
	m.insertPaste(strings.Repeat("z\n", 50))
	// Cursor is right after the marker now.
	if !strings.Contains(m.textarea.Value(), "[Pasted text +50 lines]") {
		t.Fatalf("marker not present: %q", m.textarea.Value())
	}

	if !m.deletePasteMarkerBackward() {
		t.Fatalf("expected marker deletion to be consumed")
	}
	if m.textarea.Value() != "before " {
		t.Fatalf("after delete value = %q, want %q", m.textarea.Value(), "before ")
	}
	if len(m.pastes) != 0 {
		t.Fatalf("paste not forgotten after delete: %d", len(m.pastes))
	}
	// Cursor should be at the marker's old start (end of "before ") so typing continues there.
	if off := m.cursorOffset(); off != len("before ") {
		t.Fatalf("cursor offset = %d, want %d", off, len("before "))
	}
}

func TestDeleteNotAfterMarker(t *testing.T) {
	m := &Mods{interactive: true}
	m.textarea = newInteractiveTextarea()
	m.insertPaste(strings.Repeat("z\n", 50))
	m.textarea.InsertString("xy") // cursor after "xy", not after a marker
	if m.deletePasteMarkerBackward() {
		t.Fatalf("should not consume delete when cursor is not right after a marker")
	}
}
