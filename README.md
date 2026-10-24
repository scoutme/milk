<p align="center">
  <img src="docs/assets/banner.jpg" alt="milk — Switch models, not context. A terminal agent host for routing, tools, MCP, multi-agent workflows, persistent memory, and evaluation." width="100%">
</p>

<p align="center">
  <a href="https://github.com/scoutme/milk/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/scoutme/milk"></a>
  <a href="LICENSE.md"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-blue.svg"></a>
  <img alt="Go module (version per go.mod)" src="https://img.shields.io/badge/Go-module-00ADD8?logo=go&logoColor=white">
  <img alt="Platforms: Linux, macOS, Windows, WSL2" src="https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows%20%7C%20WSL2-lightgrey">
  <a href="https://github.com/scoutme/milk"><img alt="Stars" src="https://img.shields.io/github/stars/scoutme/milk?style=flat"></a>
  <a href="https://github.com/scoutme/milk/releases"><img alt="Downloads" src="https://img.shields.io/github/downloads/scoutme/milk/total"></a>
</p>

milk routes each prompt between a cheap primary agent and a deeper escalation agent while keeping one shared conversation. Routine work stays fast and inexpensive; hard turns move to the stronger model with full context, and follow-up turns stay there until you switch back.

## Features

- **Automatic routing and handoff** — per-prompt routing from explicit flags, session state, a rules/scorer layer, and a primary-model classifier. The primary agent can escalate itself with `escalate(reason)`, and auto-sticky escalation keeps follow-ups on the stronger model.
- **Any backend, either role** — OpenAI-compatible HTTP, AWS Bedrock, Claude Code CLI, aider, smolagents, and Bearer-token providers. Any backend can be primary or escalation; escalation should be the stronger, usually pricier model.
- **Tools and MCP** — built-in bash, file I/O, grep/find, HTTP, memory, and task tools; MCP servers over stdio or HTTP with OAuth; agent-as-tool; inline images for `vision: true` agents.
- **Orchestration** — `spawn_background_agent` for asynchronous work, plus the native `/workflow` engine (`dev`, `pair`, `swarm`, or custom YAML) with typed verdicts, checkpoint/resume, and live attach.
- **Memory and safety** — a persistent percept store with consolidation, loop detection with auto-interrupt, task tracking, and session-level tool permissions.
- **Observability and evaluation** — per-role token usage, OpenTelemetry JSONL exporters, and `milk eval` for side-by-side quality, token, cache, and latency comparisons.
- **Editor and script surfaces** — `milk serve --acp` for Agent Client Protocol v2 clients, and `--output-format json|stream-json` for one-shot automation.
- **TUI and skins** — Bubble Tea v2 renderer with synchronized output, overlays, one key registry and spring-animated panels. Two colour skins: the neutral default look and the opt-in `/theme supermilk` (milk-white ground, warm ink, muted olive accent), saved in `config.json`.

## Installation

### Linux and macOS

Install the pre-built binary (recommended):

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

The installer verifies the release checksum when the checksum tooling is available and places the binary at `~/.local/bin/milk`. The pre-built binary does not require Go. If `~/.local/bin` is not on your `PATH`:

```sh
# Add this line to ~/.bashrc or ~/.zshrc, then restart the shell:
export PATH="$HOME/.local/bin:$PATH"
```

For fish:

```fish
fish_add_path ~/.local/bin
```

Pin a release by setting `MILK_VERSION`:

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | MILK_VERSION=v0.7.3 sh
```

#### From source

Requires Git and Go 1.26.2 or newer (the version required by [`go.mod`](go.mod)):

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install-from-source.sh | sh
```

This clones milk to `~/.local/share/milk` by default (`$XDG_DATA_HOME/milk` when set) and builds the binary to `~/.local/bin/milk`. Override the source directory with `MILK_INSTALL_DIR`; select a tag or branch with `MILK_VERSION` (default: `main`).

To build manually:

```sh
git clone https://github.com/scoutme/milk.git
cd milk
go build -o milk ./cmd/milk
./milk --version
```

Verify the installation with `~/.local/bin` on your `PATH`:

```sh
milk --version
```

### Windows and WSL2

