# Security Policy

## Supported versions

milk is under active development on `main`. Security fixes are made against the latest commit; there are no maintained release branches yet.

## Reporting a vulnerability

Please do not open a public GitHub issue for security vulnerabilities.

Instead, report privately via [GitHub Security Advisories](https://github.com/scoutme/milk/security/advisories/new), or email scoutme@gmail.com.

Include:

- A description of the vulnerability and its potential impact
- Steps to reproduce (proof-of-concept if possible)
- Affected version/commit

We'll acknowledge your report as soon as possible and follow up with next steps once the issue is assessed.

## Scope notes

milk executes local tools (bash, file read/write/edit) on behalf of configured agents, and can shell out to subprocess agents (Claude CLI, aider, smolagents) and remote inference providers over HTTP/SigV4/Bearer auth. Credentials and tokens live in `~/.milk/config.json` on disk in plaintext — treat that file as sensitive. Reports involving credential handling, subprocess/tool execution boundaries, or auth transports (SigV4, Bearer, token_cmd) are especially welcome.
