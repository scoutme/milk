package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// clipboardAttachMsg is sent when a clipboard probe successfully extracts
// non-text content and saves it to a temp file.
type clipboardAttachMsg struct {
	path string // temp file path; caller removes after loading
	mime string
	tool string // "xclip" or "wl-paste"
}

// clipboardNoToolMsg is sent when a clipboard probe finds no suitable tool.
type clipboardNoToolMsg struct{}

// probeClipboardCmd returns a tea.Cmd that probes the clipboard off the main
// goroutine and sends the result as a clipboardAttachMsg or clipboardNoToolMsg.
func (m model) probeClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		r, err := probeClipboard()
		if err == errClipboardNoTool {
			return clipboardNoToolMsg{}
		}
		if err != nil {
			// errClipboardNoContent or other — silent, nothing to attach.
			return nil
		}
		path, err := saveClipboardTemp(r.data, r.mimeType)
		if err != nil {
			return nil
		}
		return clipboardAttachMsg{path: path, mime: r.mimeType, tool: r.tool}
	}
}

// clipboardProbeResult holds the result of a clipboard probe attempt.
type clipboardProbeResult struct {
	data     []byte
	mimeType string // e.g. "image/png", "application/pdf"
	tool     string // "xclip" or "wl-paste"
}

// probeClipboard checks whether the system clipboard contains non-text content.
// It detects the environment (Wayland vs X11) and calls wl-paste or xclip
// accordingly. Returns (result, nil) on success, (nil, errClipboardNoTool) when
// no suitable tool is installed, and (nil, errClipboardNoContent) when the
// clipboard holds only text or is empty.
var (
	errClipboardNoTool    = fmt.Errorf("no clipboard tool found (install xclip or wl-paste)")
	errClipboardNoContent = fmt.Errorf("clipboard contains no non-text content")
)

func probeClipboard() (*clipboardProbeResult, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return probeWlPaste()
	}
	if os.Getenv("DISPLAY") != "" {
		return probeXclip()
	}
	// Neither set — try both anyway (e.g. inside WSL with an X server).
	if r, err := probeXclip(); err == nil {
		return r, nil
	}
	return nil, errClipboardNoTool
}

func probeWlPaste() (*clipboardProbeResult, error) {
	if _, err := exec.LookPath("wl-paste"); err != nil {
		return nil, errClipboardNoTool
	}
	// List available MIME types.
	out, err := exec.Command("wl-paste", "--list-types").Output()
	if err != nil {
		return nil, errClipboardNoContent
	}
	mime := pickNonTextMIME(strings.Split(strings.TrimSpace(string(out)), "\n"))
	if mime == "" {
		return nil, errClipboardNoContent
	}
	data, err := exec.Command("wl-paste", "--type", mime).Output()
	if err != nil || len(data) == 0 {
		return nil, errClipboardNoContent
	}
	return &clipboardProbeResult{data: data, mimeType: mime, tool: "wl-paste"}, nil
}

func probeXclip() (*clipboardProbeResult, error) {
	if _, err := exec.LookPath("xclip"); err != nil {
		return nil, errClipboardNoTool
	}
	// List available MIME types.
	out, err := exec.Command("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o").Output()
	if err != nil {
		return nil, errClipboardNoContent
	}
	mime := pickNonTextMIME(strings.Split(strings.TrimSpace(string(out)), "\n"))
	if mime == "" {
		return nil, errClipboardNoContent
	}
	data, err := exec.Command("xclip", "-selection", "clipboard", "-t", mime, "-o").Output()
	if err != nil || len(data) == 0 {
		return nil, errClipboardNoContent
	}
	return &clipboardProbeResult{data: data, mimeType: mime, tool: "xclip"}, nil
}

// pickNonTextMIME selects the best non-text MIME type from a list of candidates.
// Preference order: image/* > application/pdf > other non-text.
// text/* and generic/ambiguous targets (TARGETS, TIMESTAMP, etc.) are skipped.
func pickNonTextMIME(types []string) string {
	var best string
	for _, t := range types {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		// Skip X selection meta-targets and text types.
		if strings.HasPrefix(t, "text/") ||
			t == "TARGETS" || t == "TIMESTAMP" || t == "MULTIPLE" ||
			t == "SAVE_TARGETS" || t == "UTF8_STRING" || t == "STRING" ||
			t == "TEXT" || t == "COMPOUND_TEXT" {
			continue
		}
		if best == "" {
			best = t
		}
		// Prefer image/* and application/pdf over other types.
		if strings.HasPrefix(t, "image/") || t == "application/pdf" {
			best = t
			break
		}
	}
	return best
}

// saveClipboardTemp writes data to a temp file and returns the path.
// The caller is responsible for removing the file after loadAttachment consumes it.
func saveClipboardTemp(data []byte, mimeType string) (string, error) {
	ext := mimeExtension(mimeType)
	f, err := os.CreateTemp("", "milk-paste-*"+ext)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	f.Close()
	return f.Name(), nil
}

// mimeExtension returns a file extension for a MIME type.
func mimeExtension(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	default:
		if strings.HasPrefix(mime, "image/") {
			sub := strings.TrimPrefix(mime, "image/")
			if !bytes.ContainsAny([]byte(sub), "/\\. ") {
				return "." + sub
			}
		}
		return ".bin"
	}
}
