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

exec cerber -config "$CONFIG" -env "$ENV_FILE"
