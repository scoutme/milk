# Provider setup guides

milk supports multiple agent backends. Use `/agent add` in the TUI to register them, `/agent list` to see what's configured, and `/agent switch <name> as primary|escalation` to assign roles.

Each backend is stored as a named entry under `agents` in `~/.milk/config.json`. The active primary agent is set by `agent`; the escalation agent is set by `escalation_agent`.

---

## Claude Code CLI

**Provider**: `claude-cli` — runs the `claude` binary as a subprocess, not via HTTP.

```json
{
  "name": "claude",
  "provider": "claude-cli",
  "bin": "claude"
}
```

A built-in entry named `"claude"` with `provider: "claude-cli"` is always available even if not listed explicitly in `agents`. It is used as the default `escalation_agent`.

| Field | Default | Description |
|---|---|---|
| `bin` | `"claude"` | Path to the `claude` binary |
| `dangerously_skip_permissions` | `false` | Auto-approve all tool uses without prompting |
| `allowed_tools` | — | Tools pre-approved; passed as `--allowedTools` |
| `add_dirs` | — | Extra directories; passed as `--add-dir` |

---

## Local llama.cpp / Ollama / LM Studio

**Auth**: none — plain HTTP.

```json
{
  "name": "local",
  "url": "http://localhost:8080",
  "model": "qwen2.5-coder"
}
```

For Ollama the default port is `11434`; for LM Studio it's `1234`. The model name must match what the server reports (check `/v1/models`).

See [setup.md](setup.md) for the full llama.cpp reference setup.

---

## AWS Bedrock

**Auth**: AWS SigV4. milk uses the native Bedrock Converse API — no OpenAI-compat layer.

### Step 1 — IAM permissions

Your IAM user or role needs the following policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "bedrock:InvokeModel",
      "bedrock:InvokeModelWithResponseStream"
    ],
    "Resource": "arn:aws:bedrock:*::foundation-model/*"
  }]
}
```

If using inference profiles, add the profile ARN to `Resource` or use `"*"`.

### Step 2 — Configure credentials

Credentials are resolved in order:
1. Explicit fields in the agent config (`aws_key_id`, `aws_secret`, `aws_token`)
2. Env vars: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`

For temporary credentials (STS-assumed roles), use `aws_auth_refresh: true` at the config root — milk will refresh credentials automatically before each Claude turn and before local Bedrock calls at startup. See the config reference below.

### Step 3 — Add the backend entry

```json
{
  "name": "haiku",
  "url": "https://bedrock-runtime.eu-central-1.amazonaws.com",
  "model": "anthropic.claude-3-haiku-20240307-v1:0",
  "provider": "bedrock",
  "aws_region": "eu-central-1"
}
```

For cross-region inference profiles, use the profile ARN as the model:

```json
{
  "name": "haiku-profile",
  "url": "https://bedrock-runtime.eu-central-1.amazonaws.com",
  "model": "arn:aws:bedrock:eu-central-1:123456789012:application-inference-profile/abc123",
  "provider": "bedrock",
  "aws_region": "eu-central-1"
}
```

### Step 4 — Verify

```sh
milk --new --primary "say hi in one word"
```

Expected: response from Haiku (or whichever model you configured).

### Troubleshooting

| Error | Cause | Fix |
|---|---|---|
| `403 Credential should be scoped to a valid region` | `aws_region` not set and `AWS_REGION` env var absent | Add `"aws_region"` to the config entry |
| `403 Credential should be scoped to correct service` | Wrong `aws_service` value | Remove `aws_service` or set it to `"bedrock"` |
| `UnknownOperationException` | Wrong URL path or model ARN format | Verify the URL is `bedrock-runtime.<region>.amazonaws.com` with no path suffix |
| `404` on model | Model not enabled in your account | Go to Bedrock console → Model access and enable the model |

---

## OpenRouter

**Auth**: Bearer token. OpenRouter gives access to hundreds of hosted models via a single API key.

### Step 1 — Create an account and get an API key

