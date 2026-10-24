package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestWindowTitle_ReflectsState(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	if got := m.windowTitle(); got == "" || got[:4] != "milk" {
		t.Fatalf("title should start with the app name, got %q", got)
	}
	idle := m.windowTitle()
	m.busy = true
	if got := m.windowTitle(); got == idle || got[len(got)-len(titleWorking):] != titleWorking {
		t.Errorf("busy title should end with %q, got %q", titleWorking, got)
	}
	m.busy = false
	m.pendingPerm = &permRequestMsg{}
	if got := m.windowTitle(); got[len(got)-len(titleNeedsInput):] != titleNeedsInput {
		t.Errorf("pending prompt title should end with %q, got %q", titleNeedsInput, got)
	}
}

func TestView_DeclaresFocusReportingAndTitle(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	v := m.View()
	if !v.ReportFocus {
		t.Error("view must request focus reports")
	}
	if !v.AltScreen {
		t.Error("view must stay on the alt screen")
	}
	if v.WindowTitle == "" {
		t.Error("view must set a window title")
	}
}

func TestBlurPausesPanelPolling(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	m.panelGit = true
	next, _ := m.Update(tea.BlurMsg{})
	m = next.(model)
	if !m.blurred {
		t.Fatal("blur should mark the model")
	}
	_, cmd := m.Update(gitRefreshMsg{})
	if cmd == nil {
		t.Fatal("the poll timer must stay armed while blurred")
	}
	next, _ = m.Update(tea.FocusMsg{})
	if next.(model).blurred {
		t.Error("focus should clear the blurred flag")
	}
}

func TestBlurFreezesTipRotation(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	m.blurred = true
	before := m.tipIdx
	next, _ := m.Update(tipsTickMsg{gen: m.tipsGen})
	if next.(model).tipIdx != before {
		t.Error("tips must not rotate while the window is blurred")
	}
}
