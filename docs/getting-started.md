# Getting started

The fastest path to a working milk setup — no specific backend required. milk works with any provider in either role; see [docs/providers.md](providers.md) for the full catalog once you're past this page.

## Prerequisites

| Dependency | Required | Notes |
|---|---|---|
| Go 1.21+ | no | only if building from source |
| An inference server or cloud API key | no | any backend from [docs/providers.md](providers.md); milk degrades gracefully if the primary or escalation agent is absent — see [docs/operations.md — Graceful degradation](operations.md#graceful-degradation) |
| `claude` CLI | no | only if you want Claude Code as an agent; not required otherwise |
| Git Bash or WSL2 (Windows only) | yes (Windows) | milk's primary agent uses `sh`, `find`, `grep` — `cmd.exe`/PowerShell are not supported. See [docs/providers.md — Windows and WSL2](providers.md#windows-and-wsl2) |

## Install

Pre-built binary — no Go toolchain needed:

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

Installs to `~/.local/bin/milk`, verifying the release checksum. Pin a version with `MILK_VERSION=v0.2.0`. On Windows, run this inside WSL2 (see [docs/providers.md — Windows and WSL2](providers.md#windows-and-wsl2)).

Or build from source (requires Go 1.21+):

```sh
task build:local   # builds ./milk in the current directory
# or, to install to ~/.local/bin:
task build
```

`install-from-source.sh` does the equivalent in one line without a manual clone: `curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install-from-source.sh | sh`.

## Configure a backend

You need at least one working agent to do anything useful — it can serve as primary, escalation, or both. Launch milk with no config and it starts in setup mode:

```sh
milk
```

Add a backend interactively:

```
/agent add
```

The wizard asks for a name, URL/model (or `claude-cli`/`aider-cli`/`subprocess` provider), and credentials, and appends the entry to `~/.milk/config.json`. Assign it to a role:

```
/agent switch <name> as primary
/agent switch <name> as escalation
```

**There is no preferred pairing.** The only constraint milk expects you to honor yourself: the escalation agent should be smarter (and usually pricier) than the primary agent. Any provider — local, cloud, CLI-based — works in either role. A local model as escalation and a cloud model as primary is a perfectly valid, if unusual, setup. See [docs/workflows.md](workflows.md#routing) for how routing decides which agent handles a given prompt.

If you don't already have a backend in mind, [docs/providers.md](providers.md) has copy-paste examples for local llama.cpp/Ollama/LM Studio, Claude Code CLI, AWS Bedrock, OpenRouter, Together.ai, Groq, Azure OpenAI, aider, and smolagents — including a full local-hardware reference setup (NVIDIA GPU, WSL2, llama.cpp from source) if you want to run inference yourself rather than call an API.

Check what's configured any time with `/agent` (or `/agent list`).

## Your first turn

milk is built around the **TUI** — this is the intended, day-to-day way to use it, not an afterthought over a CLI. Launch it with no arguments:

```sh
milk
```

You land in an interactive REPL: a `❯` prompt, a scrollable transcript above it, and a status bar showing the current routing state and active agent. Type a prompt and press Enter:

```
❯ list Go files in the current directory
```

This routes to the primary agent, which runs its `bash` tool and returns the result. Keep going in the same session:

```
❯ now show only the test files
```

Force a specific agent for one turn with `/primary <prompt>` or `/escalate <prompt>`:

```
❯ /escalate explain the session state machine design
```

Once escalation fires — whether you asked for it or the primary agent called `escalate()` on its own — milk keeps subsequent turns on the escalation agent automatically (**auto-sticky**, shown as `<agent> (sticky)` in the status bar) until you type `/primary`. See [docs/workflows.md](workflows.md) for the full routing and session-state model.

Manage sessions from inside the TUI with `/new` (start fresh), `/drop` (delete the current one), and `/list` (sessions for this directory). Type `/help` for the full command list, `/exit` or Ctrl-D to quit.

### Single-prompt mode

`milk [flags] <prompt>` runs one turn non-interactively and exits — useful for scripting or a quick one-off check, but a secondary mode, not how milk is meant to be used day to day:

```sh
milk "list Go files in the current directory"
milk --escalate "explain the session state machine design"
milk --primary "grep for TODO comments"
milk --new "start a fresh session"
milk --list
milk --drop
```

Sessions are shared with the TUI's session model — the same `--list`/`--new`/`--drop` flags here correspond to `/list`/`/new`/`/drop` there.

### Graceful degradation

Stop either agent (or never configure one) and confirm milk warns rather than crashing, in either mode — see [docs/operations.md — Graceful degradation](operations.md#graceful-degradation) for the full behavior table.

## Where to go next

| If you want to… | See |
|---|---|
| Pick and configure a specific backend | [docs/providers.md](providers.md) |
| Understand routing, sticky escalation, or the native `/workflow` engine | [docs/workflows.md](workflows.md) |
| Give an agent tools, other agents, or MCP servers | [docs/tooling.md](tooling.md) |
| Tune memory, observability, loop detection, or remote oversight | [docs/operations.md](operations.md) |
| Compare agents/models on real scenarios | [docs/eval.md](eval.md) |
| Understand why milk is built the way it is | [docs/adr/](adr/README.md) |

## Troubleshooting

**Session in a bad state**: drop it and start fresh — `/drop` in the TUI, or `milk --drop` from the shell.

For backend-specific troubleshooting (e.g. local llama.cpp server issues), see the relevant section in [docs/providers.md](providers.md).
