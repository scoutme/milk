package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const (
	titleBase       = "milk"
	titleSeparator  = " · "
	titleNeedsInput = "needs input"
	titleWorking    = "working"
	titleIdle       = "idle"

	// State glyphs ride before the state word, plain text like the rest of
	// the title (ADR-0076): idle is a solid dot (alive, listening), needs
	// input is "?" — the status bar's pending-ask convention — and working
	// reuses the braille spinner, so the tab visibly moves exactly like the
	// in-TUI spinner (issue #367).
	titleGlyphIdle = "●"
	titleGlyphAsk  = "?"
)

// windowTitle is the terminal tab title: the app, the active agent and —
// always — the TUI state as `<glyph> <word>`: `● idle`, `⠋ working`,
// `? needs input` (permission / elicitation / wizard). One glance at the tab
// bar answers "is that session done, busy, or waiting on me?" without
// switching to it; idle being the quiet default keeps the title finite and
// scannable. The busy glyph rotates on the existing 80ms spinner tick (no
// extra timer), and bubbletea v2 writes the OSC title only when the string
// changes — a settled state costs zero title writes (issue #367). Plain text
// only, escape sequences would corrupt the title.
func (m *model) windowTitle() string {
	if m.st == nil || m.st.sess == nil {
		return titleBase
	}
	parts := []string{titleBase}
	if agent := strings.TrimSpace(ansi.Strip(agentLabel(m.st))); agent != "" {
		parts = append(parts, agent)
	}
	switch {
	case m.hasPendingPrompt():
		parts = append(parts, titleGlyphAsk+" "+titleNeedsInput)
	case m.busy:
		parts = append(parts, spinnerFrames[m.spinnerFrame%len(spinnerFrames)]+" "+titleWorking)
	default:
		parts = append(parts, titleGlyphIdle+" "+titleIdle)
	}
	return strings.Join(parts, titleSeparator)
}
