package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/scoutme/milk/internal/theme"
)

// Theme carousel (plan PR 2): bare /theme opens a centered flyover over the
// live TUI — ←/→ (or ↑↓, or the mouse wheel) cycle the registry with the
// whole screen repainting in the candidate skin, enter applies and persists,
// and any other key closes the carousel and restores the original theme
// (terminal palette included), so a browse can never strand the terminal in
// a half-painted skin. No typed filtering here on purpose: the carousel is
// about eyes-on comparison, the name argument (with tab completion) is the
// precise path.
//
// themeCarouselState is the open browse flyover. names is a registry
// snapshot (the listing order is stable while open), idx the candidate,
// orig the theme active at open (what cancel restores).
type themeCarouselState struct {
	names []string
	idx   int
	orig  string
}

// themeCarouselCandidate is the name currently previewed.
func (w *themeCarouselState) candidate() string {
	if w == nil || len(w.names) == 0 {
		return ""
	}
	return w.names[w.idx]
}

// openThemeCarousel arms the live-preview flyover behind bare /theme: browse
// the registry with the whole TUI repainting in the candidate skin, enter
// applies and persists, any other key restores and closes. The transcript
// keeps the one-line record as the audit trail (the picker precedent).
// Terminals too narrow for the box fall back to the plain listing — keys
// must never feed an overlay nobody can see.
func (m model) openThemeCarousel() (tea.Model, tea.Cmd) {
	names := themeRegistry.Names()
	if boxWidth := min(paletteMaxWidth, m.mainWidth()-4); boxWidth < paletteMinWidth || len(names) == 0 {
		m.appendTranscript(fmt.Sprintf("%s theme: %s  (use /theme <name>|list)\n", milkTag(), bold(th.name)))
		return m, nil
	}
	idx := 0
	for i, n := range names {
		if n == th.name {
			idx = i
			break
		}
	}
	m.pendingThemeCarousel = &themeCarouselState{names: names, idx: idx, orig: th.name}
	m.appendTranscript(fmt.Sprintf("%s theme: %s  (%d themes — ←/→ or wheel browse, enter apply)\n", milkTag(), bold(th.name), len(names)))
	m.refreshPrompt()
	m.syncLayout()
	return m, nil
}

// handleThemeCarouselKey routes the open carousel's keys. Arrows cycle
// (wrapping — browsing loops), enter applies, every other key cancels —
// including esc and ctrl+c: closing the browse IS the ctrl+c semantics while
// the flyover is up (the quit path stays reachable after one keypress).
func (m model) handleThemeCarouselKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	w := m.pendingThemeCarousel
	if w == nil {
		return m, nil
	}
	switch msg.String() {
	case "left", "up", "shift+tab", "ctrl+p":
		return m.themeCarouselStep(-1)
	case "right", "down", "tab", "ctrl+n":
		return m.themeCarouselStep(+1)
	case "enter", "ctrl+m":
		return m.themeCarouselConfirm()
	}
	return m.themeCarouselClose()
}

// themeCarouselStep cycles to the adjacent candidate and live-previews it.
func (m model) themeCarouselStep(delta int) (tea.Model, tea.Cmd) {
	w := m.pendingThemeCarousel
	if w == nil || len(w.names) == 0 {
		return m, nil
	}
	w.idx = (w.idx + delta + len(w.names)) % len(w.names)
	m.themeCarouselPreview(w.candidate())
	return m, nil
}

// themeCarouselPreview runs the live switch path minus persistence: the
// candidate is applied (styles rebuilt, terminal palette pushed, transcript
// re-colorized) so the whole TUI previews it while the flyover floats above.
func (m *model) themeCarouselPreview(name string) {
	if th.skin {
		restoreTerminalColors()
	}
	if !setTheme(name) {
		return
	}
	applyThemeTerminal()
	m.themeChanged()
	m.syncLayout()
}

// themeCarouselConfirm is handleThemeCmd's tail: the candidate is already
// applied, so all that remains is persisting the choice, the transcript
// record and the background re-detect (the skin may have painted the ground,
// so the light/dark verdict follows the theme).
func (m model) themeCarouselConfirm() (tea.Model, tea.Cmd) {
	w := m.pendingThemeCarousel
	m.pendingThemeCarousel = nil
	if w == nil {
		return m, nil
	}
	m.st.cfg.Theme = th.name
	m.themeChanged()
	if err := saveLocalOrGlobal(m.st.cfg, activeConfigDir(m.st)); err != nil {
		m.appendTranscript(fmt.Sprintf("%s theme: %s (config save failed: %v)\n", milkTag(), bold(th.name), err))
	} else {
		m.notify("theme: "+th.name, "/theme")
	}
	m.appendTranscript(milkTag() + " theme: " + bold(th.name) + "\n")
	m.syncLayout()
	return m, tea.Batch(tea.ClearScreen, tea.RequestBackgroundColor)
}

