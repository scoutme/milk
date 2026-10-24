package main

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type probeModel struct {
	seen []string
	done bool
}

func (p *probeModel) Init() tea.Cmd { return nil }

func (p *probeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	s := ""
	if str, ok := msg.(fmt.Stringer); ok {
		s = str.String()
	}
	p.seen = append(p.seen, fmt.Sprintf("%T | %q", msg, s))
	if len(p.seen) >= 4 {
		p.done = true
		return p, tea.Quit
	}
	return p, nil
}

func (p *probeModel) View() tea.View { return tea.NewView("") }

// Feeds raw terminal byte sequences (the common encodings of Ctrl+Enter)
// through a real bubbletea program and prints what Update receives.
func TestProbeCtrlEnterEncodings(t *testing.T) {
	input := []byte(
		"\x1b[13;5u" + // kitty CSI-u: Enter + ctrl
			"\x1b[27;5;13~" + // xterm modifyOtherKeys: ctrl+Enter
			"\x1b[13;5~" + // urxvt-style: ctrl+Enter
			"\x1b[13;2u", // kitty CSI-u: Enter + shift (for reference)
	)
	m := probeModel{}
	p := tea.NewProgram(&m, tea.WithInput(bytes.NewReader(input)), tea.WithOutput(io.Discard))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	for i, s := range m.seen {
		t.Logf("msg[%d]: %s", i, s)
	}
}

// TestIsCtrlEnterCSI verifies the three known Ctrl+Enter CSI encodings are
// recognized and that unrelated sequences are not.
func TestIsCtrlEnterCSI(t *testing.T) {
	ctrlEnter := []string{
		"?CSI[49 51 59 53 117]?",          // kitty CSI-u: 13;5u
		"?CSI[50 55 59 53 59 49 51 126]?", // xterm modifyOtherKeys: 27;5;13~
		"?CSI[49 51 59 53 126]?",          // urxvt: 13;5~
	}
	for _, s := range ctrlEnter {
		if !isCtrlEnterCSI(s) {
			t.Errorf("expected isCtrlEnterCSI(%q) = true", s)
		}
	}
	notCtrlEnter := []string{
		"?CSI[49 51 59 50 117]?", // Shift+Enter (kitty CSI-u: 13;2u)
		"?CSI[49 51 126]?",       // plain Enter (urxvt: 13~)
		"",                       // empty
		"ctrl+j",                 // regular key string
		"?CSI[49 51 59 53 117]",  // missing trailing ?
	}
	for _, s := range notCtrlEnter {
		if isCtrlEnterCSI(s) {
			t.Errorf("expected isCtrlEnterCSI(%q) = false", s)
		}
	}
}
