package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mil-ad/mods/internal/proto"
)

// attachmentExtMediaTypes maps supported image file extensions to their media
// type, used before falling back to content-sniffing.
var attachmentExtMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// resolveAttachment turns a --attachment flag value (a local path or an
// http(s) URL) into a proto.Attachment. URLs are validated with a HEAD
// request only — both Anthropic and OpenAI accept a plain URL directly, so
// there's no need to download the image client-side.
func resolveAttachment(value string) (proto.Attachment, error) {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		resp, err := http.Head(value) //nolint:gosec,noctx
		if err != nil {
			return proto.Attachment{}, fmt.Errorf("could not reach %s: %w", value, err)
		}
		_ = resp.Body.Close()
		mediaType := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(mediaType, "image/") {
			return proto.Attachment{}, fmt.Errorf("%s does not look like an image (content-type %q)", value, mediaType)
		}
		return proto.Attachment{URL: value, MediaType: mediaType}, nil
	}

	info, err := os.Stat(value)
	if err != nil {
		return proto.Attachment{}, fmt.Errorf("attachment %s: %w", value, err)
	}
	if info.IsDir() {
		return proto.Attachment{}, fmt.Errorf("attachment %s is a directory", value)
	}
	return readImageFile(value)
}

// resolveAttachments resolves each --attachment flag value in order.
func resolveAttachments(values []string) ([]proto.Attachment, error) {
	attachments := make([]proto.Attachment, 0, len(values))
	for _, v := range values {
		att, err := resolveAttachment(v)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, att)
	}
	return attachments, nil
}

// readImageFile reads path and determines its media type from its extension,
// falling back to content-sniffing. Used for both --attachment paths and
// drag-and-dropped file paths in interactive mode.
func readImageFile(path string) (proto.Attachment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return proto.Attachment{}, fmt.Errorf("could not read %s: %w", path, err)
	}
	mediaType, ok := attachmentExtMediaTypes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		mediaType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return proto.Attachment{}, fmt.Errorf("%s does not look like a supported image type (detected %q)", path, mediaType)
	}
	return proto.Attachment{Data: data, MediaType: mediaType}, nil
}
