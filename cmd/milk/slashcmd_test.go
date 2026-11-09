package main

import (
	"slices"
	"strings"
	"testing"
)

// The registry (slashcmd.go) is the single source for the command language
// both hosts speak. These tests are the invariants that grant it: the host
// handler tables stay in sync with the registry in both directions, the help
// text and completion set cover the same commands, and every listing a user
// or a client sees is rendered from registry data.

// TestRegistry_ACPSurface pins the ACP binding table to the registry in both
// directions: every ACP-hosted spec has exactly one handler binding, every
// binding resolves to a registry spec, and what available_commands_update
// advertises is exactly the registry's non-hidden ACP rows.
func TestRegistry_ACPSurface(t *testing.T) {
	bound := map[string]bool{}
	for _, c := range acpCommandTable() {
		if bound[c.name] {
			t.Errorf("duplicate binding for %s", c.name)
		}
		bound[c.name] = true
		spec, ok := specFor(c.name)
		if !ok {
			t.Errorf("binding %q is not in the registry", c.name)
			continue
		}
		if !spec.in(hostACP) {
			t.Errorf("binding %q exists but the registry says hosts=%d (not ACP)", c.name, spec.hosts)
		}
		if c.run == nil && c.runTurn == nil {
			t.Errorf("binding %q has neither run nor runTurn", c.name)
		}
		if !spec.hidden && spec.desc == "" {
			t.Errorf("advertised command %q has no desc", c.name)
		}
	}
	for _, s := range commandRegistry {
		if !s.in(hostACP) {
			continue
		}
		if !bound[s.name] {
			t.Errorf("registry spec %q is ACP-hosted but has no handler binding", s.name)
		}
	}
}

// TestRegistry_TUISurface pins the TUI's recognition and help surface to the
// registry: extractSlashCommand's set is exactly the registry's names, and
// the help text documents every visible TUI-hosted command.
func TestRegistry_TUISurface(t *testing.T) {
	// The recognition set is dual-store (static registry ∪ runtime plugin
	// commands, ADR-0104 Part IV): registryNames() IS that union by
	// construction (the old var-snapshot equality it replaced). Pin the
	// membership direction every dispatch and help lookup relies on —
	// each recognized name resolves through the registry, the plugin
	// store, or both.
	regNames := map[string]bool{}
	for _, s := range commandRegistry {
		regNames[s.name] = true
		for _, a := range s.aliases {
			regNames[a] = true
		}
	}
	for _, s := range pluginSlashSpecs() {
		regNames[s.name] = true
	}
	for _, name := range registryNames() {
		if !regNames[name] {
			t.Errorf("recognized name %q resolves nowhere", name)
		}
	}
	help := buildInteractiveHelp()
	for _, s := range commandRegistry {
		if !s.in(hostTUI) || s.hidden {
			continue
		}
		// The name must appear in help — a shared line may document it
		// alongside a sibling ("/exit  /quit").
		if !strings.Contains(help, s.name) {
			t.Errorf("help text does not mention %q", s.name)
		}
	}
}

// TestRegistry_GrammarConsistency pins the argument grammar: every help line
// belongs to the command it is filed under, and the compact ACP hint never
// invents a literal the TUI's help lines do not show.
func TestRegistry_GrammarConsistency(t *testing.T) {
	for _, s := range commandRegistry {
		if s.help == "" {
			continue
		}
		for _, line := range strings.Split(s.help, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
				continue // continuation line
			}
			own := fields[0] == s.name || slices.Contains(s.aliases, fields[0])
			if !own {
				t.Errorf("%s: help line starts with %q — filed under the wrong command", s.name, fields[0])
			}
		}
		for _, h := range []string{s.hint, s.hintTUI} {
			for _, tok := range hintLiterals(h) {
				if !strings.Contains(s.help, tok) {
					t.Errorf("%s: hint literal %q never appears in its help lines", s.name, tok)
				}
			}
		}
	}
	// Completion variants come from the same generated help, so they see the
	// same grammar: every variant key is a registry command.
	for cmd := range cmdVariants {
		if !regName(cmd) {
			t.Errorf("cmdVariants key %q is not a registry name", cmd)
		}
	}
}

