package main

import (
	"bytes"
	"encoding/base64"
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
	// WSL2: Windows clipboard is the canonical source regardless of whether an
	// X/Wayland server is also running. Try powershell.exe first, fall back to
	// the Linux tools only if it finds nothing.
	if isWSL() {
		if r, err := probePowerShell(); err == nil {
			return r, nil
		}
		// powershell found nothing non-text; don't bother with X/Wayland.
		return nil, errClipboardNoContent
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return probeWlPaste()
	}
	if os.Getenv("DISPLAY") != "" {
		return probeXclip()
	}
	// No display env at all — try both Linux tools as a last resort.
	if r, err := probeXclip(); err == nil {
		return r, nil
	}
	if r, err := probeWlPaste(); err == nil {
		return r, nil
	}
	return nil, errClipboardNoTool
}

// isWSL reports whether we are running inside WSL.
func isWSL() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// probePowerShell extracts non-text clipboard content via powershell.exe.
// Supports images (returned as PNG bytes) and file paths.
func probePowerShell() (*clipboardProbeResult, error) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return nil, errClipboardNoTool
	}
	// Try image first: save clipboard bitmap as PNG, encode as base64.
	const psImage = `Add-Type -AssemblyName System.Windows.Forms,System.Drawing; ` +
		`$img = [System.Windows.Forms.Clipboard]::GetImage(); ` +
		`if ($img -eq $null) { exit 1 }; ` +
		`$ms = New-Object System.IO.MemoryStream; ` +
		`$img.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png); ` +
		`[System.Convert]::ToBase64String($ms.ToArray())`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", psImage).Output()
	if err == nil && len(out) > 0 {
		b64 := strings.TrimSpace(string(out))
		data, err := base64.StdEncoding.DecodeString(b64)
		if err == nil && len(data) > 0 {
			return &clipboardProbeResult{data: data, mimeType: "image/png", tool: "powershell"}, nil
		}
	}
	return nil, errClipboardNoContent
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
