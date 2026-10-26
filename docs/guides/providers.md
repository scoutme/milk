# Providers & configuration

milk supports multiple agent backends. Use `/agent add` in the TUI to register them, `/agent list` to see what's configured, and `/agent switch <name> as primary|escalation` to assign roles. Each backend is a named entry under `agents` in `~/.milk/config.json`; the active primary agent is set by `agent`, the escalation agent by `escalation_agent`.

**There is no preferred or default backend for either role.** Every backend — inference-server (local/cloud HTTP, Bedrock), subprocess (aider-cli, smolagents), and `claude-cli` alike — can serve as primary or escalation. Claude Code CLI being listed first below is a zero-config-built-in convenience, not an endorsement over any other option; likewise local inference is one option for the primary role among several, not a requirement. The only constraint milk expects you to honor yourself: **the escalation agent should be smarter (and typically pricier) than the primary agent; the primary agent should be cheaper than the escalation agent.**

`claude-cli` as primary is a special case worth calling out: it works, but it's not a smart pattern for the everyday fast path — every "primary" turn spawns a full Claude Code subprocess, and self-escalation (the `escalate()` tool call a primary agent can use on its own) isn't available to it, since that tool is built into the local agent's tool loop specifically; only explicit `/escalate` works. Where it's genuinely useful: bootstrapping milk with only Claude Code available (e.g. letting it configure other agents for you via `/agent add`), or deliberately running Claude Code in both roles pointed at different backends/models.

New to milk? Start with [docs/start/getting-started.md](../start/getting-started.md) for the fastest path to a working config, then come back here for backend-specific detail.

## Primary agent

