package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const (
	titleBase       = "milk"
	titleSeparator  = " · "
	titleNeedsInput = "needs input"
	titleWorking    = "working"
)

// windowTitle is the terminal tab title: the app, the active agent and, when
// it matters at a glance from another tab, whether milk waits on the user or
// is busy. Plain text only, escape sequences would corrupt the title.
func (m *model) windowTitle() string {
	if m.st == nil || m.st.sess == nil {
		return titleBase
	}
	parts := []string{titleBase}
	if agent := strings.TrimSpace(ansi.Strip(agentLabel(m.st))); agent != "" {
		parts = append(parts, agent)
	}
	switch {
	case m.hasPendingPrompt():
		parts = append(parts, titleNeedsInput)
	case m.busy:
		parts = append(parts, titleWorking)
	}
	return strings.Join(parts, titleSeparator)
}
