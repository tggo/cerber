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

## Network model (split-horizon, LAN-keyless / public-key-required)

- **Internal DNS** (both MikroTiks): `cerber.ihatebot.com → 192.168.88.35`.
  LAN/WG clients hit firebat nginx directly; the vhost injects the client key
  for them, so the LAN is **keyless**.
- **Public DNS** (Cloudflare, proxied): `cerber.ihatebot.com → 193.56.148.246`
  → router forwards `:80/:443` → firebat nginx. From the outside the client
  **must** present `Authorization: Bearer $CERBER_CLIENT_KEY`; nginx passes it
  through and cerber validates it.

The decision is made per source IP in `deploy/nginx/cerber-maps.conf`
(`geo`/`map`). LAN ranges (`192.168.0.0/16`, `10.10.10.0/30`, loopback) get the
key injected; everyone else must supply their own.

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
