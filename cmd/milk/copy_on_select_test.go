package main

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/scoutme/milk/internal/config"
)

func stubClipboard(t *testing.T) *[]string {
	t.Helper()
	var got []string
	orig := copyToClipboard
	copyToClipboard = func(text string) { got = append(got, text) }
	t.Cleanup(func() { copyToClipboard = orig })
	return &got
}

func selectTranscriptWithMouse(m *model) tea.Cmd {
	m.appendTranscript("hello world\n")
	m.setViewportContent()
	m.handleMouse(mouseEvent{X: 0, Y: 2, Button: tea.MouseLeft, Action: mousePress}.msg())
	m.handleMouse(mouseEvent{X: 5, Y: 2, Button: tea.MouseLeft, Action: mouseMotion}.msg())
	_, cmd := m.handleMouse(mouseEvent{X: 5, Y: 2, Button: tea.MouseLeft, Action: mouseRelease}.msg())
	return cmd
}

func TestCopyOnSelect_TranscriptRelease(t *testing.T) {
	copied := stubClipboard(t)
	on := true
	m := dragTestModel()
	m.st.cfg = config.Config{CopyOnSelect: &on}

	cmd := selectTranscriptWithMouse(m)

	if len(*copied) != 1 || (*copied)[0] != "hello" {
		t.Fatalf("expected clipboard [hello], got %q", *copied)
	}
	if cmd == nil || m.copyFeedback != "copied 5 chars" {
		t.Errorf("expected copy feedback + clear cmd, got %q cmd=%v", m.copyFeedback, cmd)
	}
	if m.selAnchorLine != -1 || m.selText != "" {
		t.Error("selection should be cleared after copy so a right-click pastes")
	}
}

func TestCopyOnSelect_DisabledKeepsSelectionUncopied(t *testing.T) {
	copied := stubClipboard(t)
	m := dragTestModel()

	selectTranscriptWithMouse(m)

	if len(*copied) != 0 {
		t.Fatalf("copy_on_select unset must not copy, got %q", *copied)
	}
	if m.selText != "hello" {
		t.Errorf("selection should stay pending, got %q", m.selText)
	}
}

func TestCopyOnSelect_ClickWithoutDragDoesNotCopy(t *testing.T) {
	copied := stubClipboard(t)
	on := true
	m := dragTestModel()
	m.st.cfg = config.Config{CopyOnSelect: &on}
	m.appendTranscript("hello world\n")
	m.setViewportContent()

	m.handleMouse(mouseEvent{X: 3, Y: 2, Button: tea.MouseLeft, Action: mousePress}.msg())
	m.handleMouse(mouseEvent{X: 3, Y: 2, Button: tea.MouseLeft, Action: mouseRelease}.msg())

	if len(*copied) != 0 {
		t.Fatalf("a plain click must not copy, got %q", *copied)
	}
}

func TestCopyOnSelect_AttachRelease(t *testing.T) {
	copied := stubClipboard(t)
	on := true
	m := attachSelTestModel(t, "alpha beta\n")
	m.st.cfg = config.Config{CopyOnSelect: &on}

	attachClick(m, 0, 4)
	attachDrag(m, 5, 4)
	attachRelease(m, 5, 4)

	if len(*copied) != 1 || (*copied)[0] != "alpha" {
		t.Fatalf("expected clipboard [alpha], got %q", *copied)
	}
	if m.attachSelText != "" || m.attachSelAnchorLine != -1 {
		t.Error("attach selection should be cleared after copy")
	}
}
