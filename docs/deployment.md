# Deploying cerber to firebat

`make deploy` cross-compiles cerber for `linux/amd64`, ships it to **firebat**
(`192.168.88.35`, RentedHouse LAN) and runs it as a Docker container behind the
host nginx. The service is published as **`https://cerber.ihatebot.com`**.

## What `make deploy` does

1. Builds a static `linux/amd64` binary (`dist/cerber`) — pure Go, no qemu.
2. `rsync`s the binary, `deploy/Dockerfile`, `deploy/docker-compose.yml`,
   `deploy/config.firebat.yaml`, `.env`, and the OAuth tokens in `auths/` to
   `/opt/cerber` on firebat (LAN only, over SSH).
3. `docker compose up -d --build` — builds a tiny distroless image (just the
   binary) and (re)starts the `cerber` container.
4. Health-checks `http://127.0.0.1:18080/healthz` on the host.

Config (overridable via `.env` or the environment):

| var | default | meaning |
|---|---|---|
| `DEPLOY_HOST` | `192.168.88.35` | firebat |
| `DEPLOY_USER` | `ruslan` | SSH user |
| `DEPLOY_DIR`  | `/opt/cerber` | remote dir |
| `DEPLOY_SSH_PASS` | _(unset)_ | if set, uses `sshpass`; otherwise SSH key auth |

The container listens on `0.0.0.0:8080` internally and publishes **only** to
`127.0.0.1:18080` on the host — the nginx vhost is the sole front door.

## Network model (key required from everywhere)

- **Internal DNS** (both MikroTiks): `cerber.ihatebot.com → 192.168.88.35`.
  LAN/WG clients hit firebat Caddy directly.
- **Public DNS** (Cloudflare, proxied): `cerber.ihatebot.com → 193.56.148.246`
  → router forwards `:80/:443` → firebat Caddy.
- From **both** sides the client must present its own key
  (`Authorization: Bearer …` or `X-Api-Key`); Caddy forwards it unchanged and
  cerber validates it and attributes the spend to that key.

Until 2026-09-15 the LAN was *keyless*: the vhost injected a shared bearer for
LAN/WG source IPs. That was removed on purpose — every keyless LAN process
landed in one unbudgeted `config` bucket and the per-client accounting could
not say who was spending. Give each program a managed key from the dashboard
instead; the dashboard itself asks for a key on 401 and remembers it.

The vhost source is `deploy/caddy/cerber.caddy`; the live copy is the
`(cerber)` snippet in `/etc/caddy/Caddyfile` on firebat (root-owned).

## One-time firebat setup (already done, documented for rebuilds)

1. Add this workstation's SSH key to `~ruslan/.ssh/authorized_keys`.
2. `sudo mkdir -p /opt/cerber && sudo chown ruslan:ruslan /opt/cerber`.
3. Cloudflare A record `cerber.ihatebot.com → 193.56.148.246` (proxied).
4. MikroTik static DNS on both routers:
   `/ip dns static add name=cerber.ihatebot.com address=192.168.88.35`.
5. `sudo certbot certonly --nginx -d cerber.ihatebot.com` (HTTP-01, auto-renews).
6. Install nginx config (substitute `__CERBER_CLIENT_KEY__` from `.env`):
   - `deploy/nginx/cerber-maps.conf` → `/etc/nginx/conf.d/cerber-maps.conf`
   - `deploy/nginx/cerber.ihatebot.com` → `/etc/nginx/sites-available/` (+ symlink
     into `sites-enabled/`), then `sudo nginx -t && sudo systemctl reload nginx`.

## Providers in the firebat config

`deploy/config.firebat.yaml`: Anthropic (OAuth from `auths/`), OpenAI, Gemini,
Grok (keys from `.env`), and **ollama on gpu0** (`http://192.168.89.233:11434`,
reached over WireGuard). ollama model prefixes (`llama`, `qwen`, `gemma`,
`mistral`, `deepseek`) route to gpu0; everything else follows the usual rules.

## Operating

```bash
ssh ruslan@192.168.88.35 'cd /opt/cerber && docker compose logs -f'   # tail logs
ssh ruslan@192.168.88.35 'cd /opt/cerber && docker compose ps'        # status
make deploy                                                           # redeploy
```

Add more Claude accounts by running `cerber --claude-login` locally (writes to
`auths/`) and re-running `make deploy` — cerber pools all tokens it finds.
