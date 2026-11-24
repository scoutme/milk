package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Issue #453: "**bold with *nested single emphasis***" rendered completely raw
// behind the old star-free-content reStrong. The run-aware passes must style
// it as bold with italic inside and consume every marker.

func TestStyleInlineSpans_NestedEmphasisInStrong(t *testing.T) {
	in := "make this **bold with *nested single emphasis*** render"
	got := styleInlineSpans(in)

	if !strings.Contains(got, ansiBold) {
		t.Errorf("expected bold span, got %q", got)
	}
	if !strings.Contains(got, ansiItalic) {
		t.Errorf("expected italic span inside the strong, got %q", got)
	}
	if strings.Contains(got, "*") {
		t.Errorf("all markers must be consumed, got %q", got)
	}
	if !strings.Contains(got, "render") {
		t.Errorf("trailing text lost, got %q", got)
	}
}

func TestStyleInlineSpans_StrongPlainRegression(t *testing.T) {
	// The pre-#453 behaviour for simple strong must not change.
	got := styleInlineSpans("This is **bold text** here")
	if got != "This is "+ansiBold+"bold text"+ansiReset+" here" {
		t.Errorf("plain strong regressed: %q", got)
	}
}

func TestStyleInlineSpans_SplitStrong(t *testing.T) {
	// a**b**c — unbalanced-adjacent pairs each style independently.
	got := styleInlineSpans("a**b**c")
	want := "a" + ansiBold + "b" + ansiReset + "c"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStyleInlineSpans_UnbalancedStrongStaysRaw(t *testing.T) {
	got := styleInlineSpans("just **bold with no closer here")
	if !strings.Contains(got, "**bold") {
		t.Errorf("unbalanced opener must stay raw, got %q", got)
	}
	if strings.Contains(got, ansiBold) {
		t.Errorf("no bold should be applied, got %q", got)
	}
}

func TestStyleInlineSpans_TripleStillStrongItalic(t *testing.T) {
	got := styleInlineSpans("***already triple***")
	if !strings.Contains(got, ansiBold) || !strings.Contains(got, ansiItalic) {
		t.Errorf("reStrongEM path regressed: %q", got)
	}
	if strings.Contains(got, "*") {
		t.Errorf("markers must be consumed: %q", got)
	}
}

func TestStyleInlineSpans_StrongThenLiteralStar(t *testing.T) {
	// **a*** — CommonMark: <strong>a</strong>* : the three-star tail is a
	// strong close plus a leftover literal star (no inner emphasis is open,
	// so its first star must NOT be swallowed into the bold content).
	got := styleInlineSpans("**a***")
	if !strings.Contains(got, ansiBold) {
		t.Errorf("expected bold, got %q", got)
	}
	if strings.Contains(got, ansiItalic) {
		t.Errorf("no inner emphasis exists here: %q", got)
	}
	stripped := strings.ReplaceAll(strings.ReplaceAll(got, ansiBold, ""), ansiReset, "")
	if stripped != "a*" {
		t.Errorf("got %q, want %q (bold a + literal star)", stripped, "a*")
	}
}

func TestStyleInlineSpans_StandaloneEmphasis(t *testing.T) {
	// *emphasis* is now styled (it never was before #453).
	got := styleInlineSpans("use *emphasis* here")
	if !strings.Contains(got, ansiItalic) {
		t.Errorf("expected italic, got %q", got)
	}
	if strings.Contains(got, "*") {
		t.Errorf("markers must be consumed: %q", got)
	}
}

func TestStyleInlineSpans_EmphasisFlanking(t *testing.T) {
	// The opener must not be followed by a space: "*not emphasis *" stays raw
	// (no closer with a non-space before it either) — stars in shell-ish text
	// must not light up by accident when unbalanced.
	got := styleInlineSpans("echo * file * count")
	if strings.Contains(got, ansiItalic) {
		t.Errorf("space-flanked stars are not emphasis: %q", got)
	}
	if !strings.Contains(got, "* file *") {
		t.Errorf("markers must stay literal: %q", got)
	}
}

func TestStyleInlineSpans_EmphasisAroundCodeSpans(t *testing.T) {
	got := styleInlineSpans("*see `code` now*")
	if !strings.Contains(got, ansiItalic) {
		t.Errorf("expected emphasis wrapping the code span, got %q", got)
	}
	if strings.Contains(got, "*") {
		t.Errorf("markers must be consumed: %q", got)
	}
	// The code span keeps its own dim/yellow styling — assert on the visible
	// text (ANSI stripped), where the backticks wrap the code word.
	visible := ansi.Strip(got)
	if !strings.Contains(visible, "`code`") {
		t.Errorf("code span markers lost: visible %q (raw %q)", visible, got)
	}
}
