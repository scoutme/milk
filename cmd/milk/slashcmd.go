package main

import "strings"

// This file is the slash-command registry (issue #226): one source for the
// *command language* both hosts speak — names, aliases, argument grammar,
// one-line summaries and the verbatim TUI help lines. The TUI's help,
// completion set and extractSlashCommand recognition, and ACP's
// available_commands_update, /help listing and runSlashCommand dispatch all
// derive from it, so the surfaces can no longer drift apart.
//
// What the registry deliberately does NOT unify is behavior: handlers stay
// per-host (handleSlashCommand's switch for the TUI, acpCommandTable's
// run/runTurn funcs for ACP) because the same command may legitimately
// execute differently per host (/config init: keyboard wizard vs elicitation
// forms). Set mismatches are data — hosts says which hosts execute a
// command; hidden marks executable-but-unlisted commands (/list's
// deprecation window).

// hostSet declares which hosts execute a command.
type hostSet uint8

const (
	hostTUI hostSet = 1 << iota
	hostACP
	hostBoth = hostTUI | hostACP
)

// argsGrammar is a command's argument grammar in a small parseable form —
// the shared shape both hosts' handlers parse against ("on|off", "<id>",
// free text, …). It is the parseable counterpart of the display hint:
// TestRegistry_GrammarConsistency pins that the two agree, and both hosts'
// handlers for a grammar-bearing command parse through the one shared
// parseArgs, so one command cannot mean two things per host (issue #226).
type argsGrammar uint8

const (
	// argsFree: free text or host-structured arguments — nothing for a
	// shared parser to validate (e.g. <fact>, <task>, key=value lists).
	argsFree argsGrammar = iota
	// argsOptOnOff: [on|off] — "" queries the state, "on"/"off" set it.
	argsOptOnOff
	// argsRef: <id|prefix|name> — exactly one non-empty reference token.
	argsRef
	// argsOptRef: [<id|prefix|name>] — zero or one reference token.
	argsOptRef
)

// parse parses rest under the grammar. ok is false when the input is outside
// the grammar (the caller renders its usage line); the returned value is the
// canonical form — the on|off literal ("" for the bare query), or the single
// reference token. argsFree passes trimmed input through untouched and is
// always ok: its structure is host-defined behavior, not command language.
func (g argsGrammar) parse(rest string) (val string, ok bool) {
	rest = strings.TrimSpace(rest)
	switch g {
	case argsOptOnOff:
		switch rest {
		case "", "on", "off":
			return rest, true
		}
		return "", false
	case argsRef:
		return rest, rest != ""
	case argsOptRef:
		return rest, true
	}
	return rest, true
}

// parseArgs parses rest with the grammar the registry declares for cmd.
// Every host routes argument parsing for a grammar-bearing command through
// this one function — the parse-consistency invariant of issue #226.
func parseArgs(cmd, rest string) (val string, ok bool) {
	spec, _ := specFor(cmd)
	return spec.args.parse(rest)
}

// cmdSpec is one slash command's shared surface.
type cmdSpec struct {
	name    string // canonical name, with the leading "/"
	aliases []string
	hosts   hostSet
	// hidden: executable on its hosts, but kept out of the help text and
	// out of available_commands_update.
	hidden bool
	// args is the parseable argument grammar (the display form is hint).
	args argsGrammar
	// desc is the one-line summary used on the ACP surface
	// (available_commands_update and /help). The TUI help lines carry
	// their own inline descriptions and are verbatim.
	desc string
	// hint is the compact argument grammar ACP advertises for the command
	// (available_commands_update, ACP /help, and registryTips for commands
	// without a TUI override) — "" = none. It must only contain arguments
	// the ACP handler accepts. The TUI shows the fuller variant signatures
	// in help.
	hint string
	// hintTUI overrides hint on TUI surfaces (registryTips) for a command
	// whose TUI vocabulary differs from ACP's — empty = the shared hint.
	hintTUI string
	// help is the command's verbatim TUI help lines, already aligned,
	// joined with "\n" ("" = not shown in help — hidden commands, or
	// covered by a sibling's line like "/exit  /quit").
	help string
}