milk's primary agent runs shell tools through `sh`, `find`, and `grep`. `cmd.exe` and PowerShell cannot provide those tools, so **WSL2 is the supported full setup**. A native Windows amd64 build is available, but the built-in bash tool does not work there; the TUI, HTTP/Bedrock providers, and Claude CLI escalation do.

WSL2 requires virtualization enabled in BIOS/UEFI. `wsl --install` is available on Windows 10 version 2004 or newer and on Windows 11.

#### Recommended: install inside WSL2

In an **Administrator PowerShell**:

```powershell
wsl --install
```

Restart Windows if prompted. Then set WSL2 as the default and verify the installation:

```powershell
wsl --set-default-version 2
wsl -l -v
```

Launch the default distribution:

```powershell
wsl
```

Inside **WSL2**, run the Linux installation:

```bash
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
milk --version
milk
```

To persist the `PATH` change, add the same `export` line to `~/.bashrc` or `~/.zshrc`; for fish use `fish_add_path ~/.local/bin`.

#### Native Windows (optional, limited)

In **PowerShell**:

```powershell
irm https://raw.githubusercontent.com/scoutme/milk/main/install.ps1 | iex
```

The installer verifies the checksum when available, installs to `%LOCALAPPDATA%\milk\bin\milk.exe`, and adds that directory to your user `PATH`. Restart the terminal afterward, then verify:

```powershell
milk --version
```

Native builds are currently amd64 only.

Because the built-in bash tool needs a POSIX shell, launch `milk` from Git Bash or use WSL2 for full tool support. To pin a native install:

```powershell
$env:MILK_VERSION = "v0.7.3"
irm https://raw.githubusercontent.com/scoutme/milk/main/install.ps1 | iex
```

## Updating milk

milk checks for updates at startup at most once every 24 hours. Manual checks and installs are available from the CLI and the TUI:

| Action | CLI | TUI |
| --- | --- | --- |
| Check for a newer release | `milk update check` | `/update check` |
| Download and install | `milk update install` | `/update install` |
| Suppress a release | — | `/update skip` |
| Restart after updating | quit and relaunch | `/restart` |

In the TUI, run `/update check` before `/update install`; the install uses the release found by that check. The CLI checks automatically before installing.

`milk update install` downloads the latest applicable release, verifies the published SHA-256 when present, and replaces the binary in place.

