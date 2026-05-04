#!/usr/bin/env bash
# Start the llama.cpp server for milk.
# Reads configuration from ~/.milk/llama.env if present (see docs/setup.md).
set -euo pipefail

# Defaults — override in ~/.milk/llama.env
LLAMA_BIN="${LLAMA_BIN:-$HOME/llama.cpp/build/bin/llama-server}"
LLAMA_MODEL="${LLAMA_MODEL:-$HOME/models/qwen2.5-coder-7b/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf}"
LLAMA_HOST="${LLAMA_HOST:-127.0.0.1}"
LLAMA_PORT="${LLAMA_PORT:-8080}"
LLAMA_CTX_SIZE="${LLAMA_CTX_SIZE:-8192}"
LLAMA_GPU_LAYERS="${LLAMA_GPU_LAYERS:-99}"

ENV_FILE="$HOME/.milk/llama.env"
if [[ -f "$ENV_FILE" ]]; then
  # shellcheck source=/dev/null
  source "$ENV_FILE"
fi

if [[ ! -x "$LLAMA_BIN" ]]; then
  echo "error: llama-server not found at $LLAMA_BIN" >&2
  echo "       Set LLAMA_BIN or follow docs/setup.md" >&2
  exit 1
fi

if [[ ! -f "$LLAMA_MODEL" ]]; then
  echo "error: model not found at $LLAMA_MODEL" >&2
  echo "       Set LLAMA_MODEL or follow docs/setup.md" >&2
  exit 1
fi

echo "Starting llama.cpp server"
echo "  binary : $LLAMA_BIN"
echo "  model  : $LLAMA_MODEL"
echo "  listen : $LLAMA_HOST:$LLAMA_PORT"
echo "  ctx    : $LLAMA_CTX_SIZE tokens"
echo "  gpu    : $LLAMA_GPU_LAYERS layers"
echo ""

exec "$LLAMA_BIN" \
  --model        "$LLAMA_MODEL" \
  --host         "$LLAMA_HOST" \
  --port         "$LLAMA_PORT" \
  --ctx-size     "$LLAMA_CTX_SIZE" \
  --n-gpu-layers "$LLAMA_GPU_LAYERS" \
  --flash-attn \
  --jinja
