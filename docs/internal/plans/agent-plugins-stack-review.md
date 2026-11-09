# Agent-plugins stack — code review + fix plan

Scope: the uncommitted tree at review time — the Agent Plugins theme (Parts II–IV:
distribution, plugin agents, plugin commands) plus the focus/Esc keyboard batch —
47 files (28 modified +875/−91, 19 new). Review date: 2026-11-09.
This file is the verdict of the comprehensive review and the plan to clear it.

Status legend: [ ] not started · [~] in progress · [x] done.

---

## Part 1 — Findings

### 🔴 Critical

**C1 — relative-source path traversal escapes the marketplace snapshot**
(`cmd/milk/plugins_cmd.go:450` `installFromEntry`, `:476` `updateInstalledPlugin`;
`internal/plugin/source.go:171` `ParseMarketplaceSource`)

`filepath.Join(snapDir, TrimPrefix(ep.Source.Path, "./"))` is uncontained. The
marketplace `source` string form accepts `"../x"` (the `git-subdir` object form
rejects `..` at source.go:177 — the relative form has no such check), so a
crafted `marketplace.json` entry can point anywhere on the host
(`../../.milk/plugins/other-plugin`, `../../../…`) and `copyTree` walks it into
the install staging. `TrimPrefix("./")` doesn't neutralize `././../`. This breaks
the §4.1 containment contract the loader enforces everywhere else.

Fix plan:
- [ ] Reject `..` segments and absolute paths in `ParseMarketplaceSource`'s
      relative branch (mirror the git-subdir rule; also catch `.`/empty).
- [ ] Contain at both call sites: joined path must resolve under `snapDir`
      (filepath.Rel → reject `..` prefix), or route through the existing
      `resolveWithin`-style helper.
- [ ] Red test first: fixture marketplace entry `{"source": "../escape"}` +
      `{"source": "./x/../../escape"}` → install refuses with a named error;
      `update` refuses a tampered `rec.Path` likewise.
- Files: `internal/plugin/source.go`, `cmd/milk/plugins_cmd.go`,
  `internal/plugin/install_flow_test.go` (or `plugins_resolve_test.go`).

### 🟡 Warnings

**W1 — `!`…`` expansion: "no metacharacters" ≠ safe (flag smuggling + prefix without boundary)**
(`internal/plugin/commands.go:185-190` exec, `:207` `snippetAllowed`, `:225` `hasShellMetachars`)

The metachar check blocks command chaining but not flag *effects*: a command with
`Bash(git diff:*)` can declare `` !`git diff --output=~/.bashrc` `` (writes a
file), `Bash(git:*)` can declare `git config --global alias.x '!evil'` (persisted
code execution). Also `strings.HasPrefix(snippet, pattern)` has no word boundary —
`Bash(git diff:*)` authorizes `git difffoo`.

Fix plan:
- [ ] Word-boundary match: `snippet == pattern || strings.HasPrefix(snippet, pattern+" ")`.
- [ ] Decide and document the flag-hazard stance (ADR-0104 Part IV): either
      block known write-y flags (`--output`, `--exec`, `config --global`) or state
      plainly that the author's whitelist is the consent boundary and may be
      destructive. The current code comment overstates safety — align it either way.
- [ ] Red→green tests: `git diff --output=x` case (per chosen stance),
      `git difffoo` rejected, `git diff extra args` still accepted.

**W2 — no HTTP timeout on the action path; a hung fetch wedges `/plugins` forever**
(`cmd/milk/plugins_cmd.go:59,255,453,479`: `context.Background()` + `http.DefaultClient`)

`FetchTo`'s 2-minute default applies only when `client == nil`. With
`http.DefaultClient` a stalled peer keeps the goroutine alive and `pluginsBusy`
latched permanently ("another operation is in flight" on every later action; the
completion `pluginsActionMsg` never arrives).

Fix plan:
- [ ] One shared `&http.Client{Timeout: 2 * time.Minute}` in the action layer
      (or `context.WithTimeout` per action); same for the marketplace add path.
- [ ] Optional hardening: a "still working…" re-announcement or Ctrl+C latch
      escape after N minutes.
- [ ] Test: unreachable/hanging listener (accept-no-reply) → action returns an
      error, `pluginsBusy` clears.

