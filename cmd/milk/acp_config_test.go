package main

// Tests for /config and /init over ACP: the command handlers themselves, the
// multi-prompt setup wizard (including that a finished wizard actually swaps
// the session's runners — no restart), and its escape hatches.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/transport/acp"
)

// stubDetachedOpener replaces the /config open launcher for one test — the
// real one would pop a window on the developer's desktop.
func stubDetachedOpener(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := openPathDetached
	openPathDetached = fn
	t.Cleanup(func() { openPathDetached = prev })
}

// TestACPConfigPrintShowOpenUsage: the four /config surfaces, none of which
// may reach the model. /config open must actually launch the opener and say
// exactly what happened.
func TestACPConfigPrintShowOpenUsage(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	var openedPath string
	stubDetachedOpener(t, func(path string) (string, error) {
		openedPath = path
		return "test-opener", nil
	})

	acpPromptText(t, server, id, "/config")
	acpPromptText(t, server, id, "/config show")
	acpPromptText(t, server, id, "/config open")
	acpPromptText(t, server, id, "/config bogus")

	chunks := acpChunks(conn)
	if len(chunks) != 4 {
		t.Fatalf("chunks = %q, want exactly the four command outputs", chunks)
	}
	if !strings.Contains(chunks[0], "```json") || !strings.Contains(chunks[0], "test-local") {
		t.Errorf("/config = %q, want the merged config as fenced JSON including the test agent", chunks[0])
	}
	if !strings.Contains(chunks[1], "```json") {
		t.Errorf("/config show = %q, want annotated fenced JSON", chunks[1])
	}
	if !strings.Contains(chunks[2], "opening ") || !strings.Contains(chunks[2], "test-opener") {
		t.Errorf("/config open = %q, want it to report opening the file with the launcher", chunks[2])
	}
	if !strings.Contains(chunks[2], "global config:") || !strings.Contains(chunks[2], "config.json") {
		t.Errorf("/config open = %q, want the config file path as well", chunks[2])
	}
	if want := filepath.Join(os.Getenv("HOME"), ".milk", "config.json"); openedPath != want {
		t.Errorf("opener launched with %q, want the global config %q", openedPath, want)
	}
	if !strings.Contains(chunks[3], "usage: /config") {
		t.Errorf("unknown subcommand = %q, want usage", chunks[3])
	}
	for i, c := range chunks {
		if strings.Contains(c, "model reply") {
			t.Errorf("chunk %d reached the model: %q", i, c)
		}
		if strings.Contains(c, "\x1b[") {
			t.Errorf("chunk %d carries ANSI to the client: %q", i, c)
		}
	}
}

// TestACPConfigOpen_NoOpenerIsHonest: with no desktop opener available
// (headless host) /config open must not claim anything opened — it reports
// the failure and falls back to the config path(s).
func TestACPConfigOpen_NoOpenerIsHonest(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	stubDetachedOpener(t, func(string) (string, error) {
		return "", fmt.Errorf("no usable file opener (tried xdg-open)")
	})

	acpPromptText(t, server, id, "/config open")

	chunks := acpChunks(conn)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %q, want the one command output", chunks)
	}
	if !strings.Contains(chunks[0], "could not open it here") || !strings.Contains(chunks[0], "open the config in your editor") {
		t.Errorf("/config open = %q, want an honest failure report", chunks[0])
	}
	if !strings.Contains(chunks[0], "global config:") {
		t.Errorf("/config open = %q, want the config path fallback", chunks[0])
	}
	if strings.Contains(chunks[0], "opening ") {
		t.Errorf("/config open = %q, must not claim to have opened anything", chunks[0])
	}
}

