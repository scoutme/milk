# 23. AWS Credential Injection for Bedrock-backed Claude Subprocess

Date: 2026-05-22

## Status

Accepted

## Context

milk spawns `claude` as a subprocess, which inherits the parent shell environment. When the user's shell contains stale or conflicting AWS env vars — specifically `AWS_BEARER_TOKEN_BEDROCK` (a pre-signed Bedrock token) and `ANTHROPIC_DEFAULT_*_MODEL` vars pointing to a different AWS account — the Anthropic SDK inside Claude Code treats them as higher-priority auth over `AWS_ACCESS_KEY_ID`, causing 403 errors.

Claude Code's own auth flow reads `awsAuthRefresh` from `~/.claude/settings.json` and runs it to obtain credentials, but only when it considers existing credentials stale. It cannot override env vars already set in the process at startup.

The correct credentials are already available via a `credential_process` command configured in `~/.claude/settings.json` under `awsAuthRefresh`.

## Decision

Add an `aws_auth_refresh: bool` feature flag to `~/.milk/config.json`. When enabled:

- milk reads the `awsAuthRefresh` command from `~/.claude/settings.json` via `claudesettings.AWSAuthRefreshCommand()` before each claude turn.
- Runs the command to obtain fresh AWS credentials (credential_process JSON format).
- Injects them as explicit `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN` env vars into the subprocess.
- Strips the following conflicting vars from the inherited environment: `AWS_BEARER_TOKEN_BEDROCK`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`, `ANTHROPIC_MODEL`, `AWS_PROFILE`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`.

The command is not duplicated in milk config — milk reads it directly from `~/.claude/settings.json` so there is a single source of truth. Credentials are refreshed before each turn (not once at startup) so expiring tokens are handled automatically; the credential_process handles its own caching so this is cheap when the token is still fresh.

## Consequences

- The subprocess always uses the correct Bedrock account credentials regardless of shell environment state.
- No credential duplication: the same `awsAuthRefresh` command is used by both Claude Code and milk.
- Per-turn refresh adds ~10ms overhead (subprocess fork + JSON parse) when credentials are cached; negligible in practice.
- Mid-turn token expiration is already handled by Claude Code's internal 11-attempt retry loop.
- Feature is opt-in; users without Bedrock or without `awsAuthRefresh` in settings are unaffected.
