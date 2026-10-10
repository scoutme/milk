package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpCommand is one slash command available over ACP. The table is the single
// source of truth: what session/prompt executes and what
// available_commands_update advertises are both derived from it, so a command
// can never be advertised without a handler. hidden rows are executable but
// never advertised (the /list deprecation window: the alias must keep working
// while the rename settles).
type acpCommand struct {
	name string // with leading "/", matching the TUI's slashCommands
	desc string
	hint string
	// hidden: run, but keep out of /help and available_commands_update.
	hidden bool
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
// docs/acp-integration.md.
func acpCommandTable() []acpCommand {
	viaTUI := func(cmd string) func(*acpSession, string) (string, string) {
		return func(as *acpSession, rest string) (string, string) {
			_, dispatch, out := handleSlashCommand(cmd, rest, as.st)
			return out, dispatch
		}
	}
	return []acpCommand{
		{name: cmdWorkflow, desc: "run a workflow, or resume, inspect or clear the saved one", hint: "<name> <task> [--<role> <agent>] | resume | status | clear", runTurn: acpWorkflowCmd},
		{name: cmdSessions, desc: "list stored sessions for this directory (* = current)", hint: "[all]", run: acpSessionsCmd},
		{name: cmdNew, desc: "start a fresh session (the current one stays resumable)", hint: "[name]", run: acpNewCmd},
		{name: cmdClear, desc: "alias for /new — start a fresh session", hint: "[name]", run: acpNewCmd},
		{name: cmdResume, desc: "switch this conversation to a stored session", hint: sessionRefHint, run: acpResumeCmd},
		{name: cmdDrop, desc: "delete the current session, or the referenced one", hint: "[" + sessionRefHint + "]", run: acpDropCmd},
		{name: cmdListLegacy, desc: "deprecated alias for /sessions", hidden: true, run: func(as *acpSession, rest string) (string, string) {
			out, dispatch := acpSessionsCmd(as, rest)
			return milkTag() + " /list is deprecated — use /sessions\n\n" + out, dispatch
		}},
		{name: cmdEscalate, desc: "pin all turns to the escalation agent, or force one turn with a message", hint: "[fresh] [<message>]", run: viaTUI(cmdEscalate)},
		{name: cmdPrimary, desc: "pin all turns to the primary agent, or force one turn with a message", hint: "[<message>]", run: viaTUI(cmdPrimary)},
		{name: cmdLearn, desc: "store a persistent memory", hint: "<fact>", run: viaTUI(cmdLearn)},
		{name: cmdMemory, desc: "list stored percepts (session and global)", hint: "[global|session|<pattern>]", run: viaTUI(cmdMemory)},
		{name: cmdUsage, desc: "show token usage by model and role", hint: "", run: viaTUI(cmdUsage)},
		{name: cmdMetrics, desc: "show recent metric values", hint: "", run: viaTUI(cmdMetrics)},
		{name: cmdExport, desc: "print the session transcript (or another session's), or write it to a file", hint: "[json|<path>|session <id>]", run: viaTUI(cmdExport)},
		{name: cmdTasks, desc: "list the session's and global tasks", hint: "", run: acpTasks},
		{name: cmdTask, desc: "mark a task done", hint: "done <id>", run: acpTask},
		{name: cmdBg, desc: "list, start or stop background agents", hint: "[list|start <task>|stop <id>]", run: acpBg},
		{name: cmdSkipPerms, desc: "approve every tool call without asking, or go back to asking", hint: "[on|off]", run: acpSkipPerms},
		{name: cmdPermissions, desc: "show the permission timed-answer policy, or override it for this session", hint: "[default allow|deny [safety] | timeout <seconds> [safety]]", run: acpPermissions},
		{name: cmdThink, desc: "show or hide model reasoning in this session", hint: "[on|off]", run: acpThink},
		{name: cmdConfig, desc: "print the config, run the setup wizard, or open the config file", hint: "[show|init|open]", run: acpConfig},
		{name: cmdInit, desc: "run the setup wizard (alias for /config init)", hint: "", run: acpInit},
		{name: cmdSetup, desc: "configure Telegram remote oversight (status, enable, disable, or the setup wizard)", hint: "telegram [on|off|status]", run: acpSetup},
		{name: cmdAgent, desc: "list configured agents", hint: "[list]", run: acpAgent},
		{name: cmdUpdate, desc: "check for milk updates, install one, or skip a release", hint: "check|status|install|skip", runTurn: acpUpdate},
		{name: "/help", desc: "list the commands available in this session", hint: "", run: acpHelp},
	}
}

func acpAdvertisedCommands() []acp.AvailableCommand {
	out := make([]acp.AvailableCommand, 0, len(acpCommandTable()))
	for _, c := range acpCommandTable() {
		if c.hidden {
			continue
		}
		out = append(out, acp.Command(c.name, c.desc, c.hint))
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
		return "usage: /think [on|off]", ""
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
		if c.hidden {
			continue
		}
		sig := c.name
		if c.hint != "" {
			sig += " " + c.hint
		}
		fmt.Fprintf(&b, "  %s — %s\n", sig, c.desc)
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
		out, err := renderConfigJSON()
		if err != nil {
			return milkTag() + " error: " + err.Error(), ""
		}
		return out, ""
	case "show":
		out, err := renderConfigShow()
		if err != nil {
			return milkTag() + " error: " + err.Error(), ""
		}
		return out, ""
	case "open":
		return acpConfigOpen(), ""
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
	st, banner := initWizardStart()
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
// never claims an editor opened when nothing did.
func acpConfigOpen() string {
	var b strings.Builder
	if target, _, err := configOpenTarget(); err != nil {
		fmt.Fprintf(&b, "%s could not resolve the config path: %v\n", milkTag(), err)
	} else if opener, oerr := openPathDetached(target); oerr != nil {
		fmt.Fprintf(&b, "%s could not open it here (%v) — open the config in your editor\n", milkTag(), oerr)
	} else {
		fmt.Fprintf(&b, "%s opening %s with %s — if no window appears, use the path below\n", milkTag(), target, opener)
	}
	if config.HasLocalConfig() {
		if p, err := config.LocalConfigPath(); err == nil {
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
				return true, stripANSI(out), dispatch
			}
			out, dispatch := c.run(as, rest)
			return true, stripANSI(out), dispatch
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

func acpSkipPerms(as *acpSession, rest string) (string, string) {
	switch strings.TrimSpace(rest) {
	case "on":
		as.skipPerms.Store(true)
		return "dangerously_skip_permissions ON — all tool calls are approved without asking", ""
	case "off":
		as.skipPerms.Store(false)
		return "dangerously_skip_permissions OFF — tool calls ask for permission again", ""
	case "":
		state := "off"
		if as.skipPerms.Load() {
			state = "on"
		}
		return "dangerously_skip_permissions is " + state + "  (use /skip-permissions on|off)", ""
	}
	return "usage: /skip-permissions [on|off]", ""
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
