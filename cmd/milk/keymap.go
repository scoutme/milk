package main

import (
	"fmt"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
)

// panelKey ties a panel shortcut to the panel name /panel understands.
type panelKey struct {
	binding key.Binding
	panel   string
}

// keyMap is the one registry of the TUI's global shortcuts. The bindings in
// panels, tipPrev, tipNext, dismissToasts and palette are what Update matches
// on, so the docs cannot drift from the behaviour. The rest (input editing
// keys handled inside handleKey) are listed for /keys only and carry a
// comment where they live. It implements help.KeyMap.
type keyMap struct {
	panels        []panelKey
	tipPrev       key.Binding
	tipNext       key.Binding
	dismissToasts key.Binding
	palette       key.Binding
	thinkToggle   key.Binding
	searchBack    key.Binding
	searchForward key.Binding
	undo          key.Binding
	redo          key.Binding
	newline       key.Binding
	interrupt     key.Binding
}

func newKeyMap() keyMap {
	panel := func(keyName, panel, desc string) panelKey {
		return panelKey{
			binding: key.NewBinding(key.WithKeys(keyName), key.WithHelp(keyName, desc)),
			panel:   panel,
		}
	}
	return keyMap{
		panels: []panelKey{
			panel("f1", "memory", "memory panel"),
			panel("f2", "tasks", "tasks panel"),
			panel("f3", "background", "background jobs panel"),
			panel("f4", "workflow", "workflow panel"),
			panel("f5", "project", "project tree panel"),
			panel("f6", "git", "git graph panel"),
		},
		tipPrev:       key.NewBinding(key.WithKeys("alt+up"), key.WithHelp("alt+up", "previous tip")),
		tipNext:       key.NewBinding(key.WithKeys("alt+down"), key.WithHelp("alt+down", "next tip")),
		dismissToasts: key.NewBinding(key.WithKeys("ctrl+g"), key.WithHelp("ctrl+g", "dismiss notifications")),
		palette:       key.NewBinding(key.WithKeys(paletteOpenKey), key.WithHelp(paletteOpenKey, "command palette")),
		thinkToggle:   key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "fold or unfold the latest reasoning")),
		searchBack:    key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "search input history")),
		searchForward: key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "search history forward")),
		undo:          key.NewBinding(key.WithKeys("ctrl+z"), key.WithHelp("ctrl+z", "undo")),
		redo:          key.NewBinding(key.WithKeys("ctrl+y"), key.WithHelp("ctrl+y", "redo")),
		newline:       key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+n"), key.WithHelp("shift+enter", "new line in the input")),
		interrupt:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "interrupt the turn, again to quit")),
	}
}

// keys is the shared registry; bindings are read-only after init.
var keys = newKeyMap()

// panelFor returns the panel a pressed key toggles ("" when none).
func (k keyMap) panelFor(msg fmt.Stringer) string {
	for _, pk := range k.panels {
		if key.Matches(msg, pk.binding) {
			return pk.panel
		}
	}
	return ""
}

func (k keyMap) panelBindings() []key.Binding {
	out := make([]key.Binding, len(k.panels))
	for i, pk := range k.panels {
		out[i] = pk.binding
	}
	return out
}

// ShortHelp implements help.KeyMap.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.palette, k.interrupt, k.tipNext, k.dismissToasts}
}

// FullHelp implements help.KeyMap: one column per group.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		k.panelBindings(),
		{k.palette, k.tipPrev, k.tipNext, k.dismissToasts, k.thinkToggle},
		{k.searchBack, k.searchForward, k.undo, k.redo, k.newline, k.interrupt},
	}
}

// renderKeysHelp lists every shortcut in columns (Panels, Navigation, Input)
// sized to the viewport, for /keys.
func renderKeysHelp(width int) string {
	h := help.New()
	h.SetWidth(width)
	h.Styles = help.DefaultStyles(hasDarkBackground())
	h.ShowAll = true
	return h.FullHelpView(keys.FullHelp())
}