The agent handling the fast path — most turns, most tool calls. Configurable via any `agents` entry, including `claude-cli` (see the callout above): inference-server backends (local/cloud HTTP, or AWS Bedrock Converse natively), subprocess backends (aider-cli, smolagents), and Claude Code CLI can all be primary. Any tool-calling-capable model works on the inference-server path; see [Tested models](#tested-models) for models confirmed against milk's tool-calling loop specifically.

## Escalation agent

The agent handling turns the primary agent hands off — set via `escalation_agent` in the config root, switch at runtime with `/agent switch <name> as escalation`. Can be Claude Code CLI (`provider: "claude-cli"`), or any inference-server backend using the same protocols as the primary agent, with a role-aware system prompt (no `escalate` tool; it knows it's the escalation target). See [docs/guides/workflows.md](workflows.md) for how a turn gets routed here and what happens across the handoff.

### Same backend, two tiers — the routing principle in practice

A common, valid pattern: one provider, two agent entries at different model weights — cheap and fast for primary, larger and smarter for escalation.

```json
{
  "agents": [
    { "name": "local-fast",  "provider": "bearer", "url": "https://your-provider.example.com",
      "model": "your-model",     "api_key": "sk-...", "chat_path": "/v1/chat/completions",
      "context_window_tokens": 1000000 },
    { "name": "local-smart", "provider": "bearer", "url": "https://your-provider.example.com",
      "model": "your-model-pro", "api_key": "sk-...", "chat_path": "/v1/chat/completions",
      "context_window_tokens": 1000000 }
  ],
  "agent": "local-fast",
  "escalation_agent": "local-smart"
}
```

---

## Backends

All backends milk supports can serve as primary or escalation (see the principle above); cross-cutting config that applies *regardless* of which backend you pick — context window, prompt tiering, custom prompts, wire-format overrides, dynamic tokens — lives in [Cross-cutting agent configuration](#cross-cutting-agent-configuration) below, not repeated per backend.

### Claude Code CLI

**Provider**: `claude-cli` — runs the `claude` binary as a subprocess, not via HTTP.

```json
{ "name": "claude", "provider": "claude-cli", "bin": "claude" }
```

A built-in entry named `"claude"` with `provider: "claude-cli"` is always available even if not listed explicitly in `agents`, and is the default `escalation_agent` (a default of convenience — see the principle above). With several `claude-cli` entries (e.g. one per backend via `settings.env`), `escalation_agent` picks which one runs; leaving it unset runs `"claude"` — your own entry of that name if you define one, otherwise the built-in.

| Field | Default | Description |
|---|---|---|
| `bin` | `"claude"` | Path to the `claude` binary |
| `dangerously_skip_permissions` | `false` | Auto-approve all tool uses without prompting |
| `allowed_tools` | — | Tools pre-approved; passed as `--allowedTools` |
| `add_dirs` | — | Extra directories; passed as `--add-dir` |
| `settings` | — | JSON object passed via `--settings` (same schema as Claude's `settings.local.json`, e.g. `{"env": {...}}`) |

#### Claude CLI against a non-Anthropic backend

`claude-cli` isn't tied to Anthropic's hosted API — the `claude` binary reads its backend from environment variables, which `settings.env` can override per agent entry. This example points Claude Code at an entirely different, OpenAI-key-style provider that happens to speak the Anthropic wire format:

```json
{
  "name": "claude-alt-backend",
  "provider": "claude-cli",
  "settings": {
    "env": {
      "ANTHROPIC_BASE_URL": "https://your-provider.example.com/anthropic",
      "ANTHROPIC_AUTH_TOKEN": "sk-...",
      "ANTHROPIC_MODEL": "your-model[1m]",
      "ANTHROPIC_DEFAULT_SONNET_MODEL": "your-model[1m]",
      "ANTHROPIC_DEFAULT_OPUS_MODEL": "your-model[1m]",
      "ANTHROPIC_DEFAULT_HAIKU_MODEL": "your-model[1m]",
      "CLAUDE_CODE_USE_BEDROCK": "0"
    }
  }
}
```

Claude CLI can also be a **tool-agent** — called inline during another agent's tool loop — see [docs/guides/tooling.md — claude-cli as a tool-agent](tooling.md#claude-cli-as-a-tool-agent).

---

### Local llama.cpp / Ollama / LM Studio

**Auth**: none — plain HTTP.

```json
{ "name": "local", "url": "http://localhost:8080", "model": "qwen2.5-coder" }
```

For Ollama the default port is `11434`; for LM Studio it's `1234`. The model name must match what the server reports (check `/v1/models`). The model must support function/tool calling for either OpenAI-compatible Chat Completions or the AWS Bedrock Converse API.

#### Automatic server startup (`run_cmd`)

Launch the inference server automatically if unreachable at milk startup:

```json
{ "name": "local", "url": "http://localhost:8080", "model": "qwen2.5-coder",
  "run_cmd": "llama-server --model ~/models/qwen2.5-coder-7b.gguf --port 8080 --jinja &" }
```

milk checks reachability at startup (skips the command if already up), launches detached in its own process group so it survives milk exiting, and writes the PID to `~/.milk/servers/<agent-name>.pid`. Run via `sh -c` — on Windows, Git Bash or WSL2 is required (see [Windows and WSL2](#windows-and-wsl2)).

| CLI | TUI | Description |
|---|---|---|
| `milk server status [agent]` | `/server status [agent]` | Reachability + tracked PID |
| `milk server start [agent]` | `/server start for <agent>` | Start manually |
| `milk server stop [agent]` | `/server stop [agent]` | Send SIGTERM to tracked PID |

`agent` defaults to the active local agent when omitted.

#### Real `run_cmd` tuning examples

Three separate real configs (paths genericized), showing different tuning knobs rather than one golden path:

**Multimodal (vision) model** — `--mmproj` loads the multimodal projector alongside the base model:

```json
{ "name": "vision-local", "url": "http://localhost:8070", "model": "gemma-vision", "provider": "local",
  "context_window_tokens": 131072,
  "run_cmd": "/path/to/llama.cpp/build/bin/llama-server --model /path/to/models/gemma/model-Q4_K_M.gguf --mmproj /path/to/models/gemma/mmproj-F16.gguf --host 127.0.0.1 --port 8070 --ctx-size 131072 --n-gpu-layers 99 --flash-attn on" }
```

**KV-cache quantization** — `--cache-type-k`/`--cache-type-v q8_0` trades a little quality for meaningfully less VRAM used by the context cache, useful when running a bigger context size than your VRAM would otherwise allow:

```json
{ "name": "big-context-local", "url": "http://localhost:8080", "model": "custom-9b", "provider": "local",
  "context_window_tokens": 32768,
  "run_cmd": "/path/to/llama.cpp/build/bin/llama-server --model /path/to/models/model.gguf --host 127.0.0.1 --port 8080 --ctx-size 32768 --n-gpu-layers 99 --flash-attn on --cache-type-k q8_0 --cache-type-v q8_0 --threads 8 --threads-batch 16 --prio 2" }
```

**CPU thread tuning** — `--threads`/`--threads-batch`/`--prio` for partial-offload or CPU-heavy setups (also shown above, combined with cache quantization).

**Reference tool-calling model**:

```json
{ "name": "coder-local", "url": "http://localhost:8090", "model": "qwen2.5-coder", "provider": "local",
  "context_window_tokens": 131072,
  "run_cmd": "/path/to/llama.cpp/build/bin/llama-server --model /path/to/models/qwen2.5-coder-7b/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf --host 127.0.0.1 --port 8090 --ctx-size 131072 --n-gpu-layers 99 --flash-attn on --jinja" }
```

#### Tested models

Confirmed working with milk's tool-calling loop, served via llama.cpp with `--jinja` (the streaming tool-format detector handles format differences automatically):

| Model | Size | Tool format | Notes |
|---|---|---|---|
| **Qwen2.5-Coder-7B-Instruct** | 7B | fenced JSON (`` ```json ``) | Reference model. Reliable tool calls, good code quality. |
| **Qwen2.5-Coder-3B-Instruct** | 3B | fenced JSON | Fits in 4 GB VRAM (Q8_0 ~3.4 GB). Tool calls work; prose quality limited by size. |
| **Gemma-4-E4B** | 4B (MoE) | `<tool_call>` tags | Requires `--jinja`. Chat template handles tool markup. |

Other instruction-tuned models with OpenAI-style function calling should work. If tool calls are emitted in an unrecognised format, open an issue — adding a new format to the stream detector is straightforward.

#### Reference setup: NVIDIA GPU, Ubuntu/WSL2, llama.cpp from source

One worked example among several ways to get local inference running — not the default path, and not required if you're using a cloud provider or already have a server running. Parameters (CUDA architecture, quant size, GPU layer count, context size) will differ for other hardware; for general llama.cpp installation see the [official README](https://github.com/ggml-org/llama.cpp).

**1. CUDA toolkit** (skip if CPU-only):

```sh
wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb
sudo dpkg -i cuda-keyring_1.1-1_all.deb
sudo apt update && sudo apt install -y cuda-toolkit-12-8
```

Add to `~/.zshrc`/`~/.bashrc`:

```sh
export PATH=/usr/local/cuda-12.8/bin:$PATH
export LD_LIBRARY_PATH=/usr/local/cuda-12.8/lib64:$LD_LIBRARY_PATH
```

Verify: `nvcc --version`

**2. Build dependencies**: `sudo apt install -y cmake build-essential git`

**3. Build llama.cpp**:

```sh
git clone https://github.com/ggml-org/llama.cpp ~/llama.cpp
cd ~/llama.cpp

# GPU build — adjust -DCMAKE_CUDA_ARCHITECTURES for your GPU:
#   Ada Lovelace (RTX 40xx, RTX 500/1000 Ada): 89 · Ampere (RTX 30xx): 86 · Turing (RTX 20xx): 75
cmake -B build -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=89
cmake --build build --config Release -j$(nproc)
```

CPU-only: omit the CUDA flags (`cmake -B build && cmake --build build --config Release -j$(nproc)`). The server binary lands at `~/llama.cpp/build/bin/llama-server`.

**4. Download the model** — reference is Qwen2.5-Coder-7B-Instruct:

| Quant | Size | Fits in |
|---|---|---|
| Q4_K_M | ~4.1 GB | 4 GB VRAM (tight) |
| Q3_K_M | ~3.2 GB | 4 GB VRAM (with headroom) |
| Q8_0 | ~7.2 GB | 8 GB VRAM |

```sh
pip3 install hf-xet huggingface_hub[hf_xet,cli]
hf download bartowski/Qwen2.5-Coder-7B-Instruct-GGUF Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf --local-dir ~/models/qwen2.5-coder-7b
```

**5. Start the server**:

```sh
./scripts/llama-serve.sh
```

Reads defaults for binary path, model path, port, and GPU layers — override in `~/.milk/llama.env`:

```sh
# ~/.milk/llama.env
LLAMA_MODEL="$HOME/models/qwen2.5-coder-7b/Qwen2.5-Coder-7B-Instruct-Q3_K_M.gguf"
LLAMA_CTX_SIZE=4096   # reduce if VRAM OOMs
LLAMA_GPU_LAYERS=28   # partial offload: rest runs on CPU
```

Or invoke `llama-server` directly (see the tuning examples above for `run_cmd` variants).

Verify the server: `curl http://localhost:8080/health` → `{"status":"ok"}`.

Verify tool calls (requires `--jinja`, already in the script):

```sh
curl -s http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2.5-coder",
    "messages": [{"role":"user","content":"list go files in current dir"}],
    "tools": [{"type":"function","function":{"name":"bash","description":"run shell command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}],
    "stream": false, "temperature": 0.2
  }' | python3 -m json.tool | grep -A5 "tool_calls"
```

Expected: a `tool_calls` array with `"name": "bash"`. If the call appears inside `content` as raw text instead, `--jinja` is missing or the server started without it.

**6. Build and verify milk** — see [docs/start/getting-started.md](../start/getting-started.md#install).

#### Windows and WSL2

milk's Go core is cross-platform (config paths use `os.UserHomeDir()`, TUI uses bubbletea, no PTY or Unix-only syscalls), but the primary-agent `bash` tool hard-codes `sh -c` and will not work on native Windows. **WSL2 is the recommended path.**

1. **Install WSL2** (PowerShell, Admin): `wsl --install` — installs Ubuntu, reboot when prompted. See the [Microsoft WSL2 guide](https://learn.microsoft.com/en-us/windows/wsl/install) for other distros.
2. **Install Claude Code on Windows** — download the [Windows installer](https://claude.ai/code); `claude.exe` becomes available via WSL2 interop (`claude --version` from inside WSL2). If not found, add the install directory to `$PATH`: `export PATH="$PATH:/mnt/c/Users/<YourUser>/AppData/Local/Programs/claude"`.
3. **Install Go inside WSL2**: `sudo apt update && sudo apt install -y golang-go git` (or the [official installer](https://go.dev/doc/install) for a newer version).
4. **Install milk** inside WSL2 with the standard Linux steps: `curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh`.
5. **GPU inference (optional)** — NVIDIA drivers on Windows surface inside WSL2 via `/dev/dxg`, no separate Linux driver needed. The [reference llama.cpp setup](#reference-setup-nvidia-gpu-ubuntuwsl2-llamacpp-from-source) works as-is.

**What works on native Windows** (`go build ./cmd/milk/`, no WSL2):

| Feature | Status |
|---|---|
| Config load / session storage | Works |
| TUI (transcript, input, status bar) | Works — bubbletea + Windows Terminal VT support |
| Cloud providers (Bedrock, OpenRouter, Groq) | Works — HTTP/HTTPS only |
| Claude escalation via CLI subprocess | Works if `claude.exe` is on PATH |
| Primary agent `bash` tool | **Broken** — hard-codes `sh -c` |
| `scripts/llama-serve.sh` | **Broken** — no PowerShell equivalent |
| `install.sh` | **Broken** — requires POSIX shell |

Tracked in [issue #38](https://github.com/scoutme/milk/issues/38).

#### Troubleshooting

**400 on first call**: server started without `--jinja` — restart with `./scripts/llama-serve.sh`.

**VRAM OOM / server crash during inference**: reduce `LLAMA_CTX_SIZE`/`LLAMA_GPU_LAYERS` in `~/.milk/llama.env`, or use `--cache-type-k`/`--cache-type-v q8_0` (see the tuning examples above).

**Tool call appears as raw text in `content`**: `--jinja` missing.

---

### AWS Bedrock

**Auth**: AWS SigV4. milk uses the native Bedrock Converse API — no OpenAI-compat layer.

#### Step 1 — IAM permissions

```json
{ "Version": "2012-10-17", "Statement": [{ "Effect": "Allow",
    "Action": ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"],
    "Resource": "arn:aws:bedrock:*::foundation-model/*" }] }
```

If using inference profiles, add the profile ARN to `Resource` or use `"*"`.

#### Step 2 — Configure credentials

Resolved in order: explicit `aws_key_id`/`aws_secret`/`aws_token` fields → env vars `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN`/`AWS_REGION`. For temporary (STS-assumed) credentials, set `aws_auth_refresh: true` at the config root — milk refreshes automatically before each Claude turn and before local Bedrock calls at startup.

`aws_refresh_cmd` (per agent entry) wires a `credential_process`-compatible command directly into the SigV4 transport: on a 403, milk runs it, swaps credentials atomically, and retries once — no restart needed.

#### Step 3 — Add the backend entry

```json
{ "name": "haiku", "url": "https://bedrock-runtime.eu-central-1.amazonaws.com",
  "model": "anthropic.claude-3-haiku-20240307-v1:0", "provider": "bedrock", "aws_region": "eu-central-1" }
```

Cross-region inference profiles use the profile ARN as `model`:

```json
{ "name": "haiku-profile", "url": "https://bedrock-runtime.eu-central-1.amazonaws.com",
  "model": "arn:aws:bedrock:eu-central-1:123456789012:application-inference-profile/abc123",
  "provider": "bedrock", "aws_region": "eu-central-1" }
```

Verify: `milk --new --primary "say hi in one word"`.

#### Prompt caching (`prompt_caching`) — ⚠️ experimental, not live-tested

> This was implemented and unit-tested against AWS's documented Converse API contract, but has **not** been exercised against a real Bedrock endpoint — no Bedrock agent was available during development. The request-side gating (never sends `cachePoint` unless you opt in) is verified; live behavior on a real account/model/region is not. Contrast with local-agent implicit caching for OpenAI-compatible providers (e.g. Xiaomi MiMo), which *has* been live-verified and needs no config flag.

Set `"prompt_caching": true` on a `provider: "bedrock"` entry to enable explicit `cachePoint` breakpoints on the Converse API request. Off by default and must be opted in explicitly — sending `cachePoint` to a model/region that doesn't support it is a hard API error, not a graceful no-op. Uses up to 3 of Bedrock's 4 allowed breakpoints:

- One right after the stable system-prompt prefix — anchored there specifically (not at the end of the `system` array) so it caches independently of any per-turn percepts/current-need orientation that follow it; those change often and would otherwise invalidate the whole system block on every change.
- Two more as a rolling "double buffer" over the last two conversation messages, so a retry/edit that drops the newest message still leaves the next-to-last marker as a fallback anchor instead of forcing a full recompute.

Cache-hit stats appear in the same `cache:NN%` display used elsewhere.

#### Troubleshooting

| Error | Cause | Fix |
|---|---|---|
| `403 Credential should be scoped to a valid region` | `aws_region` unset, `AWS_REGION` absent | Add `"aws_region"` |
| `403 Credential should be scoped to correct service` | Wrong `aws_service` | Remove it or set `"bedrock"` |
| `UnknownOperationException` | Wrong URL/ARN format | Verify `bedrock-runtime.<region>.amazonaws.com`, no path suffix |
| `404` on model | Model not enabled in account | Bedrock console → Model access |

---

### OpenRouter

**Auth**: Bearer token — access to hundreds of hosted models via one key.

1. Sign up at [openrouter.ai](https://openrouter.ai) → **Keys** → **Create Key** (starts with `sk-or-`).
2. Add the entry:

```json
{ "name": "openrouter", "url": "https://openrouter.ai/api", "model": "qwen/qwen-2.5-coder-32b-instruct",
  "provider": "bearer", "api_key": "sk-or-<your-key>",
  "headers": { "HTTP-Referer": "https://github.com/scoutme/milk", "X-Title": "milk" } }
```

`HTTP-Referer`/`X-Title` are optional, recommended by OpenRouter for usage attribution.

| Model | Notes |
|---|---|
| `qwen/qwen-2.5-coder-32b-instruct` | Strong code model, reliable tool calls |
| `meta-llama/llama-4-maverick` | Fast, good general use |
| `anthropic/claude-haiku-4-5` | Claude as the primary agent |
| `deepseek/deepseek-coder-v2-instruct` | Strong code, competitive pricing |

Full list: [openrouter.ai/models](https://openrouter.ai/models).

---

### Together.ai

**Auth**: Bearer token. Sign up at [api.together.xyz](https://api.together.xyz) → **Settings** → **API Keys**.

```json
{ "name": "together", "url": "https://api.together.xyz", "model": "Qwen/Qwen2.5-Coder-32B-Instruct",
  "provider": "bearer", "api_key": "<your-together-key>" }
```

Model names use the Hugging Face format (`Org/Model-Name`).

---

### Groq

**Auth**: Bearer token — very fast inference for open-source models. Sign up at [console.groq.com](https://console.groq.com) → **API Keys**.

```json
{ "name": "groq", "url": "https://api.groq.com/openai", "model": "qwen-qwq-32b",
  "provider": "bearer", "api_key": "gsk_<your-groq-key>" }
```

Models with tool calling: `qwen-qwq-32b`, `llama-3.3-70b-versatile`, `llama3-groq-70b-8192-tool-use-preview`. Full list: [console.groq.com/docs/models](https://console.groq.com/docs/models).

---

### Azure OpenAI

**Auth**: `api-key` header (not Bearer). Azure's deployment URL contains an `/openai` prefix; milk appends `/v1/chat/completions` automatically, so set `url` to the base *before* `/v1`.

1. [Azure AI Foundry](https://ai.azure.com) or the Portal → create a resource → **Deployments** → deploy a model (e.g. `gpt-4.1`). Note the endpoint and API key.
2. Add the entry (leave `provider` empty — Azure uses header auth, not Bearer):

```json
{ "name": "azure", "url": "https://myresource.openai.azure.com/openai", "model": "gpt-4.1",
  "headers": { "api-key": "<your-azure-api-key>", "api-version": "2024-02-01" } }
```

If a deployment exposes the endpoint directly without `/v1`, add `"chat_path": "/chat/completions"`.

---

### aider

**Provider**: `aider-cli` — invokes the `aider` binary directly, no adapter script.

1. Install: `pip install aider-chat && aider --version`
2. Add the entry — pointing at a local llama.cpp server:

```json
{ "name": "aider", "provider": "aider-cli", "model": "openai/qwen2.5-coder-7b-instruct",
  "url": "http://localhost:8080/v1", "api_key": "local" }
```

Or a cloud provider:

```json
{ "name": "aider", "provider": "aider-cli", "model": "claude-opus-4-5", "api_key": "sk-ant-..." }
```

Set as escalation agent: `{ "escalation_agent": "aider" }`.

| Field | Type | Default | Description |
|---|---|---|---|
| `provider` | string | required | `"aider-cli"` |
| `bin` | string | `"aider"` | Path to the binary |
| `model` | string | — | Passed to `--model` |
| `url` | string | — | OpenAI-compatible base URL (`--openai-api-base`) |
| `api_key` | string | — | Passed as `OPENAI_API_KEY` in the subprocess env |
| `extra_args` | array | — | Raw CLI args forwarded verbatim (appended after sane defaults: `--map-tokens 2048`, `--max-chat-history-tokens 4096`, `--map-refresh files`, `--no-show-model-warnings`; any flag here overrides a default since aider uses last-value-wins parsing) |

Verify: `milk --new --escalate "list the Go files in this directory"`.

**Notes**: invoked with `--yes-always --no-pretty --edit-format diff` (non-interactive); `--no-git` added automatically outside a git repo; milk's static+dynamic system prompt passed via `--read`; token counts aren't reported by aider, so `/usage` shows zeros for this provider.

---

### smolagents (HuggingFace)

**Provider**: `subprocess` — runs the bundled `milk-smolagent` adapter (auto-deployed to `~/.milk/scripts/milk-smolagent` on first use, no manual install), which wraps HuggingFace smolagents and translates its stream to milk's NDJSON protocol.

1. Install: `pip install smolagents[litellm]` (the `litellm` extra is needed for the default `LiteLLMModel` driver; `HfApiModel` needs only `smolagents`, `TransformersModel` needs `smolagents[transformers]`).
2. Add the entry:

```json
{ "name": "smolagent", "provider": "subprocess", "model_type": "LiteLLMModel",
  "model": "openai/qwen2.5-coder-7b-instruct", "url": "http://localhost:8080/v1", "api_key": "local",
  "action_type": "code", "max_steps": 6 }
```

Set as escalation agent: `{ "escalation_agent": "smolagent" }`.

| Field | Type | Default | Description |
|---|---|---|---|
| `provider` | string | required | `"subprocess"` |
| `bin` | string | auto-deployed | Adapter script path |
| `model_type` | string | `"LiteLLMModel"` | `LiteLLMModel` / `HfApiModel` / `TransformersModel` |
| `model` | string | required | Passed to `--model-id` |
| `url` | string | — | `--api-base`, for LiteLLMModel against a local server |
| `api_key` | string | — | `--api-key`; `"local"` for unauthenticated servers |
| `action_type` | string | `"code"` | `"code"` (CodeAgent) or `"toolcalling"` (ToolCallingAgent) |
| `smolagent_tools` | array | `["bash"]` | Tools available to the agent |
| `authorized_imports` | array | — | Python import allowlist (CodeAgent only) |
| `max_steps` | int | 6 | Max reasoning steps per turn |
| `extra_args` | array | — | Raw CLI args forwarded verbatim |

Verify: `milk --new --escalate "say hello"`.

---

## Cross-cutting agent configuration

These apply to any `agents` entry, regardless of which backend above it is — not one-per-backend settings.

### Context window declaration (`context_window_tokens`)

Set on any agent entry to declare the model's context window size. milk then derives sensible defaults for two per-turn limits without requiring explicit `limits` overrides:

| Derived limit | Formula | Example (32 768 tokens) |
|---|---|---|
| `message_budget_chars` | `context_window_tokens × 3` | 98 304 chars |
| `max_tool_iterations` | `max(20, context_window_tokens / 4096)` | 20 iterations |

Explicit `limits.message_budget_chars` / `limits.max_tool_iterations` always win.

When no window is known at all (no `context_window_tokens` and no models.dev catalog match for the model name), the tool-iteration budget falls back to a fixed **40** — generous for capable cloud models, still bounded by the doom-loop gates, the per-tool timeout and the opt-in `limits.turn_deadline_secs`. Because nothing else on screen would say so, milk prints a session-start notice (TUI transcript, or an ACP message chunk for editor-driven sessions) naming both keys whenever a milk-loop agent runs in that state.

```json
{ "name": "qwythos-local", "url": "http://localhost:8080", "model": "qwythos", "provider": "local",
  "context_window_tokens": 32768, "run_cmd": "llama-server --model ~/models/qwythos.gguf --ctx-size 32768 --port 8080" }
```

For local models, read the value directly from the `--ctx-size` flag in `run_cmd`.

When `context_window_tokens` is omitted, milk falls back to a best-effort lookup against the [models.dev](https://models.dev) model catalog, matched case-insensitively against the agent's `model` string across every provider in the catalog (milk doesn't store a models.dev provider ID, so a bare name like `claude-sonnet-4-6` is matched wherever it appears). A model's context window essentially never changes once published, so the primary source is a snapshot of the catalog embedded into the `milk` binary at build time (`internal/modelsdev/snapshot.json`, regenerated periodically via `scripts/update-models-dev-snapshot.sh`) — this works fully offline with zero startup latency for any model that existed when the snapshot was last cut. A live network fetch, cached to `~/.milk/models_dev.json` and refreshed in the background past a 24h TTL, is consulted only as a fallback for a miss against the embedded snapshot (e.g. a model released since). Lookups are served from memory and never block a turn — a miss against both sources, a fetch failure, or an unmatched model name all fall through to the pre-existing behavior (no derived limits, absolute-size `ctx:` display). Explicit `context_window_tokens` always wins over either catalog source. Disable the whole fallback (embedded snapshot and network both) with `"disable_models_dev_lookup": true` — most useful when a matched value would be wrong for a custom/fine-tuned model that happens to share a name with a catalog entry; the network fetch alone adds no meaningful offline benefit to disable separately, since it's already a no-op fallback that only matters on an embedded-snapshot miss.

---

### System prompt verbosity (`system_prompt_tier`)

milk's default system prompt (`standard`) is tuned for capable models. Smaller local models benefit from a shorter prompt that frees context for history and tools.

| Value | Approx. size | Contents |
|---|---|---|
| `"minimal"` | ~60 tokens | Core task framing only |
| `"standard"` | ~700 tokens | Full default (default when omitted) |
| `"full"` | ~900 tokens | Standard plus verbose guidance |

```json
{ "name": "qwen-local", "url": "http://localhost:8090", "model": "qwen2.5-coder", "provider": "local",
  "context_window_tokens": 8192, "system_prompt_tier": "minimal" }
```

The tier is also the first knob the [layered context-reduction ladder](#layered-context-reduction-context_reduction) turns dynamically: under context pressure it steps the tier down for you, so a fixed `"minimal"` is only needed when you want it even at low pressure.

---

### Project map context (`project_map_context`)

The [`project_map` tool](tooling.md#built-in-tools) is a *pull*: the agent calls it when it wants structure. `project_map_context` adds a small *push* on top — project-structure context rebuilt into the system prompt every turn (ADR-0081):

| Value | What gets pushed | Cost |
|---|---|---|
| `"minimal"` | Framing header (project identity, docs/build landmarks) + top-level directory shape + a pointer at the tool | ~100–300 tokens (default) |
| `"off"` | Nothing — the `project_map` tool only | none |
| `"full"` | A budgeted Aider-style map: files → symbols/signatures, degrading to fit the budget (signatures → names → reference-ranked names → file tree) | ~1.5k tokens at the floor; scales with the agent's window (≤ ~12.5k at the 50k-char cap) |

Boolean aliases are accepted: `true` = `"full"`, `false` = `"off"` (also as strings). The block is rebuilt per turn (so `full` never serves yesterday's tree), rides the optional-guidance layer of the [context reduction](#layered-context-reduction-context_reduction) ladder — shed first under pressure — and the tool pointer is only included when the agent actually has `project_map` (e.g. not when `limits.excluded_tools` removes it).

**Both `full` and the tool's own defaults scale with the agent's context window** (ADR-0081 amendment): the resolved window (`context_window_tokens`, else the models.dev catalog lookup, honoring `disable_models_dev_lookup`) derives the default caps — `files = clamp(window/640, 200, 1000)` and `chars = clamp(window/6, 6000, 50000)`. The derivation is growth-only: unknown and small windows keep exactly the historical 200 files / 6000 chars defaults, call-site `max_files`/`max_chars` args plus the hard caps still override per call, and the tool's parameter descriptions state the defaults the current run will actually use — the model never has to guess.

```json
{ "name": "cloud-big", "provider": "bedrock", "model": "…",
  "project_map_context": "full" }
```

Pick per agent: `full` for a capable model with context room to spare (the Aider repo-map measurements were taken on GPT-4-class models), `minimal` for most, `off` for tiny contexts or non-code sessions. `full` costs its budget *every turn* — ~1.5k tokens at the floor, more on big windows — that's the one to think about.

---

### Layered context reduction (`context_reduction`)

A static tier is a fixed guess, but context pressure is dynamic — a request that fits at turn 2 can overflow by turn 20. `context_reduction` configures an automatic ladder that shrinks the request's *static* layers as pressure rises, before any history is touched. Pressure is the prompt size over the usable window: the previous request's provider-reported prompt tokens (a chars/4 estimate until the provider reports one) divided by the same window-minus-output-reserve denominator `compaction_trigger_tokens` derives — `context_window_tokens` minus the models.dev output limit capped at 32K (20K if unlisted) and at a quarter of the window. The level is recomputed at every tool-loop iteration from the pristine prompt and tool list (session state is never mutated), so a layer eases back on its own as soon as pressure drops — e.g. right after a compaction.

| Pressure | Layer | What shrinks |
|---|---|---|
| ≥ 55% | instructions | System-prompt tier steps down one level (`full`→`standard`, `standard`→`minimal`); the optional per-turn guidance blocks (background-agent, task-tool, memory-tool, project-map) are dropped |
| ≥ 65% | tools_compact | Tier forced to `minimal`; the project-instructions (AGENTS.md/CLAUDE.md) block is head+tail-trimmed to `instructions_trim_chars`; tool descriptions are one-lined (first sentence, ≤ 120 chars) |
| ≥ 75% | tools_prune | The tool set is pruned to the tools observed in use this session plus `protected_tools` |

Layers are cumulative (each includes the ones before it) and ordered by value per token shed: description prose first, then unused tools. History is deliberately not part of the ladder — the existing layers ([Context compaction](#context-compaction-disable_compaction), the `compaction_trigger_tokens` trigger, the `max_payload_bytes` safety net, hard drop-oldest) stay deeper and unchanged, so every reduction here is reversible.

```json
{ "name": "qwen-local", "url": "http://localhost:8090", "model": "qwen2.5-coder", "provider": "local",
  "context_window_tokens": 8192,
  "context_reduction": {
    "mode": "auto",
    "instruction_threshold_pct": 55, "tool_compact_threshold_pct": 65, "tool_prune_threshold_pct": 75,
    "instructions_trim_chars": 4000, "protected_tools": ["bash", "read_file"]
  } }
```

| Field | Type | Default | Description |
|---|---|---|---|
| `mode` | string | `"auto"` | `"auto"` runs the ladder; `"off"` disables it entirely (behavior exactly as before). Any other value is a config-load error. |
| `instruction_threshold_pct` | int | 55 | Pressure % where the instructions layer starts (tier step-down, guidance blocks dropped) |
| `tool_compact_threshold_pct` | int | 65 | Pressure % where tool descriptions are one-lined and the tier is forced to `minimal` |
| `tool_prune_threshold_pct` | int | 75 | Pressure % where the tool set is pruned to observed-in-use + `protected_tools` |
| `instructions_trim_chars` | int | 4000 | Characters of the AGENTS.md/CLAUDE.md block kept (head + tail) at the tools_compact layer |
| `protected_tools` | array of string | `["bash", "read_file"]` | Tools never pruned, regardless of use |

All fields optional. Thresholds are clamped into 1–99 and kept in ascending order (a later threshold is raised to the earlier one, never the reverse). The ladder only kicks in when the model's context size is known — `context_window_tokens` set explicitly or matched by the [models.dev lookup](#context-window-declaration-context_window_tokens); with no known window, or `mode: "off"`, behavior is exactly as before. Each active layer is counted by the OTel counter `milk.inference.context_reduction` (attribute `layer`: `instructions`|`tools_compact`|`tools_prune`).

Scope: local agents (OpenAI-compatible, Bedrock native, Responses API) only — `claude-cli` and subprocess runners manage their own context and are unaffected. Background jobs get the tool-side layers only (their prompt is already minimal). Custom `prompt`/`prompt_file` text and percepts/current-need messages are never dropped by the ladder.

---

### Project instructions (`disable_project_instructions`)

milk loads the target repo's own `AGENTS.md` into the primary/local agent's system prompt, the same way Claude Code loads `CLAUDE.md` and OpenCode loads `AGENTS.md` — repo-specific conventions the model wouldn't otherwise know. Searches upward from the working directory to the repo root (first `AGENTS.md` found wins); falls back to `CLAUDE.md` when no `AGENTS.md` exists, since a local model has no other path to it (the escalation agent's own Claude CLI subprocess already loads `CLAUDE.md` natively, so milk only injects `AGENTS.md` on that path, never `CLAUDE.md`, to avoid sending it twice). Content is cached by file mtime and capped at 12,000 chars.

On by default. Set `"disable_project_instructions": true` on an agent entry to turn it off — e.g. a repo's instructions file is irrelevant or wrong for that particular agent.

```json
{ "name": "qwen-local", "url": "http://localhost:8090", "model": "qwen2.5-coder", "provider": "local",
  "disable_project_instructions": true }
```

---

### Context compaction (`disable_compaction`)

When an agent's message history exceeds its budget (`message_budget_chars`/`local_context_budget_chars` — see [Context budget configuration](#context-budget-configuration)), the oldest turns are normally dropped outright. With compaction enabled (the default), milk instead makes one extra inference call to summarize exactly the span that would be dropped, splicing the summary in as a single message instead of discarding that context — so a fact or decision from early in a long session survives as a summary rather than vanishing outright. Falls back to a plain drop when there's nothing to summarize, the call fails, or the provider doesn't support it yet (Bedrock, the Responses API).

Set `"disable_compaction": true` on an agent entry to skip the extra call's latency/cost and go back to a plain drop-oldest-first trim — e.g. for a model that summarizes poorly.

```json
{ "name": "qwen-local", "url": "http://localhost:8090", "model": "qwen2.5-coder", "provider": "local",
  "disable_compaction": true }
```

---

### Bash command pre-approval (`bash_allowed_patterns`)

Permission grants normally apply to a whole tool at once — once `bash` is granted, every bash command is approved for the rest of the session, with no way to pre-approve just the safe, read-only commands while still gating riskier ones. `bash_allowed_patterns` is a static, per-agent allow-list of bash command prefixes that skip the permission ask/grant entirely, checked before it: an entry ending in `*` matches by prefix (`"git diff*"` matches `git diff --stat HEAD`); an entry with no `*` must match the whole command exactly. A command that doesn't match any pattern falls through to the normal grant/ask flow, completely unchanged — this only adds a fast, safe pre-approval path, it never narrows what a plain `bash` grant already allows.

```json
{ "name": "qwen-local", "url": "http://localhost:8090", "model": "qwen2.5-coder", "provider": "local",
  "bash_allowed_patterns": ["git status*", "git diff*", "git log*"] }
```

---

### Post-edit check (`post_edit_command`)

Set `post_edit_command` on an agent entry to get formatter/compiler feedback appended to every successful `edit_file`/`write_file` tool result — the model sees `gofmt`/`go vet`/`tsc` output without spending a separate `bash` call (and without leaving a broken file for the next turn to discover). The command runs via `sh -c` in the session working directory with a 60 s cap; its combined stdout/stderr is truncated to 4 KB and appended as a `[post-edit check: <cmd> — ok|exit N]` block. Failed edits never trigger it. Default: disabled (empty).

```json
{ "name": "local",
  "url": "http://localhost:8080",
  "post_edit_command": "go vet ./..." }
```

### Custom agent behaviour (`prompt` / `prompt_file`)

Any agent entry can carry a custom system prompt, **prepended** to milk's default on every turn.

```json
{ "name": "local", "url": "http://localhost:8080", "model": "qwen2.5-coder",
  "prompt": "You are a strict code reviewer. Only respond in bullet points.\n\nAvailable tools: {{milk:tools}}" }
```

Or from a file (wins over `prompt` if both are set — a config warning is shown at startup):

```json
{ "name": "local", "url": "http://localhost:8080", "model": "qwen2.5-coder",
  "prompt_file": "~/.milk/prompts/code-reviewer.md" }
```

#### Placeholders

| Placeholder | Substituted with |
|---|---|
| `{{milk:memory}}` | The current remembered-facts block from percepts |
| `{{milk:need}}` | The session's current need description |
| `{{milk:escalation}}` | The last escalation-agent summary |
| `{{milk:tools}}` | Comma-separated list of built-in primary-agent tool names |

An empty value removes the placeholder silently. If **no** `{{milk:*}}` placeholder is present, a compact `*(milk context injected below)*` footer is auto-appended so milk's own context still reaches the agent.

#### Wizard

`/agent add` asks a behaviour step after required fields: press Enter to skip, type inline text to set `prompt`, or `file=/path/to/prompt.md` to set `prompt_file`.

---

### OpenAI Responses API

Enable the [Responses API](https://platform.openai.com/docs/api-reference/responses) wire format on any local or Bearer-auth entry with `"api_format": "responses"`.

```json
{ "name": "local-responses", "url": "http://localhost:8080", "model": "qwen2.5-coder", "api_format": "responses" }
```

Defaults the endpoint to `/v1/responses` (override with `chat_path`), skips the `/health` probe, translates message history (`tool` role → `function_call_output`, assistant `tool_calls` → `function_call`), flattens tool schemas, and uses Responses-style SSE events. The default for HTTP agents otherwise is `"chat_completions"` (`/v1/chat/completions`).

Real example — an enterprise Copilot proxy using Responses instead of Chat Completions, alongside a bearer `token_cmd`:

```json
{
  "name": "copilot-lite", "provider": "bearer",
  "url": "https://copilot-api.your-enterprise-ghe.example.com", "model": "gpt-5-mini",
  "token_cmd": "gh auth token --hostname your-enterprise-ghe.example.com",
  "headers": { "Copilot-Integration-Id": "vscode-chat", "X-GitHub-Api-Version": "2026-01-09" },
  "api_format": "responses"
}
```

---

### Environment-provided keys (`api_key_env`)

Point an agent at a key that already lives in the environment instead of copying it into `config.json`: `"api_key_env": "OPENROUTER_API_KEY"` — milk resolves the variable when the key is needed, so the config stores only a pointer, never the secret itself, and rotating the key needs no config edit. A literal `api_key` on the same entry still wins over the environment. `/config init` writes keys this way when its setup scan finds them (name + masked fingerprint only).

```json
{ "name": "openrouter", "provider": "bearer", "url": "https://openrouter.ai/api/v1", "model": "anthropic/claude-sonnet-4.5", "api_key_env": "OPENROUTER_API_KEY" }
```

### Dynamic token providers (`token_cmd`)

For providers using short-lived tokens managed by an external CLI (company SSO, a vault CLI, a cloud provider's auth tool), use `token_cmd` instead of a static `api_key`. milk runs the command at startup, uses stdout as the Bearer token, and retries with a fresh token on 401/403 — run via `sh -c`, so shell syntax and env vars work.

Real example — an enterprise GitHub Copilot proxy, combining `token_cmd`, custom headers, and exposing another agent as a tool (`tools`):

```json
{
  "name": "copilot-enterprise", "provider": "bearer",
  "url": "https://copilot-api.your-enterprise-ghe.example.com", "model": "claude-sonnet-4.6",
  "token_cmd": "gh auth token --hostname your-enterprise-ghe.example.com",
  "headers": {
    "Copilot-Integration-Id": "vscode-chat",
    "Editor-Plugin-Version": "copilot-chat/0.49.0",
    "Editor-Version": "vscode/1.121.0",
    "X-GitHub-Api-Version": "2026-01-09"
  },
  "chat_path": "/chat/completions",
  "context_window_tokens": 200000,
  "tools": [
    { "agent": "aider", "description": "aider is a coding agent that directly reads source code files and applies the requested changes" }
  ]
}
```

See [docs/guides/tooling.md — Agent-as-Tool](tooling.md#agent-as-tool) for what the `tools` field does.

---

## Full config reference

### `agents` entry fields

Common to all inference-server providers (everything except `claude-cli`):

| Field | Type | Description |
|---|---|---|
| `name` | string | Unique backend name, used by `/agent switch` |
| `url` | string | Base URL of the inference server |
| `model` | string | Model name or ARN |
| `provider` | string | Auth transport: `""`/`"local"` = none, `"bedrock"` = SigV4, anything else = Bearer |
| `api_key` | string | Static Bearer token or API key |
| `token_cmd` | string | Shell command to fetch a dynamic Bearer token; wins over `api_key` |
| `headers` | object | Extra HTTP headers injected on every request |
| `chat_path` | string | Override the inference endpoint path (default `/v1/chat/completions`) |
| `api_format` | string | `""`/`"chat_completions"` (default) or `"responses"` |
| `tls_skip_verify` | bool | Disable TLS cert verification (dev/self-signed only) |
| `tls_ca_cert` | string | Path to PEM CA cert for private endpoints |
| `aws_region`, `aws_key_id`, `aws_secret`, `aws_token`, `aws_service`, `aws_refresh_cmd` | — | Bedrock-only, see [AWS Bedrock](#aws-bedrock) |
| `prompt_caching` | bool | Bedrock-only, experimental — see [Prompt caching](#prompt-caching-prompt_caching--️-experimental-not-live-tested) |
| `context_window_tokens` | int | See [Context window declaration](#context-window-declaration-context_window_tokens) |
| `system_prompt_tier` | string | See [System prompt verbosity](#system-prompt-verbosity-system_prompt_tier) |
| `project_map_context` | string/bool | See [Project map context](#project-map-context-project_map_context) |
| `context_reduction` | object | See [Layered context reduction](#layered-context-reduction-context_reduction) |
| `disable_project_instructions` | bool | See [Project instructions](#project-instructions-disable_project_instructions) |
| `disable_compaction` | bool | See [Context compaction](#context-compaction-disable_compaction) |
| `bash_allowed_patterns` | array of string | See [Bash command pre-approval](#bash-command-pre-approval-bash_allowed_patterns) |
| `post_edit_command` | string | See [Post-edit check](#post-edit-check-post_edit_command) |
| `prompt` / `prompt_file` | string | See [Custom agent behaviour](#custom-agent-behaviour-prompt-prompt_file) |
| `mcp_servers` | array of string | See [docs/guides/tooling.md](tooling.md#mcp-servers) |
| `tools` | array of AgentToolEntry | Per-agent overrides of the global `agent_tools` list — see [docs/guides/tooling.md](tooling.md#agent-as-tool) |
| `limits` | object | Per-agent overrides — see [Per-agent limit overrides](#per-agent-limit-overrides) |

### Claude CLI `agents` entry fields (`provider: "claude-cli"`)

| Field | Type | Description |
|---|---|---|
| `name` | string | Unique backend name |
| `provider` | string | Must be `"claude-cli"` |
| `bin` | string | Path to the `claude` binary (default `"claude"`) |
| `dangerously_skip_permissions` | bool | Auto-approve all tool uses |
| `allowed_tools` | array of string | Pre-approved tools; `--allowedTools` |
| `add_dirs` | array of string | Extra directories; `--add-dir` |
| `settings` | object | Passed via `--settings`; same schema as `settings.local.json` |

### Root config fields related to agents

| Field | Type | Default | Description |
|---|---|---|---|
| `agent` | string | first non-cli entry | Name of the active primary backend |
| `escalation_agent` | string | `"claude"` | Name of the escalation backend |
| `aws_auth_refresh` | bool | `false` | Run the Claude Code credential-process command before each Bedrock call |
| `sticky_escalation` | bool | `true` | See [docs/guides/workflows.md — Sticky and auto-sticky escalation](workflows.md#sticky-and-auto-sticky-escalation) |
| `copy_on_select` | bool | `false` | Copy a mouse selection to the clipboard on button release instead of requiring right-click / Ctrl+C |
| `animations` | bool | `true` | Spring animations in the TUI: side panels slide in and out, the mouse wheel scrolls with inertia. `false` makes both instant |
| `remember_panel_state` | bool | `true` | Durable side-panel preferences (ADR-0078): panel open/closed choices persist per folder to `~/.milk/panel-state/*.json` and survive content finalization. `false` = session-only panels with ADR-0059's expiry |

---

## Memory configuration

All keys go in `~/.milk/config.json`; sensible defaults apply when omitted. See [docs/guides/operations.md — Memory](operations.md#memory) for the concepts and commands these tune.

| Key | Default | Description |
|---|---|---|
| `percept_inject_max` | 25 | Max percepts injected into the escalation agent context per turn. `0` = no limit. |
| `percept_inject_max_bytes` | 2048 | Max byte size of injected percept content per turn. `0` = no limit. |
| `percept_store_max` | 0 (unlimited) | Max percepts kept in the global store; lowest-weight non-core percepts are pruned after NREM consolidation. |
| `percept_relevance_gate` | `true` | Skip percepts with zero keyword overlap with the current prompt before injection. |
| `memory_reinjection_turns` | 20 | Re-inject memory/need instructions into escalation context after this many escalation turns. `0` disables. |
| `memory_reinjection_bytes` | 40000 | Re-inject after this many bytes of escalation output. `0` disables. |
| `local_memory_result_max_bytes` | 2048 | Max byte size of `get_memory`/`list_memory` results to the primary agent. `-1` = no limit. |
| `local_tool_result_max_bytes` | derived (see below) | Max byte size of any other tool result (`bash`, `read_file`, …) to the primary agent, keeping both head and tail (the error/exit status at the end of shell/build/test output survives, not just the head). Unlike memory tool results, these had no cap at all before this setting — a single verbose shell/build/test output could otherwise balloon a turn's payload well before the payload-size trim loop ever ran. `0` = no limit. **Default when unset:** ~10% of the agent's context window (`context_window_tokens`, or the models.dev lookup) clamped to 4–50 KB — so a 1M-token model gets the standard 50 KB (the cap OpenCode, MiMo-Code and Claude Code use) while a 32K window gets ~12.8 KB; 20000 when the window is unknown. A capped result keeps its head and tail and the omission marker says how to read the rest: `read_file` offset/limit, or — for `bash`, `grep` and other tools — the path of a file under `~/.milk/tool-output/` holding the full output (private, removed after 7 days), which the model can `grep` or page with `read_file`. |
| `local_memory_reinjection_turns` | 20 | Re-inject into the primary agent's context after this many local turns. `-1` disables. |
| `local_memory_reinjection_bytes` | 40000 | Re-inject after this many bytes of primary agent output. `-1` disables. |
| `local_max_tool_iterations` | — (unset) | Global override for the per-agent tool-iteration budget. Unset = no global cap: each agent resolves its own (`max(20, context_window_tokens/4096)`, or **40** when the window is unknown). Set `> 0` to force one cap for every agent; `-1` = unlimited. |

---

## Context budget configuration

| Key | Default | Description |
|---|---|---|
| `context_budget_chars` | 12000 | Max characters per summary brick injected into the escalation system prompt; oldest turns dropped first. |
| `local_context_budget_chars` | 24000 | Max total characters in the primary agent's `messages` array per turn. When over budget, the oldest turns that would be dropped are summarized into one message instead (see [Context compaction](#context-compaction-disable_compaction)) unless `disable_compaction` is set, in which case they're dropped outright as before. `0` = no limit. |

---

## Per-agent limit overrides

Any `agents` entry accepts a `limits` object overriding global context/memory settings for that agent specifically — e.g. a tighter context window for a small Bedrock model without affecting the primary agent.

```json
{
  "agents": [
    { "name": "haiku-aws", "provider": "bedrock", "model": "anthropic.claude-haiku-4-5",
      "limits": {
        "context_budget_chars": 6000, "message_budget_chars": 12000,
        "percept_inject_max": 5, "percept_inject_max_bytes": 512,
        "memory_result_max_bytes": 1024, "tool_result_max_bytes": 8000, "memory_reinjection_turns": 10,
        "memory_reinjection_bytes": 20000, "percept_relevance_gate": true
      } }
  ]
}
```

All fields optional; omitted → global value applies.

**Integer semantics**: omitted/`null` = use global config value; `0` = built-in hardcoded default; positive = exact value; negative (e.g. `-1`) = disabled/unlimited.

| Field | Global key | Built-in default | Description |
|---|---|---|---|
| `context_budget_chars` | `context_budget_chars` | 12000 | Max chars per summary brick |
| `message_budget_chars` | `local_context_budget_chars` | 24000 | Max chars in message history per turn |
| `percept_inject_max` | `percept_inject_max` | 25 | Max percepts injected per turn |
| `percept_inject_max_bytes` | `percept_inject_max_bytes` | 2048 | Max total bytes of injected percept content |
| `memory_result_max_bytes` | `local_memory_result_max_bytes` | 2048 | Max bytes of a memory tool result |
| `tool_result_max_bytes` | `local_tool_result_max_bytes` | derived from window (4–50 KB; 20000 if unknown) | Max bytes of any other tool result (`bash`, `read_file`, …) |
| `memory_reinjection_turns` | `memory_reinjection_turns`/`local_memory_reinjection_turns` | 20 | Re-inject after N turns |
| `memory_reinjection_bytes` | `memory_reinjection_bytes`/`local_memory_reinjection_bytes` | 40000 | Re-inject after N bytes of output |
| `percept_relevance_gate` | `percept_relevance_gate` | `true` | Keyword-intersection filter before injection |
| `max_tool_iterations` | `local_max_tool_iterations` | `max(20, window/4096)`; **40** when the window is unknown | Max tool-call cycles per turn (`-1` = unlimited). A session whose milk-loop agent lands on the 40 fallback prints a start-of-session notice suggesting `context_window_tokens` or this key. |
| `compaction_trigger_tokens` | — | 85% of (window − output reserve) | Prompt size (tokens, as the provider reports it for the previous request) at which a running turn summarizes its own older history instead of letting a byte trim hard-drop it. The reserve is the models.dev output limit capped at 32K (20K if unlisted) and at a quarter of the window. Needs a known `context_window_tokens`; `-1` disables. The [`context_reduction`](#layered-context-reduction-context_reduction) pressure thresholds use the same window − output-reserve denominator. |
| `max_payload_bytes` | — | 900 KB, or ~4 bytes × window above that | Max HTTP request body; past it, history is trimmed per request (a safety net for reverse proxies — the token trigger above is the primary mechanism). |
| `included_tools` | — | (all) | Whitelist of built-in tools for this agent |
| `excluded_tools` | — | (none) | Built-in tools to remove for this agent (applied after `included_tools`) |
| `tool_timeout_secs` | — | 120 | See [docs/guides/tooling.md — Tool batch dispatch](tooling.md#tool-batch-dispatch-barrier-model) |
| `turn_timeout_secs` | — | 600 | **Advisory** idle warning for a turn: measured from the last activity event (streamed output, tool start/end, retries), warned once per idle stretch and re-armed when work resumes. `-1` disables. Never ends a turn. |
| `turn_deadline_secs` | — | off (0/-1) | **Opt-in** wall-clock turn deadline: at the deadline milk asks the agent to wrap up with a forced text-only closing summary (milk-loop agents) or sends a wrap-up notice (CLI/subprocess agents), then hard-stops the turn after a grace period if it will not end (immediately at the deadline+grace for external-process agents, which cannot be steered; a milk-loop turn that is actively writing its summary is never killed). Set generously — this is a real bound on turn length. |

> **Turn bounds, all three of them**: `turn_timeout_secs` only warns (slow ≠ stuck — the clock resets on activity), `turn_deadline_secs` is the optional real bound (graceful summary first, hard stop only as a last resort), and `agentTimeout`'s 10-minute constant bounds only milk's *routing/classifier* step — it never runs or stops an agent turn. Runaway turns are otherwise bounded by the doom-loop gates (3 consecutive identical tool-call iterations, duplicate-call ladder, reasoning n-gram monitor), the per-tool timeout, and Ctrl+C / `/interrupt`.

> **Large context window agents**: set `context_window_tokens` and let milk auto-derive `message_budget_chars`/`max_tool_iterations`; `limits` overrides remain available for exact values. Both `milk config init` and the TUI's `/agent add` prompt for it, proposing a models.dev catalog match as the default when the model name is recognized.

> **Small local models**: set `system_prompt_tier: "minimal"` and use `limits.included_tools` to restrict the tool set to the 7–8 tools the model will actually use — recovers ~700 tokens of prompt and ~1000–1500 tokens of tool-schema space per turn.

That recipe is the static alternative: decide once at config time and pay for it at every pressure level. The dynamic alternative is the automatic [layered context reduction](#layered-context-reduction-context_reduction) ladder (`context_reduction`, on by default): as pressure climbs it steps the prompt tier down, trims the project instructions, one-lines tool descriptions, and finally prunes the tool set to what the session actually uses plus a protected core — then eases back on its own whenever pressure drops. Pick the static recipe when you want minimal prompts even at low pressure; let the ladder handle the rest.

---

## More configuration

- **MCP servers, tool-agents, attachments** — [docs/guides/tooling.md](tooling.md)
- **Memory usage, observability, loop detection, remote oversight, task tracking** — [docs/guides/operations.md](operations.md)
- **Routing rules, sticky escalation, the `/workflow` engine** — [docs/guides/workflows.md](workflows.md)
