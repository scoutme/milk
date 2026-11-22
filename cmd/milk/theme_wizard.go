package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/theme"
)

// Theme builder wizard (plan PR 3): /theme new asks for the ingredients of a
// theme and saves it as ~/.milk/themes/<name>.json — materialized (every
// colour spelled out, so the theme keeps working even if its base ever
// disappears) with a `derived_from` provenance field. Three modes: from an
// existing theme (pre-filled, edit any colour), from scratch (type the
// palette), and a darken/lighten variant of an existing theme (carousel UX:
// ←/→ lightness, shift+←/→ saturation, live preview). Every step previews
// through the real switch path, and esc anywhere restores the original
// theme — a build session can never strand the terminal.

type themeWizardMode int

const (
	twFromExisting themeWizardMode = iota
	twFromScratch
	twVariant
)

type themeWizardStep int

const (
	twStepMode themeWizardStep = iota
	twStepBase
	twStepVariant
	twStepName
	twStepColor
	twStepMore
	twStepToken
	twStepTerminal
	twStepTagline
	twStepReview
)

// twCoreTokens are the six colours the builder asks for up front — enough
// for the derivations to fill the rest. "more colors…" opens the rest.
var twCoreTokens = []string{"base", "raised", "text", "muted", "accent", "hairline"}

// twTokenSet is the full token vocabulary the free-entry step accepts.
var twTokenSet = map[string]bool{
	"base": true, "raised": true, "toast": true, "hairline": true, "thumb": true,
	"text": true, "muted": true, "accent": true, "accent_lo": true,
	"think": true, "focus": true,
}

// themeWizardState is the open builder session.
type themeWizardState struct {
	mode  themeWizardMode
	step  themeWizardStep
	sel   int // choice-step selection (mode/base/more/terminal)
	base  string
	name  string
	colors  map[string]theme.Color
	colorIdx int
	more   bool
	deriveTerm bool
	tagline string
	dl, ds float64 // variant: lightness / saturation deltas
	orig   string // theme active at open — what cancel restores
	err    string // shown inline instead of advancing
}

func (w *themeWizardState) put(key string, c theme.Color) {
	if w.colors == nil {
		w.colors = map[string]theme.Color{}
	}
	w.colors[key] = c
}

// candidate builds the theme under construction: the base palette (when a
// base is chosen) overridden by the typed colours, resolved through the
// common-basis derivations. Variant mode transforms the base wholesale.
func (w *themeWizardState) candidate() *theme.Resolved {
	if w.mode == twVariant {
		base, ok := themeRegistry.Get(w.base)
		if !ok {
			return nil
		}
		out := theme.Transform(base, w.dl, w.ds)
		out.Name = w.name
		if out.Name == "" {
			out.Name = "new" // preview placeholder; save always goes through the name step
		}
		out.DerivedFrom = base.Name
		return out
	}
	t := &theme.Theme{Name: w.name, Look: theme.LookSkin, Tagline: w.tagline, DerivedFrom: w.base}
	if t.Name == "" {
		t.Name = "new" // preview placeholder; save always goes through the name step
	}
	if base, ok := themeRegistry.Get(w.base); ok {
		t.Look = base.Look
		t.Tagline = base.Tagline
		t.ToolRows, t.Scrollbar = base.ToolRows, base.Scrollbar
		t.Colors = base.Colors
	}
	if w.tagline != "" {
		t.Tagline = w.tagline
	}
	for key, c := range w.colors {
		setThemeToken(&t.Colors, key, c)
	}
	rt := theme.Resolve(t)
	if w.deriveTerm {
		ramp := theme.DeriveANSI16(rt.Colors)
		rt.Terminal = &theme.Terminal{
			Bg: pickHex(rt.Colors.Base), Fg: pickHex(rt.Colors.Text), Cursor: pickHex(rt.Colors.Text),
			Ansi16: ramp[:],
		}
	}
	return rt
}

// setThemeToken fills one palette field by token name.
func setThemeToken(p *theme.Palette, key string, c theme.Color) {
	switch key {
	case "base":
		p.Base = c
	case "raised":
		p.Raised = c
	case "toast":
		p.Toast = c
	case "hairline":
		p.Hairline = c
	case "thumb":
		p.Thumb = c
	case "text":
		p.Text = c
	case "muted":
		p.Muted = c
	case "accent":
		p.Accent = c
	case "accent_lo":
		p.AccentLo = c
	case "think":
		p.Think = c
	case "focus":
		p.Focus = c
	}
}

// pickHex selects the variant for the current terminal ground.
func pickHex(c theme.Color) string {
	if hasDarkBackground() {
		return c.Dark
	}
	return c.Light
}

