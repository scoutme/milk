package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestKeyMap_PanelKeysMatchPanels(t *testing.T) {
	want := map[string]string{"f1": "memory", "f2": "tasks", "f3": "background", "f4": "workflow", "f5": "project", "f6": "git"}
	for keyName, panel := range want {
		code := rune(tea.KeyF1 + rune(keyName[1]-'1'))
		if got := keys.panelFor(tea.KeyPressMsg{Code: code}); got != panel {
			t.Errorf("%s -> %q, want %q", keyName, got, panel)
		}
	}
	if keys.panelFor(tea.KeyPressMsg{Code: 'a', Text: "a"}) != "" {
		t.Error("a letter must not toggle a panel")
	}
}

func TestKeyMap_EveryBindingHasHelpAndKeys(t *testing.T) {
	for _, group := range keys.FullHelp() {
		for _, b := range group {
			if b.Help().Key == "" || b.Help().Desc == "" || len(b.Keys()) == 0 {
				t.Errorf("binding %+v needs keys and help text", b.Help())
			}
		}
	}
}

func TestKeysCommand_PrintsShortcuts(t *testing.T) {
	out := ansi.Strip(renderKeysHelp(100))
	for _, want := range []string{"f1", "command palette", "alt+down", "ctrl+g"} {
		if !strings.Contains(out, want) {
			t.Errorf("/keys output misses %q:\n%s", want, out)
		}
	}
}

func TestTipNavRef_AgreesWithTipBindings(t *testing.T) {
	ref := strings.ToLower(tipNavRef)
	for _, b := range []string{keys.tipPrev.Help().Key, keys.tipNext.Help().Key} {
		part := strings.TrimPrefix(b, "alt+")
		if !strings.Contains(ref, part) {
			t.Errorf("tipNavRef %q should mention %q", tipNavRef, part)
		}
	}
}
