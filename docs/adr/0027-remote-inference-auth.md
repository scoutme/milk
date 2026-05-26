# 27. Remote Inference Authentication Transports

Date: 2026-05-25
Status: accepted

## Context

The local agent was originally hardwired to a local llama.cpp instance (no auth, plain HTTP). As inference cloud providers (AWS Bedrock, OpenRouter, Together.ai, Groq, …) became viable targets for the local agent slot, several auth models need to be supported:

1. **No auth** — local/plain (unchanged)
2. **Bearer token** — `Authorization: Bearer <key>` (OpenRouter, Together.ai, Groq, …)
3. **AWS SigV4** — per-request HMAC signing (Bedrock)
4. **Custom headers** — arbitrary key/value headers layered on top of any of the above
5. **Dynamic tokens** — Bearer token sourced from a CLI command (short-lived tokens, CLI-managed auth)

In addition, private/on-prem endpoints (Azure proxies, corporate gateways) may use self-signed or private CA TLS certificates, requiring configurable TLS trust.

## Decision

### Transport stack

Use `http.RoundTripper` composition instead of a monolithic HTTP client. Three transport types are defined in `transport.go`:

- `headerTransport` — injects a static `map[string]string` on every request; wraps any inner transport
- `sigv4Transport` — signs each request with AWS Signature Version 4 before forwarding to the inner transport
- `tokenCmdTransport` — runs a shell command to obtain a Bearer token; caches it and re-runs on 401/403

`NewFromConfig` in `local.go` builds the transport stack bottom-up:
1. Base transport: `http.DefaultTransport`, or a custom `*http.Transport` with `TLSClientConfig` when TLS overrides are set (`buildBaseTransport`)
2. `sigv4Transport` wrapper when `provider = "bedrock"`
3. `tokenCmdTransport` wrapper when `token_cmd` is non-empty (takes precedence over static `api_key`)
4. `headerTransport` wrapper when `headers` is non-empty or `api_key` is set

### AWS credential resolution

For Bedrock, credentials are resolved in order: explicit `aws_key_id` / `aws_secret` / `aws_token` config fields → `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` / `AWS_REGION` / `AWS_DEFAULT_REGION` env vars → region parsed from the URL hostname when `aws_region` is empty. This mirrors the AWS SDK default chain without pulling in the SDK as a dependency.

### TLS overrides

Two config fields per `local_agents` entry:
- `tls_skip_verify` — disables cert verification (self-signed dev endpoints only)
- `tls_ca_cert` — path to a PEM CA file for private CA trust

When neither is set, `http.DefaultTransport` is used unchanged.

### Azure OpenAI workaround

Azure OpenAI is OpenAI-compatible but uses a non-standard URL path
(`https://<resource>.openai.azure.com/openai/deployments/<deployment>/chat/completions?api-version=<ver>`)
and an `api-key` header instead of `Authorization: Bearer`.

Until a dedicated `azure` provider with URL templating is implemented (tracked in GitHub Issues), the workaround is:

```json
{
  "name": "azure-example",
  "url": "https://<resource>.openai.azure.com/openai/deployments/<deployment>/chat/completions?api-version=2024-02-01",
  "model": "<deployment-name>",
  "headers": { "api-key": "<your-api-key>" }
}
```

Leave `provider` unset (defaults to no Bearer injection). The `api-key` header is injected by `headerTransport`.

### Chat path override

Some servers (enterprise proxies, non-standard deployments) expose `/chat/completions` without the `/v1` prefix. The `chat_path` field in a `local_agents` entry overrides the default `/v1/chat/completions` path.

## Config fields (per `local_agents` entry)

| Field | Type | Default | Purpose |
|---|---|---|---|
| `provider` | string | `""` | `"bedrock"` = SigV4; anything else = Bearer via `api_key` / `token_cmd` |
| `api_key` | string | `""` | Static Bearer token (non-Bedrock providers); superseded by `token_cmd` |
| `token_cmd` | string | `""` | Shell command whose stdout is the Bearer token; re-run on 401/403 |
| `chat_path` | string | `""` | Override inference endpoint path (default `/v1/chat/completions`) |
| `headers` | object | `{}` | Arbitrary extra headers (Azure `api-key`, OpenRouter `HTTP-Referer`, …) |
| `aws_region` | string | `""` | AWS region for Bedrock (fallback: env var, then parsed from URL) |
| `aws_key_id` | string | `""` | AWS access key ID (overrides env var) |
| `aws_secret` | string | `""` | AWS secret access key (overrides env var) |
| `aws_token` | string | `""` | AWS session token (optional) |
| `aws_service` | string | `"bedrock"` | AWS service name for SigV4 scope |
| `tls_skip_verify` | bool | `false` | Disable TLS cert verification |
| `tls_ca_cert` | string | `""` | Path to PEM CA cert for private CA trust |

## Consequences

- Any OpenAI-compatible cloud provider can now be used as the local agent slot with no code changes — only config
- Bedrock credential rotation via env vars (e.g. ECS task roles, instance profiles) works without config changes
- Azure is functional via workaround but requires a manual full-URL; a cleaner `azure` provider is a follow-up
- `sigv4Transport` is a minimal hand-rolled implementation; it does not implement the full AWS SDK credential chain (no IMDS, no profile files, no assume-role) — good enough for explicit credentials and env vars
- `tokenCmdTransport` supports any CLI-based auth flow (GitHub OAuth, Vault tokens, cloud CLI tools) with automatic retry on token expiry
