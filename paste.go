package main

import (
	"fmt"
	"strings"
)

// pasteCollapseLines is the line-count threshold above which a paste is
// collapsed into a placeholder marker instead of inserted verbatim. A paste
// must have MORE than this many lines to be collapsed (so up to and including
// this many lines pastes normally).
const pasteCollapseLines = 3

// pasteLineCount returns the number of lines in s, ignoring a single trailing
// newline so that "a\nb\n" counts as 2 lines rather than 3.
func pasteLineCount(s string) int {
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// registerPaste stores the pasted text and returns a unique placeholder marker
// to insert in its place. Markers read like "[Pasted text +160 lines]"; on the
// rare collision (two pastes with the same line count) a " (k)" suffix is added
// to keep the marker unique within the input.
func (m *Mods) registerPaste(text string) string {
	if m.pastes == nil {
		m.pastes = map[string]string{}
	}
	n := pasteLineCount(text)
	marker := fmt.Sprintf("[Pasted text +%d lines]", n)
	for k := 2; ; k++ {
		if _, exists := m.pastes[marker]; !exists {
			break
		}
		marker = fmt.Sprintf("[Pasted text +%d lines (%d)]", n, k)
	}
	m.pastes[marker] = text
	return marker
}

// insertPaste inserts pasted text into the textarea. Large multi-line pastes
// are collapsed into a placeholder marker (expanded again on submit); smaller
// pastes are inserted verbatim.
func (m *Mods) insertPaste(text string) {
	if text == "" {
		return
	}
	if pasteLineCount(text) <= pasteCollapseLines {
		m.textarea.InsertString(text)
	} else {
		m.textarea.InsertString(m.registerPaste(text))
	}
	m.syncTextareaHeight()
}

// expandPastes replaces any paste placeholder markers in s with their original
// text. Markers that were deleted from the input simply aren't present and so
// are left untouched in the map (cleared on submit).
func (m *Mods) expandPastes(s string) string {
	for marker, text := range m.pastes {
		s = strings.ReplaceAll(s, marker, text)
	}
	return s
}

// clearPastes drops all stored pastes. Called after the input is submitted or
// reset so markers don't leak into a later prompt.
func (m *Mods) clearPastes() {
	m.pastes = nil
}

// cursorOffset returns the cursor's position as a rune offset into the
// textarea's full value.
func (m *Mods) cursorOffset() int {
	val := []rune(m.textarea.Value())
	lines := strings.Split(m.textarea.Value(), "\n")
	row := m.textarea.Line()
	li := m.textarea.LineInfo()
	off := li.StartColumn + li.ColumnOffset // logical column within the row
	for i := 0; i < row && i < len(lines); i++ {
		off += len([]rune(lines[i])) + 1 // +1 for the newline
	}
	if off > len(val) {
		off = len(val)
	}
	return off
}

// setCursorToOffset moves the textarea cursor to the given rune offset. The
// textarea exposes no absolute cursor setter, so we walk to the top-left and
// then step down to the target logical row before setting the column.
func (m *Mods) setCursorToOffset(off int) {
	val := []rune(m.textarea.Value())
	if off > len(val) {
		off = len(val)
	}
	row, col := 0, 0
	for i := 0; i < off; i++ {
		if val[i] == '\n' {
			row++
			col = 0
		} else {
			col++
		}
	}
	// Walk to the absolute beginning (account for soft-wrapped sub-lines).
	for m.textarea.Line() > 0 || m.textarea.LineInfo().RowOffset > 0 {
		m.textarea.CursorUp()
	}
	for m.textarea.Line() < row {
		m.textarea.CursorDown()
	}
	m.textarea.SetCursor(col)
}

// deletePasteMarkerBackward handles backspace/ctrl+w when the cursor sits
// immediately after a paste placeholder marker: it removes the whole marker as
// a single unit and forgets its stored text. Returns false (not consumed) when
// the cursor isn't right after a marker, so normal deletion can proceed.
func (m *Mods) deletePasteMarkerBackward() bool {
	if len(m.pastes) == 0 {
		return false
	}
	val := []rune(m.textarea.Value())
	off := m.cursorOffset()
	before := string(val[:off])

	// Pick the longest matching marker in case one marker is a suffix of
	// another (e.g. the " (k)" disambiguated variants).
	match := ""
	for marker := range m.pastes {
		if strings.HasSuffix(before, marker) && len(marker) > len(match) {
			match = marker
		}
	}
	if match == "" {
		return false
	}

	start := off - len([]rune(match))
	newVal := string(val[:start]) + string(val[off:])
	delete(m.pastes, match)
	m.textarea.SetValue(newVal)
	m.setCursorToOffset(start)
	m.syncTextareaHeight()
	return true
}
