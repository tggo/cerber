#!/usr/bin/env bash
# Blue-green rollout, run on the firebat host from the deploy dir (/opt/cerber).
# Builds the image, starts the idle colour, waits for it to pass /healthz, then
# gracefully stops the previously-active colour. The host Caddy load-balances both
# ports with active health checks, so it follows the live container automatically
# — no Caddy reload, no sudo, no downtime.
set -euo pipefail

PORT_A=18080
PORT_B=18082   # 18081 is taken by radioclubnikolaev

running() { docker ps --format '{{.Names}}' | grep -qx "$1"; }
healthy() { # $1 = port
  for _ in $(seq 1 30); do
    if curl -fsS "http://127.0.0.1:$1/healthz" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

echo "› building image"
docker compose build

# Legacy single container (pre-blue-green) holds :18080; treat it as active and
# deploy into the other colour so the migration is also zero-downtime.
legacy=0
running cerber && legacy=1

if running cerber-a; then
  active=cerber-a; idle=cerber-b; idle_port=$PORT_B
elif running cerber-b; then
  active=cerber-b; idle=cerber-a; idle_port=$PORT_A
elif [ "$legacy" = 1 ]; then
  active=""; idle=cerber-b; idle_port=$PORT_B   # legacy occupies 18080 (a)
else
  active=""; idle=cerber-a; idle_port=$PORT_A   # cold start
fi
echo "› active=${active:-${legacy:+legacy-cerber}}; bringing up idle=$idle (:$idle_port)"

docker compose up -d --no-deps --force-recreate "$idle"

if ! healthy "$idle_port"; then
  echo "✗ $idle failed health check on :$idle_port — leaving the old one in place"
  docker compose logs --tail=40 "$idle" || true
  docker compose stop "$idle" >/dev/null 2>&1 || true
  exit 1
fi
echo "› $idle healthy; Caddy will route to it"

# Give Caddy's active health check a moment to mark the new colour up before we
# retire the old one (belt-and-suspenders; dial-retry covers the gap anyway).
sleep 4

if [ "$legacy" = 1 ]; then
  echo "› removing legacy single container 'cerber'"
  docker rm -f cerber >/dev/null 2>&1 || true
fi
if [ -n "$active" ]; then
  echo "› draining + stopping old active=$active"
  docker compose stop "$active"
fi

docker compose ps
echo "✓ rollout complete (serving from $idle)"
