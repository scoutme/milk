package main

import (
	tea "github.com/charmbracelet/bubbletea"
	"testing"
)

// TestHandleKey_PasteAtBufferStart_SetsLeadingPasted guards issue #151's
// core mechanism: a bracketed-paste event landing at the very start of an
// empty buffer must taint the leading token, since that's the position
// extractSlashCommand/stripBangPrefix/shelldetect all inspect.
func TestHandleKey_PasteAtBufferStart_SetsLeadingPasted(t *testing.T) {
	m := newTestModelForPanels(t)
	if m.leadingPasted {
		t.Fatal("expected leadingPasted false before any input")
	}

	pasted := "Here is my earlier turn:\n> /learn milk likes Go\nand the reply"
	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})
	nm := next.(model)

	if !nm.leadingPasted {
		t.Error("expected leadingPasted true after a paste landing at buffer offset 0")
	}
}

// TestHandleKey_PasteAfterTypedPrefix_DoesNotTaint guards the workflow this
// fix must not break: typing a command, then pasting its argument, must
// still let the command through — the paste doesn't occupy the leading
// token's position.
func TestHandleKey_PasteAfterTypedPrefix_DoesNotTaint(t *testing.T) {
	m := newTestModelForPanels(t)

	typed, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/escalate ")})
	m = typed.(model)
	if m.leadingPasted {
		t.Fatal("typing alone must never set leadingPasted")
	}

	pastedArg := "a long paragraph of context pasted as the escalation reason"
	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pastedArg), Paste: true})
	nm := next.(model)

	if nm.leadingPasted {
		t.Error("expected leadingPasted to stay false: the paste landed after a typed leading token, not at it")
	}
	if got := nm.ta.Value(); got != "/escalate "+pastedArg {
		t.Errorf("ta.Value() = %q, want typed prefix + pasted argument", got)
	}
}

// TestHandleKey_BufferEmptiedAfterPaste_ClearsLeadingPasted guards a
// staleness gap: if the user backspaces away a tainted paste entirely, the
// next thing they type must not be treated as tainted too.
func TestHandleKey_BufferEmptiedAfterPaste_ClearsLeadingPasted(t *testing.T) {
	m := newTestModelForPanels(t)
	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!echo pwned"), Paste: true})
	m = next.(model)
	if !m.leadingPasted {
		t.Fatal("setup: expected leadingPasted true after paste at start")
	}

	// Backspace away every character of the pasted content.
	for range []rune("!echo pwned") {
		n, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
		m = n.(model)
	}
	if m.ta.Value() != "" {
		t.Fatalf("setup: expected empty buffer after backspacing, got %q", m.ta.Value())
	}
	if m.leadingPasted {
		t.Error("expected leadingPasted to clear once the buffer emptied out")
	}
}

// TestSubmitInput_LeadingPasted_SuppressesTriggers guards the consumption
// side: submitInput must treat a tainted leading token as inert prompt
// text — never handing it to stripBangPrefix, extractSlashCommand, or the
// /paste clipboard probe — and must fall through to dispatchAgent instead.
func TestSubmitInput_LeadingPasted_SuppressesTriggers(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"bang prefix", "!echo pwned-test-marker"},
		{"slash command", "/help"},
		{"paste probe token", cmdPaste},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModelForPanels(t)
			m.leadingPasted = true

			next, _ := m.submitInput(tc.input, "> ")
			nm := next.(model)

			if nm.ptyPane != nil {
				t.Error("expected no PTY pane launched — bang/direct-bash must be suppressed when tainted")
			}
			if nm.pendingDirectBash != nil {
				t.Error("expected no direct-bash confirmation prompt when tainted")
			}
			if !nm.busy {
				t.Error("expected input to fall through to dispatchAgent (busy=true) instead of triggering a command")
			}
			if nm.leadingPasted {
				t.Error("expected leadingPasted to be consumed (cleared) by submitInput")
			}
		})
	}
}

// TestSubmitInput_TypedInput_StillTriggersSlashCommand is the control case:
// with no taint, a leading slash command must still dispatch normally.
func TestSubmitInput_TypedInput_StillTriggersSlashCommand(t *testing.T) {
	m := newTestModelForPanels(t)
	m.leadingPasted = false

	next, _ := m.submitInput("/help", "> ")
	nm := next.(model)

	if nm.busy {
		t.Error("expected /help to be handled as a command (busy=false), not dispatched to the agent")
	}
}
