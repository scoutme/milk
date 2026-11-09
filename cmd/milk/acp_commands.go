package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpCommand is one ACP host binding: the handler for a registry command
// (slashcmd.go). Name, description, argument hint and hidden-ness all come
// from the registry — this table binds behavior only, so what
// session/prompt executes and what available_commands_update advertises can
// never drift from the command language the TUI documents. hidden rows are
// executable but never advertised (the /list deprecation window: the alias
// must keep working while the rename settles).
type acpCommand struct {
	name string // registry name, with the leading "/"
	// run returns output text for the client and, optionally, a prompt to
	// dispatch as an ordinary turn afterwards (/escalate <msg>, /primary <msg>).
	run func(as *acpSession, rest string) (output, dispatch string)
	// runTurn, when set, is used instead of run by commands that run long
	// enough to need the turn's cancellable context and to stream messages
	// (workflows).
	runTurn func(t *acpTurn, rest string) (output, dispatch string)
}

// acpTurn is the per-prompt context handed to turn-aware commands:
// cancelled by session/cancel, with say() to stream message text.
type acpTurn struct {
	ctx context.Context
	as  *acpSession
	say func(string)
}

// acpCommandTable lists the headless-capable subset of the TUI's commands —
// nearly all of them these days (wizards, workflows and background jobs
// included). Only genuinely TUI-bound commands (panels, colorization,
// attachments, editor launching, …) are deliberately absent — see
// docs/reference/acp-integration.md.
func acpCommandTable() []acpCommand {
	viaTUI := func(cmd string) func(*acpSession, string) (string, string) {
		return func(as *acpSession, rest string) (string, string) {
			_, dispatch, out := handleSlashCommand(cmd, rest, as.st)
			return out, dispatch
		}
	}
	tbl := []acpCommand{
		{name: cmdWorkflow, runTurn: acpWorkflowCmd},
		{name: cmdSessions, run: acpSessionsCmd},
		{name: cmdNew, run: acpNewCmd},
		{name: cmdClear, run: acpNewCmd},
		{name: cmdResume, run: acpResumeCmd},
		{name: cmdDrop, run: acpDropCmd},
		{name: cmdListLegacy, run: func(as *acpSession, rest string) (string, string) {
			out, dispatch := acpSessionsCmd(as, rest)
			return milkTag() + " /list is deprecated — use /sessions\n\n" + out, dispatch
		}},
		{name: cmdEscalate, run: viaTUI(cmdEscalate)},
		{name: cmdPrimary, run: viaTUI(cmdPrimary)},
		{name: cmdLearn, run: viaTUI(cmdLearn)},
		{name: cmdMemory, run: viaTUI(cmdMemory)},
		{name: cmdUsage, run: viaTUI(cmdUsage)},
		{name: cmdMetrics, run: viaTUI(cmdMetrics)},
		{name: cmdExport, run: viaTUI(cmdExport)},
		{name: cmdTasks, run: acpTasks},
		{name: cmdTask, run: acpTask},
		{name: cmdBg, run: acpBg},
		{name: cmdSkipPerms, run: acpSkipPerms},
		{name: cmdPermissions, run: acpPermissions},
		{name: cmdThink, run: acpThink},
		{name: cmdInterrupt, run: acpInterrupt},
		{name: cmdConfig, run: acpConfig},
		{name: cmdInit, run: acpInit},
		{name: cmdSetup, run: acpSetup},
		{name: cmdAgent, run: acpAgent},
		{name: cmdUpdate, runTurn: acpUpdate},
		{name: "/help", run: acpHelp},
	}
	// Plugin commands (ADR-0104 Part IV) join the ACP surface through the
	// same viaTUI binding: recognized, advertised (acpAdvertisedCommands
	// iterates this table) and dispatched by the same lookup loop — one
	// code path with the static entries, backed by the runtime store.
	for _, s := range pluginSlashSpecs() {
		tbl = append(tbl, acpCommand{name: s.name, run: viaTUI(s.name)})
	}
	return tbl
}

// acpSpec returns the registry entry for a binding's name. The binding
// table is pinned to the registry by TestRegistry_ACPSurface — an unknown
// name is a bug, not a runtime possibility.
func acpSpec(name string) cmdSpec {
	spec, _ := specFor(name)
	return spec
}

func acpAdvertisedCommands() []acp.AvailableCommand {
	out := make([]acp.AvailableCommand, 0, len(acpCommandTable()))
	for _, c := range acpCommandTable() {
		spec := acpSpec(c.name)
		if spec.hidden {
			continue
		}
		out = append(out, acp.Command(spec.name, spec.desc, spec.hint))
	}
	return out
}

func acpThink(as *acpSession, rest string) (string, string) {
	switch strings.TrimSpace(rest) {
	case "on":
		as.showThinking.Store(true)
	case "off":
		as.showThinking.Store(false)
	case "":
	default:
		return "usage: /think [on|off] (expand|collapse|view are TUI-only)", ""
	}
	state := "off"
	if as.showThinking.Load() {
		state = "on"
	}
	return "reasoning visibility: " + state, ""
}

