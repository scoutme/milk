package main

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/testutil"
)

// The theme carousel (plan PR 2): bare /theme arms the live-preview browse,
// arrows/wheel cycle candidates, enter applies + persists, and ANY other key
// closes and restores the original theme — a browse can never leave the TUI
// in a half-painted skin.

func carouselTestModel(t *testing.T) model {
	t.Helper()
	testutil.SandboxHome(t)
	t.Setenv("USERPROFILE", t.TempDir())
	old := th.name
	t.Cleanup(func() { setTheme(old) })
	m := layoutTestModel(t, 80, 24)
	out, _ := m.handleThemeCmd("")
	m = out.(model)
	if m.pendingThemeCarousel == nil {
		t.Fatal("bare /theme must arm the carousel")
	}
	return m
}

func TestThemeCarouselBrowseCancelRestores(t *testing.T) {
	m := carouselTestModel(t)
	orig := m.pendingThemeCarousel.orig

	out, _ := m.themeCarouselStep(+1)
	m = out.(model)
	if th.name == orig {
		t.Fatal("stepping must live-preview the next theme")
	}
	if got := m.pendingThemeCarousel.candidate(); got != th.name {
		t.Errorf("previewed %q but candidate is %q", th.name, got)
	}

	out, _ = m.handleThemeCarouselKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = out.(model)
	if m.pendingThemeCarousel != nil {
		t.Error("any other key must close the carousel")
	}
	if th.name != orig {
		t.Errorf("cancel restored %q, want %q", th.name, orig)
	}
}

func TestThemeCarouselCtrlCAlsoCloses(t *testing.T) {
	m := carouselTestModel(t)
	orig := m.pendingThemeCarousel.orig
	out, _ := m.themeCarouselStep(+1)
	m = out.(model)
	out, _ = m.handleThemeCarouselKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = out.(model)
	if m.pendingThemeCarousel != nil || th.name != orig {
		t.Errorf("ctrl+c must close and restore (open=%v theme=%q)",
			m.pendingThemeCarousel != nil, th.name)
	}
}

func TestThemeCarouselEnterAppliesAndPersists(t *testing.T) {
	m := carouselTestModel(t)
	out, _ := m.themeCarouselStep(+1)
	m = out.(model)
	want := m.pendingThemeCarousel.candidate()

	out, cmd := m.handleThemeCarouselKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = out.(model)
	if m.pendingThemeCarousel != nil {
		t.Error("enter must close the carousel")
	}
	if th.name != want {
		t.Errorf("enter applied %q, want %q", th.name, want)
	}
	if m.st.cfg.Theme != want {
		t.Errorf("cfg.Theme = %q, want %q", m.st.cfg.Theme, want)
	}
	if cmd == nil {
		t.Fatal("confirm must return the ClearScreen/RequestBackgroundColor batch")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("confirm must return a batch")
	}
	wantType := reflect.TypeOf(tea.RequestBackgroundColor())
	found := false
	for _, c := range batch {
		if c != nil && reflect.TypeOf(c()) == wantType {
			found = true
		}
	}
	if !found {
		t.Error("the batch must re-detect the terminal background (the skin may paint it)")
	}
}

func TestThemeCarouselWheelCyclesAndPressCloses(t *testing.T) {
	m := carouselTestModel(t)
	orig := m.pendingThemeCarousel.orig
	wheelDown := mouseEvent{Action: mousePress, Button: tea.MouseWheelDown, X: 10, Y: 10}.msg()
	out, _ := m.handleMouse(wheelDown)
	m = out.(model)
	if th.name == orig {
		t.Error("wheel down must cycle to the next theme")
	}
	wheelUp := mouseEvent{Action: mousePress, Button: tea.MouseWheelUp, X: 10, Y: 10}.msg()
	out, _ = m.handleMouse(wheelUp)
	m = out.(model)
	if th.name != orig {
		t.Errorf("wheel up must cycle back (got %q)", th.name)
	}
	press := mouseEvent{Action: mousePress, Button: tea.MouseLeft, X: 10, Y: 10}.msg()
	out, _ = m.handleMouse(press)
	m = out.(model)
	if m.pendingThemeCarousel != nil {
		t.Error("a mouse press must close the carousel")
	}
	if th.name != orig {
		t.Errorf("closing restored %q, want %q", th.name, orig)
	}
}

func TestThemeCarouselWrapsAround(t *testing.T) {
	m := carouselTestModel(t)
	w := m.pendingThemeCarousel
	w.idx = 0
	out, _ := m.themeCarouselStep(-1)
	if got := m.pendingThemeCarousel.idx; got != len(w.names)-1 {
		t.Errorf("stepping before the first theme wrapped to %d, want %d", got, len(w.names)-1)
	}
	_ = out
}

func TestThemeCarouselNarrowTerminalFallsBack(t *testing.T) {
	testutil.SandboxHome(t)
	old := th.name
	t.Cleanup(func() { setTheme(old) })
	m := layoutTestModel(t, 24, 10)
	out, _ := m.handleThemeCmd("")
	m = out.(model)
	if m.pendingThemeCarousel != nil {
		t.Error("a narrow terminal must fall back to the plain listing")
	}
}

func TestThemeViewTextShowsAnyThemesJSON(t *testing.T) {
	// Builtins: the embedded descriptor, same format as a drop-in file.
	body, ok := themeViewText("supermilk")
	if !ok || !strings.Contains(body, `"name": "supermilk"`) || !strings.Contains(body, "ansi16") {
		t.Errorf("builtin view must show its JSON, got %q ok=%v", body, ok)
	}
	if _, ok := themeViewText("nope"); ok {
		t.Error("an unknown theme must not resolve for view")
	}
	// /theme view through the command surface: the active theme by default.
	testutil.SandboxHome(t)
	old := th.name
	t.Cleanup(func() { setTheme(old) })
	m := layoutTestModel(t, 80, 24)
	out, _ := m.handleThemeCmd("view")
	m = out.(model)
	if !strings.Contains(m.transcript.String(), `"name": "off"`) {
		t.Error("/theme view must show the active theme's JSON")
	}
}