// parseTypedColor accepts "#rrggbb" (fixed) or "dark/light" (a pair for the
// two grounds) from the wizard's colour steps.
func parseTypedColor(s string) (theme.Color, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return theme.Color{}, false
	}
	if i := strings.IndexAny(s, "/"); i >= 0 {
		dark, err1 := theme.ParseHex(s[:i])
		light, err2 := theme.ParseHex(s[i+1:])
		if err1 != nil || err2 != nil {
			return theme.Color{}, false
		}
		return theme.Color{Dark: dark, Light: light}, true
	}
	hex, err := theme.ParseHex(s)
	if err != nil {
		return theme.Color{}, false
	}
	return theme.Fixed(hex), true
}

// openThemeWizard arms the builder behind /theme new.
func (m model) openThemeWizard() (tea.Model, tea.Cmd) {
	m.pendingThemeWizard = &themeWizardState{orig: th.name, deriveTerm: true, step: twStepMode}
	m.appendTranscript(milkTag() + " theme builder: esc cancels at any point and restores " + bold(th.name) + "\n")
	m.refreshPrompt()
	m.syncLayout()
	return m, nil
}

// themeWizardCancel aborts the build and restores the original theme.
func (m model) themeWizardCancel() (tea.Model, tea.Cmd) {
	w := m.pendingThemeWizard
	m.pendingThemeWizard = nil
	if w != nil && th.name != w.orig {
		if th.skin {
			restoreTerminalColors()
		}
		setTheme(w.orig)
		applyThemeTerminal()
	}
	m.ta.Reset()
	m.appendTranscript(milkTag() + " theme builder: cancelled\n")
	m.refreshPrompt()
	m.syncLayout()
	return m, tea.Batch(tea.ClearScreen, tea.RequestBackgroundColor)
}

// themeWizardPreview applies the candidate through the real switch path
// minus persistence, so the whole TUI shows the theme being built. Skipped
// while there is nothing to show yet (scratch mode before the first colour).
func (m *model) themeWizardPreview(w *themeWizardState) {
	rt := w.candidate()
	// Nothing meaningful to show until the palette has a base — an empty
	// candidate must never reach the skin render path.
	if rt == nil || rt.Colors.Base.Absent() {
		return
	}
	if th.skin {
		restoreTerminalColors()
	}
	applyThemeDef(themeDefFromResolved(rt))
	applyThemeTerminal()
	m.colorizeForce = true
	m.viewportDirty = true
	m.syncLayout()
}

// themeWizardSave writes the finished theme to ~/.milk/themes/<name>.json,
// reloads the registry and switches to it — the same tail as /theme <name>.
func (m model) themeWizardSave(w *themeWizardState) (tea.Model, tea.Cmd) {
	rt := w.candidate()
	if rt == nil {
		return m.themeWizardCancel()
	}
	out := theme.Materialize(rt)
	if err := theme.Validate(out); err != nil {
		w.err = err.Error()
		return m, nil
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		w.err = err.Error()
		return m, nil
	}
	dir := ""
	if stateDir, err := config.Dir(); err == nil {
		dir = filepath.Join(stateDir, "themes")
	}
	if dir == "" {
		w.err = "no state root — cannot save the theme file"
		return m, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.err = err.Error()
		return m, nil
	}
	path := filepath.Join(dir, out.Name+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		w.err = err.Error()
		return m, nil
	}
	m.pendingThemeWizard = nil
	loadThemeRegistry(m.themeProjectDir())
	setTheme(out.Name)
	applyThemeTerminal()
	m.st.cfg.Theme = th.name
	m.colorizeForce = true
	m.viewportDirty = true
	m.ta.Reset()
	if err := saveLocalOrGlobal(m.st.cfg, activeConfigDir(m.st)); err != nil {
		m.appendTranscript(fmt.Sprintf("%s theme: %s (config save failed: %v)\n", milkTag(), bold(th.name), err))
	} else {
		m.notify("theme: "+th.name, "/theme")
	}
	m.appendTranscript(fmt.Sprintf("%s theme builder: saved %s → %s\n", milkTag(), bold(out.Name), path))
	m.syncLayout()
	return m, tea.Batch(tea.ClearScreen, tea.RequestBackgroundColor)
}

