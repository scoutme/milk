package main

import (
	"strings"
	"testing"
)

// TestInteractiveHelp_MentionsAllPanelsAndShortcuts guards against the exact
// drift found in production: /help once documented only /panel memory and
// /panel tasks and no F-keys at all, even though /panel background,
// /panel workflow, and the F1-F4 shortcuts were all fully wired (repl.go's
// F-key switch, panelShortcut). Checks against panelShortcut directly so a
// future fifth panel with no shortcut text fails here instead of silently
// shipping undocumented, the same way this one did.
func TestInteractiveHelp_MentionsAllPanelsAndShortcuts(t *testing.T) {
	for _, panel := range []string{"/panel memory", "/panel tasks", "/panel background", "/panel workflow"} {
		if !strings.Contains(interactiveHelp, panel) {
			t.Errorf("interactiveHelp missing %q", panel)
		}
	}
	for _, region := range []panelRegion{regionMemory, regionTasks, regionBackground, regionWorkflow} {
		hint := panelShortcut(region)
		if hint == "" {
			t.Fatalf("panelShortcut(%v) returned no hint — update this test alongside the region", region)
		}
		if !strings.Contains(interactiveHelp, hint) {
			t.Errorf("interactiveHelp doesn't mention %q, the shortcut for panel region %v", hint, region)
		}
	}
}