// TestOpenPathDetached_LaunchesPlatformOpener proves the real launcher (no
// stub) execs the platform opener with the path and returns without waiting:
// a fake opener on $PATH records its argv.
func TestOpenPathDetached_LaunchesPlatformOpener(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses rundll32/cmd start, covered by the stubbed handler tests")
	}
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$@\" > " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake opener: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	opener, err := openPathDetached("/tmp/example-config.json")
	if err != nil {
		t.Fatalf("openPathDetached: %v", err)
	}
	if opener != name {
		t.Errorf("opener = %q, want %q", opener, name)
	}
	// The opener runs detached: poll briefly for its record.
	for i := 0; i < 200; i++ {
		if data, err := os.ReadFile(logPath); err == nil {
			if got := strings.TrimSpace(string(data)); got != "/tmp/example-config.json" {
				t.Errorf("opener argv = %q, want the path", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("fake opener never ran — /config open would claim to open a file without launching anything")
}

// TestACPInitWizard_ConfiguresSessionWithoutRestart is the adoption scenario:
// an ACP-only user (editor integration, never a TUI) runs the wizard across
// prompts, and the very next prompt must run on the agent they configured.
func TestACPInitWizard_ConfiguresSessionWithoutRestart(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	as := server.session(id)
	stubURL := as.cfg.ActiveAgent().URL
	if stubURL == "" {
		t.Fatal("test config has no agent URL")
	}

	// One prompt per wizard answer; want[i] is the substring the answer must
	// provoke next (prompt or completion summary).
	steps := []struct{ in, want string }{
		{"/config init", "primary agent name"},
		{"", "provider — select"},
		{"1", "server URL"},
		{stubURL, "run_cmd:"},
		{"", "model name"},
		{"test-model", "context window"},
		{"", "escalation agent?"},
		{"n", "config written to"},
	}
	for i, s := range steps {
		acpPromptText(t, server, id, s.in)
		chunks := acpChunks(conn)
		if len(chunks) != i+1 {
			t.Fatalf("after prompt %d (%q): %d chunks, want %d — wizard answers must not leak to the model: %q",
				i, s.in, len(chunks), i+1, chunks)
		}
		if !strings.Contains(chunks[i], s.want) {
			t.Errorf("after prompt %d (%q): chunk = %q, want substring %q", i, s.in, chunks[i], s.want)
		}
	}

	if as.pendingInit != nil {
		t.Fatal("wizard must be finished after the final answer")
	}
	if got := as.st.cfg.ActiveAgent().Name; got != "local" {
		t.Errorf("st.cfg active agent = %q, want local (wizard default name)", got)
	}
	if got := as.st.cfg.ActiveAgent().URL; got != stubURL {
		t.Errorf("st.cfg agent URL = %q, want %q", got, stubURL)
	}

	// The config really hit disk (global scope — no local config in cwd).
	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".milk", "config.json"))
	if err != nil {
		t.Fatalf("reading written config: %v", err)
	}
	if !strings.Contains(string(data), `"local"`) || !strings.Contains(string(data), stubURL) {
		t.Errorf("written config = %s, want the wizard's agent", data)
	}

	// And the rebuilt runners use it: the next prompt reaches the stub
	// backend through the wizard-configured agent.
	acpPromptText(t, server, id, "hello")
	chunks := acpChunks(conn)
	last := chunks[len(chunks)-1]
	if !strings.Contains(last, "model reply") {
		t.Errorf("post-wizard turn = %q, want the stub model's reply (runners must be rebuilt, no restart)", last)
	}
}

// TestACPInitAliasAndRestart: /init starts the wizard; re-running /config init
// mid-wizard restarts it without a misleading "cancelled" note.
func TestACPInitAliasAndRestart(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/init")
	if as.pendingInit == nil {
		t.Fatal("/init must start the setup wizard over ACP")
	}
	acpPromptText(t, server, id, "/config init")
	if as.pendingInit == nil {
		t.Fatal("/config init must (re)start the wizard")
	}
	chunks := acpChunks(conn)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %q, want the two wizard banners", chunks)
	}
	if strings.Contains(chunks[1], "cancelled") {
		t.Errorf("restart mid-wizard must not report a cancellation: %q", chunks[1])
	}
}