1. Sign up at [openrouter.ai](https://openrouter.ai)
2. Go to **Keys** → **Create Key**
3. Copy the key (starts with `sk-or-`)

### Step 2 — Add the backend entry

```json
{
  "name": "openrouter",
  "url": "https://openrouter.ai/api",
  "model": "qwen/qwen-2.5-coder-32b-instruct",
  "provider": "bearer",
  "api_key": "sk-or-<your-key>",
  "headers": {
    "HTTP-Referer": "https://github.com/scoutme/milk",
    "X-Title": "milk"
  }
}
```

The `HTTP-Referer` and `X-Title` headers are optional but recommended by OpenRouter for usage attribution.

### Step 3 — Pick a model

Any model that supports tool/function calling works. Good options:

| Model | Notes |
|---|---|
| `qwen/qwen-2.5-coder-32b-instruct` | Strong code model, reliable tool calls |
| `meta-llama/llama-4-maverick` | Fast, good general use |
| `anthropic/claude-haiku-4-5` | Claude as the primary agent |
| `deepseek/deepseek-coder-v2-instruct` | Strong code, competitive pricing |

Full model list: [openrouter.ai/models](https://openrouter.ai/models)

---

## Together.ai

**Auth**: Bearer token.

### Step 1 — Get an API key

1. Sign up at [api.together.xyz](https://api.together.xyz)
2. Go to **Settings** → **API Keys** → **Create**

### Step 2 — Add the backend entry

```json
{
  "name": "together",
  "url": "https://api.together.xyz",
  "model": "Qwen/Qwen2.5-Coder-32B-Instruct",
  "provider": "bearer",
  "api_key": "<your-together-key>"
}
```

Note: Together.ai model names use the Hugging Face format (`Org/Model-Name`).

---

## Groq

**Auth**: Bearer token. Groq offers extremely fast inference for open-source models.

### Step 1 — Get an API key

1. Sign up at [console.groq.com](https://console.groq.com)
2. Go to **API Keys** → **Create API Key**

### Step 2 — Add the backend entry

```json
{
  "name": "groq",
  "url": "https://api.groq.com/openai",
  "model": "qwen-qwq-32b",
  "provider": "bearer",
  "api_key": "gsk_<your-groq-key>"
}
```

Models with tool calling: `qwen-qwq-32b`, `llama-3.3-70b-versatile`, `llama3-groq-70b-8192-tool-use-preview`. Full list: [console.groq.com/docs/models](https://console.groq.com/docs/models).

---

## Azure OpenAI

**Auth**: `api-key` header (not Bearer). Azure uses a non-standard URL path structure.

> **Note**: Azure's deployment URL contains the `/openai` prefix, and milk appends `/v1/chat/completions` automatically. Set `url` to the base *before* `/v1`, so the combined path is correct.

### Step 1 — Deploy a model

1. Go to [Azure AI Foundry](https://ai.azure.com) or the Azure Portal
2. Create an Azure OpenAI resource
3. Under **Deployments**, deploy a model (e.g. `gpt-4.1`)
4. Note the **Endpoint** (e.g. `https://myresource.openai.azure.com`) and the **API key**

### Step 2 — Add the backend entry

```json
{
  "name": "azure",
  "url": "https://myresource.openai.azure.com/openai",
  "model": "gpt-4.1",
  "headers": {
    "api-key": "<your-azure-api-key>",
    "api-version": "2024-02-01"
  }
}
```

Leave `provider` empty — Azure uses header-based auth, not Bearer.

### Step 3 — Override the chat path (if needed)

Some Azure deployments expose the endpoint directly without `/v1`. In that case add:

```json
"chat_path": "/chat/completions"
```

---

## Dynamic token providers (token_cmd)

For providers that use short-lived tokens managed by an external CLI (e.g. a company SSO, a vault CLI, or a cloud provider's auth tool), use `token_cmd` instead of a static `api_key`.

milk runs the command at startup, uses stdout as the Bearer token, and retries with a fresh token on 401/403.

Example using a custom auth helper:

```json
{
  "name": "my-provider",
  "url": "https://inference.mycompany.com",
  "model": "gpt-4o",
  "provider": "bearer",
  "token_cmd": "my-auth-cli token --scope inference"
}
```

The command is run with `sh -c`, so environment variables and shell syntax work.

---

## Full config reference

### Inference-server `agents` entry fields

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | required | Unique backend name, used by `/agent switch` |
| `url` | string | required | Base URL of the inference server |
| `model` | string | required | Model name or ARN |
| `provider` | string | `""` | Auth transport: `""` = none, `"bedrock"` = SigV4, `"bearer"` or any string = Bearer |
| `api_key` | string | — | Static Bearer token or API key |
| `token_cmd` | string | — | Shell command to fetch a dynamic Bearer token |
| `headers` | object | — | Extra HTTP headers (key→value) injected on every request |
| `chat_path` | string | `/v1/chat/completions` | Override the inference endpoint path |
| `tls_skip_verify` | bool | false | Disable TLS cert verification (dev/self-signed only) |
| `tls_ca_cert` | string | — | Path to PEM CA cert for private/self-signed endpoints |
| `aws_region` | string | `AWS_REGION` env | AWS region (Bedrock only) |
| `aws_key_id` | string | `AWS_ACCESS_KEY_ID` env | AWS access key ID (Bedrock only) |
| `aws_secret` | string | `AWS_SECRET_ACCESS_KEY` env | AWS secret access key (Bedrock only) |
| `aws_token` | string | `AWS_SESSION_TOKEN` env | AWS session token for temporary credentials (Bedrock only) |
| `aws_service` | string | `bedrock` | Override the SigV4 service name (Bedrock only) |

### Claude CLI `agents` entry fields (`provider: "claude-cli"`)

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | required | Unique backend name |
| `provider` | string | required | Must be `"claude-cli"` |
| `bin` | string | `"claude"` | Path to the `claude` binary |
| `dangerously_skip_permissions` | bool | false | Auto-approve all tool uses |
| `allowed_tools` | array | — | Pre-approved tools; passed as `--allowedTools` |
| `add_dirs` | array | — | Extra directories; passed as `--add-dir` |

### Root config fields related to agents

| Field | Type | Default | Description |
|---|---|---|---|
| `agent` | string | first non-cli entry | Name of the active primary backend |
| `escalation_agent` | string | `"claude"` | Name of the escalation backend |
| `aws_auth_refresh` | bool | false | Run the Claude Code credential-process command before each Bedrock call |
