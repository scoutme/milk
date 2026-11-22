# ADR-0106: Additive `config init` + harness import

- **Status:** Accepted 2026-11-22
- **Context:** the `/config init` commit was found to be silently destructive on re-runs, and the discovery scan only ever looked at the environment — harnesses the user already configured (Claude Code, Claude Desktop) were invisible
- **Supersedes/amends:** nothing — complements [ADR-0045](0045-local-config-overrides.md) (scope handling) and [ADR-0101](0101-config-write-safety.md) (the persist guard this leans on)

## Context

Two problems with `/config init` / `milk config init`, both discovered while evaluating whether "import from other harnesses" was worth building:

1. **The commit was a rebuild, not an edit.** `commitInitWizardConfig` built the config from `defaults()` + the wizard's answers (`config.InitConfig`) and forced it over whatever was there. Re-running the wizard on a configured machine silently reset every field the wizard never asks about — MCP servers, routing rules, permissions, OTel, extra agents — recoverable only from the global `config.json.bak`, or not at all for a project-local overlay (local saves keep no backup). Worse, `getting-started.md` framed re-running as safe ("your settings stay saved").
2. **No harness import.** Surveying Claude Code, Codex, Gemini CLI, Copilot CLI, Goose, aider, Continue.dev, opencode and MiMo-Code: the universal convention is discovering env API keys and reusing your *own* stored login — nobody migrates another harness's config files except MiMo-Code (one-click "Import from Claude Code": settings env map, `mcpServers`, commands/instructions/skills, session history). milk already had the universal half (env scan, `claude auth status`); the missing half — MCP server and credential import — is where manual setup actually hurts, since MCP entries (command/args/env/scope) are the most verbose thing to retype.

## Decision

### 1. Two commit modes; additive is the default

- **Additive (default):** the wizard's answers merge into the existing config at the chosen scope. The primary agent is upserted by name and made active (`agent`); a chosen Claude Code escalation is set up (`escalation_agent` + a `claude-cli` entry) but a "no" only means "don't set one up" — an existing escalation is never removed; picked tool agents and imported MCP servers are appended merge-by-name. Everything else — MCP servers, routing rules, permissions, memory, OTel, extra agents — is untouched. The save is an ordinary `saveScoped` (ADR-0101's persist guard applies: a config the process failed to load refuses the merge with a pointer to `/config open` or `rebuild`).
- **Rebuild (`/config init rebuild`, alias `reinit`; `milk config init --rebuild`):** the pre-additive behaviour, kept deliberately — a from-scratch config is the right tool after a botched hand-edit or a provider switch, and the forced save remains ADR-0101's one documented recovery path after a failed load. It now **says what it did**: the banner announces the mode before the first question, and the completion summary lists the previous entries the rebuild reset (with `config.json.bak` named for the global scope; the local scope is called out as backup-less).

A deliberate asymmetry: the additive merge records what it *kept* ("kept your existing settings (N MCP servers, M agents, …)"), the rebuild records what it *reset*. Neither is silent.

### 2. Harness import: read-only scan, explicit pick, origin-tagged

New `cmd/milk/harness_import.go` reads other harnesses' config files and offers two kinds of adoptable content (nothing is ever written to the harness's files; nothing is copied without a pick):

- **MCP servers** — Claude Code's `mcpServers` maps (`~/.claude.json` including the per-project map, `~/.claude/settings*.json`, `<project>/.claude/settings*.json`) and Claude Desktop's `claude_desktop_config.json`. Converted 1:1 (`command`/`args`/`env`/`cwd` → stdio; `url`/`type`/`headers` → http/sse), persisted with a new `origin` field naming the source file, and assigned to the primary and escalation agents. An existing server with the same name is **never overwritten** (skipped and reported). Same-name sources dedupe most-specific-first (project settings beat user settings beat `~/.claude.json` beat Desktop).
- **Provider credentials** — the `env` maps of Claude's settings files, mapped onto the wizard's existing key sources (`OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, …). The finding references the env var when it is exported (`api_key_env` pointer — the established "secrets never enter the config as values" posture); only when the key exists *just* in the file does the finding carry the literal, and then the confirmation says so explicitly ("copied from `~/.claude/settings.json` into config.json (same as typing it)"). `ANTHROPIC_BASE_URL`/`ANTHROPIC_MODEL` reshape the Anthropic finding — the relay/proxy setup that key is usually paired with. Display is always a masked fingerprint.

Two entry points over the same scan: the wizard (credentials join the "I looked around" first question; MCP servers get their own step after the escalation question — blank imports nothing, the pick must be explicit) and the standalone `/mcp import [all|name,…]` (TUI + `milk config mcp import`; ACP reaches it through the wizard's import step).

## Consequences

- Re-running `/config init` is now genuinely safe on a configured machine, and the docs say exactly that. Existing automation that *relied* on init wiping the config must switch to `rebuild` — the completion summary and the banner make the mode visible either way.
- The additive merge writes the merged config to the chosen scope, like every other config save in milk (`/mcp add`, `/agent add`, …) — a local overlay written this way materialises global values, the same pre-existing trade-off as any `/mcp add --local`; empty-overlay sparseness (ADR-0045's inheritance) is preserved for `--empty` bootstraps, which remain untouched.
- `config.LoadMergedIn` at commit time is the merge base (not the process's live snapshot): the wizard is also a recovery surface, and disk is the truth. A corrupt file yields a pointed error (`/config open` or `rebuild`) instead of a merge.
- Origin is informational only (shown in `/mcp list`); it never affects resolution. If a future import source needs provenance-aware behaviour (e.g. re-sync), that is a new decision.
- Scope is honest about what is *not* imported: session history (milk's sessions are state-heavy, low reuse), other harnesses' agent/routing configs (formats don't map), and rules/instructions (already covered by `AGENTS.md`/`CLAUDE.md` loading) are out.
