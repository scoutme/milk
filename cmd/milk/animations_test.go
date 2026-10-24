package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/scoutme/milk/internal/config"
)

func withAnimations(t *testing.T) {
	t.Helper()
	old := animationsEnabled
	animationsEnabled = true
	t.Cleanup(func() { animationsEnabled = old })
}

// runTicks feeds animation ticks until the model stops asking for more.
func runTicks(t *testing.T, m model) model {
	t.Helper()
	for i := 0; i < 600; i++ {
		next, cmd := m.Update(animTickMsg{})
		m = next.(model)
		if cmd == nil {
			return m
		}
	}
	t.Fatal("animation never settled")
	return m
}

func tallModel(t *testing.T) model {
	t.Helper()
	m := layoutTestModel(t, 140, 40)
	for i := 0; i < 200; i++ {
		m.appendTranscript("line of transcript text\n")
	}
	m.setViewportContent()
	m.vp.SetYOffset(100)
	return m
}

func TestSmoothScroll_GlidesToTargetWithoutOvershoot(t *testing.T) {
	withAnimations(t)
	m := tallModel(t)
	start := m.vp.YOffset()
	cmd := m.smoothScroll(&m.vp, -9)
	if cmd == nil {
		t.Fatal("a wheel step must arm the animation tick")
	}
	if m.vp.YOffset() != start {
		t.Fatal("the offset must not jump: the spring moves it frame by frame")
	}
	last := start
	for i := 0; i < 600; i++ {
		next, c := m.Update(animTickMsg{})
		m = next.(model)
		if y := m.vp.YOffset(); y > last || y < start-9 {
			t.Fatalf("offset %d left the path %d -> %d", y, start, start-9)
		} else {
			last = y
		}
		if c == nil {
			break
		}
	}
	if m.vp.YOffset() != start-9 {
		t.Errorf("settled at %d, want %d", m.vp.YOffset(), start-9)
	}
}

func TestSmoothScroll_ClampsAtTheEdges(t *testing.T) {
	withAnimations(t)
	m := tallModel(t)
	m.vp.SetYOffset(2)
	m.smoothScroll(&m.vp, -30)
	m = runTicks(t, m)
	if m.vp.YOffset() != 0 {
		t.Errorf("scrolling past the top must stop at 0, got %d", m.vp.YOffset())
	}
}

func TestSmoothScroll_StandsDownWhenSomethingElseScrolls(t *testing.T) {
	withAnimations(t)
	m := tallModel(t)
	m.smoothScroll(&m.vp, -30)
	m.vp.SetYOffset(150) // e.g. new output followed to the bottom
	m = runTicks(t, m)
	if m.vp.YOffset() != 150 {
		t.Errorf("the animation must not fight an outside scroll, got %d", m.vp.YOffset())
	}
}

func TestSmoothScroll_InstantWhenAnimationsAreOff(t *testing.T) {
	m := tallModel(t)
	start := m.vp.YOffset()
	if cmd := m.smoothScroll(&m.vp, -3); cmd != nil {
		t.Error("no tick when animations are off")
	}
	if m.vp.YOffset() != start-3 {
		t.Errorf("offset = %d, want %d", m.vp.YOffset(), start-3)
	}
}

func TestPanelSlide_OpensDisplacedAndSettlesInPlace(t *testing.T) {
	withAnimations(t)
	m := layoutTestModel(t, 140, 40)
	m.panelTasks = false
	settled := ansi.Strip(m.viewString())

	prev := m
	m.panelTasks = true
	m.syncLayout()
	if cmd := m.trackAnimations(&prev); cmd == nil {
		t.Fatal("opening a panel must start the slide")
	}
	if shift, sliding := m.anim.slideShift(regionTasks); !sliding || shift != panelSlotWidth {
		t.Fatalf("the panel starts fully displaced, got shift=%d sliding=%v", shift, sliding)
	}
	m = runTicks(t, m)
	if _, sliding := m.anim.slideShift(regionTasks); sliding {
		t.Error("the slide must end")
	}
	if got := ansi.Strip(m.viewString()); got == settled {
		t.Error("the open panel must be part of the final layout")
	}
}

func TestPanelSlide_ClosingLeavesAGhostThatSlidesOut(t *testing.T) {
	withAnimations(t)
	oldTTY := isTTY
	isTTY = true // panels render blank rows without a TTY
	t.Cleanup(func() { isTTY = oldTTY })
	m := layoutTestModel(t, 140, 40)
	m.panelTasks = true
	m.syncLayout()
	prev := m
	m.panelTasks = false
	m.syncLayout()
	if cmd := m.trackAnimations(&prev); cmd == nil {
		t.Fatal("closing the rightmost panel must start the slide-out")
	}
	pa := m.anim.panels[regionTasks]
	if pa == nil || pa.ghost == nil {
		t.Fatal("the closing panel keeps a ghost of its last frame")
	}
	if got := ansi.Strip(m.viewString()); !strings.Contains(got, "tasks") {
		t.Error("the ghost is drawn over the freed columns on the first frame")
	}
	m = runTicks(t, m)
	if len(m.anim.panels) != 0 {
		t.Error("the ghost is gone once it has slid out")
	}
}

func TestPanelSlide_NoAnimationOnResizeOrWhenOff(t *testing.T) {
	m := layoutTestModel(t, 140, 40)
	prev := m
	m.panelTasks = true
	if cmd := m.trackAnimations(&prev); cmd != nil {
		t.Error("animations off: panels switch at once")
	}
	withAnimations(t)
	m2 := layoutTestModel(t, 140, 40)
	next, _ := m2.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if a := next.(model).anim; a != nil && len(a.panels) > 0 {
		t.Error("a resize must not animate panels")
	}
}

func TestSlideRight_KeepsWidthAndDisplacesContent(t *testing.T) {
	block := strings.Repeat("x", panelSlotWidth) + "\n" + strings.Repeat("y", panelSlotWidth)
	got := slideRight(block, 10)
	for _, line := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(line); w != panelSlotWidth {
			t.Errorf("width %d, want %d: %q", w, panelSlotWidth, line)
		}
		if !strings.HasPrefix(line, strings.Repeat(" ", 10)) {
			t.Errorf("content must start 10 columns in: %q", line)
		}
	}
	if slideRight(block, 0) != block {
		t.Error("no shift, no change")
	}
}

func TestConfig_AnimationsDefaultOnAndSwitchable(t *testing.T) {
	var c config.Config
	if !c.AnimationsEnabled() {
		t.Error("animations default to on")
	}
	off := false
	c.Animations = &off
	if c.AnimationsEnabled() {
		t.Error("animations: false must turn them off")
	}
}
