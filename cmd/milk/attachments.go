package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PendingAttachment holds a file staged for inclusion in the next agent turn.
type PendingAttachment struct {
	Path     string // original file path
	Name     string // base filename
	MIMEType string // detected MIME type
	Data     []byte // file contents
}

// isImage reports whether the attachment is an image type.
func (a PendingAttachment) isImage() bool {
	return strings.HasPrefix(a.MIMEType, "image/")
}

// loadAttachment reads path from disk, detects its MIME type, and returns
// a PendingAttachment ready for staging. Returns an error when the file
// cannot be read.
func loadAttachment(path string) (PendingAttachment, error) {
	path = expandHome(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return PendingAttachment{}, fmt.Errorf("reading %q: %w", path, err)
	}
	name := filepath.Base(path)
	mime := detectMIME(path, data)
	return PendingAttachment{Path: path, Name: name, MIMEType: mime, Data: data}, nil
}

// expandHome replaces a leading "~/" with the user's home directory.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// detectMIME returns a best-guess MIME type for the file at path with the
// given contents. It uses the file extension first, then sniffs up to 512
// bytes of content for common magic bytes.
func detectMIME(path string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".go":
		return "text/x-go"
	case ".py":
		return "text/x-python"
	case ".js", ".mjs":
		return "text/javascript"
	case ".ts":
		return "text/typescript"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "text/yaml"
	case ".toml":
		return "text/toml"
	case ".md":
		return "text/markdown"
	case ".txt", ".log":
		return "text/plain"
	case ".sh", ".bash", ".zsh":
		return "text/x-shellscript"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	}

	// Sniff magic bytes.
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	switch {
	case len(sniff) >= 4 && sniff[0] == 0x89 && sniff[1] == 'P' && sniff[2] == 'N' && sniff[3] == 'G':
		return "image/png"
	case len(sniff) >= 3 && sniff[0] == 0xFF && sniff[1] == 0xD8 && sniff[2] == 0xFF:
		return "image/jpeg"
	case len(sniff) >= 6 && string(sniff[:6]) == "GIF87a" || len(sniff) >= 6 && string(sniff[:6]) == "GIF89a":
		return "image/gif"
	case len(sniff) >= 4 && string(sniff[:4]) == "RIFF" && len(sniff) >= 12 && string(sniff[8:12]) == "WEBP":
		return "image/webp"
	case len(sniff) >= 4 && string(sniff[:4]) == "%PDF":
		return "application/pdf"
	}

	// Default to plain text for everything else (most source files).
	return "text/plain"
}

// attachmentContextBlock returns a text block suitable for injection into an
// agent context for text-based attachments. PDF and other binary non-image
// types are noted as binary with a size hint rather than dumped verbatim.
func attachmentContextBlock(a PendingAttachment) string {
	if a.isImage() {
		// Images go through attachmentDataURI, not context blocks.
		return fmt.Sprintf("[attached image: %s — use the data URI provided]\n", a.Name)
	}
	if a.MIMEType == "application/pdf" || isBinaryMIME(a.MIMEType) {
		return fmt.Sprintf("[attached file: %s (binary, %d bytes) — text extraction not supported]\n", a.Name, len(a.Data))
	}
	return fmt.Sprintf("[attached file: %s]\n```\n%s\n```\n", a.Name, string(a.Data))
}

// attachmentDataURI returns a data URI for an image attachment, suitable for
// injection into multipart content or escalation context blocks.
func attachmentDataURI(a PendingAttachment) string {
	enc := base64.StdEncoding.EncodeToString(a.Data)
	return fmt.Sprintf("data:%s;base64,%s", a.MIMEType, enc)
}

// isBinaryMIME reports whether mime indicates a binary (non-text) type.
func isBinaryMIME(mime string) bool {
	return !strings.HasPrefix(mime, "text/") &&
		mime != "application/json" &&
		!strings.HasSuffix(mime, "+json") &&
		!strings.HasSuffix(mime, "+xml")
}

// isLikelyFilePath reports whether s looks like an absolute file path that
// exists on disk. It must start with "/" or "~/", and after home-expansion the
// path must exist.
// unquoteFilePath strips surrounding single quotes added by terminals that
// shell-quote paths on drag-and-drop (e.g. GNOME Terminal pastes '/path/to/file').
func unquoteFilePath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	return s
}

// looksLikeFilePath returns true when s has a file-path prefix — but does NOT
// check whether the path actually exists. Used to decide whether to insert
// @path into the input area when the user declines to attach.
func looksLikeFilePath(s string) bool {
	s = unquoteFilePath(s)
	return strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "~/") ||
		strings.HasPrefix(s, `\\`) // Windows UNC / network path
}

func isLikelyFilePath(s string) bool {
	s = unquoteFilePath(s)
	if !looksLikeFilePath(s) {
		return false
	}
	// Single "/" is not a file attachment.
	if s == "/" {
		return false
	}
	expanded := expandHome(s)
	info, err := os.Stat(expanded)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// attachmentPlaceholder returns the compact placeholder stored in session
// history in place of attachment data.
func attachmentPlaceholder(a PendingAttachment) string {
	return fmt.Sprintf("[attached: %s]", a.Name)
}

// formatAttachmentsForCLI builds the text block injected into the escalation
// context when attachments are present. Images are embedded as data URIs;
// text files are inlined verbatim.
func formatAttachmentsForCLI(attachments []PendingAttachment) string {
	if len(attachments) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Attached files\n\n")
	for _, a := range attachments {
		if a.isImage() {
			fmt.Fprintf(&b, "[attached image: %s]\n%s\n\n", a.Name, attachmentDataURI(a))
		} else {
			b.WriteString(attachmentContextBlock(a))
			b.WriteByte('\n')
		}
	}
	return b.String()
}
