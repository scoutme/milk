# Provider setup guides

milk supports multiple agent backends. Use `/agent add` in the TUI to register them, `/agent list` to see what's configured, and `/agent switch <name> as primary|escalation` to assign roles.

Each backend is stored as a named entry under `agents` in `~/.milk/config.json`. The active primary agent is set by `agent`; the escalation agent is set by `escalation_agent`.

For local hardware setup (llama.cpp, CUDA, model download) and the local testing procedure, see [docs/setup.md](setup.md).

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

## OpenAI Responses API

milk supports the [OpenAI Responses API](https://platform.openai.com/docs/api-reference/responses) (`/v1/responses`) as an alternative wire format for HTTP agents. Enable it with `"api_format": "responses"` on any local or Bearer-auth agent entry.

```json
{
  "name": "local-responses",
  "url": "http://localhost:8080",
  "model": "qwen2.5-coder",
  "api_format": "responses"
}
```

When `api_format` is `"responses"`:
- The inference endpoint defaults to `/v1/responses` (override with `chat_path` if needed).
- `skip_health_check` is automatically set — the `/health` probe is skipped.
- Message history is translated: `tool` role → `function_call_output` items; assistant `tool_calls` → `function_call` items.
- Tool schemas are flattened from the Chat Completions nested `"function"` wrapper to the flat Responses API format.
- Streaming uses SSE event types (`response.output_text.delta`, `response.function_call_arguments.delta`, `response.completed`).

The default for HTTP agents is `"chat_completions"` (or `""`), which uses `/v1/chat/completions`.

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

### Automatic server startup (`run_cmd`)

Add `run_cmd` to launch the inference server automatically if it is not reachable when milk starts:

```json
{
  "name": "local",
  "url": "http://localhost:8080",
  "model": "qwen2.5-coder",
  "run_cmd": "llama-server --model ~/models/qwen2.5-coder-7b.gguf --port 8080 --jinja &"
}
```

- milk checks reachability at startup; if the server is already up the command is skipped.
- The process is launched detached (its own process group) so it survives milk exiting.
- milk writes the PID to `~/.milk/servers/<agent-name>.pid` for later teardown.
- `run_cmd` is executed via `sh -c`. On Windows, Git Bash or WSL2 is required (see [docs/setup.md](setup.md#windows-and-wsl2)).

**Server lifecycle commands**

| CLI | TUI | Description |
|-----|-----|-------------|
| `milk server status [agent]` | `/server status [agent]` | Reachability + tracked PID |
| `milk server start [agent]`  | `/server start for <agent>` | Start server manually (same as auto-start) |
| `milk server stop [agent]`   | `/server stop [agent]` | Send SIGTERM to tracked PID |

The `agent` argument defaults to the active local agent when omitted.

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

### Step 3 — Verify

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

## aider

**Provider**: `aider-cli` — invokes the `aider` binary directly. No adapter script required.

### Step 1 — Install aider

```sh
pip install aider-chat
aider --version
```

### Step 2 — Add the backend entry

Pointing aider at a local llama.cpp server:

```json
{
  "name": "aider",
  "provider": "aider-cli",
  "model": "openai/qwen2.5-coder-7b-instruct",
  "url": "http://localhost:8080/v1",
  "api_key": "local"
}
```

Or using a cloud provider:

```json
{
  "name": "aider",
  "provider": "aider-cli",
  "model": "claude-opus-4-5",
  "api_key": "sk-ant-..."
}
```

Set as the escalation agent:

```json
{
  "escalation_agent": "aider"
}
```

| Field | Type | Default | Description |
|---|---|---|---|
| `provider` | string | required | Must be `"aider-cli"` |
| `bin` | string | `"aider"` | Path to the aider binary |
| `model` | string | — | Model identifier passed to `--model` (e.g. `claude-opus-4-5`, `openai/qwen...`) |
| `url` | string | — | OpenAI-compatible API base URL (`--openai-api-base`); for local servers |
| `api_key` | string | — | API key passed as `OPENAI_API_KEY` in the subprocess environment |
| `extra_args` | array | — | Raw CLI arguments forwarded verbatim to aider (e.g. `["--auto-commits"]`). Appended after sane defaults (`--map-tokens 2048`, `--max-chat-history-tokens 4096`, `--map-refresh files`, `--no-show-model-warnings`) — any flag in `extra_args` overrides a sane default since aider uses last-value-wins parsing. |

### Step 3 — Verify

```sh
milk --new --escalate "list the Go files in this directory"
```

Expected: aider's response streamed into the TUI, with file-edit hints shown for any edits it makes.

### Notes

- aider is invoked with `--yes-always --no-pretty --edit-format diff` to run non-interactively.
- `--no-git` is added automatically when the current directory is not inside a git repository.
- Context files (milk's static + dynamic system prompt) are passed via `--read`. Aider treats them as reference material.
- Token counts are not reported by aider; cost tracking via `/usage` will show zeros for this provider.

---

## smolagents (HuggingFace)

**Provider**: `subprocess` — runs `milk-smolagent` as a subprocess. The adapter script wraps HuggingFace smolagents and translates its stream events to milk's NDJSON protocol. The script is bundled inside the milk binary and auto-deployed to `~/.milk/scripts/milk-smolagent` on first use — no manual installation required.

### Step 1 — Install smolagents

```sh
pip install smolagents[litellm]
```

The `litellm` extra is needed for `LiteLLMModel` (default driver for OpenAI-compatible endpoints):

| Model driver | Install |
|---|---|
| `LiteLLMModel` (OpenAI-compat, default) | `pip install smolagents[litellm]` |
| `HfApiModel` (HuggingFace Inference API) | `pip install smolagents` |
| `TransformersModel` (local model weights) | `pip install smolagents[transformers]` |

### Step 2 — Add the backend entry

```json
{
  "name": "smolagent",
  "provider": "subprocess",
  "model_type": "LiteLLMModel",
  "model": "openai/qwen2.5-coder-7b-instruct",
  "url": "http://localhost:8080/v1",
  "api_key": "local",
  "action_type": "code",
  "max_steps": 6
}
```

Set this as the escalation agent in the root config:

```json
{
  "escalation_agent": "smolagent"
}
```

| Field | Type | Default | Description |
|---|---|---|---|
| `provider` | string | required | Must be `"subprocess"` |
| `bin` | string | auto-deployed | Path to the adapter script; defaults to the bundled copy at `~/.milk/scripts/milk-smolagent` |
| `model_type` | string | `"LiteLLMModel"` | smolagents model driver: `LiteLLMModel`, `HfApiModel`, `TransformersModel` |
| `model` | string | required | Model identifier passed to `--model-id` |
| `url` | string | — | API base URL (`--api-base`); for LiteLLMModel pointing at a local server |
| `api_key` | string | — | API key (`--api-key`); use `"local"` for unauthenticated local servers |
| `action_type` | string | `"code"` | `"code"` (CodeAgent) or `"toolcalling"` (ToolCallingAgent) |
| `smolagent_tools` | array | `["bash"]` | Tools available to the agent |
| `authorized_imports` | array | — | Python module import allowlist (CodeAgent only) |
| `max_steps` | int | `6` | Max reasoning steps per turn |
| `extra_args` | array | — | Raw CLI arguments forwarded verbatim to milk-smolagent |

### Step 4 — Verify

```sh
milk --new --escalate "say hello"
```

Expected: streamed response with step/observation progress visible in the TUI.

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
| `api_format` | string | `""` | Wire protocol: `""` / `"chat_completions"` = OpenAI Chat Completions (default), `"responses"` = OpenAI Responses API |
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

---

## Memory configuration

All keys go in `~/.milk/config.json`. Sensible defaults apply when omitted.

| Key | Default | Description |
|-----|---------|-------------|
| `percept_inject_max` | 25 | Max percepts injected into the escalation agent context per turn. Lowest-weight percepts are dropped. Set to 0 for no limit. |
| `percept_inject_max_bytes` | 2048 | Max byte size of percept content injected into the escalation agent context per turn. Set to 0 for no limit. |
| `percept_store_max` | 0 (unlimited) | Max percepts kept in the global store. Lowest-weight non-core percepts are pruned after NREM consolidation. |
| `percept_relevance_gate` | true | Skip percepts with zero keyword overlap with the current prompt before injection (escalation agent context block and local agent `list_memory` results). Set to false to disable. |
| `memory_reinjection_turns` | 20 | Re-inject memory/need instructions into the escalation agent context after this many escalation turns (guards against context truncation in embedded agents like Claude Code). Set to 0 to disable. |
| `memory_reinjection_bytes` | 40000 | Re-inject memory/need instructions after this many bytes of escalation agent output. Set to 0 to disable. |
| `local_memory_result_max_bytes` | 2048 | Max byte size of `get_memory` / `list_memory` tool results returned to the local agent per call. Set to -1 for no limit. |
| `local_memory_reinjection_turns` | 20 | Re-inject memory/need instructions into the local agent's context after this many local turns. Set to -1 to disable. |
| `local_memory_reinjection_bytes` | 40000 | Re-inject memory/need instructions after this many bytes of local agent output. Set to -1 to disable. |
| `local_max_tool_iterations` | 20 | Max consecutive tool-call / response cycles the local agent may execute per turn before the turn is aborted. Set to -1 for unlimited. |

---

## Context budget configuration

| Key | Default | Description |
|-----|---------|-------------|
| `context_budget_chars` | 12000 | Max characters per summary brick (`last_local_summary` / `last_claude_summary`) injected into the escalation system prompt. Oldest turns are dropped first. |
| `local_context_budget_chars` | 24000 | Max total characters in the local agent's `messages` array per turn. Oldest user+assistant pairs are dropped when over budget. Set to 0 for no limit. |

---

## Per-agent limit overrides

Any entry in the `agents` array accepts a `limits` object that overrides the global context and memory settings for that specific agent. This lets you, for example, give a small Bedrock model a tighter context window without affecting the primary agent.

```json
{
  "agents": [
    {
      "name": "haiku-aws",
      "provider": "bedrock",
      "model": "anthropic.claude-haiku-4-5",
      "limits": {
        "context_budget_chars": 6000,
        "message_budget_chars": 12000,
        "percept_inject_max": 5,
        "percept_inject_max_bytes": 512,
        "memory_result_max_bytes": 1024,
        "memory_reinjection_turns": 10,
        "memory_reinjection_bytes": 20000,
        "percept_relevance_gate": true
      }
    }
  ]
}
```

All fields are optional. When omitted, the global value (or built-in default) applies.

**Integer field semantics:**

| Value | Meaning |
|-------|---------|
| omitted / `null` | Use global config value |
| `0` | Use built-in hardcoded default |
| positive integer | Use this exact value |
| negative (e.g. `-1`) | Disabled / unlimited |

| Field | Global key | Built-in default | Description |
|-------|-----------|-----------------|-------------|
| `context_budget_chars` | `context_budget_chars` | 12000 | Max chars per summary brick injected into the escalation system prompt |
| `message_budget_chars` | `local_context_budget_chars` | 24000 | Max chars in message history per turn (oldest pairs dropped when over budget) |
| `percept_inject_max` | `percept_inject_max` | 25 | Max percepts injected per turn |
| `percept_inject_max_bytes` | `percept_inject_max_bytes` | 2048 | Max total bytes of injected percept content |
| `memory_result_max_bytes` | `local_memory_result_max_bytes` | 2048 | Max bytes of a `get_memory` / `list_memory` tool result |
| `memory_reinjection_turns` | `memory_reinjection_turns` / `local_memory_reinjection_turns` | 20 | Re-inject memory instructions after N turns |
| `memory_reinjection_bytes` | `memory_reinjection_bytes` / `local_memory_reinjection_bytes` | 40000 | Re-inject memory instructions after N bytes of output |
| `percept_relevance_gate` | `percept_relevance_gate` | `true` | Enable keyword-intersection filter before percept injection |
| `max_tool_iterations` | `local_max_tool_iterations` | 20 | Max tool-call cycles per turn (-1 = unlimited) |

> **Tip — large context window agents:** If your primary agent has a large context window (e.g. Copilot, GPT-4o, Claude 3.7), use `limits` to raise `max_tool_iterations` (suggest `100`), `message_budget_chars` (suggest `3000000`), and `context_budget_chars` (suggest `200000`). The `milk config init` wizard prompts for these automatically when you answer "y" to the large context window question.

---

## Remote oversight (Telegram)

Forward agent activity and permission prompts to a mobile device.

**Quick setup** (interactive wizard):

```
/setup telegram
```

Follows the prompts: paste your bot token from @BotFather, send the bot a message, and milk resolves your chat ID automatically and saves the config.

**Manual config** (`~/.milk/config.json`):

```json
{
  "remote_oversight": {
    "backend": "telegram",
    "telegram": {
      "token": "<bot-token-from-botfather>",
      "chat_id": <your-numeric-chat-id>
    },
    "perm_timeout_secs": 120,
    "timeout_action": "deny",
    "notify_tools": true
  }
}
```

**Enable / disable at runtime** (credentials are preserved):

```
/setup telegram on
/setup telegram off
```

| Key | Default | Description |
|-----|---------|-------------|
| `backend` | `""` | Transport backend. `"telegram"` to enable; `""` to disable. |
| `perm_timeout_secs` | 120 | How long to wait for a remote permission reply before falling back to `timeout_action`. |
| `timeout_action` | `"deny"` | Action when remote permission reply times out. `"allow"` or `"deny"`. |
| `notify_tools` | true | Forward escalation agent tool-call notifications. |

**What gets forwarded:**
- Turn start (agent name, target, prompt snippet)
- Tool calls (name + key argument)
- Agent response text (capped at 3000 chars)
- Permission prompts with y/n reply — first response (TUI or Telegram) wins

**Remote input:** send any message to the bot and it is injected as a new turn (shown as `[telegram] …` in the transcript). Ignored while an agent turn is in progress.
