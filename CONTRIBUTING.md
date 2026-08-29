# Contributing to milk

Issues and pull requests are welcome contributions — bug reports, feature requests, docs fixes, and code changes alike.

## Reporting issues

Open a GitHub issue with:

- What you expected vs. what happened
- Your `~/.milk/config.json` agent setup (redact any tokens/keys)
- Relevant transcript or log output, if applicable

## Submitting a pull request

1. Fork the repo and create a branch off `main` (see [docs/branching-strategy.md](docs/branching-strategy.md) for naming conventions: `feat/<scope>`, `fix/<scope>`, `chore/<scope>`, `docs/<scope>`).
2. Make your change. Keep commits scoped and use [Conventional Commits](https://www.conventionalcommits.org/) format:
   ```
   feat(router): add weighted signal for tool-call density
   fix(session): repair corrupted index on load
   ```
3. Run the test suite and build before opening the PR:
   ```
   go build ./...
   go test ./...
   ```
4. If your change affects a documented behavior (routing, config, session states, etc.), update the relevant file under `docs/` or `CLAUDE.md`.
5. Open the PR against `main`. Describe what changed and why; link any related issue.

`main` is protected — no direct commits. All changes land via PR.

## Architecture decisions

Non-trivial design choices are recorded as ADRs under [docs/adr/](docs/adr/). If your PR makes an architectural call worth remembering, add one (one file per ADR, `NNNN-kebab-title.md`).

## Where to look first

- [docs/getting-started.md](docs/getting-started.md) — fastest path to a working setup
- [docs/providers.md](docs/providers.md) — agent/provider configuration
- [docs/workflows.md](docs/workflows.md) — routing, sticky escalation, the native `/workflow` engine
- [docs/tooling.md](docs/tooling.md) — built-in tools, agent-as-tool, MCP servers
- [docs/operations.md](docs/operations.md) — memory, observability, loop detection, remote oversight
- [docs/eval.md](docs/eval.md) — evaluation harness
- [docs/spec.md](docs/spec.md) — architecture and CLI reference
- [CLAUDE.md](CLAUDE.md) — project structure and key design decisions

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
