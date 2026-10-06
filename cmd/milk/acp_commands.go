package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpCommand is one slash command available over ACP. The table is the single
// source of truth: what session/prompt executes and what
// available_commands_update advertises are both derived from it, so a command
// can never be advertised without a handler.
type acpCommand struct {
	name string // with leading "/", matching the TUI's slashCommands
	desc string
	hint string
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

// acpCommandTable lists the headless-capable subset of the TUI's commands.
// TUI-only commands (panels, colorization, attachments, wizards, workflows,
// background jobs, …) are deliberately absent — see docs/acp-integration.md.
func acpCommandTable() []acpCommand {
	viaTUI := func(cmd string) func(*acpSession, string) (string, string) {
		return func(as *acpSession, rest string) (string, string) {
			_, dispatch, out := handleSlashCommand(cmd, rest, as.st)
			return out, dispatch
		}
	}
	return []acpCommand{
		{name: cmdWorkflow, desc: "run a workflow, or resume, inspect or clear the saved one", hint: "<name> <task> [--<role> <agent>] | resume | status | clear", runTurn: acpWorkflowCmd},
		{cmdEscalate, "pin all turns to the escalation agent, or force one turn with a message", "[fresh] [<message>]", viaTUI(cmdEscalate), nil},
		{cmdPrimary, "pin all turns to the primary agent, or force one turn with a message", "[<message>]", viaTUI(cmdPrimary), nil},
		{cmdLearn, "store a persistent memory", "<fact>", viaTUI(cmdLearn), nil},
		{cmdMemory, "list stored percepts (session and global)", "[global|session|<pattern>]", viaTUI(cmdMemory), nil},
		{cmdUsage, "show token usage by model and role", "", viaTUI(cmdUsage), nil},
		{cmdMetrics, "show recent metric values", "", viaTUI(cmdMetrics), nil},
		{cmdExport, "print the session transcript, or write it to a file", "[json|<path>]", viaTUI(cmdExport), nil},
		{"/list", "list sessions for the current directory", "", viaTUI("/list"), nil},
		{cmdTasks, "list the session's and global tasks", "", acpTasks, nil},
		{cmdTask, "mark a task done", "done <id>", acpTask, nil},
		{cmdBg, "list, start or stop background agents", "[list|start <task>|stop <id>]", acpBg, nil},
		{cmdSkipPerms, "approve every tool call without asking, or go back to asking", "[on|off]", acpSkipPerms, nil},
		{cmdThink, "show or hide model reasoning in this session", "[on|off]", acpThink, nil},
		{cmdConfig, "print the config, run the setup wizard, or open the config file", "[show|init|open]", acpConfig, nil},
		{cmdInit, "run the setup wizard (alias for /config init)", "", acpInit, nil},
		{cmdAgent, "list configured agents", "[list]", acpAgent, nil},
		{"/help", "list the commands available in this session", "", acpHelp, nil},
	}
}

func acpAdvertisedCommands() []acp.AvailableCommand {
	table := acpCommandTable()
	out := make([]acp.AvailableCommand, 0, len(table))
	for _, c := range table {
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
// opens the path), init starts the setup wizard, driven one answer per prompt
// while as.pendingInit is set.
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

// acpStartInitWizard arms the session's wizard and returns the banner plus the
// first prompt. Subsequent session/prompt calls are fed to initWizardApply
// from runTurn until the wizard reports Done.
func acpStartInitWizard(as *acpSession) string {
	st, banner := initWizardStart()
	as.pendingInit = st
	return banner
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

// initWizardRestartPrompt reports whether prompt is the command that (re)starts
// the setup wizard — used to skip the "cancelled" note when a restart lands
// mid-wizard (see runTurn).
func initWizardRestartPrompt(prompt string) bool {
	cmd, rest, ok := extractSlashCommand(prompt)
	if !ok {
		return false
	}
	return cmd == cmdInit || (cmd == cmdConfig && strings.EqualFold(strings.TrimSpace(rest), "init"))
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
