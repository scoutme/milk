package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── detectMIME ──────────────────────────────────────────────────────────────

func TestDetectMIME_ByExtension(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"photo.png", "image/png"},
		{"photo.jpg", "image/jpeg"},
		{"photo.jpeg", "image/jpeg"},
		{"anim.gif", "image/gif"},
		{"img.webp", "image/webp"},
		{"doc.pdf", "application/pdf"},
		{"readme.txt", "text/plain"},
		{"readme.md", "text/markdown"},
		{"data.json", "application/json"},
		{"cfg.yaml", "text/yaml"},
		{"cfg.yml", "text/yaml"},
		{"code.go", "text/x-go"},
		{"script.py", "text/x-python"},
		{"app.js", "text/javascript"},
		{"app.ts", "text/typescript"},
	}
	for _, tc := range cases {
		got := detectMIME(tc.path, nil)
		if got != tc.want {
			t.Errorf("detectMIME(%q): want %q, got %q", tc.path, tc.want, got)
		}
	}
}

func TestDetectMIME_ByMagicBytes_PNG(t *testing.T) {
	// PNG magic: 0x89 P N G
	data := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	got := detectMIME("unknown", data)
	if got != "image/png" {
		t.Errorf("PNG magic: expected image/png, got %q", got)
	}
}

func TestDetectMIME_ByMagicBytes_JPEG(t *testing.T) {
	data := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	got := detectMIME("unknown", data)
	if got != "image/jpeg" {
		t.Errorf("JPEG magic: expected image/jpeg, got %q", got)
	}
}

func TestDetectMIME_ByMagicBytes_PDF(t *testing.T) {
	data := []byte("%PDF-1.4 rest of header")
	got := detectMIME("unknown", data)
	if got != "application/pdf" {
		t.Errorf("PDF magic: expected application/pdf, got %q", got)
	}
}

func TestDetectMIME_ByMagicBytes_GIF(t *testing.T) {
	data := []byte("GIF89a" + strings.Repeat("\x00", 10))
	got := detectMIME("unknown", []byte(data))
	if got != "image/gif" {
		t.Errorf("GIF magic: expected image/gif, got %q", got)
	}
}

func TestDetectMIME_DefaultsToTextPlain(t *testing.T) {
	got := detectMIME("unknownfile", []byte("some random text content"))
	if got != "text/plain" {
		t.Errorf("unknown file: expected text/plain, got %q", got)
	}
}

// ── loadAttachment ──────────────────────────────────────────────────────────

func TestLoadAttachment_TextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	content := "hello world\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := loadAttachment(path)
	if err != nil {
		t.Fatalf("loadAttachment: %v", err)
	}
	if a.Name != "hello.txt" {
		t.Errorf("Name: want hello.txt, got %q", a.Name)
	}
	if string(a.Data) != content {
		t.Errorf("Data: want %q, got %q", content, a.Data)
	}
	if a.MIMEType != "text/plain" {
		t.Errorf("MIMEType: want text/plain, got %q", a.MIMEType)
	}
	if a.isImage() {
		t.Error("isImage(): expected false for text file")
	}
}

func TestLoadAttachment_ImageFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	// Minimal PNG header magic bytes.
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	if err := os.WriteFile(path, pngMagic, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := loadAttachment(path)
	if err != nil {
		t.Fatalf("loadAttachment: %v", err)
	}
	if a.MIMEType != "image/png" {
		t.Errorf("MIMEType: want image/png, got %q", a.MIMEType)
	}
	if !a.isImage() {
		t.Error("isImage(): expected true for PNG")
	}
}

func TestLoadAttachment_MissingFile(t *testing.T) {
	_, err := loadAttachment("/nonexistent/path/file.txt")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

// ── attachmentContextBlock ──────────────────────────────────────────────────

func TestAttachmentContextBlock_TextFile(t *testing.T) {
	a := PendingAttachment{
		Name:     "notes.txt",
		MIMEType: "text/plain",
		Data:     []byte("line one\nline two\n"),
	}
	block := attachmentContextBlock(a)
	if !strings.Contains(block, "[attached file: notes.txt]") {
		t.Errorf("block missing file header: %q", block)
	}
	if !strings.Contains(block, "line one") {
		t.Errorf("block missing file content: %q", block)
	}
	if !strings.Contains(block, "```") {
		t.Errorf("block missing fenced code block: %q", block)
	}
}

func TestAttachmentContextBlock_PDF(t *testing.T) {
	a := PendingAttachment{
		Name:     "doc.pdf",
		MIMEType: "application/pdf",
		Data:     []byte("%PDF-1.4"),
	}
	block := attachmentContextBlock(a)
	if !strings.Contains(block, "doc.pdf") {
		t.Errorf("block missing filename: %q", block)
	}
	if !strings.Contains(block, "binary") {
		t.Errorf("block should note binary type: %q", block)
	}
}

// ── attachmentDataURI ───────────────────────────────────────────────────────

func TestAttachmentDataURI_PNG(t *testing.T) {
	a := PendingAttachment{
		Name:     "img.png",
		MIMEType: "image/png",
		Data:     []byte{0x89, 'P', 'N', 'G'},
	}
	uri := attachmentDataURI(a)
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Errorf("expected data URI prefix, got %q", uri[:min(len(uri), 40)])
	}
}

