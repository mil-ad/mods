package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mil-ad/mods/internal/proto"
)

// imageMarkerRe matches an attachment placeholder marker, e.g. "[Image #2]".
var imageMarkerRe = regexp.MustCompile(`\[Image #(\d+)\]`)

// registerAttachment stores att and returns a unique placeholder marker to
// insert in its place, e.g. "[Image #1]", "[Image #2]". Numbering is based on
// the highest marker number currently present in the textarea, not a
// never-reset counter, so pasting after deleting all attachments starts back
// at "[Image #1]" instead of leaving a gap.
func (m *Mods) registerAttachment(att proto.Attachment) string {
	if m.attachments == nil {
		m.attachments = map[string]proto.Attachment{}
	}
	next := 1
	for _, match := range imageMarkerRe.FindAllStringSubmatch(m.textarea.Value(), -1) {
		if n, err := strconv.Atoi(match[1]); err == nil && n >= next {
			next = n + 1
		}
	}
	marker := fmt.Sprintf("[Image #%d]", next)
	m.attachments[marker] = att
	return marker
}

// insertAttachment inserts an attachment placeholder marker into the
// textarea. Unlike paste markers, this marker is never stripped from the
// text before sending (see collectAttachments) — it gives the model useful
// anchor text next to the image, and means history/browse-mode rendering
// shows it for free.
func (m *Mods) insertAttachment(att proto.Attachment) {
	marker := m.registerAttachment(att)
	m.textarea.InsertString(marker)
	m.syncTextareaHeight()
}

// insertClipboardImage attaches an image from the OS clipboard, reporting
// whether one was there. Callers fall back to a text paste when it wasn't.
func (m *Mods) insertClipboardImage() bool {
	data, ok := readClipboardImage()
	if !ok {
		return false
	}
	m.insertAttachment(proto.Attachment{Data: data, MediaType: clipboardImageMediaType})
	return true
}

// insertDroppedImage attaches the image at the path in pasted, reporting
// whether it did. Terminals deliver a drag-and-dropped file as a bracketed
// paste of its path, so this is fed the pasted text: anything that isn't a
// readable image path is left for normal text-paste handling.
func (m *Mods) insertDroppedImage(pasted string) bool {
	path, ok := looksLikeImagePath(pasted)
	if !ok {
		return false
	}
	att, err := readImageFile(path)
	if err != nil {
		return false
	}
	m.insertAttachment(att)
	return true
}

// looksLikeImagePath reports whether s is the path of a supported image file,
// returning it cleaned up. Terminals vary in how they decorate a dropped path
// (surrounding quotes, a file:// prefix, a trailing newline), so those are
// trimmed before checking that the path really points at an image.
func looksLikeImagePath(s string) (path string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return "", false
	}
	s = strings.Trim(s, `'"`)
	s = strings.TrimPrefix(s, "file://")
	if _, ok := attachmentExtMediaTypes[strings.ToLower(filepath.Ext(s))]; !ok {
		return "", false
	}
	info, err := os.Stat(s)
	if err != nil || info.IsDir() {
		return "", false
	}
	return s, true
}

// collectAttachments returns the attachments whose markers still appear in s,
// in the order they appear, skipping duplicates. Markers the user deleted are
// simply absent from s and so are dropped. Unlike expandPastes, the text
// itself is left unchanged: the marker stays in the prompt as anchor text for
// the image blocks sent alongside it.
func (m *Mods) collectAttachments(s string) []proto.Attachment {
	if len(m.attachments) == 0 {
		return nil
	}
	var atts []proto.Attachment
	seen := map[string]bool{}
	for _, marker := range imageMarkerRe.FindAllString(s, -1) {
		att, ok := m.attachments[marker]
		if !ok || seen[marker] {
			continue
		}
		seen[marker] = true
		atts = append(atts, att)
	}
	return atts
}

// clearAttachments drops all stored attachments. Called after the input is
// submitted or reset so markers don't leak into a later prompt.
func (m *Mods) clearAttachments() {
	m.attachments = nil
}

// deleteAttachmentMarkerBackward handles backspace/ctrl+w when the cursor
// sits immediately after an attachment placeholder marker: it removes the
// whole marker as a single unit and forgets its stored attachment. Returns
// false (not consumed) when the cursor isn't right after a marker, so normal
// deletion (or paste-marker deletion) can proceed.
func (m *Mods) deleteAttachmentMarkerBackward() bool {
	return deleteMarkerBackward(m, m.attachments)
}