// handleThemeWizardKey routes one key to the open builder.
func (m model) handleThemeWizardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	w := m.pendingThemeWizard
	if w == nil {
		return m, nil
	}
	key := msg.String()
	w.err = ""
	switch key {
	case "esc", "ctrl+c":
		return m.themeWizardCancel()
	}

	// Variant mode is the carousel-shaped step: arrows drive the transform.
	if w.step == twStepVariant {
		switch key {
		case "left", "right":
			if key == "left" {
				w.dl = clampf(w.dl-0.05, -0.25, 0.25)
			} else {
				w.dl = clampf(w.dl+0.05, -0.25, 0.25)
			}
			m.themeWizardPreview(w)
			return m, nil
		case "shift+left", "shift+right":
			if key == "shift+left" {
				w.ds = clampf(w.ds-0.05, -0.25, 0.25)
			} else {
				w.ds = clampf(w.ds+0.05, -0.25, 0.25)
			}
			m.themeWizardPreview(w)
			return m, nil
		case "enter", "ctrl+m":
			m.ta.Reset()
			w.step = twStepName
			return m, nil
		}
		return m, nil
	}

	// Choice steps: arrows move, enter accepts.
	if w.step == twStepMode || w.step == twStepBase || w.step == twStepMore || w.step == twStepTerminal {
		n := w.choiceLen()
		switch key {
		case "up", "ctrl+p", "shift+tab", "down", "ctrl+n", "tab":
			delta := 1
			if key == "up" || key == "ctrl+p" || key == "shift+tab" {
				delta = -1
			}
			w.sel = (w.sel + delta + n) % n
			if w.step == twStepBase {
				m.themeWizardPreview(w) // preview the browsed base
			}
			return m, nil
		case "enter", "ctrl+m":
			return m.themeWizardAcceptChoice()
		}
		return m, nil
	}

	// Review: enter saves.
	if w.step == twStepReview {
		if key == "enter" || key == "ctrl+m" {
			return m.themeWizardSave(w)
		}
		return m, nil
	}

	// Text steps: enter takes the typed value (empty = keep the default).
	if key == "enter" || key == "ctrl+m" {
		typed := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		return m.themeWizardAcceptText(typed)
	}
	cmd := m.updateTA(msg)
	m.syncLayout()
	return m, cmd
}

// choiceLen is the number of rows on the current choice step.
func (w *themeWizardState) choiceLen() int {
	switch w.step {
	case twStepMode:
		return 3
	case twStepBase:
		return len(themeRegistry.Names())
	case twStepMore:
		return 2
	case twStepTerminal:
		return 2
	}
	return 1
}

// themeWizardAcceptChoice consumes the selection on a choice step.
func (m model) themeWizardAcceptChoice() (tea.Model, tea.Cmd) {
	w := m.pendingThemeWizard
	switch w.step {
	case twStepMode:
		w.mode = themeWizardMode(w.sel)
		w.sel = 0
		if w.mode == twFromScratch {
			w.step = twStepName
		} else {
			w.step = twStepBase
		}
	case twStepBase:
		names := themeRegistry.Names()
		if w.sel < len(names) {
			w.base = names[w.sel]
		}
		w.sel = 0
		if w.mode == twVariant {
			w.step = twStepVariant
		} else {
			w.step = twStepName
		}
	case twStepMore:
		w.more = w.sel == 1
		w.sel = 0
		if w.more {
			w.step = twStepToken
		} else {
			w.step = twStepTerminal
		}
	case twStepTerminal:
		w.deriveTerm = w.sel == 0
		w.sel = 0
		w.step = twStepTagline
	}
	m.themeWizardPreview(w)
	return m, nil
}

// themeWizardAcceptText consumes the typed answer on a text step.
func (m model) themeWizardAcceptText(typed string) (tea.Model, tea.Cmd) {
	w := m.pendingThemeWizard
	switch w.step {
	case twStepName:
		w.name = strings.ToLower(typed)
		if w.name == "" {
			w.err = "a name is required (lowercase letters, digits, _ and -)"
			return m, nil
		}
		if _, taken := themeRegistry.Get(w.name); taken {
			w.err = fmt.Sprintf("%q is already a theme — pick another name", w.name)
			return m, nil
		}
		if w.mode == twVariant {
			w.step = twStepReview
		} else {
			w.step = twStepColor
			w.colorIdx = 0
		}
		m.themeWizardPreview(w)
	case twStepColor:
		if typed != "" {
			c, ok := parseTypedColor(typed)
			if !ok {
				w.err = `want "#rrggbb" or "#rrggbb/#rrggbb"`
				return m, nil
			}
			w.put(twCoreTokens[w.colorIdx], c)
			m.themeWizardPreview(w)
		}
		w.colorIdx++
		if w.colorIdx >= len(twCoreTokens) {
			w.step = twStepMore
		}
	case twStepToken:
		if typed == "" {
			w.step = twStepTerminal
			m.themeWizardPreview(w)
			return m, nil
		}
		fields := strings.Fields(typed)
		if len(fields) != 2 || !twTokenSet[fields[0]] {
			w.err = `want "<token> #rrggbb" (or "#rrggbb/#rrggbb") — tokens: ` + strings.Join(twCoreTokens, ", ") + ", …"
			return m, nil
		}
		c, ok := parseTypedColor(fields[1])
		if !ok {
			w.err = `want "#rrggbb" or "#rrggbb/#rrggbb"`
			return m, nil
		}
		w.put(fields[0], c)
		m.themeWizardPreview(w)
	case twStepTagline:
		w.tagline = typed
		w.step = twStepReview
		m.themeWizardPreview(w)
	}
	return m, nil
}

