// Package ansi provides a shared, TTY-gated ANSI styling helper for packages
// that cannot import cmd/milk's own colorize() (internal/agent/local and
// friends sit below cmd/milk in the import graph). cmd/milk keeps its own
// colorize()/dim() for in-package use; this package exists only for the
// lower-level packages that need the same behavior.
package ansi

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// IsTTY reports whether stdout is a real terminal, checked once at process
// start — matches cmd/milk/ansi.go's isTTY so behavior is identical when the
// TUI is attached, and ANSI-free when piped (e.g. one-shot CLI output
// redirected to a file or `jq`).
var IsTTY = term.IsTerminal(int(os.Stdout.Fd()))

const (
	CodeReset = "\033[0m"
	CodeDim   = "\033[2m"
)

// Colorize wraps s in code/CodeReset, closing the reset before any embedded
// newline rather than after it — an SGR reset placed after a newline byte
// leaves the color "open" for the terminal until some later, unrelated
// escape happens to cancel it. Returns s unchanged when not attached to a
// real terminal. Mirrors cmd/milk/ansi.go's colorize() byte-for-byte.
func Colorize(s, code string) string {
	if !IsTTY || s == "" {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	var out strings.Builder
	for _, line := range lines {
		if line == "" {
			continue
		}
		if body, ok := strings.CutSuffix(line, "\n"); ok {
			if body != "" {
				out.WriteString(code)
				out.WriteString(body)
				out.WriteString(CodeReset)
			}
			out.WriteByte('\n')
			continue
		}
		out.WriteString(code)
		out.WriteString(line)
		out.WriteString(CodeReset)
	}
	return out.String()
}

// Dim wraps s in ANSI dim styling, or returns it unchanged when not attached
// to a real terminal.
func Dim(s string) string { return Colorize(s, CodeDim) }
