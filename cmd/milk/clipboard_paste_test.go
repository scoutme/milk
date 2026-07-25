package main

import (
	"strings"
	"testing"
)

// ── pickNonTextMIME ──────────────────────────────────────────────────────────

func TestPickNonTextMIME_PrefersPNG(t *testing.T) {
	types := []string{"TARGETS", "TIMESTAMP", "UTF8_STRING", "image/png", "image/bmp"}
	got := pickNonTextMIME(types)
	if got != "image/png" {
		t.Errorf("expected image/png, got %q", got)
	}
}

func TestPickNonTextMIME_PrefersPDF(t *testing.T) {
	types := []string{"TARGETS", "application/octet-stream", "application/pdf"}
	got := pickNonTextMIME(types)
	if got != "application/pdf" {
		t.Errorf("expected application/pdf, got %q", got)
	}
}

func TestPickNonTextMIME_SkipsTextTypes(t *testing.T) {
	types := []string{"text/plain", "text/html", "UTF8_STRING", "STRING", "TEXT"}
	got := pickNonTextMIME(types)
	if got != "" {
		t.Errorf("expected empty (all text), got %q", got)
	}
}

func TestPickNonTextMIME_SkipsXSelectionTargets(t *testing.T) {
	types := []string{"TARGETS", "TIMESTAMP", "MULTIPLE", "SAVE_TARGETS", "COMPOUND_TEXT"}
	got := pickNonTextMIME(types)
	if got != "" {
		t.Errorf("expected empty (all meta-targets), got %q", got)
	}
}

func TestPickNonTextMIME_FallsBackToGenericBinary(t *testing.T) {
	types := []string{"TARGETS", "application/octet-stream"}
	got := pickNonTextMIME(types)
	if got != "application/octet-stream" {
		t.Errorf("expected application/octet-stream, got %q", got)
	}
}

func TestPickNonTextMIME_EmptyList(t *testing.T) {
	got := pickNonTextMIME(nil)
	if got != "" {
		t.Errorf("expected empty for nil list, got %q", got)
	}
}

func TestPickNonTextMIME_AllEmpty(t *testing.T) {
	got := pickNonTextMIME([]string{"", "  ", ""})
	if got != "" {
		t.Errorf("expected empty for blank-only list, got %q", got)
	}
}

func TestPickNonTextMIME_ImageBeforeOctetStream(t *testing.T) {
	// image/* should beat application/octet-stream even when octet-stream comes first.
	types := []string{"application/octet-stream", "image/jpeg"}
	got := pickNonTextMIME(types)
	if got != "image/jpeg" {
		t.Errorf("expected image/jpeg (preferred over octet-stream), got %q", got)
	}
}

// ── mimeExtension ────────────────────────────────────────────────────────────

func TestMimeExtension_KnownTypes(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"image/png", ".png"},
		{"image/jpeg", ".jpg"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"image/svg+xml", ".svg"},
		{"application/pdf", ".pdf"},
	}
	for _, tc := range cases {
		got := mimeExtension(tc.mime)
		if got != tc.want {
			t.Errorf("mimeExtension(%q): want %q, got %q", tc.mime, tc.want, got)
		}
	}
}

func TestMimeExtension_UnknownImageSubtype(t *testing.T) {
	// image/tiff → .tiff (derived from sub-type)
	got := mimeExtension("image/tiff")
	if got != ".tiff" {
		t.Errorf("mimeExtension(image/tiff): want .tiff, got %q", got)
	}
}

func TestMimeExtension_UnknownBinary(t *testing.T) {
	got := mimeExtension("application/octet-stream")
	if got != ".bin" {
		t.Errorf("mimeExtension(application/octet-stream): want .bin, got %q", got)
	}
}

func TestMimeExtension_ImageSubtypeWithSlash(t *testing.T) {
	// Pathological: "image/foo/bar" should fall back to .bin, not ".foo/bar".
	got := mimeExtension("image/foo/bar")
	if strings.Contains(got, "/") {
		t.Errorf("mimeExtension should never return extension with slash, got %q", got)
	}
}

// ── saveClipboardTemp ────────────────────────────────────────────────────────

func TestSaveClipboardTemp_WritesAndReturnsPath(t *testing.T) {
	data := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	path, err := saveClipboardTemp(data, "image/png")
	if err != nil {
		t.Fatalf("saveClipboardTemp: %v", err)
	}
	// Verify extension.
	if !strings.HasSuffix(path, ".png") {
		t.Errorf("expected .png suffix, got %q", path)
	}
	// Verify the saved file is a valid attachment.
	a, err := loadAttachment(path)
	if err != nil {
		t.Fatalf("loadAttachment on temp file: %v", err)
	}
	if a.MIMEType != "image/png" {
		t.Errorf("MIMEType: want image/png, got %q", a.MIMEType)
	}
	if !a.isImage() {
		t.Error("isImage(): expected true for PNG temp file")
	}
}

func TestSaveClipboardTemp_PDFExtension(t *testing.T) {
	data := []byte("%PDF-1.4 minimal")
	path, err := saveClipboardTemp(data, "application/pdf")
	if err != nil {
		t.Fatalf("saveClipboardTemp: %v", err)
	}
	if !strings.HasSuffix(path, ".pdf") {
		t.Errorf("expected .pdf suffix, got %q", path)
	}
}

func TestSaveClipboardTemp_UnknownMIMEGetsBinExtension(t *testing.T) {
	data := []byte{0x00, 0x01, 0x02}
	path, err := saveClipboardTemp(data, "application/octet-stream")
	if err != nil {
		t.Fatalf("saveClipboardTemp: %v", err)
	}
	if !strings.HasSuffix(path, ".bin") {
		t.Errorf("expected .bin suffix for octet-stream, got %q", path)
	}
}
