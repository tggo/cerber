#!/usr/bin/env bash
# verify-claude.sh — end-to-end check that the real `claude -p` CLI works THROUGH
# cerber against the real Anthropic API.
#
#   real `claude -p`  ->  cerber (/v1/messages)  ->  api.anthropic.com
#
# It builds cerber, starts it with the .env PLAYGROUND_API_KEY as the upstream
# credential, points Claude Code at cerber via ANTHROPIC_BASE_URL, and runs a
# tiny prompt. Requires: a .env with PLAYGROUND_API_KEY, and the `claude` CLI.
#
# Usage:  ./scripts/verify-claude.sh
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

[ -f .env ] || { echo "✗ .env not found (need PLAYGROUND_API_KEY)"; exit 1; }
command -v claude >/dev/null || { echo "✗ 'claude' CLI not found on PATH"; exit 1; }

PORT="${PORT:-8099}"
ACCESS_KEY="verify-claude-key"
MODEL="${MODEL:-claude-haiku-4-5-20251001}"

# Temp config: client access key + Anthropic credential pulled from .env.
CFG="$(mktemp -t cerber-verify.XXXXXX.yaml)"
cat > "$CFG" <<YAML
server: { addr: "127.0.0.1:${PORT}" }
logging: { level: "debug" }
access: { keys: ["${ACCESS_KEY}"] }
providers:
  anthropic:
    credentials:
      - { type: api_key, name: playground, key: "\${PLAYGROUND_API_KEY}" }
YAML

echo "› building cerber"
go build -o bin/cerber ./cmd/cerber

echo "› starting cerber on 127.0.0.1:${PORT}"
./bin/cerber -config "$CFG" -env "${ROOT}/.env" >/tmp/cerber-verify.log 2>&1 &
CERBER_PID=$!
cleanup() { kill "$CERBER_PID" 2>/dev/null || true; rm -f "$CFG"; }
trap cleanup EXIT

# Wait for health.
for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null && break
  sleep 0.2
done

echo "› running: claude -p through cerber (model ${MODEL})"
OUT="$(ANTHROPIC_BASE_URL="http://127.0.0.1:${PORT}" \
      ANTHROPIC_API_KEY="${ACCESS_KEY}" \
      ANTHROPIC_MODEL="${MODEL}" \
      claude -p "Reply with exactly one word: pong" --model "${MODEL}" 2>/tmp/claude-verify.err)" || {
  echo "✗ claude -p failed:"; cat /tmp/claude-verify.err; echo "--- cerber log ---"; tail -20 /tmp/cerber-verify.log; exit 1;
}

echo "claude -p output: ${OUT}"
if echo "$OUT" | grep -qi pong; then
  echo "✓ PASS — real claude -p went through cerber to Anthropic"
else
  echo "✗ unexpected output (no 'pong')"; echo "--- cerber log ---"; tail -20 /tmp/cerber-verify.log; exit 1
fi

echo "--- cerber request log (last lines) ---"
grep -i '"request"' /tmp/cerber-verify.log | tail -3 || true
