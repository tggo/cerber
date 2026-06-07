#!/bin/sh
# cerber container entrypoint: ensure the impersonation cert exists, then run.
set -e

CERT_DIR="${CERT_DIR:-/work/certs}"
CONFIG="${CERBER_CONFIG:-/work/config.tls.yaml}"
ENV_FILE="${CERBER_ENV:-/work/.env}"

if [ ! -f "$CERT_DIR/ca.pem" ]; then
  echo "› generating TLS impersonation cert in $CERT_DIR"
  cerber --gen-cert --cert-dir "$CERT_DIR" --impersonate "${IMPERSONATE:-api.anthropic.com}"
fi

# Seed Claude Code's login from auth_dir so it behaves like a normal Max session
# (no API key, no /login prompt). cerber stays the sole token owner.
AUTH_DIR="${AUTH_DIR:-/work/auths}"
if ls "$AUTH_DIR"/*.json >/dev/null 2>&1; then
  echo "› seeding Claude Code credentials from $AUTH_DIR"
  cerber --seed-claude-creds --auth-dir "$AUTH_DIR" || echo "  (no oauth token to seed; run cerber --claude-login)"
fi

exec cerber -config "$CONFIG" -env "$ENV_FILE"