- **A restart is required.** The running process keeps the old version until you restart it; starting a new session is not enough. In the TUI, `/restart` relaunches milk and resumes the current session.
- **POSIX:** the binary is replaced with an atomic rename, and the next start uses the new version.
- **Windows:** the running `milk.exe` is renamed aside and replaced. If replacement is blocked, the verified download is staged and applied automatically on the next start.
- **Skipped releases:** `/update skip` suppresses that tag until a newer release appears. `milk update check` honors the skip; `milk update install` ignores it and installs the latest applicable release.
- **Pre-releases:** included by default. Set `"update_channel": "stable"` in the active config (`~/.milk/config.json` by default) to exclude them.
- **Version pinning:** the built-in updater has no version flag and always moves to the latest applicable release. Reinstall with `MILK_VERSION` to pin; running the updater later will move off the pin.
- **Source installs:** rerun `install-from-source.sh` with the desired `MILK_VERSION` (default: `main`) to fetch and rebuild. The built-in updater replaces only the installed binary, not the source checkout.
- **Manual upgrades:** with milk stopped, download the binary and `.sha256` from [Releases](https://github.com/scoutme/milk/releases) and replace the installed binary.

Run update commands in the environment where milk is installed: inside WSL2 for a WSL2 install, or in PowerShell/Git Bash for a native Windows install.

## Quick start

Launch the TUI:

```sh
milk
```

With no agents configured, milk starts in setup mode. Add a backend with the interactive wizard:

```text
/agent add
```

The wizard appends the backend to the active milk config (`~/.milk/config.json` by default). Assign roles; changes are saved immediately and apply from the next turn:

```text
/agent switch <name> as primary
/agent switch <name> as escalation
```

One backend can serve both roles, but the escalation agent should be the stronger model. Inspect the current setup with `/agent` or `/agent list`.

Start working in the same session:

```text
❯ list the Go files in this directory
❯ /escalate explain the routing design and its tradeoffs
❯ /primary
```

`/escalate <prompt>` forces one turn to the escalation agent; `/escalate` alone pins subsequent turns there. `/primary` switches back. Type `/help` for the full command list.

For scripting and one-shot use:

```sh
milk "list the Go files in this directory"
milk --escalate "review the routing design"
milk --output-format stream-json "summarize this repository"
```

## Supported backends

Both roles accept any backend. The only routing constraint is that the escalation agent should be smarter and usually pricier than the primary agent.

| `provider` value | Backend |
| --- | --- |
| `claude-cli` | Claude Code CLI, run as a `claude` subprocess |
| `local`, `""`, or omitted | Any OpenAI-compatible server: llama.cpp, Ollama, LM Studio, vLLM, Azure OpenAI |
| `bedrock` | AWS Bedrock Converse API with SigV4 signing |
| `aider-cli` | aider CLI |
| `subprocess` | Generic NDJSON subprocess (the smolagents adapter is bundled) |
| any other value | Bearer-token HTTP: OpenRouter, Groq, Together.ai, GitHub Models, and similar |

`claude-cli` requires the `claude` binary on `PATH`; `aider-cli` requires the `aider-chat` Python package; `subprocess` requires `smolagents[litellm]`.

A built-in `"claude"` entry using `claude-cli` is always available and is the default escalation agent. See [Providers & configuration](docs/guides/providers.md) for configuration examples and Windows/WSL2 notes.

## How it works

Each prompt passes through this decision chain:

1. **Explicit flags** — `--escalate` or `--primary` override everything.
2. **Session state** — after escalation, follow-up turns go directly back to the escalation agent.
3. **Rules layer** — hard token/keyword thresholds, then a weighted signal scorer.
4. **Primary-model classifier** — when the scorer is inconclusive, the primary model chooses `local` or `escalate`.
5. **Default** — the primary agent, which can call `escalate(reason)` if it cannot complete the task.

On escalation, milk reformats the primary conversation as context for the escalation agent, so no separate setup step is needed. Auto-sticky then keeps later turns on the escalation agent — shown as `<agent> (sticky)` in the status bar — until `/primary` switches back.

## Documentation

Full documentation: **[milkai.dev](https://milkai.dev)**

| Topic | Guide |
| --- | --- |
| Install and first run | [Getting started](docs/start/getting-started.md) |
| Backend and provider configuration | [Providers & configuration](docs/guides/providers.md) |
| Routing, sessions, and workflows | [Routing, sessions & workflows](docs/guides/workflows.md) |
| Tools, MCP, and tool-agents | [Tooling, MCP & tool-agents](docs/guides/tooling.md) |
| Memory, observability, and operations | [Operations](docs/guides/operations.md) |
| Evaluation harness | [Evaluation](docs/reference/eval.md) |
| ACP integration | [ACP integration](docs/reference/acp-integration.md) |
| CLI and architecture reference | [Specification](docs/reference/spec.md) |
| Design decisions | [ADRs](docs/adr/README.md) |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for issue and pull-request guidance. Commits follow [Conventional Commits](https://www.conventionalcommits.org/); `main` is protected and changes land through pull requests. This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).

## Security

Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/scoutme/milk/security/advisories/new) or as described in [SECURITY.md](SECURITY.md). Do not open a public issue for a security report.

## License

MIT. See [LICENSE.md](LICENSE.md).

## Acknowledgments

milk's prompt/context-management and loop-detection design was informed by studying other agent harnesses — independent implementations, not ports:

- **[Claude Code](https://claude.com/claude-code)** — the escalation-agent subprocess milk drives, and the reference for project-instruction loading (`AGENTS.md`/`CLAUDE.md`).
- **[MiMo-Code](https://github.com/XiaomiMiMo/MiMo-Code)** — compared for loop-detection thresholds, prompt-caching breakpoints, and checkpoint/distillation memory.
- **[OpenCode](https://github.com/anomalyco/opencode)** — compared for project-instruction loading, sub-agent context isolation, and prompt-caching strategy.

See [the full comparison](docs/internal/analysis/prompt-context-management-review.md) for details.
