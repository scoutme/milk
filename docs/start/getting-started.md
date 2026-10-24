# Getting started

The fastest path to a working milk setup — no specific backend required. milk works with any provider in either role; see [docs/guides/providers.md](../guides/providers.md) for the full catalog once you're past this page.

## Prerequisites

| Dependency | Required | Notes |
|---|---|---|
| Go 1.26.2+ | no | only if building from source |
| An inference server or cloud API key | no | any backend from [docs/guides/providers.md](../guides/providers.md); milk degrades gracefully if the primary or escalation agent is absent — see [docs/guides/operations.md — Graceful degradation](../guides/operations.md#graceful-degradation) |
| `claude` CLI | no | only if you want Claude Code as an agent; not required otherwise |
| Git Bash or WSL2 (Windows only) | yes (Windows) | milk's primary agent uses `sh`, `find`, `grep` — `cmd.exe`/PowerShell are not supported. See [docs/guides/providers.md — Windows and WSL2](../guides/providers.md#windows-and-wsl2) |

## Install

Pre-built binary — no Go toolchain needed:

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

Installs to `~/.local/bin/milk`, verifying the release checksum. Pin a version with `MILK_VERSION=v0.2.0`. On Windows, run this inside WSL2 (see [docs/guides/providers.md — Windows and WSL2](../guides/providers.md#windows-and-wsl2)).

Stay current with `milk update check` / `milk update install` (TUI: `/update install`). On Windows the running `milk.exe` is renamed aside and the verified download takes its place — applied on your next restart of milk, staged and retried automatically if the rename is refused; see [docs/guides/operations.md — Self-update](../guides/operations.md#self-update).

Or build from source (requires Go 1.26.2+):

```sh
task build:local   # builds ./milk in the current directory
# or, to install to ~/.local/bin:
task build
```

`install-from-source.sh` does the equivalent in one line without a manual clone: `curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install-from-source.sh | sh`.

## Configure a backend

You need at least one working agent to do anything useful. Launch milk with no config and run the guided setup:

```sh
milk
```

```
/config init
```

(`/init` is the short alias.) It is a handful of plain questions, one decision per screen, and every question has a default you can accept with Enter:

0. **Here is what I found.** Before asking anything, setup looks (read-only) at your machine and offers what it finds as the first choice: API keys already exported in your environment (OpenRouter, Groq, Together, DeepSeek, OpenAI, Anthropic), local model servers that are running (Ollama, llama.cpp, LM Studio) and Claude Code with its sign-in state. Pick one with a number and you skip most of the questions. Keys are shown by variable name and a masked fingerprint only, and the config stores a pointer (`api_key_env`), never the key itself. If nothing is found it says so and lists where to get a key.
1. **Which AI answers your everyday prompts?** — a model on this computer (Ollama, llama.cpp, LM Studio, vLLM), a cloud API key (OpenRouter, Groq, Together, GitHub Copilot, …), AWS Bedrock, or the Claude Code CLI you already have. This is the *primary*: cheap and fast.
2. **Where is it / which model / what key?** — only what that choice needs. A local server defaults to `http://localhost:11434` (Ollama); a cloud API to OpenRouter. Keys are saved only in `config.json` and never sent to the model.
3. **Hand hard tasks to Claude Code?** (If Claude Code is installed but signed out, setup offers to open your browser to log in — `claude auth login` — instead of asking you to find an API key.) — the *escalation*: a stronger AI milk switches to when a task is too hard for the primary. Defaults to yes if the `claude` CLI is on your `PATH`, no otherwise.
4. **Fine-tune optional settings?** — the server start command, context size and agents-as-tools. Skip it and milk uses sensible defaults; anything you skip can be changed later with `/config`.

Then milk writes `~/.milk/config.json`, applies it immediately (no restart), and sends one tiny request to your primary. If it answers you see "you're ready" plus the first commands to try (`/help`, `/sessions`, `/permissions`); if not, you get the reason — wrong URL, server down, rejected key or model — and your settings stay saved so you can re-run `/config init` or edit them with `/config open`. The same questions in the same order run over ACP (`milk serve --acp`), as forms, clickable choices or plain chat.

Editing `~/.milk/config.json` by hand, or adding more backends one at a time with `/agent add`, is the advanced alternative. Assign any configured agent to a role:

```
/agent switch <name> as primary
/agent switch <name> as escalation
```

**There is no preferred pairing.** Every backend, including `claude-cli`, can serve either role — the only constraint milk expects you to honor yourself is that the escalation agent should be smarter (and usually pricier) than the primary agent. A local model as escalation and a cloud model as primary is a perfectly valid, if unusual, setup. `claude-cli` as primary specifically works but isn't a smart everyday choice (every turn spawns a full subprocess, and it can't self-escalate); see [docs/guides/providers.md](../guides/providers.md#primary-agent) for where it's actually useful. See [docs/guides/workflows.md](../guides/workflows.md#routing) for how routing decides which agent handles a given prompt.

If you don't already have a backend in mind, [docs/guides/providers.md](../guides/providers.md) has copy-paste examples for local llama.cpp/Ollama/LM Studio, Claude Code CLI, AWS Bedrock, OpenRouter, Together.ai, Groq, Azure OpenAI, aider, and smolagents — including a full local-hardware reference setup (NVIDIA GPU, WSL2, llama.cpp from source) if you want to run inference yourself rather than call an API.

Check what's configured any time with `/agent` (or `/agent list`).

## Your first turn

Launch the **TUI** with no arguments — this is how milk is meant to be used day to day:

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

Once escalation fires — whether you asked for it or the primary agent called `escalate()` on its own — milk keeps subsequent turns on the escalation agent automatically (**auto-sticky**, shown as `<agent> (sticky)` in the status bar) until you type `/primary`. See [docs/guides/workflows.md](../guides/workflows.md) for the full routing and session-state model.

Manage sessions from inside the TUI with `/new` (start fresh — alias `/clear`), `/drop` (delete the current one), `/resume <id|prefix|name>` (switch to a stored session), and `/sessions` (sessions for this directory; `/list` is a deprecated alias). Type `/help` for the full command list, `/exit` or Ctrl-D to quit.

### A realistic session

A real (lightly trimmed) transcript from a two-agent setup, showing routing, memory, and escalation in one session:

```
❯ list the go files under internal/router
mimo-local: ▸ thinking · 9 lines
  » find_files: internal/router
Here are the Go files under `internal/router`:
- internal/router/router.go
- internal/router/rules.go
- internal/router/rules_test.go
```

A simple lookup stays on the primary agent, which runs `find_files` itself — no escalation needed.

```
❯ /learn this project prefers editing over rewriting files when possible
```

`/learn` records the fact as a percept. It shows up in the memory panel (`/panel memory`) immediately, marked with a star for "just updated":

```
GLOBAL (core)
★ #5028a1 this project prefers
  editing over rewriting files …
```

```
❯ /escalate explain the tradeoffs of the router weighted-scorer design versus a pure keyword approach
mimo-pro-local: ▸ thinking · 24 lines
  » read_file: internal/router/router.go
  » read_file: internal/router/rules.go
  » read_file: internal/router/rules_test.go
Here's a breakdown of the tradeoffs between the current weighted-scorer design and a pure keyword approach:

## Weighted-Scorer Design (what you have)
...
1. Composability / nuance. A single signal isn't decisive — signals *combine*. "Why should
   we evaluate this approach?" fires both open-question (+3) and escalate-verb (+4) = 7,
   crossing the escalate threshold of 6. A keyword-only router would need an explicit entry
   for every combination or miss the escalation.
...
```

The escalation agent reads the actual source files rather than answering from assumptions, then gives a genuine multi-point analysis. After this turn, the status bar shows `mimo-pro-local (sticky)` — **auto-sticky** kept the session on the escalation agent, so a follow-up question continues the conversation there without you having to type `/escalate` again. `/primary` breaks back out whenever you're ready.

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

Sessions are shared with the TUI's session model — the same `--list`/`--new`/`--drop` flags here correspond to `/sessions`/`/new`/`/drop` there.

### Graceful degradation

Stop either agent (or never configure one) and confirm milk warns rather than crashing, in either mode — see [docs/guides/operations.md — Graceful degradation](../guides/operations.md#graceful-degradation) for the full behavior table.

## Where to go next

| If you want to… | See |
|---|---|
| Pick and configure a specific backend | [docs/guides/providers.md](../guides/providers.md) |
| Understand routing, sticky escalation, or the native `/workflow` engine | [docs/guides/workflows.md](../guides/workflows.md) |
| Give an agent tools, other agents, or MCP servers | [docs/guides/tooling.md](../guides/tooling.md) |
| Tune memory, observability, loop detection, or remote oversight | [docs/guides/operations.md](../guides/operations.md) |
| Compare agents/models on real scenarios | [docs/reference/eval.md](../reference/eval.md) |
| Understand why milk is built the way it is | [docs/adr/](../adr/README.md) |

## Troubleshooting

**Session in a bad state**: drop it and start fresh — `/drop` in the TUI, or `milk --drop` from the shell.

For backend-specific troubleshooting (e.g. local llama.cpp server issues), see the relevant section in [docs/guides/providers.md](../guides/providers.md).
