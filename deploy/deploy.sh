#!/usr/bin/env bash
# Build cerber for linux/amd64 and ship it to firebat as a Docker stack.
#
#   make deploy            # uses defaults / .env
#   DEPLOY_HOST=... make deploy
#
# Reads deploy target + secrets from .env (same file cerber loads). The OAuth
# tokens in ./auths and the secret .env are copied over SSH on the LAN only.
# Auth: SSH key by default; set DEPLOY_SSH_PASS=... to use password (sshpass).
set -euo pipefail
cd "$(dirname "$0")/.."

# shellcheck disable=SC1091
[ -f .env ] && { set -a; . ./.env; set +a; }

HOST="${DEPLOY_HOST:-192.168.88.35}"
USER="${DEPLOY_USER:-ruslan}"
DIR="${DEPLOY_DIR:-/opt/cerber}"

SSH="ssh -o StrictHostKeyChecking=accept-new"
RSYNC_RSH="ssh -o StrictHostKeyChecking=accept-new"
if [ -n "${DEPLOY_SSH_PASS:-}" ]; then
  command -v sshpass >/dev/null || { echo "DEPLOY_SSH_PASS set but sshpass not installed" >&2; exit 1; }
  SSH="sshpass -p $DEPLOY_SSH_PASS ssh -o StrictHostKeyChecking=accept-new"
  RSYNC_RSH="sshpass -p $DEPLOY_SSH_PASS ssh -o StrictHostKeyChecking=accept-new"
fi
TARGET="$USER@$HOST"

echo "› building cerber (linux/amd64)"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/cerber ./cmd/cerber

echo "› preparing $TARGET:$DIR"
# shellcheck disable=SC2086
$SSH "$TARGET" "mkdir -p $DIR/auths $DIR/logs $DIR/data && chmod 700 $DIR"

echo "› syncing artifacts"
rsync -az --rsh="$RSYNC_RSH" \
  dist/cerber \
  deploy/Dockerfile \
  deploy/docker-compose.yml \
  deploy/config.firebat.yaml \
  .env \
  "$TARGET:$DIR/"
# OAuth tokens. cerber refreshes them server-side and Anthropic ROTATES the
# refresh token on every refresh, so firebat's copy is the source of truth —
# overwriting it with a stale local copy kills the credential (invalid_grant).
# Default: seed only missing accounts (--ignore-existing), never clobber.
# After a local re-login (`cerber --claude-login`), push the fresh token with
#   DEPLOY_PUSH_CREDS=1 make deploy
if [ "${DEPLOY_PUSH_CREDS:-}" = "1" ]; then
  echo "  (DEPLOY_PUSH_CREDS=1: force-pushing local auths, overwriting server tokens)"
  rsync -az --rsh="$RSYNC_RSH" auths/ "$TARGET:$DIR/auths/"
else
  rsync -az --ignore-existing --rsh="$RSYNC_RSH" auths/ "$TARGET:$DIR/auths/"
fi

echo "› building image + restarting container on firebat"
# shellcheck disable=SC2086
$SSH "$TARGET" "cd $DIR && docker compose up -d --build && sleep 2 && docker compose ps"

echo "› health check (inside host)"
# shellcheck disable=SC2086
$SSH "$TARGET" "curl -fsS http://127.0.0.1:18080/healthz && echo" || echo "  (healthz not ready yet — check: cd $DIR && docker compose logs)"

echo "✓ deployed. https://cerber.ihatebot.com (LAN keyless / public needs the client key)"
