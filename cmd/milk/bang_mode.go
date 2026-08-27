package main

import "strings"

// stripBangPrefix reports whether input begins with the "!" direct-execution
// trigger (Claude Code style) and, if so, returns the shell command with the
// marker and surrounding whitespace removed. Used by submitInput to catch
// literal "!"-prefixed input from any source (Telegram, etc.); interactive
// TUI input instead goes through bangMode (see handleKey/handleEnter), which
// consumes the "!" keystroke before it ever reaches the textarea.
func stripBangPrefix(input string) (string, bool) {
	if !strings.HasPrefix(input, "!") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(input, "!")), true
}
