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

// TestExtractSlashCommand_LeadingTokenOnly guards against issue #151's root
// cause: a command mentioned anywhere in the input (typed or pasted) used to
// fire, not just when it's the deliberate first thing in the line.
func TestExtractSlashCommand_LeadingTokenOnly(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantCmd   string
		wantRest  string
		wantFound bool
	}{
		{
			name:      "leading command fires",
			input:     "/help",
			wantCmd:   "/help",
			wantRest:  "",
			wantFound: true,
		},
		{
			name:      "leading command with prompt",
			input:     "/escalate please look into this",
			wantCmd:   "/escalate",
			wantRest:  "please look into this",
			wantFound: true,
		},
		{
			name:      "command mid-sentence is inert",
			input:     "please run /help now",
			wantCmd:   "",
			wantRest:  "please run /help now",
			wantFound: false,
		},
		{
			name:      "command embedded in a pasted transcript is inert",
			input:     "Here is my earlier turn:\n> /learn milk likes Go\nand the reply",
			wantCmd:   "",
			wantRest:  "Here is my earlier turn:\n> /learn milk likes Go\nand the reply",
			wantFound: false,
		},
		{
			name:      "unknown leading slash word is not a command",
			input:     "/notacommand foo bar",
			wantCmd:   "",
			wantRest:  "/notacommand foo bar",
			wantFound: false,
		},
		{
			name:      "empty input",
			input:     "",
			wantCmd:   "",
			wantRest:  "",
			wantFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, rest, found := extractSlashCommand(tc.input)
			if cmd != tc.wantCmd || rest != tc.wantRest || found != tc.wantFound {
				t.Errorf("extractSlashCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.input, cmd, rest, found, tc.wantCmd, tc.wantRest, tc.wantFound)
			}
		})
	}
}