// clampf bounds a float to [lo, hi].
func clampf(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---- rendering ----

// themeWizardTitle is the human label per step.
func themeWizardTitle(w *themeWizardState) string {
	switch w.step {
	case twStepMode:
		return "how should the new theme start?"
	case twStepBase:
		return "which theme is the starting point?"
	case twStepVariant:
		return "tune the variant"
	case twStepName:
		return "name the new theme"
	case twStepColor:
		return fmt.Sprintf("color %d/%d — %s", w.colorIdx+1, len(twCoreTokens), twCoreTokens[w.colorIdx])
	case twStepMore:
		return "add more colors than the core six?"
	case twStepToken:
		return "extra color — token and hex (empty line to continue)"
	case twStepTerminal:
		return "terminal palette?"
	case twStepTagline:
		return "tagline (empty for none)"
	default:
		return "review"
	}
}

// themeWizardHint is the per-step answer hint.
func themeWizardHint(w *themeWizardState) string {
	switch w.step {
	case twStepName:
		return `lowercase letters, digits, _ and -`
	case twStepColor, twStepToken:
		return `"#rrggbb" or "#rrggbb/#rrggbb" (dark/light)`
	case twStepTagline:
		return `enter accepts`
	}
	return ""
}

// overlayThemeWizard draws the open builder over the main area.
func (m *model) overlayThemeWizard(block string) string {
	if m.pendingThemeWizard == nil {
		return block
	}
	width := m.mainWidth()
	boxWidth := min(paletteMaxWidth, width-4)
	if boxWidth < paletteMinWidth {
		return block
	}
	return composeOverlays(block, overlayLayer{content: m.renderThemeWizard(boxWidth), x: centeredX(width, boxWidth), y: paletteTopRow})
}

// renderThemeWizard renders the builder's box for the current step.
func (m *model) renderThemeWizard(boxWidth int) string {
	w := m.pendingThemeWizard
	if w == nil {
		return ""
	}
	inner := boxWidth - 4
	title := lipgloss.NewStyle().Bold(true).Render("theme builder") + dim("  "+themeWizardTitle(w))
	lines := []string{title}

	switch {
	case w.step == twStepMode || w.step == twStepBase || w.step == twStepMore || w.step == twStepTerminal:
		rows := w.choiceRows()
		for i, row := range rows {
			lines = append(lines, paletteRow(paletteEntry{name: row}, inner, i == w.sel))
		}
	case w.step == twStepVariant:
		rt := w.candidate()
		lines = append(lines,
			fmt.Sprintf("  %s → %s", dim(w.base), bold(orName(w.name, "(new variant)"))),
			fmt.Sprintf("  lightness %+.0f%% · saturation %+.0f%%", w.dl*100, w.ds*100),
		)
		if rt != nil {
			lines = append(lines, "  "+carouselSwatches(inner, rt.Colors))
		}
		lines = append(lines, dim("  ←/→ lightness · shift+←/→ saturation · enter name it"))
	case w.step == twStepReview:
		rt := w.candidate()
		if rt != nil {
			data, _ := json.MarshalIndent(theme.Materialize(rt), "", "  ")
			rows := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			for i, row := range rows {
				if i >= 12 {
					lines = append(lines, dim("  …"))
					break
				}
				lines = append(lines, "  "+row)
			}
		}
		lines = append(lines, dim("  enter save · esc cancel"))
	default:
		lines = append(lines, "  › "+m.ta.Value()+"▏")
		if h := themeWizardHint(w); h != "" {
			lines = append(lines, dim("  "+h))
		}
		// The palette under construction, always visible next to the question.
		if rt := w.candidate(); rt != nil {
			lines = append(lines, "  "+carouselSwatches(inner, rt.Colors))
		}
		lines = append(lines, dim("  enter accept · esc cancel"))
	}
	if w.err != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#c05050")).Render("  "+w.err))
	}
	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tkAccent.adaptive()).
		Padding(0, 1).
		Width(boxWidth).
		Render(body)
}

// choiceRows are the visible rows of the current choice step.
func (w *themeWizardState) choiceRows() []string {
	switch w.step {
	case twStepMode:
		return []string{"from an existing theme", "from scratch", "darken/lighten variant of a theme"}
	case twStepBase:
		return themeRegistry.Names()
	case twStepMore:
		return []string{"no", "yes"}
	case twStepTerminal:
		return []string{"derive an ANSI palette from the colors", "leave the terminal palette alone"}
	}
	return nil
}

// orName is a placeholder name for unnamed candidates.
func orName(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}