// TestACPInitWizard_SlashCommandCancelsWizard: ACP has no esc key, so any
// recognized slash command aborts the wizard and runs instead.
func TestACPInitWizard_SlashCommandCancelsWizard(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/config init")
	if as.pendingInit == nil {
		t.Fatal("wizard not started")
	}
	acpPromptText(t, server, id, "/help")

	if as.pendingInit != nil {
		t.Error("a slash command must cancel the pending wizard")
	}
	chunks := acpChunks(conn)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %q, want banner + cancelled-help", chunks)
	}
	if !strings.Contains(chunks[1], "setup wizard cancelled") {
		t.Errorf("chunk = %q, want the cancellation note", chunks[1])
	}
	if !strings.Contains(chunks[1], "Commands available over ACP") {
		t.Errorf("chunk = %q, want the /help output after the note", chunks[1])
	}
}

// TestACPInitWizard_CancelWord: plain-text abort while the wizard waits.
func TestACPInitWizard_CancelWord(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/config init")
	acpPromptText(t, server, id, "cancel")

	if as.pendingInit != nil {
		t.Error("cancel must clear pendingInit")
	}
	chunks := acpChunks(conn)
	if len(chunks) != 2 || !strings.Contains(chunks[1], "setup wizard cancelled") {
		t.Errorf("chunks = %q, want banner + cancellation", chunks)
	}
}

// TestACPInitWizard_BackgroundFollowupIsNotWizardInput: a synthetic background
// follow-up arriving mid-wizard must still be dispatched as a turn — its text
// is not an answer to any wizard question.
func TestACPInitWizard_BackgroundFollowupIsNotWizardInput(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/config init")
	acpPromptText(t, server, id, backgroundFollowupPrompt)

	if as.pendingInit == nil {
		t.Fatal("the wizard must survive a background follow-up turn")
	}
	chunks := acpChunks(conn)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %q, want banner + follow-up reply", chunks)
	}
	if !strings.Contains(chunks[1], "model reply") {
		t.Errorf("follow-up = %q, want it dispatched to the model, not consumed by the wizard", chunks[1])
	}
	acpPromptText(t, server, id, "cancel")
}

// TestACPSessionNew_RereadsConfigFromDisk: a session created after the config
// changed (by an earlier session's wizard or an external editor) must see the
// new file, not the server's startup snapshot.
func TestACPSessionNew_RereadsConfigFromDisk(t *testing.T) {
	server, _ := acpTestServer(t, "ok")

	home := os.Getenv("HOME")
	cfgPath := filepath.Join(home, ".milk", "config.json")
	if err := os.WriteFile(cfgPath, []byte(
		`{"agent":"disk-model-agent","agents":[{"name":"disk-model-agent","url":"http://127.0.0.1:1","model":"disk-model","provider":"local"}]}`),
		0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	id := acpNewSession(t, server)
	as := server.session(id)
	if got := as.cfg.ActiveAgent().Model; got != "disk-model" {
		t.Errorf("session cfg model = %q, want disk-model (config re-read at session/new)", got)
	}
}

// TestACPCommands_AdvertisedIncludeConfigAndInit: the advertisement includes
// both names (TestACPCommands_AdvertisedEqualsExecutable additionally proves
// each is executable and not TUI-only).
func TestACPCommands_AdvertisedIncludeConfigAndInit(t *testing.T) {
	adv := acpAdvertisedCommands()
	got := map[string]bool{}
	for _, c := range adv {
		got[c.Name] = true
	}
	for _, name := range []string{"config", "init"} {
		if !got[name] {
			t.Errorf("available_commands_update missing /%s (have %v)", name, got)
		}
	}
	var cfg *acp.AvailableCommand
	for i := range adv {
		if adv[i].Name == "config" {
			cfg = &adv[i]
		}
	}
	if cfg == nil || cfg.Input == nil || cfg.Input.Hint == "" {
		t.Errorf("/config advertised without a hint: %+v", cfg)
	}
}
