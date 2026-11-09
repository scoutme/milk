# ADR-0104: Agent Plugins — package format, loading, and distribution

- **Status:** Accepted
- **Date:** 2026-11-08

## Context

milk has MCP servers (config-declared) and no skills. The [Agent Plugins
Specification v1.0.0](https://agent-plugins.org/specification) standardizes
directory-based plugin packages — `plugin.json` manifest, `skills/*/SKILL.md`
(Agent Skills), `mcp.json` MCP servers — that install by copying a directory
and work across conformant clients. Adopting it gives milk a real plugin
ecosystem for free and finally lands the skills half of the Phase-5 parking
lot (#252).

The **distribution** half has no standard: the spec states explicitly that it
"does not prescribe: installation sources, registries, or marketplaces" —
installation is client-owned. The ecosystem answers that freedom with one
de-facto shape (evidence: the MiniCut plugin ships five parallel layouts,
one per harness):

- **Claude Code**: `.claude-plugin/marketplace.json` catalogs plugins with
  per-entry `source` refs (relative path, `github`, `git-subdir`, `url`,
  `archive` zip, `npm`, `command`); UX is `marketplace add` + install by
  `name@marketplace`.
- **Gemini CLI**: `gemini-extension.json` bundles (prompts, MCP, commands,
  themes, hooks, sub-agents, skills); UX is install-by-GitHub-URL plus a
  curated gallery.
- **OpenAI Codex / Cursor**: per-client manifests (`<.harness>-plugin/plugin.json`)
  with Claude-shaped fields; repo-based install.
- **DeepSeek Harness**: plugin-as-code (an npm module patching the harness
  event framework) — the outlier.
- **MCP-first harnesses**: plugins ≈ MCP server + instructions; the MCP
  Registry `server.json` (npm naming `io.github.<owner>/<repo>`) is their
  standard.

Convergent patterns: git repo (or zip/npm tarball) as the distribution unit,
install by URL/slug, a machine-readable catalog paired with `add`/`install`
commands, and nobody makes the user run git themselves.

## Decision

milk is a **conformant Agent Plugins v1 client** (`internal/plugin`) and adds
a **client-owned distribution layer** that reuses the ecosystem's de-facto
catalog interchange. The theme is one pipeline: *catalog → fetch → shim if
needed → load*.

### Part I — Loading & conformance (the portable core)

Plugins load from two roots at startup:

- **user:** `<state root>/plugins/` (~/.milk/plugins)
- **project:** `<cwd>/.milk/plugins/` (repo-local, like the rest of `.milk`)

`PLUGIN_DATA` (spec §9.1) is `<state root>/plugin-data/<plugin name>`,
created when a stdio server launches and preserved across updates.

| Spec | milk |
| --- | --- |
| manifest validation (§5, closed schema) | `internal/plugin/manifest.go` — unknown fields report+ignore, every other violation rejects the plugin |
| package containment (§4.1) | `paths.go` — `./`-relative paths resolved and symlink-checked against the plugin root, narrowest failure boundary ladder |
| skills (§7.1 → Agent Skills spec) | `skills.go` — `skills/*/SKILL.md`, immediate children only, frontmatter validated (name/description constraints), invalid skills skipped with a report |
| MCP `mcp.json` (§7.2) | `mcp.go` — closed per-entry union (stdio / streamable-http / sse), single-token `command`, cwd forms, reserved env names |
| placeholder expansion (§9.2) | `expand.go` — single-pass `${PLUGIN_ROOT}`/`${PLUGIN_DATA}` in args/env-values/cwd only, replacement text never rescanned |
| reserved env (§9.1) | `internal/mcp.env` — `PLUGIN_ROOT`/`PLUGIN_DATA` set after the configured env overlay, replacing same-name entries |
| failure isolation (§11.3) | loader boundaries: manifest fatal rejects the plugin; a broken component location disables only that component type; a broken skill/server entry skips only that entry |
| unsupported transports (§11.3 rule 4) | legacy `sse` servers are skipped with a report (milk connects Streamable HTTP); `streamable-http` maps to milk's `http` transport |

**Client extension namespace** — `dev.milkai.milk` (the domain milk controls):

```json
"extensions": { "dev.milkai.milk": { "mcp_assign": ["primary"], "disable_skills": true } }
```

- `mcp_assign`: agent names that receive the plugin's MCP servers (default:
  every agent). Names resolve like `AgentConfig.MCPServers` refs.
- `disable_skills`: don't surface this plugin's skills in the `skill` tool.

**How components reach milk:**

- **MCP servers** go through a **runtime seam**, `config.SetPluginMCPServers` —
  `EffectiveMCPServers` appends plugin servers after the agent's own refs.
  They are *never persisted*: config writes would otherwise fossilize
  plugin-derived entries into `config.json`. Names are namespaced
  `plugin.<plugin>.<server>` (collision-proof).
- **Skills** feed a `skill` tool (`internal/agent/local/skill.go`) using
  progressive disclosure exactly as the Agent Skills spec prescribes: the tool
  description carries **name + description lines only**; `skill(name)` returns
  the SKILL.md body (re-read from disk at activation, 64 KB cap). The tool is
  omitted entirely when no skills are installed (zero token cost), and it is
  a base tool, so `included_tools`/`excluded_tools` apply.
- Stdio servers run with `cwd` (default: plugin root) and the configured env
  overlay plus the reserved variables — through milk's own MCP client. The
  claude-cli/smolagent native-MCP paths pass command/args/env to their config
  generators; `cwd` is honored by milk's client only.

### Part II — Distribution: familiar catalogs, no git required

**Interchange format.** milk reads the ecosystem's de-facto marketplace
manifest — Claude Code's **`marketplace.json`** — from
`.claude-plugin/marketplace.json`, falling back to a root `marketplace.json`
(the portable spelling). This is conformant by the spec's own division of
responsibility (installation is client-owned) and maximally familiar: the
catalogs authors already publish are the catalogs milk consumes. Marketplace
*hosting* stays outside milk (push the catalog repo anywhere).

**Install UX.** Same gestures as the ecosystem, one new default:

```
milk plugins add <owner/repo | git-url | https://…/pkg.zip | npm:pkg>
milk plugins marketplace add <owner/repo | path>    # register a catalog
milk plugins install <name>[@<marketplace>]
milk plugins list · details <name> · update [name] · remove <name>
milk plugins shim <dir>                             # write portable manifests
```

- `add <slug>` is the familiar one-liner (Gemini's UX): treats the target as
  an implicit single-plugin marketplace — fetch, shim if needed, install.
- Install ids are `name@marketplace` (Claude's convention); bare `name`
  resolves when unambiguous.
- `<marketplace>` resolves on **both keys**: the catalog's own `name` (what
  published docs type — `minicut@minicut`) and the snapshot directory
  `marketplace add` created (the source's repo basename) — the two axes are
  named differently and both must land on the same snapshot. An unknown key
  lists what's registered; same-name collisions name every candidate.

**No-clone fetching (milk's one deliberate divergence).** Distribution units
are fetched over plain HTTPS — a git binary is never invoked and no history
is downloaded. Per `marketplace.json` source type:

| source | fetch |
| --- | --- |
| relative path (`"./"`, `./plugins/x`) | one download of the marketplace snapshot, extract the subtree |
| `github` (`owner/repo`, optional `ref`/`sha`) | GitHub tarball/codeload archive endpoint, pinned ref honored |
| `git-subdir` (`url` + `path`) | archive endpoint of that repo, extract `path/` only |
| `url` (any git URL) | host's archive endpoint where one exists; otherwise rejected with a named error |
| `archive` (zip over HTTPS) | straight download + extraction filter |
| `npm` | registry tarball |
| `command` | **rejected** (executes arbitrary code at install time; revisit only with a consent story) |

An **extraction filter** keeps only what a package needs —
`plugin.json`, `mcp.json`, `skills/**`, `mcp/**`, `scripts/**`, `LICENSE`
plus files a SKILL.md body references by relative path — and reports
everything skipped. (MiniCut: 34 archive files → 8 kept.)

**Shim generation (compatibility bridge).** When an install target lacks a
portable root `plugin.json`, `shim` synthesizes it (and `mcp.json` from
Claude-form `mcpServers`/`mcp/server.json`), mapping name/description/version
through. `add`/`install` do this automatically and record it in the install
bookkeeping; `milk plugins shim <dir>` does it in place for hand-managed
trees. The shim only ever *creates missing manifests* — it never overwrites
portable files.

**Bookkeeping & lifecycle.** Installs are recorded in
`<state root>/plugin-installs.json` per plugin: marketplace id, source spec,
resolved ref/sha, installed version, shim flag. Installed trees live in the
user plugin root (`~/.milk/plugins/<name>`); they are **copies** — removing a
marketplace never removes its plugins. `update` re-resolves the source (honoring
`ref`/`sha` pins), replaces the tree atomically (tmp + rename) and preserves
`PLUGIN_DATA`; `remove` deletes the tree and the record. Project-level
`.milk/plugins/` stays drop-in-only (no bookkeeping) — that is the hand-made
path, and `git clone` there remains fully supported.

**Two faces, one action layer.** Every action is a single function: the
`milk plugins …` CLI prints its report, and the TUI's `/plugins …` suite
runs the same function off the UI loop (fetches are network-bound), delivers
the report to the transcript, and — after a mutation — re-runs discovery in
place, so a plugin installed from the TUI is live immediately (skill catalog,
MCP seam, registry swap — no restart). The hosts execute one implementation
and cannot drift; a second operation is refused while one is in flight.
**Bare `/plugins` is the browse face of the same layer**: it prints the
installed view as the record and opens the palette-style flyover over every
registered catalog's entries (↑↓/type-to-filter, Enter dispatches
`install <name>@<marketplace>` through the suite); labels disambiguate to
`name@dir` across colliding catalogs and the install key is always the
snapshot directory (unique by construction). It deliberately arms **no
timed default** — the bg-agent picker's 60s auto-answer shape would mean
silence could install software, so browsing only ever acts on an explicit
Enter.

**Trust posture.** Installing is trust-by-user (the same trust as editing
`config.json`): stdio MCP servers execute local commands, so every install is
user-initiated and inspectable — no background fetching, no auto-update.
`/plugins` surfaces what a plugin brings (skills, servers, extension keys)
and every loader report; a future ADR-0052-style consent gate is the recorded
escape hatch if plugins ever arrive through unattended paths.

### Part III — Agents: a de-facto directory, read as client behavior

Claude Code plugins can ship subagents as `agents/<name>.md`. The portable
spec's component set deliberately does **not** include them — §3 defines a
Component as "a skill or MCP server entry" — so milk reads `agents/` as
**client behavior** under §4.1 containment: the same posture as reading
`.claude-plugin/` for shimming (one unmodified plugin works across clients),
and the extraction surface keeps the directory next to `skills/`.

The frontmatter is Claude's subagent shape (`name`, `description`, `tools`,
`model`); the body becomes the agent's system prompt; the description becomes
the agent-as-tool schema description.

- **Bare names, config wins.** `code-reviewer` resolves exactly as Claude
  names it. A name colliding with a configured agent (or `claude`) drops the
  plugin copy at the seam with a report — config is authoritative.
- **Transport is inherited.** Plugins ship behavior, never credentials: the
  synthesized entry clones the primary's endpoint/auth/TLS — or the
  configured agent `model:` names, Claude-style; an unknown `model:` warns
  and falls back to the primary. The body becomes `Prompt` **in memory
  only**: the `SetPluginAgents` seam is never persisted (same contract as
  `SetPluginMCPServers`; a config save can never fossilize plugin content).
- **Invocation is agent-as-tool, on by default.** A plugin agent joins the
  resolution set *and* the exposure list — stateless fresh turns, the cycle
  guard, invoker-scoped memory and the retry wrapper all apply unchanged.
  `extensions["dev.milkai.milk"].expose_as_tool: false` keeps the agent
  resolvable (background jobs, workflow roles, completions) without exposing
  it as a tool. The frontmatter `tools:` whitelist maps onto
  `limits.included_tools` through a Claude→milk name table
  (`Read→read_file`, `Grep→grep`, `Glob→find_files`, `Bash→bash`,
  `Write→write_file`, `Edit→edit_file`, `MultiEdit→multiedit`,
  `WebFetch→web_fetch`); unmapped names warn and drop — a whitelist entry
  that resolves to nothing would silently strand the agent. An absent
  `tools:` means no restriction (full default tool set).
- **Surfaces**: `/agent list` marks entries `(plugin)` (read-only — edits
  belong to the plugin, and a config entry with the same name shadows it);
  completions, the bg-agent picker and workflow roles all offer them; the
  startup availability probe needs no extra ping (the inherited endpoint was
  already probed with the primary).

### Part IV — Commands: the second de-facto component type

Claude Code plugins can ship custom slash commands as `commands/<name>.md`
(`description`, `argument-hint`, `allowed-tools` frontmatter + a prompt
template body). Same conformance posture as Part III: commands are not a
portable component type, so milk reads `commands/` as **client behavior**
under §4.1 — immediate children only (nested `commands/git/commit.md` warns
and skips, the skills rule), frontmatter-absent files are prose (silent),
and the extraction surface keeps the directory next to `agents/`.

- **Registration is dual-store, never a shared-slice mutation.** Specs live
  in a runtime store under `pluginsMu`; `specFor`, `registryNames` (the
  recognition set — the old `slashCommands` init-var became a per-read
  `registryNames()`, so completion and `extractSlashCommand` see installs
  live) and `buildInteractiveHelp` (a final live `── Plugins ──` block; /help
  renders a fresh build) consult static ∪ store. `commandRegistry` itself is
  never written — ACP goroutines reading the static table race nothing.
- **Collisions**: built-ins always win; first plugin wins among themselves —
  each drop reports into `/plugins`.
- **Dispatch** is the host-neutral default branch of `handleSlashCommand`:
  the built prompt returns through the existing `dispatch` seam — TUI
  `dispatchAgent`, ACP `viaTUI` (plugin specs join `acpCommandTable`, so
  advertising and dispatch ride the same store) — and busy input queues as a
  raw slash line upstream, exactly like every built-in command.
- **Prompt building** (`Command.BuildPrompt`): `$ARGUMENTS` substitutes (or
  the arguments append when the template never references it), then Claude's
  dynamic-context `!`…`` snippets — executed **only** when the snippet
  matches the command's own `Bash(…)` allowed-tools prefix **and** contains
  no shell metacharacters and no write/exec-class flag (--output, --exec,
  --upload-pack, --receive-pack, --separate-git-dir, --template). The `:*`
  command form of Bash(…) matches at word boundaries (Bash(git diff:*)
  never authorizes `git difffoo`); the bare-`*` form is the author's
  explicit fuzzy stem and keeps string-prefix semantics. Everything else —
  undeclared, metachar'd, flag'd, failed, timed out (5 s, 8 KB output
  cap) — stays in the prompt **verbatim** for the model to run with its
  own tools. **Honest scope (review W1)**: these cuts stop command chaining
  and the obvious smuggling shapes; they cannot make a declared whitelist
  non-destructive (`git config --global …` fits `Bash(git:*)`). The consent
  boundary is the author's declaration plus the user's decision to install
  the plugin — milk does not sandbox snippets and does not claim to.
- **Surfaces**: help block, tab completion (hint = `argument-hint`), ACP
  advertising with the derived-or-frontmatter description, `/plugins` detail
  counts, and hot reload with the rest of the store.
- Deliberately out: nested command directories, `$1`-style positional args
  (only `$ARGUMENTS`), and Claude's `allowed-tools` affecting the model
  mid-turn (v1 scopes it to expansion; a command runs as a normal user turn
  under the session's own permission rules).

## Consequences

- Three install paths, one loader: **drop a directory** (hand-made, project
  or user), **`shim` a tree**, or **`add`/`install` from a catalog** — all
  converge on a conformant plugin directory, so Part I never changes.
- No git dependency anywhere in the install path; the only network verbs are
  HTTPS GETs to archive/registry endpoints, honoring `ref`/`sha` pins.
- milk consumes Claude-format marketplaces without speaking Claude: the
  format is an interchange, the runtime is 100% Agent Plugins. If a neutral
  marketplace format emerges (the spec TSC spans Amazon, Cursor, Microsoft,
  OpenAI, Vercel), accepting it is a new fetcher, not a redesign.
- Plugins load at startup; the suite's install/update/remove actions
  **hot-reload** the set in place (the shared action layer re-runs discovery
  after a mutation, on both hosts). Hand edits to a plugin's
  `manifest`/`mcp.json` on disk still take effect on restart (no filesystem
  watcher); skill bodies are re-read per activation.
  Distribution commands (`add`, `install`, `update`, `remove`) mutate the
  plugin roots and are effective on the next restart.
- Plugins can ship **agents** (`agents/*.md`, the ecosystem's de-facto
  subagent format) — synthesized config entries behind a never-persisted
  seam, exposed as agent-as-tool by default. The portable core still claims
  only skills and MCP (§3), so conformance is unchanged. Agent prompt edits
  land on restart, and a tool-agent runner caches its built prompt at first
  call — a mid-session `/plugins` reload refreshes resolution, not runners
  already cached for that session.
- milk claims conformance for **both** component types with **stdio +
  streamable-http** MCP transports; `sse` is the only declared-but-unsupported
  piece, skipped per spec §11.3 rule 4.
- Out of scope, deliberately: marketplace hosting/publishing, auto-update
  policy, `command` sources, hooks/commands/themes beyond skills + MCP, and
  extension namespaces other than `dev.milkai.milk` — each gets its own ADR
  if ever pulled.