func (s cmdSpec) in(host hostSet) bool { return s.hosts&host != 0 }

// hintFor returns the display hint for the given host: ACP always sees the
// base hint (its structured-input grammar); the TUI sees hintTUI when the
// command diverges, else the shared hint. Mirrors how completion follows
// the help lines per host (see the "ACP only" rule on commandRegistry).
func (s cmdSpec) hintFor(host hostSet) string {
	if host&hostTUI != 0 && s.hintTUI != "" {
		return s.hintTUI
	}
	return s.hint
}

// commandRegistry lists every executable slash command. Order is the tab
// completion cycle order (slashCommands derives from it); the help text's
// display order is defined separately by helpBlocks below.
//
// Hosts may diverge on ARGUMENTS for a command that genuinely executes
// differently per host: hint is ACP's compact grammar (the only form the
// ACP surface advertises), and hintTUI overrides it on TUI surfaces
// (registry tips) when the TUI accepts a different vocabulary. Completion
// follows the help lines instead — a help line whose description starts
// with "ACP only" documents the command for TUI help but is excluded from
// TUI completion variants (buildCmdVariants), so neither surface offers
// arguments the other host would reject.
var commandRegistry = []cmdSpec{
	{name: cmdEscalate, hosts: hostBoth, desc: "pin all turns to the escalation agent, or force one turn with a message", hint: "[fresh] [<message>]", help: `  /escalate              pin all turns to escalation agent (/primary to unpin)
  /escalate <msg>        force this turn to escalation agent, then resume routing
  /escalate fresh        force a new escalation context (new session + memory instructions re-injected)
  /escalate fresh <msg>  same, single-turn override`},
	{name: cmdPrimary, hosts: hostBoth, desc: "pin all turns to the primary agent, or force one turn with a message", hint: "[<message>]", help: `  /primary               pin all turns to primary agent (/escalate to unpin)
  /primary <msg>         force this turn to primary agent, then resume routing`},
	{name: cmdPaste, hosts: hostTUI, desc: "probe clipboard for binary content (image, PDF, …) and auto-stage it as an attachment", help: `  /paste                 probe clipboard for binary content (image, PDF, …)
                         auto-stages it as an attachment; requires xclip (X11)
                         or wl-paste (Wayland); also fires on empty paste event`},
	{name: cmdLearn, hosts: hostBoth, desc: "store a persistent memory", hint: "<fact>", help: `  /learn <fact>          store a persistent memory`},
	{name: cmdOtel, hosts: hostTUI, desc: "show or manage OTel observability files", hint: "[on|off|trim|debug enable|disable]", help: `  /otel                  show OTel file sizes and record counts
  /otel on               enable OTel for this session
  /otel off              disable OTel for this session
  /otel trim             archive current OTel files and start fresh
  /otel debug enable     enable full debug logging (log_context, debug_*, log_level=DEBUG)
  /otel debug disable    disable debug logging (restores defaults)`},
	{name: cmdMetrics, hosts: hostBoth, desc: "show recent metric values", help: `  /metrics               show latest metric values`},
	{name: cmdUsage, hosts: hostBoth, desc: "show token usage by model and role", help: `  /usage                 token usage report for this session and all-time totals`},
	{name: cmdMemory, hosts: hostBoth, desc: "list stored percepts (session and global)", hint: "[global|session|<pattern>|show <pat|#id>|stale|archive|restore <id>]", help: `  /memory                list all percepts (session + global)
  /memory global         list only global percepts
  /memory session        list only session percepts
  /memory <pat>          list percepts whose content matches <pat>
  /memory show <pat|#id>  show full details of matching percepts
  /memory stale          global percepts by idle session time, and which are due for archiving
  /memory archive        list archived (stale) global percepts
  /memory restore <id>   bring an archived percept back to the global store`},
	{name: cmdExport, hosts: hostBoth, desc: "print the session transcript (or another session's), or write it to a file", hint: "[--no-reasoning] [json|<path>|session <id>]", help: `  /export                print session transcript (text)
  /export json           print session transcript as JSON
  /export <path>         write session transcript to file
  /export session <id>   export another session (id or unambiguous prefix)
  /export --no-reasoning omit reasoning blocks (text fences / JSON thinking) — they are included by default`},
	{name: cmdHistory, hosts: hostTUI, desc: "show or set the input history navigation mode", hint: "[global|session]", help: `  /history               show current history navigation mode
  /history global        navigate global input history
  /history session       navigate session input history (default)`},
	{name: cmdPanel, hosts: hostTUI, desc: "toggle a side panel (memory, tasks, background, workflow, project, git)", hint: "memory|tasks|background|workflow|project|git", help: `  /panel memory          toggle the memory panel (right side) — also F1
  /panel tasks           toggle the tasks panel (right side) — also F2
  /panel background      toggle the background-agents panel (right side) — also F3
  /panel workflow        toggle the workflow panel (right side) — also F4
  /panel project         toggle the project tree panel (right side) — also F5
                         click a dir to expand/collapse, a file to preview it,
                         double-click a file to insert an @-mention
  /panel git             toggle the git graph panel (right side) — also F6
                         click a commit to preview its summary and diff`},
	{name: cmdForget, hosts: hostTUI, desc: "delete a percept (asks for confirmation)", hint: "<pat|#id>", help: `  /forget <pat|#id>      delete a percept (asks for confirmation)`},
	{name: cmdSkipPerms, hosts: hostBoth, desc: "approve every tool call without asking, or go back to asking", hint: "[on|off]", help: `  /skip-permissions      show current skip-permissions state
  /skip-permissions on   all agents auto-approve tool uses (no prompts)
  /skip-permissions off  agents prompt before running side-effecting tools
  (a toggle also clears the /permissions mode allow-all|deny-all session mode)`},
	{name: cmdPermissions, hosts: hostBoth, desc: "show the permission policy, or override it for this session", hint: "[mode allow-all|deny-all|off | default allow|deny [safety] | timeout <secs> [safety]]", help: `  /permissions                      show the session mode, timed-answer policy + pending asks
  /permissions mode allow-all|deny-all|off  answer every tool ask without prompting (never the doom-loop gate)
  /permissions default allow|deny [safety]  session override of the timed default
  /permissions timeout <secs> [safety]      session override of the ask deadline`},
	{name: cmdAgent, hosts: hostBoth, desc: "list configured agents", hint: "[list|add|remove <name>|switch <name>|tool <sub>]", help: `  /agent                 show active primary and escalation agents
  /agent list            list all configured agents (* = active)
  /agent add             add a new agent interactively
  /agent add name=… url=… model=… [provider=…] [api_key=…] [aws_region=…] [run_cmd=…]
  /agent remove <name>   remove an agent from config (must not be active)
  /agent switch <name> [as primary|escalation]   (prompts if args missing)
  /agent tool list [<agent>|global]              show tool-agents (default: primary)
  /agent tool enable <tool> [for <agent>|global]  enable a tool-agent entry
  /agent tool disable <tool> [for <agent>|global]  disable a tool-agent entry
  /agent tool add <tool> description=<desc> [for <agent>|global]  add a new tool-agent entry
  /agent tool remove <tool> [for <agent>|global]  remove a tool-agent entry`},
	{name: cmdTheme, hosts: hostTUI, desc: "switch the TUI skin (off = original look)", hint: "[off|supermilk]", help: `  /theme                 show the active theme
  /theme off             original look (default)
  /theme supermilk       milk-white skin, soft ink, hairlines`},
	{name: cmdColorize, hosts: hostTUI, desc: "show or set the markdown colorization mode", hint: "[off|fenced|balanced|full]", help: `  /colorize              show current colorization mode
  /colorize off          no colorization
  /colorize fenced       highlight fenced code blocks only
  /colorize balanced     fenced blocks + inline markdown (default)
  /colorize full         full glamour markdown render (experimental)`},
	{name: cmdInterrupt, hosts: hostBoth, desc: "interrupt the running turn — the Ctrl+C equivalent, usable from remote surfaces", help: `  /interrupt            cancel the running agent turn (same as Ctrl+C; works from Telegram too)`},
	{name: cmdThink, hosts: hostBoth, desc: "inspect or fold model reasoning (ACP: toggle thought relay)", hint: "[on|off]", hintTUI: "[expand|collapse|view]", help: `  /think                 show thinking-block count and fold state
  /think expand          expand every thinking block
  /think collapse        fold every thinking block
  /think view            swap the main area to a thinking-only stream
  /think on|off          ACP only: relay or withhold thought chunks`},
	{name: cmdTips, hosts: hostTUI, desc: "show, hide or browse the idle tips under the input", hint: "[on|off|next|prev|pick [<n>|<text>]|search <query>]", help: `  /tips                  show tip line state and the current tip's position
  /tips on               show the idle tip line under the input
  /tips off              hide the idle tip line
  /tips next             show the next tip (Alt+Down)
  /tips prev             show the previous tip (Alt+Up)
  /tips pick             list every tip with its number
  /tips pick <n>         show tip number <n>
  /tips pick <text>      show the tip whose text matches <text>
  /tips search <query>   list the tips matching <query>`},
	{name: cmdSetup, hosts: hostBoth, desc: "configure Telegram remote oversight (status, enable, disable, or the setup wizard)", hint: "telegram [on|off|status]", help: `  /setup telegram        configure Telegram remote oversight interactively
  /setup telegram on     enable Telegram (credentials must already be configured)
  /setup telegram off    disable Telegram (credentials are preserved)
  /setup telegram status show current Telegram oversight status`},
	{name: cmdConfig, hosts: hostBoth, desc: "print the config, run the setup wizard, or open the config file", hint: "[show|init|open]", help: `  /config                print the current config (merged global + local overrides)
  /config show           print the merged config tagged [global]/[local]/[default]
  /config init           run the setup wizard (configure primary + escalation agents)
  /config open           open config in $EDITOR / system default editor`},
	{name: cmdInit, hosts: hostBoth, desc: "run the setup wizard (alias for /config init)", help: `  /init                  alias for /config init — run the setup wizard`},
	{name: cmdOpen, hosts: hostTUI, desc: "open any file in $EDITOR (agent can also call the open_file tool)", hint: "<file>", help: `  /open <file>           open any file in $EDITOR (agent can also call the open_file tool)`},
	{name: cmdMCP, hosts: hostTUI, desc: "list and manage MCP servers", hint: "[list|add|remove <name>|enable <name>|disable <name>|tools [<name>]|reconnect [<name>]|assign|unassign|auth <name>]", help: `  /mcp                   list configured MCP servers and their status
  /mcp list [<agent>]    list MCP servers for an agent (default: primary)
  /mcp add                add a new MCP server interactively
  /mcp add name=… url=… [transport=http] [auth=…] [api_key=…] [timeout=…]
  /mcp add name=… transport=stdio command=… [args=arg1,arg2,…]
  /mcp remove <name>     remove an MCP server by name
  /mcp enable <name>     enable a disabled MCP server
  /mcp disable <name>    disable an MCP server (keeps config)
  /mcp tools [<name>]    list tools exported by connected MCP server(s)
  /mcp reconnect [<name>]  reset dead server(s) so they retry on next use
  /mcp assign <server> for <agent>   add server to agent's mcp_servers list
  /mcp unassign <server> for <agent>  remove server from agent's list
  /mcp auth <name>       run OAuth authorization flow for an MCP server`},
	// No help lines: /update predates the registry and is executable but
	// undocumented in the TUI help text (pre-existing gap; see the
	// known-undocumented pin in slashcmd_test.go).
	{name: cmdUpdate, hosts: hostBoth, desc: "check for milk updates, install one, or skip a release", hint: "check|status|install|skip", help: `  /update [check]        check the GitHub releases API for a newer milk
  /update status         show the update state and the available release
  /update install        download and apply the pending update (then restart)
  /update skip           stop offering the pending release (never announced again)`},
	{name: cmdRestart, hosts: hostTUI, desc: "quit and relaunch milk with the same starting command", help: `  /restart              quit and relaunch milk — same command, same session
                         (via ` + "`task run`" + ` rebuild when launched that way; refused while a turn,
                         job, workflow or shell is running)`},
	{name: cmdWorkflow, hosts: hostBoth, desc: "run a workflow, or resume, inspect or clear the saved one", hint: "<name> <task> [--<role> <agent>] | resume | status | cancel | reconfigure | clear", help: `  /workflow                                          list available workflows
  /workflow <name> [<task>] [--<role> <agent> ...]   start a registered workflow, e.g. dev (designer→generator→evaluator),
                                                      pair (adds a user checkpoint before each verdict), swarm (concurrent
                                                      items + a final pass) — or a custom one from the workflows dir under milk's state root
  /workflow status                                   show the running (or saved) workflow
  /workflow cancel                                   stop the running workflow; /workflow resume continues it
  /workflow resume                                   resume workflow from last checkpoint
  /workflow reconfigure                              reassign agent roles for current workflow (preserves state)
  /workflow clear                                    delete saved workflow state for this session`},
	{name: cmdServer, hosts: hostTUI, desc: "show or manage the local inference server", hint: "[status [<agent>]|start for <agent>|stop [<agent>]]", help: `  /server status [<agent>]          show server status (reachable + PID)
  /server start for <agent>         start the inference server manually
  /server stop [<agent>]            stop the server tracked for an agent`},
	{name: cmdReload, hosts: hostTUI, desc: "re-parse config.json immediately (same as the auto-watcher)", help: `  /reload                re-parse config.json immediately (same as the auto-watcher) — agent changes rebuild the runners for the next turn`},
	{name: cmdTasks, hosts: hostBoth, desc: "list the session's and global tasks", help: `  /tasks                 list current session and global tasks`},
	{name: cmdTask, hosts: hostBoth, desc: "mark a task done", hint: "done <id>", help: `  /task done <id>        mark a task done`},
	{name: cmdAttach, hosts: hostTUI, desc: "stage a file or image for the next agent turn", hint: "<path>", help: `  /attach <path>         stage a file or image for the next agent turn
                         text files → quoted block in message body
                         images (png/jpg/gif/webp) → multipart vision payload`},
	{name: cmdBash, hosts: hostTUI, desc: "manage the direct-bash auto-allow list", hint: "[list|allow <prefix>|deny <prefix>]", help: `  /bash list             list the auto-allow prefixes (no confirmation prompt)
  /bash allow <prefix>   add a prefix to the auto-allow list and save config
  /bash deny <prefix>    remove a prefix from the auto-allow list and save config`},
	{name: cmdBg, hosts: hostBoth, desc: "list, start or stop background agents", hint: "[list|show <id>|start <task>|stop <id>]", help: `  /bg                    list background agents (ID, status, label, elapsed)
  /bg list               same as bare /bg
  /bg show <id>          print one job's full result
  /bg start <task>       spawn a background agent to research <task>
  /bg stop <id>          terminate a running background agent`},
	{name: cmdKeys, hosts: hostTUI, desc: "list the keyboard shortcuts", help: `  /keys                        list the keyboard shortcuts (panels, navigation, input)`},
	{name: cmdNotifications, hosts: hostTUI, desc: "show or clear the notification history", hint: "[list [<count>]|clear]", help: `  /notifications               show last 20 notifications (timestamps + hints)
  /notifications list          show the full notification history, unfiltered
  /notifications list <count>  show the last <count> notifications
  /notifications clear         clear notification history`},
	{name: cmdNew, hosts: hostBoth, desc: "start a fresh session (the current one stays resumable)", hint: "[name]", help: `  /new [name]            start a fresh session (the current one stays resumable)`},
	{name: cmdClear, hosts: hostBoth, desc: "alias for /new — start a fresh session", hint: "[name]", help: `  /clear [name]          alias for /new — start a fresh session`},
	{name: cmdDrop, hosts: hostBoth, args: argsOptRef, desc: "delete the current session, or the referenced one", hint: "[" + sessionRefHint + "]", help: `  /drop [<id|prefix|name>]  delete the current session, or the referenced one`},
	{name: cmdSessions, hosts: hostBoth, desc: "list stored sessions for this directory (* = current)", hint: "[all]", help: `  /sessions [all]        list stored sessions for this directory (* = current)`},
	{name: cmdResume, hosts: hostBoth, args: argsRef, desc: "switch this conversation to a stored session", hint: sessionRefHint, help: `  /resume <id|prefix|name>  switch to a stored session (transcript is reseeded)`},
	{name: cmdListLegacy, hosts: hostBoth, hidden: true, desc: "deprecated alias for /sessions"},
	{name: "/help", hosts: hostBoth, desc: "list the commands available in this session", help: `  /help                  show this help`},
	// /quit shares /exit's help line — the line names both commands.
	{name: "/exit", hosts: hostTUI, desc: "quit", help: `  /exit  /quit           quit`},
	{name: "/quit", hosts: hostTUI, desc: "quit (alias for /exit)"},
}

