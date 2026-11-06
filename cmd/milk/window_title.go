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

	// State glyphs ride *before the agent name*, replacing the app-name
	// separator (so a long agent name can push the state word out of a
	// truncated tab title but never the glyph — and the layout mirrors the
	// status bar, where the spinner precedes the agent). Plain text like
	// the rest of the title (ADR-0076): idle is a solid dot (alive,
	// listening), needs input is "?" — the status bar's pending-ask
	// convention — and working reuses the braille spinner, so the tab
	// visibly moves exactly like the in-TUI spinner (issue #367).
	titleGlyphIdle = "●"
	titleGlyphAsk  = "?"
)

// windowTitle is the terminal tab title: `milk <glyph> <agent> · <state>`
// — the glyph replaces the separator after the app name, sitting right
// before the agent like the status bar's spinner, and the state word trails
// (`● … idle`, `⠋ … working`, `? … needs input` for permission / elicitation
// / wizard). One glance at the tab bar answers "is that session done, busy,
// or waiting on me?" without switching to it; idle being the quiet default
// keeps the title finite and scannable. The busy glyph rotates on the
// existing 80ms spinner tick (no extra timer), and bubbletea v2 writes the
// OSC title only when the string changes — a settled state costs zero title
// writes (issue #367). Plain text only, escape sequences would corrupt the
// title.
func (m *model) windowTitle() string {
	if m.st == nil || m.st.sess == nil {
		return titleBase
	}
	glyph, word := titleGlyphIdle, titleIdle
	switch {
	case m.hasPendingPrompt():
		glyph, word = titleGlyphAsk, titleNeedsInput
	case m.busy:
		glyph, word = spinnerFrames[m.spinnerFrame%len(spinnerFrames)], titleWorking
	}
	rest := word
	if agent := strings.TrimSpace(ansi.Strip(agentLabel(m.st))); agent != "" {
		rest = agent + titleSeparator + word
	}
	return titleBase + " " + glyph + " " + rest
}