// TestRegistry_ListingsFromRegistry pins that what clients see is registry
// data: /help and available_commands_update describe exactly the
// non-hidden ACP rows, with the registry's desc and hint.
func TestRegistry_ListingsFromRegistry(t *testing.T) {
	want := map[string]cmdSpec{}
	for _, s := range commandRegistry {
		if s.in(hostACP) && !s.hidden {
			want[s.name] = s
		}
	}
	advertised := acpAdvertisedCommands()
	if len(advertised) != len(want) {
		t.Errorf("advertised %d commands, registry has %d non-hidden ACP specs", len(advertised), len(want))
	}
	for _, a := range advertised {
		name := "/" + a.Name
		spec, ok := want[name]
		if !ok {
			t.Errorf("advertised %q is not a non-hidden registry ACP spec", name)
			continue
		}
		if a.Description != spec.desc {
			t.Errorf("%s: description %q, registry says %q", name, a.Description, spec.desc)
		}
		wantHint := ""
		if spec.hint != "" {
			wantHint = spec.hint
		}
		gotHint := ""
		if a.Input != nil {
			gotHint = a.Input.Hint
		}
		if gotHint != wantHint {
			t.Errorf("%s: hint %q, registry says %q", name, gotHint, wantHint)
		}
	}
	// /help (the ACP listing) shows every advertised signature.
	out, _ := acpHelp(nil, "")
	for _, s := range want {
		sig := s.name
		if s.hint != "" {
			sig += " " + s.hint
		}
		if !strings.Contains(out, sig+" — "+s.desc) {
			t.Errorf("acpHelp does not list %q", sig)
		}
	}
}

func regName(cmd string) bool {
	_, ok := specFor(cmd)
	return ok
}

// TestRegistry_HelpLinesRealNewlines pins the help-rendering wart fixed in
// the #208 refinement: a help entry's line breaks must be real newlines — a
// literal "\n" inside a raw-string help block renders as two visible
// characters and merges the entry's lines into one.
func TestRegistry_HelpLinesRealNewlines(t *testing.T) {
	for _, s := range commandRegistry {
		if strings.Contains(s.help, "\\n") {
			t.Errorf("%s: help contains a literal backslash-n — use real newlines", s.name)
		}
	}
}

// hintLiterals extracts the literal (non-placeholder) words of a compact
// argument grammar like "[list|start <task>|stop <id>]" — the words a user
// must type. Tokens touched by brackets or angle brackets are placeholders
// ("<id|prefix|name>", "[name]", …) and are skipped; what remains must show
// up in the command's help lines.
func hintLiterals(hint string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(hint, func(r rune) bool {
		return r == ' ' || r == '|' || r == ']' || r == '['
	}) {
		if strings.ContainsAny(tok, "<>[]") {
			continue // placeholder
		}
		out = append(out, tok)
	}
	return out
}

// TestRegistry_HintsCoverArguedUsage pins the tip-derivation contract: a
// command whose help lines show argument usage carries a hint. The hint is
// what registryTips renders into "{/name <hint>}" and what ACP advertises
// as the input grammar — an argued command without one would advertise
// itself bare on both surfaces (the /server gap).
func TestRegistry_HintsCoverArguedUsage(t *testing.T) {
	for _, s := range commandRegistry {
		if s.hint != "" {
			continue
		}
		for _, line := range strings.Split(s.help, "\n") {
			fields := strings.Fields(line)
			// An argument follows the name immediately as a <placeholder>
			// or [optional] part — description prose never starts that way.
			if len(fields) > 1 && fields[0] == s.name && strings.ContainsAny(fields[1], "<[") {
				t.Errorf("%s: help line %q shows arguments but the registry has no hint — add one", s.name, strings.TrimSpace(line))
			}
		}
	}
}