// themeCarouselClose cancels the browse: the original theme (and the
// terminal's own palette) is restored through the same preview path, then
// the background is re-detected — a cancelled browse leaves no trace.
func (m model) themeCarouselClose() (tea.Model, tea.Cmd) {
	w := m.pendingThemeCarousel
	m.pendingThemeCarousel = nil
	if w == nil {
		return m, nil
	}
	if th.name != w.orig {
		m.themeCarouselPreview(w.orig)
	}
	m.appendTranscript(milkTag() + " theme: browse cancelled\n")
	m.syncLayout()
	return m, tea.Batch(tea.ClearScreen, tea.RequestBackgroundColor)
}

// overlayThemeCarousel draws the open carousel flyover over the main area,
// centered near the top — the palette/picker placement.
func (m *model) overlayThemeCarousel(block string) string {
	if m.pendingThemeCarousel == nil {
		return block
	}
	width := m.mainWidth()
	boxWidth := min(paletteMaxWidth, width-4)
	if boxWidth < paletteMinWidth {
		return block
	}
	box := m.renderThemeCarousel(boxWidth)
	return composeOverlays(block, overlayLayer{content: box, x: centeredX(width, boxWidth), y: paletteTopRow})
}

// renderThemeCarousel renders the flyover box: the candidate's name strip
// (a wrapping window of five, the candidate bracketed), a swatch strip
// painted with the candidate's own palette, its source/look/tagline, and the
// key footer.
func (m *model) renderThemeCarousel(boxWidth int) string {
	w := m.pendingThemeCarousel
	if w == nil {
		return ""
	}
	inner := boxWidth - 4
	name := w.candidate()
	rt, ok := themeRegistry.Get(name)
	if !ok {
		return ""
	}
	src, _ := themeRegistry.Source(name)

	title := lipgloss.NewStyle().Bold(true).Render("theme") +
		dim("  ←/→ or wheel switch · enter apply · any other key cancel")
	strip := carouselStrip(w.names, w.idx, 5)
	swatches := carouselSwatches(inner, rt.Colors)
	info := dim(fmt.Sprintf("%s · %s · %s", src, rt.Look, taglineOr(rt.Tagline)))

	body := strings.Join([]string{title, strip, swatches, info}, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tkAccent.adaptive()).
		Padding(0, 1).
		Width(boxWidth).
		Render(body)
}

// carouselStrip is the ‹ name · name · [candidate] · name › row — a wrapping
// window of window names centered on idx, the candidate bracketed instead of
// dimmed so it reads at a glance.
func carouselStrip(names []string, idx, window int) string {
	if len(names) == 0 {
		return ""
	}
	if window > len(names) {
		window = len(names)
	}
	parts := make([]string, 0, window)
	start := idx - window/2
	for i := start; i < start+window; i++ {
		j := ((i % len(names)) + len(names)) % len(names)
		if j == idx {
			parts = append(parts, bold("["+names[j]+"]"))
		} else {
			parts = append(parts, dim(names[j]))
		}
	}
	return "‹ " + strings.Join(parts, dim(" · ")) + " ›"
}

// carouselSwatches paints four blocks in the candidate's own base/raised/
// text/accent with hex captions — what the eye needs to compare themes that
// differ only subtly. Width-fitted: captions drop first on a narrow box.
func carouselSwatches(width int, p theme.Palette) string {
	tokens := []struct {
		name string
		c    theme.Color
	}{
		{"base", p.Base},
		{"raised", p.Raised},
		{"text", p.Text},
		{"accent", p.Accent},
	}
	var parts []string
	for _, t := range tokens {
		if t.c.Absent() {
			continue
		}
		hex := t.c.Dark
		if !hasDarkBackground() && t.c.Light != "" {
			hex = t.c.Light
		}
		block := colorize("  ", themeColorOf(t.c).sgrBg())
		parts = append(parts, block+" "+dim(hex))
	}
	return strings.Join(parts, "  ")
}

// taglineOr falls back to the default tagline line.
func taglineOr(t string) string {
	if t != "" {
		return t
	}
	return defaultTagline
}

// themeViewText renders a theme's original descriptor — the drop-in file's
// own content or the embedded builtin, identical formats — as the /theme
// view body: the copy-paste starting point for theme authors (and the same
// JSON the builder's Review step shows).
func themeViewText(name string) (string, bool) {
	return themeRegistry.JSON(name)
}
