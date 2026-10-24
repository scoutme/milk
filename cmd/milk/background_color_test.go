package main

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The terminal answers tea.RequestBackgroundColor after startup; a light
// answer must flip the adaptive colors and rebuild the chrome styles.
func TestBackgroundColorMsg_RebuildsAdaptiveStyles(t *testing.T) {
	was := hasDarkBackground()
	t.Cleanup(func() {
		setDarkBackground(was)
		refreshThemeColors()
	})
	setDarkBackground(true)
	refreshThemeColors()
	darkBar := styleStatusBar.GetBackground()

	m := layoutTestModel(t, 80, 24)
	next, _ := m.Update(tea.BackgroundColorMsg{Color: color.White})
	_ = next.(model)

	if hasDarkBackground() {
		t.Fatal("a white background must clear the dark flag")
	}
	if styleStatusBar.GetBackground() == darkBar {
		t.Error("status bar style was not rebuilt for the light background")
	}
}

func TestBackgroundColorMsg_SameValueKeepsStyles(t *testing.T) {
	was := hasDarkBackground()
	t.Cleanup(func() { setDarkBackground(was) })
	if setDarkBackground(was) {
		t.Error("setting the current value must report no change")
	}
}
