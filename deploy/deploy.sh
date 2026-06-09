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
  deploy/rollout.sh \
  .env \
  "$TARGET:$DIR/"
# OAuth tokens, synced with --update (-u): push only files that are NEWER locally
# (i.e. a fresh `cerber --claude-login`/`--xai-login`), and NEVER overwrite a
# token the server refreshed more recently. cerber refreshes tokens server-side
# and Anthropic ROTATES the refresh token on every refresh, so clobbering a
# server token with a stale local copy kills the account (invalid_grant). -a
# preserves mtimes so the -u comparison is correct. Set DEPLOY_FORCE_CREDS=1 to
# force-overwrite all (rarely needed).
if [ "${DEPLOY_FORCE_CREDS:-}" = "1" ]; then
  echo "  (DEPLOY_FORCE_CREDS=1: force-overwriting ALL server tokens with local)"
  rsync -az --rsh="$RSYNC_RSH" auths/ "$TARGET:$DIR/auths/"
else
  rsync -auz --rsh="$RSYNC_RSH" auths/ "$TARGET:$DIR/auths/"
fi

echo "› blue-green rollout on firebat (Caddy follows the health check)"
# shellcheck disable=SC2086
$SSH "$TARGET" "cd $DIR && bash rollout.sh"

echo "› health check via the public vhost"
if curl -fsS -m 10 https://cerber.ihatebot.com/healthz >/dev/null 2>&1; then
  echo "  ok"
else
  echo "  (could not reach https://cerber.ihatebot.com/healthz from here)"
fi

echo "✓ deployed (zero-downtime). https://cerber.ihatebot.com"
