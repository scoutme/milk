package main

// The terminal-paint protocol (theme.go): every switch restores the
// terminal's own palette and STAGES the paint for the background answer —
// painting before the answer would make the query echo milk's own colour
// and latch it forever (the bug that kept off/space light after a visit to
// the light supermilk skin). These tests pin the staging contract at every
// site: stage on entry, perform when the answer arrives (even if the
// verdict did not change), fall back on the timeout, never leave the flag
// set.

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/testutil"
)

// bgAnswer is a BackgroundColorMsg carrying the given darkness.
func bgAnswer(dark bool) tea.Msg {
	c := color.NRGBA{R: 0xf0, G: 0xf0, B: 0xf0, A: 0xff}
	if dark {
		c = color.NRGBA{R: 0x10, G: 0x10, B: 0x10, A: 0xff}
	}
	return tea.BackgroundColorMsg{Color: c}
}

// paintProtocolEnv sandboxes the config writes the switch paths perform and
// pins the detected ground so assertions on the latch are deterministic.
func paintProtocolEnv(t *testing.T) {
	t.Helper()
	testutil.SandboxHome(t)
	t.Setenv("USERPROFILE", t.TempDir())
	setDarkBackground(true)
	t.Cleanup(func() { setDarkBackground(true) })
}

// TestThemeSwitchStagesTerminalPaint: /theme <name> must not paint before
// the answer — it restores, stages, and returns the re-detect batch; the
// answer then performs the paint even when the verdict happens to be
// unchanged (the common case: dark terminal, dark verdict).
func TestThemeSwitchStagesTerminalPaint(t *testing.T) {
	paintProtocolEnv(t)

	m := layoutTestModel(t, 80, 24)
	out, cmd := m.handleThemeCmd("supermilk")
	m = out.(model)
	if !m.pendingTerminalApply {
		t.Fatal("/theme must stage the terminal paint — a pre-answer paint would latch its own colour")
	}
	if cmd == nil {
		t.Fatal("/theme must return the re-detect batch")
	}

	// Answer with the SAME verdict as the current latch: the paint must
	// still happen (pending cleared), or a theme would never reach the
	// terminal on a terminal whose ground never changes.
	out, _ = m.Update(bgAnswer(true))
	m = out.(model)
	if m.pendingTerminalApply {
		t.Error("the answer must perform the staged paint even when the verdict is unchanged")
	}
}

// TestThemeSwitchAnswerFlipsLatch: an answer that changes the verdict
// rebuilds the chrome AND performs the staged paint — the healed path for a
// session whose latch was poisoned before the fix.
func TestThemeSwitchAnswerFlipsLatch(t *testing.T) {
	paintProtocolEnv(t)

	m := layoutTestModel(t, 80, 24)
	out, _ := m.handleThemeCmd("off")
	m = out.(model)
	if !m.pendingTerminalApply {
		t.Fatal("switch must stage the paint")
	}

	out, _ = m.Update(bgAnswer(false)) // the terminal reports light
	m = out.(model)
	if m.pendingTerminalApply {
		t.Error("a verdict change must still perform the staged paint")
	}
	if hasDarkBackground() {
		t.Error("the latch must follow the terminal's answer")
	}
}

// TestTerminalPaintTimeoutFallback: a terminal that never answers OSC 11
// still gets its paint — from the timeout, with the current verdict — and
// an already-performed paint is not redone.
func TestTerminalPaintTimeoutFallback(t *testing.T) {
	paintProtocolEnv(t)

	m := layoutTestModel(t, 80, 24)
	m.pendingTerminalApply = true
	out, _ := m.Update(terminalPaintTimeoutMsg{})
	m = out.(model)
	if m.pendingTerminalApply {
		t.Error("the timeout must perform (and clear) the staged paint")
	}

	out, _ = m.Update(terminalPaintTimeoutMsg{}) // second fire: no-op
	m = out.(model)
	if m.pendingTerminalApply {
		t.Error("an idle timeout must leave the flag clear")
	}
}

// TestCarouselAndWizardStageTerminalPaint: the browse confirm, the browse
// cancel and the builder's cancel all re-anchor the terminal through the
// same staging — none of them may paint or query without restoring first.
func TestCarouselAndWizardStageTerminalPaint(t *testing.T) {
	paintProtocolEnv(t)

	t.Run("carousel confirm", func(t *testing.T) {
		m := carouselTestModel(t)
		if m.pendingTerminalApply {
			t.Fatal("arming the carousel must not stage a paint (no switch yet)")
		}
		out, cmd := m.themeCarouselConfirm()
		m = out.(model)
		if !m.pendingTerminalApply {
			t.Error("confirm must stage the terminal paint")
		}
		if cmd == nil {
			t.Error("confirm must return the re-detect batch")
		}
	})

	t.Run("carousel close", func(t *testing.T) {
		m := carouselTestModel(t)
		out, cmd := m.themeCarouselClose()
		m = out.(model)
		if !m.pendingTerminalApply {
			t.Error("cancel-close must stage the terminal paint")
		}
		if cmd == nil {
			t.Error("cancel-close must return the re-detect batch")
		}
	})

	t.Run("wizard cancel", func(t *testing.T) {
		m := layoutTestModel(t, 80, 24)
		setTheme("supermilk")
		t.Cleanup(func() { setTheme("off") })
		m.pendingThemeWizard = &themeWizardState{mode: twVariant, orig: "off"}
		out, cmd := m.themeWizardCancel()
		m = out.(model)
		if !m.pendingTerminalApply {
			t.Error("the builder's cancel must stage the terminal paint")
		}
		if cmd == nil {
			t.Error("cancel must return the re-detect batch")
		}
		if th.name != "off" {
			t.Errorf("cancel must restore the original theme, got %q", th.name)
		}
	})
}
