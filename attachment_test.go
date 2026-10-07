package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mil-ad/mods/internal/proto"
)

// pngMagic is the PNG signature, enough for http.DetectContentType to sniff a
// file as image/png without embedding a whole real image in the tests.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func newTestMods(t *testing.T) *Mods {
	t.Helper()
	m := &Mods{interactive: true}
	m.textarea = newInteractiveTextarea()
	m.textarea.SetWidth(80)
	return m
}

// testAttachment builds an attachment tagged with name so tests can tell which
// one came back from collectAttachments.
func testAttachment(name string) proto.Attachment {
	return proto.Attachment{Data: []byte(name), MediaType: "image/png"}
}

func attachmentNames(atts []proto.Attachment) []string {
	names := make([]string, len(atts))
	for i, att := range atts {
		names[i] = string(att.Data)
	}
	return names
}

func TestInsertAttachmentNumbersSequentially(t *testing.T) {
	m := newTestMods(t)
	m.insertAttachment(testAttachment("a"))
	m.insertAttachment(testAttachment("b"))

	if got, want := m.textarea.Value(), "[Image #1][Image #2]"; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
}

// Deleting every attachment should reset numbering: a later paste reads as
// "[Image #1]", not "[Image #2]" with no #1 in sight.
func TestInsertAttachmentRenumbersAfterDeletingAll(t *testing.T) {
	m := newTestMods(t)
	m.insertAttachment(testAttachment("a"))
	m.insertAttachment(testAttachment("b"))

	// The cursor sits after the last marker, so backspace removes them in turn.
	for i := range 2 {
		if !m.deleteAttachmentMarkerBackward() {
			t.Fatalf("delete %d not consumed", i)
		}
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("value after deleting both = %q, want empty", got)
	}

	m.insertAttachment(testAttachment("c"))
	if got, want := m.textarea.Value(), "[Image #1]"; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
}

// ...but a marker that survived must not have its number reused, or two
// different images would share one marker.
func TestInsertAttachmentSkipsSurvivingNumbers(t *testing.T) {
	m := newTestMods(t)
	m.insertAttachment(testAttachment("a"))
	m.insertAttachment(testAttachment("b"))

	// Delete only "[Image #1]", leaving "[Image #2]" in place.
	m.setCursorToOffset(len("[Image #1]"))
	if !m.deleteAttachmentMarkerBackward() {
		t.Fatal("delete not consumed")
	}
	if got, want := m.textarea.Value(), "[Image #2]"; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}

	m.setCursorToOffset(len("[Image #2]"))
	m.insertAttachment(testAttachment("c"))
	if got, want := m.textarea.Value(), "[Image #2][Image #3]"; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
}

func TestDeleteAttachmentMarkerBackward(t *testing.T) {
	m := newTestMods(t)
	m.textarea.InsertString("before ")
	m.insertAttachment(testAttachment("a"))

	if !m.deleteAttachmentMarkerBackward() {
		t.Fatal("expected marker deletion to be consumed")
	}
	if got, want := m.textarea.Value(), "before "; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
	if len(m.attachments) != 0 {
		t.Fatalf("attachment not forgotten: %d left", len(m.attachments))
	}
	// Cursor lands where the marker started so typing continues there.
	if off, want := m.cursorOffset(), len("before "); off != want {
		t.Fatalf("cursor offset = %d, want %d", off, want)
	}
}

func TestDeleteAttachmentMarkerBackwardNotAfterMarker(t *testing.T) {
	m := newTestMods(t)
	m.insertAttachment(testAttachment("a"))
	m.textarea.InsertString("xy") // cursor after "xy", not after a marker

	if m.deleteAttachmentMarkerBackward() {
		t.Fatal("should not consume delete when cursor is not right after a marker")
	}
}

func TestCollectAttachments(t *testing.T) {
	m := newTestMods(t)
	m.insertAttachment(testAttachment("a"))
	m.textarea.InsertString(" and ")
	m.insertAttachment(testAttachment("b"))

	for _, tt := range []struct {
		name string
		text string
		want []string
	}{
		{"as typed", m.textarea.Value(), []string{"a", "b"}},
		{"in the order they appear", "[Image #2] then [Image #1]", []string{"b", "a"}},
		{"one marker deleted", "just [Image #2]", []string{"b"}},
		{"all markers deleted", "no markers left", nil},
		{"repeated marker sent once", "[Image #1] [Image #1]", []string{"a"}},
		{"unknown marker ignored", "[Image #9]", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := attachmentNames(m.collectAttachments(tt.text))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestClearAttachments(t *testing.T) {
	m := newTestMods(t)
	m.insertAttachment(testAttachment("a"))
	m.clearAttachments()

	if got := m.collectAttachments("[Image #1]"); got != nil {
		t.Fatalf("collectAttachments after clear = %v, want nil", got)
	}
}

func TestLooksLikeImagePath(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(png, pngMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "dir.png")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		in   string
		want string // "" means not an image path
	}{
		{"plain path", png, png},
		// Terminals may wrap or decorate the dropped path.
		{"trailing newline", png + "\n", png},
		{"quoted", `"` + png + `"`, png},
		{"file:// url", "file://" + png, png},
		{"non-image extension", text, ""},
		{"missing file", filepath.Join(dir, "gone.png"), ""},
		{"directory", subdir, ""},
		{"multiple paths", png + "\n" + png, ""},
		{"empty", "", ""},
		// Ordinary pasted prose must keep going to the text-paste path.
		{"prose", "look at this screenshot.png I took", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := looksLikeImagePath(tt.in)
			if tt.want == "" {
				if ok {
					t.Fatalf("looksLikeImagePath(%q) = %q, true; want false", tt.in, got)
				}
				return
			}
			if !ok || got != tt.want {
				t.Fatalf("looksLikeImagePath(%q) = %q, %v; want %q, true", tt.in, got, ok, tt.want)
			}
		})
	}
}

func TestReadImageFile(t *testing.T) {
	dir := t.TempDir()

	// Extension wins: media type comes from it without inspecting content.
	jpg := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(jpg, pngMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	att, err := readImageFile(jpg)
	if err != nil {
		t.Fatalf("readImageFile(%q): %v", jpg, err)
	}
	if att.MediaType != "image/jpeg" {
		t.Errorf("MediaType = %q, want image/jpeg", att.MediaType)
	}
	if len(att.Data) == 0 || att.URL != "" {
		t.Errorf("want inline data and no URL, got %d bytes / URL %q", len(att.Data), att.URL)
	}

	// No usable extension: fall back to sniffing the content.
	noExt := filepath.Join(dir, "clipboard-dump")
	if err := os.WriteFile(noExt, pngMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	att, err = readImageFile(noExt)
	if err != nil {
		t.Fatalf("readImageFile(%q): %v", noExt, err)
	}
	if att.MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", att.MediaType)
	}

	// Not an image at all: rejected rather than sent as a broken attachment.
	text := filepath.Join(dir, "notes")
	if err := os.WriteFile(text, []byte("just some text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageFile(text); err == nil {
		t.Error("expected an error for a non-image file")
	}
}

func TestAPISupportsAttachments(t *testing.T) {
	for api, want := range map[string]bool{
		"openai":    true,
		"anthropic": true,
		"azure":     true,
		"azure-ad":  true,
		"localai":   true, // OpenAI-compatible endpoints go through the same client
		"google":    false,
		"cohere":    false,
		"ollama":    false,
	} {
		if got := apiSupportsAttachments(api); got != want {
			t.Errorf("apiSupportsAttachments(%q) = %v, want %v", api, got, want)
		}
	}
}
