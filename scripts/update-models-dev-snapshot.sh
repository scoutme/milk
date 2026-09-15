#!/usr/bin/env bash
# Regenerates internal/modelsdev/snapshot.json, the build-time baseline
# consulted by Config.AgentContextWindowTokens's models.dev fallback (see
# internal/modelsdev/modelsdev.go). Run periodically and commit the result —
# model context windows essentially never change once published, so this
# doesn't need to run on every build.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$repo_root/internal/modelsdev/snapshot.json"

curl -sS --fail --max-time 30 https://models.dev/api.json -o "$out"
python3 -c "
import json
with open('$out') as f:
    data = json.load(f)
with open('$out', 'w') as f:
    json.dump(data, f, separators=(',', ':'))
"
echo "wrote $out ($(wc -c < "$out") bytes)"
