package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/charmbracelet/x/ansi"
)

func withTTY(t *testing.T) {
	t.Helper()
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })
}

// TestDimBlurredInput_StripsCursorAndDimsLines pins the blurred-input
// transform (issue #367): while the window is unfocused the input renders
// inactive — no reverse-video cursor cell (nothing blinks in a backgrounded
// tab), no leftover color runs, every non-empty line self-containedly
// dimmed — while visible text and row structure stay untouched, so the
// scroll position survives the toggle. Stripping is not isTTY-gated.
func TestDimBlurredInput_StripsCursorAndDimsLines(t *testing.T) {
	withTTY(t)

	in := "❯ \x1b[7;37mH\x1b[mello \x1b[32mgreen\x1b[m\n\n  \x1b[7;37mW\x1b[mork"
	wantPlain := "❯ Hello green\n\n  Work"

	got := dimBlurredInput(in)
	if _, _, ok := findReverseCell(got); ok {
		t.Errorf("cursor cell must not survive the blur transform: %q", got)
	}
	if plain := ansi.Strip(got); plain != wantPlain {
		t.Errorf("visible text/rows changed:\ngot  %q\nwant %q", plain, wantPlain)
	}
	for i, line := range strings.Split(got, "\n") {
		if line == "" {
			continue // paintLines leaves blank rows unwrapped
		}
		if !strings.HasPrefix(line, dimCode()) || !strings.HasSuffix(line, ansiReset) {
			t.Errorf("line %d must be self-contained dim: %q", i, line)
		}
	}

	// Non-TTY: no SGR at all, still cursorless.
	isTTY = false
	if got := dimBlurredInput(in); got != wantPlain {
		t.Errorf("non-TTY blurred input = %q, want plain stripped %q", got, wantPlain)
	}
}

// TestColorizeInput_V2ReverseCursorSurvives pins the cursor fix: Bubble Tea
// v2 renders the virtual cursor as a reverse-attributed SGR run with extra
// attributes ("\x1b[7;37m<char>\x1b[m"), not the v1-exact "\x1b[7m".
// colorizeInput rewrites line 0 from stripped text, so unless the cell is
// found and re-injected the cursor vanishes from the rendered input — which
// is exactly how the missing blink presented.
func TestColorizeInput_V2ReverseCursorSurvives(t *testing.T) {
	withTTY(t)
	m := layoutTestModel(t, 120, 40)

	for _, tc := range []struct {
		name string
		view string
	}{
		{"v2 reverse+fg", "❯ \x1b[7;37mh\x1b[mello"},
		{"v1 plain reverse", "❯ \x1b[7mh\x1b[mello"},
		{"reverse after fg", "❯ \x1b[37;7mh\x1b[mello"},
		{"cursor on space (empty input)", "❯ \x1b[7;37m \x1b[m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := m.colorizeInput(tc.view)
			if !strings.Contains(got, "\x1b[7m") {
				t.Fatalf("cursor cell lost in output: %q", got)
			}
			// The re-injected cell must sit right after the prompt.
			idx := strings.Index(got, "\x1b[7m")
			if !strings.HasPrefix(got, "❯ \x1b[7m") {
				t.Errorf("cursor not re-injected at the input column (byte %d): %q", idx, got)
			}
		})
	}
}

// TestColorizeInput_CursorSurvivesPromptlessPath: when line 0 carries no "❯"
// prompt (an ask/permission row replaced it), the extracted cursor must not
// be silently dropped on the early return.
func TestColorizeInput_CursorSurvivesPromptlessPath(t *testing.T) {
	withTTY(t)
	m := layoutTestModel(t, 120, 40)
	got := m.colorizeInput("Allow? \x1b[7;37my\x1b[m")
	if !strings.Contains(got, "\x1b[7m") {
		t.Fatalf("cursor cell lost on the promptless path: %q", got)
	}
}

// TestColorizeInput_NoCursorNoReverse: with the cursor blinked out (no cell
// in the view) the output must not gain a stray reverse cell.
func TestColorizeInput_NoCursorNoReverse(t *testing.T) {
	withTTY(t)
	m := layoutTestModel(t, 120, 40)
	got := m.colorizeInput("❯ hello world")
	if strings.Contains(got, "\x1b[7m") {
		t.Errorf("phantom reverse cell without a cursor: %q", got)
	}
}

// TestTextareaCursor_BlinkChainReArms guards the full model-level loop:
// Init's initial blink message must start a chain that re-arms on every
// BlinkMsg and toggles the cursor cell in the textarea view. Value semantics
// matter here — Update returns a new model, and dropping the first return
// silently kills the chain (that mistake once masqueraded as the product bug).
func TestTextareaCursor_BlinkChainReArms(t *testing.T) {
	m := layoutTestModel(t, 140, 40)

	next, cmd := m.Update(textarea.Blink())
	m = next.(model)
	if cmd == nil {
		t.Fatal("initial blink message produced no follow-up command")
	}
	for round := 1; round <= 3; round++ {
		msg := cmd()
		before := m.ta.View()
		next, cmd = m.Update(msg)
		m = next.(model)
		if cmd == nil {
			t.Fatalf("blink loop did not re-arm after round %d", round)
		}
		if m.ta.View() == before {
			t.Fatalf("cursor cell did not toggle in round %d", round)
		}
	}
}