**W3 — commands/*.md frontmatter: YAML list form silently yields zero allowed tools**
(`internal/plugin/commands.go:96-118` line-based parser)

`allowed-tools:` with list items (`- Bash(git diff:*)`) parses to no tools → no
expansion, no warning. `agents.go` already uses `yaml.v3` with scalar-or-list —
commands should match.

Fix plan:
- [ ] Parse command frontmatter with `yaml.v3` (reuse the agents shape:
      `Tools any`), keep report-and-continue on unknown keys.
- [ ] Test: list-form `allowed-tools` expands; malformed frontmatter still
      skips-with-report.

**W4 — install replacement is not atomic; failure can destroy the old install**
(`internal/plugin/installs.go:162-169`)

`os.RemoveAll(target)` then `os.Rename(staging, target)`: rename failure leaves
nothing; `in.Save()` failure after the rename reports "failed" though the plugin
landed without a record.

Fix plan:
- [ ] Rename-aside swap: old → `.trash-<pid>`, staging → target, then remove
      trash (failure restores the old dir).
- [ ] Distinguish "installed but bookkeeping failed" from a real failure in the
      returned error/report.
- [ ] Test: injected rename/save failure leaves the previous install intact.

**W5 — `marketplace add` directory naming collides silently; URL sources name badly**
(`cmd/milk/plugins_cmd.go:249-252`)

`filepath.Base(src.Repo+src.URL)`: two same-basename repos map to one dir and
`_ = os.RemoveAll(dest)` destroys the earlier catalog; URL sources produce dirs
like `plugin.zip`.

Fix plan:
- [ ] Slug from full ref (`owner-repo` for github; host+path for URL; package
      for npm), and refuse (or rename with a suffix) on an existing foreign dir
      instead of deleting.
- [ ] Test: second `marketplace add` with a colliding basename does not wipe
      the first; URL source gets a sane directory name.

### 🟢 Suggestions

**S1 — tar extraction re-decompresses per kept entry** (`fetch.go:283,357`):
`eachTarOne` runs a full gzip pass per kept file (k× decompress; pathological for
the documented "big assets" case). One pass materializing kept entries (or a
name→offset map) fixes it; the zip path is already single-pass. Also
`io.Copy(LimitReader(rc, size+1))` (:300) silently truncates if a crafted entry
misreports size — compare written vs claimed and fail loud.

**S2 — substring host matching / weak shorthand** (`fetch.go:91,96`;
`source.go:88`): parse URLs with `net/url`, compare `Host` exactly (require
https), and reject `..` segments in `repoShorthand` (`a../b` currently matches).

**S3 — `firstBodyLine` byte truncation** (`commands.go:74`): `t[:69]` can split a
UTF-8 rune; truncate at a rune boundary.

**S4 — `plugins_cmd.go` is 874 lines** mixing cobra face, TUI face, resolution
and the browse flyover; `handlePluginsCmd` exists as both a model method and a
package-level view function (called at :520) — split
(`plugins_cli.go` / `plugins_tui.go`) and rename the renderer.

**S5 — `registryNames()` per keystroke** (completion.go:580,1058) allocates +
takes the plugin-store RLock per keystroke; a snapshot cached at
`setPluginCommands` would cost nothing. Negligible as-is.

**S6 — test gaps map exactly to C1/W2/W3**: no relative-`..` escape fixture, no
hung-fetch/latch test, no YAML-list command fixture. Add with the fixes.

**S7 — docs**: ADR-0104 + tooling/CLAUDE/operations match the code; only W1's
"consent" wording needs the honesty pass above.

---

## Part 2 — Fix plan (execution order)

Phase 1 — PR blockers (one PR or one commit series before the stack ships):
1. C1 containment: parser rejection + call-site containment + red→green tests.
2. W2 timeout: shared bounded client + latch-clearing test.
3. W1 expansion safety: word-boundary match + flag-hazard decision (ADR wording)
   + tests.
4. W3 yaml frontmatter for commands + test.
5. W4 atomic swap + honest bookkeeping error + test.
6. W5 marketplace naming + collision test.

Phase 2 — hardening (follow-up PR, can trail):
7. S1 single-pass tar extraction + size-verify.
8. S2 URL parse + shorthand tightening.
9. S3 rune-safe truncation.
10. S6's remaining fixtures if not folded into Phase 1.

Phase 3 — optional cleanup:
11. S4 file split / rename, S5 cached registryNames snapshot.

Verification contract per fix: red→green proof for the regression test, full
suite + `-race` on `cmd/milk` + `internal/plugin`, gofmt/vet clean.

PR packaging (unchanged from the pre-review plan): split by concern —
`feat(plugin)` (the theme) and `fix(tui)` (focus/Esc batch) — with Phase 1
folded into the `feat(plugin)` branch before it goes up.


---

## Progress log

- [x] **C1** — `ee9b67b`: parse-time validation (before the owner/repo shorthand can
  claim `../x`) + `plugin.ResolveRelative` at both install joins. Red→green on
  both levels; the red run also exposed the shorthand-swallow (`./beta` parsed
  as GitHub repo).
- [x] **W1** — `25bd781`: **refined from the plan** — the word-boundary rule now
  applies to the `:*` command form only; the bare-`*` form keeps string-prefix
  semantics (Claude's explicit fuzzy stem — `Bash(echo ON*)` legitimately
  expands `echo ONE`, pinned). Dangerous-flag deny-list + honest-scope pass on
  code + ADR. Red→green with the pre-fix matcher executing both hazards.
- [x] **W2** — `fd0d7ea`: shared `pluginsHTTP` (2 min) at all four call sites,
  DefaultClient deliberately unbounded in the test so the mechanism is what's
  proven. Red captured the bug in miniature: the first run hung the test
  harness until the fixture's handler was made Close-aware.
- [x] **W3** — `baf4954`: yaml.v3 frontmatter (scalar + list `allowed-tools`,
  `[scope]` hint round-trip), invalid YAML warns-and-skips. Red reproduced the
  review's exact complaint (`allowed-tools = []`).
- [x] **W4** — `e29e309`: rename-aside swap (restore-on-failure), distinct
  "installed …, but bookkeeping failed" error, plugin half kept + returned.
- [x] **W5** — `0e798dd`: per-source snapshot names (`owner-repo` / host-base),
  refuse-before-fetch. **Deviation from "refuse/rename", deliberate**: refuse
  only — with unique slugs a collision cannot come from a different source and
  there is no marketplace-update command (re-register = remove first). Existing
  old-name snapshots keep resolving via the dir-alias key.

Phase 1 verified: `internal/plugin` + full `cmd/milk` suites green locally at
each step. Phase 2 (S1–S3 hardening) and Phase 3 (S4–S5 cleanup) are [ ] —
not started.

## Progress log — Phase 2

- [x] **S1** — `68b2bc1`: tar extraction materializes kept payloads in ONE extra
  scan (was k+1 decompressions), and `readBounded` pins the size contract.
  **Review correction, honestly recorded**: the silent-truncation mechanism was
  a phantom — archive readers enforce declared sizes (the red attempt proved a
  smaller-declared header *cuts* the payload at declared, it cannot smuggle
  more), so the detectable classes (payload-beyond-declared, stream-ends-early)
  live at the wrapper layer and are unit-pinned; the security direction that
  matters (no bytes beyond declared) holds at reader level and is now asserted.
- [x] **S2** — `68b2bc1`: `archiveEndpoint` matches `net/url` Host exactly,
  https-only. Red: `http://github.com/…` fetched in the clear,
  `https://evil.com/github.com/…` spoofed GitHub by substring.
- [x] **S3** — `853676e`: rune-safe `firstBodyLine` truncation
  (`strings.ToValidUTF8`). Red: split byte visible in the derived description.

## Progress log — Phase 3

- [x] **S4** — `87139b1`: plugins_cmd.go split by face (shared action layer /
  plugins_cli.go / plugins_tui.go); the package-level `handlePluginsCmd` view
  renderer renamed `renderPluginsView` (the model-method name collision the
  review flagged). Behavior-identical: full cmd/milk suite green.
- [x] **S5** — `c3ec7d5`: immutable `[]cmdSpec` snapshot in an atomic.Value for
  the per-keystroke `registryNames` readers; `setPluginCommands` is the single
  writer. Existing flip tests pin coherence.

All phases complete. Branch: `feat/agent-plugins-stack` — `996e1f7` = the
reviewed PRE state, one commit per fix after it.