func acpAgent(as *acpSession, rest string) (string, string) {
	if sub := strings.TrimSpace(rest); sub != "" && sub != "list" {
		return fmt.Sprintf("/agent %s is not available over ACP — only /agent list", sub), ""
	}
	return execAgentList(as.st), ""
}

func acpHelp(*acpSession, string) (string, string) {
	var b strings.Builder
	b.WriteString("Commands available over ACP:\n")
	for _, c := range acpCommandTable() {
		spec := acpSpec(c.name)
		if spec.hidden {
			continue
		}
		sig := spec.name
		if spec.hint != "" {
			sig += " " + spec.hint
		}
		fmt.Fprintf(&b, "  %s — %s\n", sig, spec.desc)
	}
	return strings.TrimRight(b.String(), "\n"), ""
}

// acpConfig implements /config over ACP. print/show render the merged config,
// open answers with the file path (an ACP agent runs headless inside somebody
// else's editor — it can't suspend into an editor of its own, so the client
// opens the path), init starts the setup wizard: elicitation form dialogs
// when the client supports them, otherwise one chat prompt per answer while
// as.pendingInit is set.
func acpConfig(as *acpSession, rest string) (string, string) {
	switch sub := strings.ToLower(strings.TrimSpace(rest)); sub {
	case "":
		out, err := renderConfigJSON(activeConfigDir(as.st))
		if err != nil {
			return milkTag() + " error: " + err.Error(), ""
		}
		return out, ""
	case "show":
		out, err := renderConfigShow(activeConfigDir(as.st))
		if err != nil {
			return milkTag() + " error: " + err.Error(), ""
		}
		return out, ""
	case "open":
		return acpConfigOpen(activeConfigDir(as.st)), ""
	case "init":
		return acpStartInitWizard(as), ""
	}
	return milkTag() + " usage: /config | /config show | /config init | /config open", ""
}

// acpInit implements /init — the wizard's original name, kept as an alias of
// /config init in both hosts (it is in slashCommands too, so extractSlashCommand
// recognizes it over ACP and in the TUI).
func acpInit(as *acpSession, _ string) (string, string) {
	return acpStartInitWizard(as), ""
}

// acpStartInitWizard starts the setup wizard and drives it as far as this
// client's input surfaces allow: elicitation form dialogs and clickable
// choice prompts inside this turn (see runInitDialogs), typed chat answers
// in later turns for whatever remains (as.pendingInit) — every question
// carrying what 'default' means for it, so no empty turn is ever needed.
func acpStartInitWizard(as *acpSession) string {
	// One wizard at a time: runTurn has already cancelled a pending
	// telegram wizard for this slash command; keep the invariant even if a
	// future call path skips that block.
	as.pendingTelegram = nil
	st, banner := initWizardStart(activeConfigDir(as.st))
	text, done := as.runInitDialogs(as.currentCtx(), st)
	if !done {
		as.pendingInit = st
	}
	return banner + text
}

// acpOpenFile backs the open_file tool for an ACP session. There is no TTY to
// suspend into $EDITOR, so — like /config open — it hands the file to the
// platform opener. Only existing regular files are opened: the path comes from
// the model, and an opener will happily launch whatever it is pointed at.
func acpOpenFile(cwd, path string) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if _, err := openPathDetached(path); err != nil {
		return fmt.Errorf("could not open %s here: %w", path, err)
	}
	return nil
}

