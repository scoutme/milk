package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/plugin"
)

// TestPluginCommands_RecognitionDispatchAndSurfaces walks one command-carrying
// plugin through every surface a built-in command uses (ADR-0104 Part IV):
// recognition (specFor + registryNames + extractSlashCommand), dispatch
// (handleSlashCommand → built prompt with whitelisted expansion + $ARGUMENTS),
// /help's live block, and the ACP table + advertising. The loader runs for
// real (disk fixture → plugin.Load), so frontmatter parsing and the
// !`…` expansion are the production paths, not fakes.
func TestPluginCommands_RecognitionDispatchAndSurfaces(t *testing.T) {
	t.Cleanup(func() { setPluginCommands(nil) })

	dir := t.TempDir()
	manifest := `{"$schema": "` + plugin.SchemaPlugin + `", "name": "code-review"}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdPath := filepath.Join(dir, "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(cmdPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nallowed-tools: Bash(echo:*)\nargument-hint: [scope]\ndescription: Perform a comprehensive code review\n---\n" +
		"ctx: !`echo LIVE` review of $ARGUMENTS\n"
	if err := os.WriteFile(cmdPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := plugin.Load(dir, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	cmds, warns := buildPluginSlashCmds([]loadedPlugin{{plugin: p}})
	if len(warns) != 0 || len(cmds) != 1 {
		t.Fatalf("cmds=%d warns=%v", len(cmds), warns)
	}
	setPluginCommands(cmds)

	// Recognition: the one lookup everything routes through.
	spec, ok := specFor("/review")
	if !ok || spec.desc != "Perform a comprehensive code review" || spec.hint != "[scope]" {
		t.Fatalf("specFor = %+v ok=%v", spec, ok)
	}
	if !spec.in(hostTUI) || !spec.in(hostACP) {
		t.Errorf("plugin command must be hosted on both surfaces: hosts=%d", spec.hosts)
	}
	if !slices.Contains(registryNames(), "/review") {
		t.Error("registryNames must include the plugin command (completion + extract)")
	}
	name, rest, found := extractSlashCommand("/review the auth layer")
	if !found || name != "/review" || rest != "the auth layer" {
		t.Fatalf("extract = %q %q %v", name, rest, found)
	}

	// Dispatch: host-neutral default branch builds the prompt — whitelisted
	// !`echo LIVE` expands, $ARGUMENTS substitutes, no output side-channel.
	_, dispatch, out := handleSlashCommand("/review", "the auth layer", &interactiveState{})
	if out != "" {
		t.Errorf("dispatch must be silent, got %q", out)
	}
	if !strings.Contains(dispatch, "ctx: LIVE review of the auth layer") {
		t.Errorf("built prompt = %q", dispatch)
	}

	// Unknown commands still error (no regression of the default branch).
	if _, _, out := handleSlashCommand("/definitely-not-a-command", "", &interactiveState{}); !strings.Contains(out, "unknown command") {
		t.Errorf("unknown command output = %q", out)
	}

	// Help: the live Plugins block.
	help := buildInteractiveHelp()
	if !strings.Contains(help, "── Plugins") || !strings.Contains(help, "/review") {
		t.Errorf("help missing the plugin block:\n%s", help)
	}

	// ACP: table entry + advertising ride the same store.
	var entry *acpCommand
	for _, c := range acpCommandTable() {
		if c.name == "/review" {
			c := c
			entry = &c
		}
	}
	if entry == nil || entry.run == nil {
		t.Fatal("acpCommandTable must include the plugin command with a binding")
	}
	var adv *struct{ name, desc string }
	for _, a := range acpAdvertisedCommands() {
		// The wire form drops the leading "/" (acp.Command strips it).
		if a.Name == "review" {
			adv = &struct{ name, desc string }{a.Name, a.Description}
		}
	}
	if adv == nil || adv.desc == "" {
		t.Errorf("plugin command must be advertised with its description, got %+v", adv)
	}
}

// TestPluginCommands_CollisionsDropWithReport pins the ownership rule:
// built-ins always win, first plugin wins among themselves — each drop
// reports into the discovery log.
func TestPluginCommands_CollisionsDropWithReport(t *testing.T) {
	mk := func(name, cmdName string) loadedPlugin {
		return loadedPlugin{plugin: &plugin.Plugin{
			Manifest: plugin.Manifest{Name: name},
			Commands: []plugin.Command{{Name: cmdName, Description: "d"}},
		}}
	}
	built, warns := buildPluginSlashCmds([]loadedPlugin{mk("p1", "help"), mk("p1", "review"), mk("p2", "review")})
	if len(built) != 1 || built[0].spec.name != "/review" {
		t.Fatalf("built = %+v, want just the first /review", built)
	}
	if len(warns) != 2 {
		t.Fatalf("warns = %v, want builtin-collision + dup reports", warns)
	}
	if !strings.Contains(warns[0], "collides with an existing") || !strings.Contains(warns[1], "collides with an existing") {
		t.Errorf("warns = %v", warns)
	}
}
