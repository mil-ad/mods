package main

import "strings"

// deleteMarkerBackward is the shared mechanics behind deletePasteMarkerBackward
// and deleteAttachmentMarkerBackward: both paste and attachment placeholders
// are plain marker text glued into the textarea buffer (see paste.go and
// attachment_interactive.go), so backspacing one as a single unit — rather
// than character by character — looks the same regardless of what the marker
// stands for. Removes whichever key in markers is a suffix of the text
// immediately before the cursor, deletes it from markers, and moves the
// cursor to where it started. Returns false (not consumed) when the cursor
// isn't right after a marker, so normal deletion (or another marker kind)
// can proceed.
func deleteMarkerBackward[T any](m *Mods, markers map[string]T) bool {
	if len(markers) == 0 {
		return false
	}
	val := []rune(m.textarea.Value())
	off := m.cursorOffset()
	before := string(val[:off])

	// Pick the longest matching marker in case one marker is a suffix of
	// another (e.g. paste's " (k)" disambiguated variants).
	match := ""
	for marker := range markers {
		if strings.HasSuffix(before, marker) && len(marker) > len(match) {
			match = marker
		}
	}
	if match == "" {
		return false
	}

	start := off - len([]rune(match))
	newVal := string(val[:start]) + string(val[off:])
	delete(markers, match)
	m.textarea.SetValue(newVal)
	m.setCursorToOffset(start)
	m.syncTextareaHeight()
	return true
}
