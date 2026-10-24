package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	paletteMaxRows  = 8
	paletteMaxWidth = 72
	paletteMinWidth = 32
	paletteTopRow   = 1
	paletteOpenKey  = "ctrl+o"
)

// paletteState is the command palette: a floating, filterable list of the
// slash commands, opened with Ctrl+O. It only fills the input line; running
// the command stays the user's Enter, so arguments can still be typed.
type paletteState struct {
	query string
	sel   int
}

// paletteEntry is one selectable row.
type paletteEntry struct {
	name string
	hint string
	desc string
}

// paletteEntries lists the TUI commands that match query, best first: name
// prefix, then name substring, then description substring.
func paletteEntries(query string) []paletteEntry {
	needle := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(query), "/"))
	var prefix, inName, inDesc []paletteEntry
	for _, spec := range commandRegistry {
		if spec.hidden || !spec.in(hostTUI) {
			continue
		}
		entry := paletteEntry{name: spec.name, hint: spec.hintFor(hostTUI), desc: spec.desc}
		name := strings.ToLower(strings.TrimPrefix(spec.name, "/"))
		switch {
		case needle == "" || strings.HasPrefix(name, needle):
			prefix = append(prefix, entry)
		case strings.Contains(name, needle):
			inName = append(inName, entry)
		case strings.Contains(strings.ToLower(spec.desc), needle):
			inDesc = append(inDesc, entry)
		}
	}
	return append(append(prefix, inName...), inDesc...)
}

func (m model) paletteAvailable() bool {
	return m.ptyPane == nil && m.attached == nil && !m.hasPendingPrompt() && !m.searching
}

func (m model) openPalette() (tea.Model, tea.Cmd) {
	m.palette = &paletteState{}
	return m, nil
}

// handlePaletteKey consumes every key while the palette is open.
func (m model) handlePaletteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := *m.palette
	entries := paletteEntries(state.query)
	switch msg.String() {
	case "esc", "ctrl+c", paletteOpenKey:
		m.palette = nil
		return m, nil
	case "up", "ctrl+p", "shift+tab":
		state.sel = max(state.sel-1, 0)
	case "down", "ctrl+n", "tab":
		state.sel = min(state.sel+1, max(len(entries)-1, 0))
	case "enter":
		m.palette = nil
		if len(entries) == 0 {
			return m, nil
		}
		return m.insertPaletteCommand(entries[state.sel])
	case "backspace":
		runes := []rune(state.query)
		if len(runes) > 0 {
			state.query = string(runes[:len(runes)-1])
			state.sel = 0
		}
	default:
		if msg.Text == "" {
			return m, nil
		}
		state.query += msg.Text
		state.sel = 0
	}
	m.palette = &state
	return m, nil
}

// insertPaletteCommand puts the chosen command in the input line.
func (m model) insertPaletteCommand(entry paletteEntry) (tea.Model, tea.Cmd) {
	text := entry.name + " "
	if strings.TrimSpace(m.ta.Value()) == "" {
		m.ta.SetValue(text)
	} else {
		m.ta.InsertString(text)
	}
	m.syncLayout()
	return m, nil
}

// overlayPalette draws the palette over the main area, centered near the top.
func (m *model) overlayPalette(block string) string {
	if m.palette == nil {
		return block
	}
	width := m.mainWidth()
	boxWidth := min(paletteMaxWidth, width-4)
	if boxWidth < paletteMinWidth {
		return block
	}
	box := m.renderPalette(boxWidth)
	return composeOverlays(block, overlayLayer{content: box, x: centeredX(width, boxWidth), y: paletteTopRow})
}

func (m *model) renderPalette(boxWidth int) string {
	inner := boxWidth - 4 // border + one column of padding per side
	state := *m.palette
	entries := paletteEntries(state.query)

	lines := []string{
		lipgloss.NewStyle().Bold(true).Render("commands") + dim("  "+paletteOpenKey+" / esc to close"),
		"› " + state.query + "▏",
	}
	start := 0
	if state.sel >= paletteMaxRows {
		start = state.sel - paletteMaxRows + 1
	}
	end := min(start+paletteMaxRows, len(entries))
	for i := start; i < end; i++ {
		lines = append(lines, paletteRow(entries[i], inner, i == state.sel))
	}
	if len(entries) == 0 {
		lines = append(lines, dim("no command matches"))
	}
	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tkAccent.adaptive()).
		Padding(0, 1).
		Width(boxWidth).
		Render(body)
}

// paletteRow renders "/name hint  description" truncated to width; the
// selected row is reversed so it reads on any theme.
func paletteRow(e paletteEntry, width int, selected bool) string {
	label := e.name
	if e.hint != "" {
		label += " " + e.hint
	}
	row := ansi.Truncate(label+"  "+e.desc, width, "…")
	if selected {
		return lipgloss.NewStyle().Reverse(true).Render(row)
	}
	name := ansi.Truncate(e.name, width, "…")
	rest := strings.TrimPrefix(row, name)
	return lipgloss.NewStyle().Bold(true).Render(name) + dim(rest)
}
