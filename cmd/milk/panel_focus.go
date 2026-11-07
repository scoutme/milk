package main

// Keyboard focus for the side panels (issue #412).
//
// The main area (transcript + input) and each visible side panel compete for
// one keyboard target: PgUp/PgDn (and the ctrl+u/ctrl+f aliases) scroll
// whichever panel holds focus, shift+pgup/shift+pgdown rotate focus through
// [main, visible panels…], a left click sets it, and focus renders as state —
// a focus tint on the panel's rows, the input area dimmed while focus is
// elsewhere (#367's render path). Focus is transient view state: never
// persisted, never toasts, never moves layout.

// focusedPanel returns the side panel that currently owns keyboard
// navigation, or regionNone for the main area. It self-heals: a focused
// panel that has been closed or pushed out of the width budget no longer
// counts, so every consumer — the dimmed input, the tint, the scroll
// routing — falls back to the main area without needing a reset at each
// mutation site (closing, /new, budget changes, theme switches).
func (m *model) focusedPanel() panelRegion {
	if m.panelFocus != regionNone && m.panelVisible(m.panelFocus) {
		return m.panelFocus
	}
	return regionNone
}

// rotatePanelFocus moves keyboard focus one step through
// [main, visible panels…] in panelRenderOrder, wrapping at both ends.
// Main is deliberately part of the cycle: rotation is the only keyboard
// path back to the input area — and shift+pgup the way back out of it.
// Panels hidden by the width budget are not in the sequence (only what is
// actually rendered may be focused, ADR-0089), so with no visible panel the
// cycle is main→main: a no-op, not a panic.
func (m *model) rotatePanelFocus(dir int) {
	seq := []panelRegion{regionNone}
	for _, r := range panelRenderOrder {
		if m.panelVisible(r) {
			seq = append(seq, r)
		}
	}
	cur := m.focusedPanel()
	idx := 0
	for i, r := range seq {
		if r == cur {
			idx = i
			break
		}
	}
	m.panelFocus = seq[((idx+dir)%len(seq)+len(seq))%len(seq)]
}

// scrollFocusedPanel pages the focused panel's own scroll offset — the same
// per-region offset the wheel and drag use — by half the panel height, the
// keyboard twin of the transcript's PgUp/PgDn. Clamped exactly like the
// wheel: [0, panelMaxOffset]. A main-focused caller reaches here only via
// focusedPanel()==regionNone guards, and this double-checks anyway.
func (m *model) scrollFocusedPanel(dir int) {
	region := m.focusedPanel()
	if region == regionNone {
		return
	}
	p := m.panelOffsetPtr(region)
	if p == nil {
		return
	}
	h := m.viewportHeight()
	step := max(h/2, 1)
	switch {
	case dir < 0:
		*p = max(*p-step, 0)
	case dir > 0:
		*p = min(*p+step, m.panelMaxOffset(region, h))
	}
}

// panelRowBg is the row background shared by a side panel's body and its
// scrollbar: the focus tint wins over the alternating parity (a focused
// panel must read as focused wherever it landed), which wins over plain.
func (m *model) panelRowBg(region panelRegion) string {
	if m.focusedPanel() == region {
		return focusBgCode()
	}
	if m.panelAltBackground(region) {
		return panelAltBackgroundCode()
	}
	return ""
}
