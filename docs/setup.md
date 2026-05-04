# Setup and local testing

## Prerequisites

| Dependency | Required | Notes |
|---|---|---|
| Go 1.21+ | yes | build only |
| NVIDIA driver | for GPU | WSL2: check with `nvidia-smi` |
| CUDA toolkit 12.x | for GPU | see step 1 |
| cmake + build-essential | yes | build llama.cpp |
| llama.cpp | yes | built from source |
| claude CLI | no | degrades to local-only if absent |

---

## Step 1 — CUDA toolkit (skip if CPU-only)

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

## Step 2 — Build dependencies

```sh
sudo apt install -y cmake build-essential git
```

---

## Step 3 — Build llama.cpp

```sh
git clone https://github.com/ggml-org/llama.cpp ~/llama.cpp
cd ~/llama.cpp

# GPU build (Ada Lovelace = sm_89)
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

## Step 4 — Download the model

```sh
pip3 install huggingface_hub

# Recommended: Q4_K_M (~4.1 GB, fits in 4 GB VRAM)
huggingface-cli download \
  bartowski/Qwen2.5-Coder-7B-Instruct-GGUF \
  Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf \
  --local-dir ~/models/qwen2.5-coder-7b
```

If VRAM is tight (OOM during inference), use Q3_K_M (~3.2 GB):

```sh
huggingface-cli download \
  bartowski/Qwen2.5-Coder-7B-Instruct-GGUF \
  Qwen2.5-Coder-7B-Instruct-Q3_K_M.gguf \
  --local-dir ~/models/qwen2.5-coder-7b
```

---

## Step 5 — Start the server

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

## Step 6 — Build and verify milk

```sh
go build -o milk ./cmd/milk
./milk config
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
go test ./...
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