func TestAttachmentDataURI_JPEG(t *testing.T) {
	a := PendingAttachment{
		Name:     "photo.jpg",
		MIMEType: "image/jpeg",
		Data:     []byte{0xFF, 0xD8, 0xFF},
	}
	uri := attachmentDataURI(a)
	if !strings.HasPrefix(uri, "data:image/jpeg;base64,") {
		t.Errorf("expected data URI prefix, got %q", uri[:min(len(uri), 40)])
	}
}

// ── unquoteFilePath ─────────────────────────────────────────────────────────

func TestUnquoteFilePath_SingleQuotes(t *testing.T) {
	if got := unquoteFilePath("'/tmp/file.png'"); got != "/tmp/file.png" {
		t.Errorf("got %q, want /tmp/file.png", got)
	}
}

func TestUnquoteFilePath_NoQuotes(t *testing.T) {
	if got := unquoteFilePath("/tmp/file.png"); got != "/tmp/file.png" {
		t.Errorf("got %q, want /tmp/file.png", got)
	}
}

func TestUnquoteFilePath_Whitespace(t *testing.T) {
	if got := unquoteFilePath("  '/tmp/file.png'  "); got != "/tmp/file.png" {
		t.Errorf("got %q, want /tmp/file.png", got)
	}
}

// ── looksLikeFilePath ───────────────────────────────────────────────────────

func TestLooksLikeFilePath_Absolute(t *testing.T) {
	if !looksLikeFilePath("/nonexistent/path") {
		t.Error("want true for /... path")
	}
}

func TestLooksLikeFilePath_Home(t *testing.T) {
	if !looksLikeFilePath("~/nonexistent") {
		t.Error("want true for ~/... path")
	}
}

func TestLooksLikeFilePath_UNC(t *testing.T) {
	if !looksLikeFilePath(`\\server\share\file.txt`) {
		t.Error("want true for UNC \\\\server\\share path")
	}
}

func TestLooksLikeFilePath_Quoted(t *testing.T) {
	if !looksLikeFilePath("'/tmp/file.png'") {
		t.Error("want true for single-quoted path")
	}
}

func TestLooksLikeFilePath_Relative(t *testing.T) {
	if looksLikeFilePath("relative/path.txt") {
		t.Error("want false for relative path")
	}
}

func TestLooksLikeFilePath_PlainText(t *testing.T) {
	if looksLikeFilePath("hello world") {
		t.Error("want false for plain text")
	}
}

// ── isLikelyFilePath ────────────────────────────────────────────────────────

func TestIsLikelyFilePath_ExistingAbsolute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isLikelyFilePath(path) {
		t.Errorf("isLikelyFilePath(%q): expected true for existing file", path)
	}
}

func TestIsLikelyFilePath_NonExistent(t *testing.T) {
	if isLikelyFilePath("/nonexistent/path/no/file.txt") {
		t.Error("isLikelyFilePath: expected false for non-existent path")
	}
}

func TestIsLikelyFilePath_Directory(t *testing.T) {
	dir := t.TempDir()
	if isLikelyFilePath(dir) {
		t.Errorf("isLikelyFilePath(%q): expected false for directory", dir)
	}
}

func TestIsLikelyFilePath_RelativePath(t *testing.T) {
	if isLikelyFilePath("relative/path.txt") {
		t.Error("isLikelyFilePath: expected false for relative path")
	}
}

func TestIsLikelyFilePath_JustSlash(t *testing.T) {
	if isLikelyFilePath("/") {
		t.Error("isLikelyFilePath: expected false for bare /")
	}
}

func TestIsLikelyFilePath_Whitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Leading/trailing whitespace should still match after TrimSpace.
	if !isLikelyFilePath("  " + path + "  ") {
		t.Errorf("isLikelyFilePath with whitespace: expected true for existing file %q", path)
	}
}

func TestIsLikelyFilePath_SingleQuoted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Terminal drag-and-drop pastes the path in single quotes.
	if !isLikelyFilePath("'" + path + "'") {
		t.Errorf("isLikelyFilePath: expected true for single-quoted existing file %q", path)
	}
}

// ── attachmentPlaceholder ───────────────────────────────────────────────────

func TestAttachmentPlaceholder(t *testing.T) {
	a := PendingAttachment{Name: "screenshot.png"}
	got := attachmentPlaceholder(a)
	want := "[attached: screenshot.png]"
	if got != want {
		t.Errorf("attachmentPlaceholder: want %q, got %q", want, got)
	}
}

// min is available in Go 1.21+ builtins; replicate for compat with older test tools.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