// TestRegistry_ThinkHintsDivergePerHost pins issue #276: /think executes
// differently per host (TUI folds/streams reasoning views, ACP toggles the
// data-plane thought relay), so each surface advertises only its own
// vocabulary — ACP's available_commands_update and /help see [on|off], the
// TUI tip renderer sees [expand|collapse|view].
func TestRegistry_ThinkHintsDivergePerHost(t *testing.T) {
	spec, ok := specFor(cmdThink)
	if !ok {
		t.Fatal("/think missing from the registry")
	}
	if spec.hint != "[on|off]" {
		t.Errorf("/think ACP hint = %q, want [on|off]", spec.hint)
	}
	if got := spec.hintFor(hostTUI); got != "[expand|collapse|view]" {
		t.Errorf("/think TUI hint = %q, want [expand|collapse|view]", got)
	}
	if tip := registryTip(spec); strings.Contains(tip, "on|off") || !strings.Contains(tip, "[expand|collapse|view]") {
		t.Errorf("registry tip for /think = %q, want the TUI fold vocabulary", tip)
	}
	seen := false
	for _, a := range acpAdvertisedCommands() {
		if "/"+a.Name != cmdThink {
			continue
		}
		seen = true
		got := ""
		if a.Input != nil {
			got = a.Input.Hint
		}
		if got != "[on|off]" {
			t.Errorf("ACP advertises /think hint %q, want [on|off]", got)
		}
	}
	if !seen {
		t.Error("/think missing from ACP's advertised commands")
	}
}

// TestThinkCompletion_ExcludesACPOnlyVariants pins the completion half of
// issue #276: TUI tab completion derives from the help lines and must offer
// the TUI's fold vocabulary while excluding the "ACP only"-annotated
// /think on|off line — the TUI's handler rejects those arguments.
func TestThinkCompletion_ExcludesACPOnlyVariants(t *testing.T) {
	vars := cmdVariants[cmdThink]
	if len(vars) == 0 {
		t.Fatal("no completion variants for /think")
	}
	want := map[string]bool{"/think": false, "/think expand": false, "/think collapse": false, "/think view": false}
	for _, v := range vars {
		if _, ok := want[v.sig]; ok {
			want[v.sig] = true
		}
		if strings.Contains(v.sig, "on|off") {
			t.Errorf("TUI completion offers the ACP-only variant %q", v.sig)
		}
	}
	for sig, seen := range want {
		if !seen {
			t.Errorf("TUI completion missing variant %q", sig)
		}
	}
}

// TestACPThinkHint_AdvertisesOnlyAcceptedArgs pins issue #276's acceptance
// behaviorally: every literal the shared hint advertises must be an
// argument the real ACP handler accepts, and the TUI-only arguments must be
// rejected — re-adding expand|collapse|view to the ACP hint fails here even
// if acpThink's wording changes.
func TestACPThinkHint_AdvertisesOnlyAcceptedArgs(t *testing.T) {
	spec, ok := specFor(cmdThink)
	if !ok {
		t.Fatal("/think missing from the registry")
	}
	server, _ := acpTestServer(t, "ok")
	as := server.session(acpNewSession(t, server))
	for _, lit := range hintLiterals(spec.hint) {
		out, _ := acpThink(as, lit)
		if strings.HasPrefix(out, "usage:") {
			t.Errorf("hint advertises %q but acpThink rejects it: %q", lit, out)
		}
	}
	for _, lit := range []string{"expand", "collapse", "view"} {
		out, _ := acpThink(as, lit)
		if !strings.HasPrefix(out, "usage:") {
			t.Errorf("acpThink accepts the TUI-only argument %q: %q", lit, out)
		}
	}
}