// specFor returns the registry entry for a command name (or alias).
func specFor(name string) (cmdSpec, bool) {
	for _, s := range commandRegistry {
		if s.name == name {
			return s, true
		}
		for _, a := range s.aliases {
			if a == name {
				return s, true
			}
		}
	}
	return cmdSpec{}, false
}

// registryNames returns every executable name — canonical names in registry
// order plus aliases. This is the recognition set for extractSlashCommand and
// the tab-completion cycle (slashCommands).
func registryNames() []string {
	names := make([]string, 0, len(commandRegistry))
	for _, s := range commandRegistry {
		names = append(names, s.name)
		names = append(names, s.aliases...)
	}
	return names
}

// helpBlock is one section of the TUI help text: a verbatim header line, the
// registry names it lists in display order, and an optional verbatim prose
// note rendered after the commands (sections like Keyboard carry only a
// note). This defines the help's display order independently of the
// registry's completion order.
type helpBlock struct {
	header string
	names  []string
	note   string
}

var helpBlocks = []helpBlock{
	{header: "── Routing ──────────────────────────────────────────────────────────────", names: []string{cmdEscalate, cmdPrimary}},
	{header: "── Sessions ─────────────────────────────────────────────────────────────", names: []string{cmdSessions, cmdResume, cmdNew, cmdClear, cmdDrop, cmdExport}},
	{header: "── Agents ───────────────────────────────────────────────────────────────", names: []string{cmdAgent, cmdSkipPerms}},
	{header: "── Permissions ──────────────────────────────────────────────────────────", names: []string{cmdPermissions}},
	{header: "── Memory ───────────────────────────────────────────────────────────────", names: []string{cmdLearn, cmdMemory, cmdForget, cmdPanel, cmdTasks, cmdTask}},
	{header: "── Background agents ───────────────────────────────────────────────────", names: []string{cmdBg}},
	{header: "── Display ──────────────────────────────────────────────────────────────", names: []string{cmdColorize, cmdTheme, cmdThink, cmdTips, cmdHistory, cmdNotifications, cmdKeys}},
	{header: "── Observability ────────────────────────────────────────────────────────", names: []string{cmdUsage, cmdMetrics, cmdOtel}},
	{header: "── MCP Servers ───────────────────────────────────────────────────────────", names: []string{cmdMCP}},
	{header: "── Workflow ───────────────────────────────────────────────────────────────", names: []string{cmdWorkflow}},
	{header: "── Direct bash ───────────────────────────────────────────────────────────", names: []string{cmdBash}},
	{header: "── Server ────────────────────────────────────────────────────────────────", names: []string{cmdServer}},
	{header: "── Setup ─────────────────────────────────────────────────────────────────", names: []string{cmdConfig, cmdInit, cmdReload, cmdOpen, cmdSetup}},
	{header: "── Attachments ──────────────────────────────────────────────────────────", names: []string{cmdAttach, cmdPaste}},
	{header: "── General ──────────────────────────────────────────────────────────────", names: []string{"/help", cmdUpdate, cmdRestart, cmdInterrupt, "/exit"}},
	{header: "── Multi-value parameters ───────────────────────────────────────────────", note: `  The server and agent name parameters of /mcp assign|unassign (both
  sides of "for") and the "for <agent>|global" scope of
  /agent tool enable|disable|add|remove accept comma separated lists,
  e.g. "… for alice,bob" targets both agents — with several servers
  every server×agent pair is applied. Validation is all-or-nothing:
  one bad name rejects the whole call, lists each invalid item, and
  applies nothing. Tab completion starts a fresh segment at each comma.
  Values containing spaces or the word "for" (e.g. /agent tool add
  coder description="agent for code review") must be quoted — the
  scope separator is the last standalone "for" token. Missing required
  arguments open a step-by-step wizard instead of a usage line.`},
	{header: "── Keyboard ─────────────────────────────────────────────────────────────", note: `  Scrolling
    Mouse wheel / PgUp/PgDn / Ctrl+U / Ctrl+F   scroll transcript

  Agent control
    Ctrl+C   interrupt current agent turn (or copy if selection is active)
    Ctrl+T   toggle the most recent thinking block (the streaming one, if any)
             reasoning renders as foldable blocks on their own background,
             folded by default — the header's line count updates live while
             streaming; click a ▸ header to toggle just that block (new blocks
             keep that fold state; while folded, a new turn folds the previous
             turns' blocks — /think expand opts the tidy out);
             /think expand|collapse folds or opens them all

  Notifications
    Ctrl+G                      dismiss all open notification toasts
                                (history stays: /notifications)

  Transcript selection
    Mouse drag                    select by line+column
    Shift+Arrow / Ctrl+Arrow      extend or reduce selection (when active)
    Ctrl+Click                    extend selection to clicked position
    Ctrl+C / right-click          copy selection to clipboard
    Esc                           clear selection

  Input history
    Up / Down (single-line input)   navigate history
    Ctrl+Up / Ctrl+Down             navigate history (or extend transcript selection when active)
    Ctrl+R                          reverse incremental search
    Ctrl+S                          forward incremental search

  Multi-line input
    Ctrl+N / Shift+Alt+Enter / Alt+Enter   insert newline
    Paste                                  multi-line paste sent as one block

  Input editing
    Ctrl+Z / Ctrl+Y                 undo / redo the input edit history
    Alt+Left / Alt+Right (Alt+B/F)  move the cursor one word back / forward

  Panels
    F1 / F2 / F3 / F4   show/hide the memory / tasks / background-agents / workflow
                        panel — same as /panel <name>, works in any mode

  Tips
    Alt+Up / Alt+Down   show the previous / next idle tip (same as /tips prev|next)

  Memory panel
    Double-click entry   print memory entry details to transcript

  @ prefix
    @path   reference a file path (Tab-completes)

  Shell input
    !command   run the command directly in a shell pane — no agent and no
               confirmation, even while a turn runs (Claude Code style).
               Shell-looking input without the "!" runs directly only with
               direct bash enabled: auto-allowed prefixes (/bash) run at
               once, anything else asks Y/n first`},
}

// buildInteractiveHelp assembles the TUI help text from the registry: each
// block's verbatim header, its commands' verbatim help lines in display
// order, and any prose note. The result is byte-identical to the historical
// hand-maintained const — only now every command line belongs to a registry
// entry, so a command cannot exist on one surface and be missing or stale on
// the other.
func buildInteractiveHelp() string {
	var b strings.Builder
	b.WriteByte('\n')
	for i, blk := range helpBlocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(blk.header)
		for _, name := range blk.names {
			if spec, ok := specFor(name); ok && spec.help != "" {
				b.WriteByte('\n')
				b.WriteString(spec.help)
			}
		}
		if blk.note != "" {
			b.WriteByte('\n')
			b.WriteString(blk.note)
		}
	}
	return b.String()
}
