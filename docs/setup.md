# Setup and local testing

> **Scenario note:** This guide covers a specific reference setup: NVIDIA GPU on Ubuntu/WSL2, llama.cpp built from source, Qwen2.5-Coder 7B. The parameters (CUDA architecture, quant size, GPU layer count, context size) will differ for other hardware. For a general llama.cpp installation reference see the [official llama.cpp documentation](https://github.com/ggml-org/llama.cpp).

## Prerequisites

| Dependency | Required | Notes |
|---|---|---|
| Go 1.21+ | yes | build only |
| llama.cpp server | no | any OpenAI-compatible local inference server works; degrades to Claude-only if absent |
| claude CLI | no | degrades to local-only if absent |

---

## Local inference backend

milk communicates with the local model via the OpenAI-compatible API (default `http://localhost:8080`). Any server that exposes this interface can be used. llama.cpp is the reference option, but alternatives such as [Ollama](https://ollama.com), [LM Studio](https://lmstudio.ai), or [vLLM](https://github.com/vllm-project/vllm) work as long as:

- the endpoint matches `llama_url` in `~/.milk/config.json`
- the loaded model supports function/tool calling (Qwen2.5-Coder recommended)

For general llama.cpp installation instructions see the [official llama.cpp README](https://github.com/ggml-org/llama.cpp). The steps below document the reference setup.

---

## Reference setup: NVIDIA GPU, Ubuntu/WSL2, llama.cpp from source

### Step 1 — CUDA toolkit (skip if CPU-only)

```sh
wget https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb
sudo dpkg -i cuda-keyring_1.1-1_all.deb
sudo apt update
sudo apt install -y cuda-toolkit-12-8
```

Add to `~/.zshrc` (or `~/.bashrc`):

```sh
export PATH=/usr/local/cuda-12.8/bin:$PATH
export LD_LIBRARY_PATH=/usr/local/cuda-12.8/lib64:$LD_LIBRARY_PATH
```

Verify: `nvcc --version`

---

### Step 2 — Build dependencies

```sh
sudo apt install -y cmake build-essential git
```

---

### Step 3 — Build llama.cpp

```sh
git clone https://github.com/ggml-org/llama.cpp ~/llama.cpp
cd ~/llama.cpp

# GPU build — adjust -DCMAKE_CUDA_ARCHITECTURES for your GPU:
#   Ada Lovelace (RTX 40xx, RTX 500/1000 Ada): 89
#   Ampere (RTX 30xx): 86
#   Turing (RTX 20xx): 75
cmake -B build \
  -DGGML_CUDA=ON \
  -DCMAKE_CUDA_ARCHITECTURES=89
cmake --build build --config Release -j$(nproc)
```

For CPU-only, omit the CUDA flags:

```sh
cmake -B build
cmake --build build --config Release -j$(nproc)
```

The server binary is at `~/llama.cpp/build/bin/llama-server`.

---

### Step 4 — Download the model

The reference model is **Qwen2.5-Coder-7B-Instruct**, chosen for its reliable function/tool calling support. Quant size depends on available VRAM:

| Quant | Size | Fits in |
|---|---|---|
| Q4_K_M | ~4.1 GB | 4 GB VRAM (tight) |
| Q3_K_M | ~3.2 GB | 4 GB VRAM (with headroom) |
| Q8_0 | ~7.2 GB | 8 GB VRAM |

Larger VRAM or a different GPU architecture may accommodate the 14B variant. Any Qwen2.5-Coder GGUF with tool calling support can be substituted by adjusting `llama_model` in `~/.milk/config.json`.

```sh
pip3 install hf-xet huggingface_hub[hf_xet,cli]

# Reference: Q4_K_M (~4.1 GB, fits in 4 GB VRAM)
hf download \
  bartowski/Qwen2.5-Coder-7B-Instruct-GGUF \
  Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf \
  --local-dir ~/models/qwen2.5-coder-7b
```

If VRAM is tight (OOM during inference), use Q3_K_M (~3.2 GB):

```sh
hf download \
  bartowski/Qwen2.5-Coder-7B-Instruct-GGUF \
  Qwen2.5-Coder-7B-Instruct-Q3_K_M.gguf \
  --local-dir ~/models/qwen2.5-coder-7b
```

---

### Step 5 — Start the server

```sh
./scripts/llama-serve.sh
```

The script reads defaults for the binary path, model path, port, and GPU layers. Override any of them in `~/.milk/llama.env`:

```sh
# ~/.milk/llama.env
LLAMA_MODEL="$HOME/models/qwen2.5-coder-7b/Qwen2.5-Coder-7B-Instruct-Q3_K_M.gguf"
LLAMA_CTX_SIZE=4096   # reduce if VRAM OOMs
LLAMA_GPU_LAYERS=28   # partial offload: rest runs on CPU
```

Verify the server is up:

```sh
curl http://localhost:8080/health
# {"status":"ok"}
```

Verify tool calls are working (requires `--jinja` flag, already included in the script):

```sh
curl -s http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2.5-coder",
    "messages": [{"role":"user","content":"list go files in current dir"}],
    "tools": [{"type":"function","function":{"name":"bash","description":"run shell command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}],
    "stream": false,
    "temperature": 0.2
  }' | python3 -m json.tool | grep -A5 "tool_calls"
```

Expected: a `tool_calls` array with `"name": "bash"`. If you see the call inside `content` as raw text instead, the `--jinja` flag is missing or the server was started without it.

---

### Step 6 — Build and verify milk

```sh
task build:local   # builds ./milk in the current directory
./milk config
```

Or to install directly to `~/.local/bin`:

```sh
task build
```

A custom destination is also supported:

```sh
task build DEST=/usr/local/bin/milk
```

Expected output:

```
llama_url:      http://localhost:8080
llama_model:    qwen2.5-coder
claude_bin:     claude
default_route:  local
escalate_above_tokens: 2000
escalate_keywords:     [architect refactor entire design explain why]
```

---

## Local testing procedure

### Automated tests (no dependencies)

```sh
task test
```

All tests run without llama.cpp or claude. They use temp directories and mock readers.

### Manual smoke tests

**Config and session management** (no server needed):

```sh
./milk config
./milk --new --session test "hello"
./milk --list
./milk --drop
./milk --list   # should be empty
```

**Local model routing** (llama.cpp must be running):

```sh
# Should route to local, run bash tool, return file list
./milk "list Go files in the current directory"

# Resume: second call should continue the same session
./milk "now show only the test files"

# Force local even if rules would escalate
./milk --local "grep for TODO comments"
```

**Escalation to Claude** (both services running):

```sh
# Force escalation via flag
./milk --escalate "explain the session state machine design"

# Self-escalation: model should call escalate_to_claude()
./milk "design a plugin architecture for milk"

# CLAUDE_WAITING: Claude asks a follow-up; next turn bypasses router
./milk --escalate "what would you need to know to refactor the router?"
./milk "focus on the rules layer"   # goes directly to --resume, no routing

# Break back to local
./milk --local "grep for TODO"
```

**Graceful degradation**:

```sh
# Stop llama.cpp, then:
./milk "list Go files"
# Expected: "[milk] warning: llama.cpp unreachable — routing all to Claude"

# Stop claude (rename binary temporarily), then:
./milk "list Go files"
# Expected: "[milk] warning: claude CLI unavailable — local only"
```

### Troubleshooting

**400 on first local call**: server started without `--jinja`. Restart with `./scripts/llama-serve.sh`.

**VRAM OOM / server crash during inference**: reduce `LLAMA_CTX_SIZE=4096` or `LLAMA_GPU_LAYERS=28` in `~/.milk/llama.env`.

**Tool call appears as raw text in content**: `--jinja` missing (see above).

**Session in bad state**: drop it and start fresh.

```sh
./milk --drop
```