// acpConfigOpen answers /config open. An ACP agent runs headless inside
// somebody else's editor and cannot suspend into an editor of its own (the
// TUI does, via tea.ExecProcess) — but it can still make the file appear:
// openPathDetached hands it to the platform file opener on the machine milk
// runs on (usually the machine the client's editor runs on too). The reply
// reports exactly what happened and always carries the path(s) as well — it
// never claims an editor opened when nothing did. dir is the currently
// selected directory the local overlay resolved against ("" = process cwd).
func acpConfigOpen(dir string) string {
	var b strings.Builder
	if target, _, err := configOpenTarget(dir); err != nil {
		fmt.Fprintf(&b, "%s could not resolve the config path: %v\n", milkTag(), err)
	} else if opener, oerr := openPathDetached(target); oerr != nil {
		fmt.Fprintf(&b, "%s could not open it here (%v) — open the config in your editor\n", milkTag(), oerr)
	} else {
		fmt.Fprintf(&b, "%s opening %s with %s — if no window appears, use the path below\n", milkTag(), target, opener)
	}
	if config.HasLocalConfigIn(dir) {
		if p, err := config.LocalConfigPathIn(dir); err == nil {
			fmt.Fprintf(&b, "%s local config:  %s  (unset fields inherit from global)\n", milkTag(), p)
		}
	}
	if dir, err := config.Dir(); err == nil {
		fmt.Fprintf(&b, "%s global config: %s\n", milkTag(), filepath.Join(dir, "config.json"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// wizardSwapPrompt reports whether prompt launches a wizard — /init,
// /config init, or /setup telegram — whichever one. runTurn uses it to keep
// the "cancelled — restart with …" note out of the new wizard's banner (a
// slash command cancels whichever wizard is pending before it runs).
func wizardSwapPrompt(prompt string) bool {
	cmd, rest, ok := extractSlashCommand(prompt)
	if !ok {
		return false
	}
	switch cmd {
	case cmdInit:
		return true
	case cmdConfig:
		return strings.EqualFold(strings.TrimSpace(rest), "init")
	case cmdSetup:
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(rest)), "telegram")
	}
	return false
}

// acpInterrupt is /interrupt over ACP with no turn running — the busy case
// is intercepted in prompt() before turnMu (it must not queue behind the
// turn it exists to cancel).
func acpInterrupt(_ *acpSession, _ string) (string, string) {
	return milkTag() + " nothing to interrupt — no agent turn running\n", ""
}

// runSlashCommand executes prompt as a slash command if it is one. handled is
// false for ordinary prompts (sent on to the model untouched). A command the
// TUI has but ACP doesn't is handled with an explanatory message rather than
// leaking to the model as text.
func (as *acpSession) runSlashCommand(t *acpTurn, prompt string) (handled bool, output, dispatch string) {
	cmd, rest, found := extractSlashCommand(prompt)
	if !found {
		return false, "", ""
	}
	for _, c := range acpCommandTable() {
		if c.name == cmd {
			if c.runTurn != nil {
				out, dispatch := c.runTurn(t, rest)
				return true, ansi.Strip(out), dispatch
			}
			out, dispatch := c.run(as, rest)
			return true, ansi.Strip(out), dispatch
		}
	}
	return true, fmt.Sprintf("%s is only available in the milk TUI. Available over ACP: %s", cmd, acpCommandNames()), ""
}

func acpCommandNames() string {
	var names []string
	for _, c := range acpCommandTable() {
		names = append(names, c.name)
	}
	return strings.Join(names, ", ")
}

// acpBusySafe are the ACP commands that run immediately while a turn is in
// flight instead of queueing behind turnMu — the permission family, whose
// whole point is to be usable while a turn is waiting on (or about to raise)
// an ask. Each must have a plain `run` handler that only touches state behind
// as.mu / an atomic (acpPermissions, acpSkipPerms), and each must also be in
// the TUI's busySafeCommands (pinned by a test).
var acpBusySafe = map[string]bool{
	cmdPermissions: true,
	cmdSkipPerms:   true,
}

// runBusySafeCommand executes prompt now if it is an acpBusySafe command,
// echoing the prompt and the result to the client like a normal command turn
// but without taking the turn lock or touching the running turn. ok is false
// for any other prompt.
func (as *acpSession) runBusySafeCommand(prompt string) (resp acp.PromptResponse, ok bool) {
	cmd, rest, found := extractSlashCommand(prompt)
	if !found || !acpBusySafe[cmd] {
		return acp.PromptResponse{}, false
	}
	for _, c := range acpCommandTable() {
		if c.name != cmd || c.run == nil {
			continue
		}
		msgID := as.liveID("msg")
		as.emitHistoryMessage(histUser, msgID, prompt)
		out, _ := c.run(as, rest)
		if out = ansi.Strip(out); out != "" {
			as.notify(acp.AgentMessageChunk(as.liveID("busycmd"), out))
		}
		return acp.PromptResponse{MessageID: msgID}, true
	}
	return acp.PromptResponse{}, false
}

// acpSkipPerms is /skip-permissions over ACP: the shared grammar
// (execSkipPermsCore), applied to the session's atomic and returned as a
// plain slash result.
func acpSkipPerms(as *acpSession, rest string) (string, string) {
	next, msg, changed := execSkipPermsCore(rest, as.skipPerms.Load())
	as.skipPerms.Store(next)
	if changed {
		as.mu.Lock()
		msg += clearSessionPermMode(&as.permOverrides)
		as.mu.Unlock()
	}
	return msg, ""
}

// acpPermissions is /permissions over ACP — the same grammar as the TUI's
// handlePermissionsCmd (permask.go's execPermissionsCore). ACP has no local
// prompt queue: pending asks live in the client's own permission dialogs.
func acpPermissions(as *acpSession, rest string) (string, string) {
	as.mu.Lock()
	defer as.mu.Unlock()
	out, listing := execPermissionsCore(rest, as.cfg, &as.permOverrides)
	if listing {
		return milkTag() + " permission asks — every ask races the client dialog, remote oversight, and (for background asks) the timed answer:\n" +
			formatPermPolicy(as.cfg, as.permOverrides), ""
	}
	return out, ""
}
