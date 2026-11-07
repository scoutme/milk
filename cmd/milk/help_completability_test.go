package main

import (
	"slices"
	"strings"
	"testing"
)

// TestInteractiveHelp_Completability guards the buildCmdVariants contract: a
// help line reaches Tab completion only when it starts with "/" AND separates
// signature from description with at least two spaces (buildCmdVariants splits
// on "  " — a single space silently drops the variant, which is how
// "/setup telegram status" and "/export --no-reasoning" went missing from
// completion while still rendering in /help).
//
// Lines excluded from completion must be exactly the known non-variant lines
// below: usage examples whose primary line is a completable variant, prose
// continuations that merely mention a command, and ACP-only documentation
// (excluded by design). A new line appearing in the diff means a subcommand
// just became invisible to Tab — fix the two-space separator, or extend the
// allowlist with the reason.
func TestInteractiveHelp_Completability(t *testing.T) {
	allow := map[string]string{
		"/agent add name=… url=… model=… [provider=…] [api_key=…] [aws_region=…] [run_cmd=…]": "usage example; /agent add itself is a variant",
		"/think on|off          ACP only: relay or withhold thought chunks":                   "ACP-only by design (buildCmdVariants skips these)",
		"/mcp add name=… url=… [transport=http] [auth=…] [api_key=…] [timeout=…]":             "usage example; /mcp add itself is a variant",
		"/mcp add name=… transport=stdio command=… [args=arg1,arg2,…]":                        "usage example; /mcp add itself is a variant",
		"/agent tool enable|disable|add|remove accept comma separated lists,":                 "prose continuation; the real /agent tool <op> lines are variants",
		"/think expand|collapse folds or opens them all":                                      "prose continuation; /think expand and /think collapse are separate variants",
	}

	dropped := map[string]bool{}
	for line := range strings.SplitSeq(interactiveHelp, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "/") {
			continue
		}
		// Replicate buildCmdVariants' drop conditions exactly.
		parts := strings.SplitN(trimmed, "  ", 2)
		droppedBySep := len(parts) < 2
		droppedByDesc := false
		if !droppedBySep {
			desc := strings.TrimSpace(parts[1])
			droppedByDesc = desc == "" || strings.HasPrefix(desc, "ACP only")
		}
		if droppedBySep || droppedByDesc {
			dropped[trimmed] = true
		}
	}

	for line := range dropped {
		if _, ok := allow[line]; !ok {
			t.Errorf("help line is invisible to Tab completion (no two-space sig/desc separator?): %q — fix its spacing or add it to the allowlist with a reason", line)
		}
	}
	for line := range allow {
		if !dropped[line] {
			t.Errorf("allowlisted line is no longer dropped from completion — remove it from the allowlist: %q", line)
		}
	}
}

// TestSetupAndExportVariants_Present pins the two completions that the
// single-space separator regression removed.
func TestSetupAndExportVariants_Present(t *testing.T) {
	want := map[string][]string{
		"/setup":  {"/setup telegram", "/setup telegram on", "/setup telegram lite", "/setup telegram off", "/setup telegram status"},
		"/export": {"/export", "/export json", "/export <path>", "/export session <id>", "/export --no-reasoning"},
	}
	for cmd, sigs := range want {
		var got []string
		for _, v := range cmdVariants[cmd] {
			got = append(got, v.sig)
		}
		for _, sig := range sigs {
			if !slices.Contains(got, sig) {
				t.Errorf("cmdVariants[%q] missing %q (have: %v)", cmd, sig, got)
			}
		}
	}
}
