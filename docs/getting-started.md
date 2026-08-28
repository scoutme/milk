# Getting started

The fastest path to a working milk setup — no specific backend required. milk works with any provider in either role; see [docs/providers.md](providers.md) for the full catalog once you're past this page.

## Prerequisites

| Dependency | Required | Notes |
|---|---|---|
| Go 1.21+ | yes | build only |
| An inference server or cloud API key | no | any backend from [docs/providers.md](providers.md); milk degrades gracefully if the primary or escalation agent is absent — see [docs/operations.md — Graceful degradation](operations.md#graceful-degradation) |
| `claude` CLI | no | only if you want Claude Code as an agent; not required otherwise |
| Git Bash or WSL2 (Windows only) | yes (Windows) | milk's primary agent uses `sh`, `find`, `grep` — `cmd.exe`/PowerShell are not supported. See [docs/providers.md — Windows and WSL2](providers.md#windows-and-wsl2) |

## Build

```sh
task build:local   # builds ./milk in the current directory
# or, to install to ~/.local/bin:
task build
# or to a custom destination:
task build DEST=/usr/local/bin/milk
```

## Configure a backend

You need at least one working agent to do anything useful — it can serve as primary, escalation, or both. Run milk with no config and it starts in setup mode:

```sh
./milk config
```

Then add a backend interactively:

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

Verify what's configured:

```sh
./milk config
```

## Your first turn

**Automated tests** (no running agent required — uses temp dirs and mocks):

```sh
task test
```

**Config and session management** (no server needed):

```sh
./milk --new --session test "hello"
./milk --list
./milk --drop
./milk --list   # should be empty
```

**Primary-agent routing** (an agent must be configured and reachable):

```sh
./milk "list Go files in the current directory"     # routes to primary, runs the bash tool
./milk "now show only the test files"                # resumes the same session
./milk --primary "grep for TODO comments"             # force primary even if rules would escalate
```

**Escalation** (both primary and escalation agents configured):

```sh
./milk --escalate "explain the session state machine design"       # force escalation
./milk "design a plugin architecture for milk"                     # self-escalation: primary calls escalate()

./milk --escalate "what would you need to know to refactor the router?"
./milk "focus on the rules layer"       # ESCALATION_WAITING: goes directly to --resume, no routing

./milk --primary "grep for TODO"        # break back to primary
```

**Graceful degradation** — stop either agent and confirm milk warns rather than crashing; see [docs/operations.md — Graceful degradation](operations.md#graceful-degradation) for the full behavior table.

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

**Session in a bad state**: drop it and start fresh.

```sh
./milk --drop
```

For backend-specific troubleshooting (e.g. local llama.cpp server issues), see the relevant section in [docs/providers.md](providers.md).
