# 98. Eval filesystem isolation: bwrap read-only root with an rw allowlist

- **Status:** accepted
- **Date:** 2026-11-02
- **Decides:** issue [#355](https://github.com/scoutme/milk/issues/355)

## Context

`milk eval` isolated each run in a temporary scenario workdir
(`setupWorkdir`) — and that was all: `cmd.Dir = workdir` and nothing else.
Nothing prevented an agent from writing anywhere on the machine, and nothing
detected it afterwards. During the `routing-reps` experiment (2026-10-28/29)
scoring-run agents **escaped into the live milk checkout** and authored real
work there across repetitions — cross-repetition contamination, one escape
chain even merged as legitimate-looking work (PR #352), detection by manual
tree inspection only.

The issue already rejected the cheap fixes, so they are not re-opened here:
pre/post `git status` guards (one worktree only, operator's own edits
false-positive), prompt preambles (alter the evaluated context — a
confound), tool-trail write tripwires (block legitimate external writes:
binaries, config/state roots, `/tmp` scratch). What remained: **real
OS-level isolation**, recorded as "hard and over-complex" until someone
designed it. This is the design.

Constraints any solution had to satisfy: invisible to the agent's context;
cover *all* writable paths; keep legitimate surfaces (agent binaries, state
roots, seeded configs, `$HOME` config) reachable; cross-platform story;
comparability with unsandboxed baselines.

## Decision

**A bubblewrap backend behind `isolate.mode: off | auto | bwrap` (experiment
YAML) and `--isolate` (CLI), default off.** One mount shape carries the whole
design:

```
bwrap --unshare-user --unshare-pid
      --ro-bind / / --tmpfs /tmp --proc /proc --dev /dev
      --bind <workdir> <workdir> --chdir <workdir>
      --bind <rw> <rw> …
      -- <agent argv>
```

- **The whole filesystem is read-only by default; the rw allowlist is
  explicit.** There is no read-allowlist to maintain — binaries, `~/.config/*`,
  git identity stay *visible* (constraint: legitimate surfaces reachable,
  satisfied by construction). The risk inverts the right way: a missing *read*
  or *write* fails loudly (tool error, caught in first testing), whereas a
  missing *block* would fail silently — so writes are enumerated, reads are not.
- **HOME subtrees are per-run live copies.** `claude-code` reads *and writes*
  `~/.claude`/`~/.claude.json`; `mimo-code` reads and writes `~/.mimocode`/
  `~/.config/mimocode`. At adapter start each is copied to a temp dir and the
  copy is bound **over the live path**: the agent sees byte-identical state
  (comparability with unsandboxed runs holds; constraint 5), every write lands
  in the copy, copies are deleted at stop (freshness mirrors the state-root
  sandbox). Harness-side access resolves through `isoView` — claude's
  transcript and pre-written project settings are read/written *through the
  same mount* the agent uses, or the pre-write would be invisible to it.
- **The network stays shared** (no `--unshare-net`): localhost model servers
  and API access must keep working; the issue is about writes, not traffic.
  Read access to the network is not in scope.
- **`auto` degrades loudly, `bwrap` errors.** Resolution probes a trivial
  `bwrap --ro-bind / / /bin/true` (a PATH hit alone would claim isolation
  where the kernel refuses user namespaces). `auto` without a working bwrap →
  one explicit `WARNING … running WITHOUT filesystem isolation` and a normal
  unsandboxed run (macOS, Windows, restricted WSL); explicit `bwrap` → hard
  error before anything launches. No silent downgrades in either direction.
- **Isolation is a whole-comparison flag**, resolved once per command and
  applied to every adapter (with a note for any non-Isolatable adapter —
  none today): mixing one sandboxed side with one unsandboxed side would
  change what is measured. Default off keeps every existing baseline exactly
  as it was.
- **The acceptance test needs no LLM**: a wrapped `/bin/sh` attempts every
  escape class from the incident (HOME, other checkouts, rootfs) and each
  must fail while workdir, `rw:` and `/tmp` stay writable — the sandbox's
  `/tmp` is a private tmpfs, so scratch writes do not leak out either. CI
  installs bubblewrap on Linux; elsewhere the test skips with a note.

Judge/scoring, results writing and the harness itself stay outside the
sandbox: only agent processes are wrapped.

## Consequences

- During an isolated run, writes to `$HOME` (including every real checkout
  living there), other worktrees/clones and the rootfs fail with EROFS —
  the routing-reps class of incident is structurally impossible, and
  `file_changes` can finally be trusted as complete *for the workdir* while
  the sandbox guarantees nothing else changed either.
- **Linux is the only platform with real containment**; macOS/Windows get
  warn-and-run (`auto`), and the guarantee paragraph in
  `docs/internal/eval-artifacts.md` tells operators to check the stderr line
  before trusting it. If bwrap ever becomes an unwanted dependency, landlock
  (kernel, no helper binary) is the recorded fallback — more subtle (ABI
  matrix, `ENOSYS` paths), which is why it is not first.
- Seed copies cost a few hundred MB of `/tmp` per adapter start (claude
  ~185 MB, mimo ~250 MB) — seconds at most, negligible next to a run; a
  `fresh`-seeding mode (skip copying history the agent doesn't need) is the
  obvious future optimization, deferred until measurements ask for it.
- `isolate:` without `sandbox.mode: fresh` for milk-tui leaves the live
  `~/.milk` read-only — milk starts, writes fail, and the adapter warns once
  at Start; the two knobs stay separate (state isolation vs filesystem
  isolation) because they solve different problems.
- Docker/podman and VMs were considered and remain rejected: image/binary
  mounting plus tmux-inside-container plus host networking to localhost
  models reworks the harness for the same filesystem guarantee.
