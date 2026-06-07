# TLS impersonation (Docker only)

Claude Code enables **1M context** and **tool-search** only when it believes it is
talking to the first-party host `api.anthropic.com` (it decides from the URL). Any
custom `ANTHROPIC_BASE_URL` loses both, so with hundreds of MCP tools the context
fills immediately ("0% until auto-compact").

The fix: make cerber **be** `api.anthropic.com` for the client. Doing that on a
real machine would hijack *all* Anthropic traffic, so this runs **only in Docker**,
where the hosts redirect, CA trust, and Claude Code all live inside the container.
The host is never touched.

```
claude (in container, no ANTHROPIC_BASE_URL)
  -> https://api.anthropic.com           (extra_hosts -> 127.0.0.1)
  -> cerber :443 (TLS, cert trusted via NODE_EXTRA_CA_CERTS)
  -> DoH resolves the real api.anthropic.com, forwards there
```

## How it works

- **`extra_hosts: api.anthropic.com:127.0.0.1`** — inside the container, that
  hostname resolves to cerber.
- **`cerber --gen-cert`** — makes a local CA + a leaf cert for `api.anthropic.com`
  (run automatically by the entrypoint). `NODE_EXTRA_CA_CERTS=/work/certs/ca.pem`
  makes Claude Code trust it.
- **`tls.use_doh: true`** — cerber resolves the real Anthropic IP via DNS-over-HTTPS
  (Cloudflare `1.1.1.1`), bypassing the container's hosts redirect, so it can reach
  the actual API.
- **`access.allow_localhost: true`** — cerber accepts whatever Claude Code sends and
  injects its own pooled credentials upstream.

## Run

```bash
docker compose -f docker-compose.tls.yml up -d --build
docker compose -f docker-compose.tls.yml exec cerber claude
```

Quick checks inside the container:

```bash
docker compose -f docker-compose.tls.yml exec cerber \
  curl -s --cacert /work/certs/ca.pem https://api.anthropic.com/healthz   # -> ok
docker compose -f docker-compose.tls.yml exec cerber \
  claude -p "say pong" --model claude-sonnet-4-6                          # -> pong
```

## Getting 1M context + tool-search

These are **Max-subscription** features, so Claude Code in the container must be
logged into your Max account (not API-key mode). Mount your host login by
uncommenting in `docker-compose.tls.yml`:

```yaml
    volumes:
      - ${HOME}/.claude:/root/.claude
```

and remove the `ANTHROPIC_API_KEY=local` env (so Claude Code uses the subscription
OAuth). Then `claude` in the container — seeing `api.anthropic.com` + your Max
login — enables 1M context and tool-search, and the requests flow through cerber.

## Notes / limits

- **Docker only.** Never add `api.anthropic.com -> 127.0.0.1` to your host
  `/etc/hosts` or trust the CA on the host — it would reroute all Anthropic traffic.
- `127.0.0.1:8088 -> 8080` exposes the dashboard/stats/metrics on the host.
- cerber still injects its own pooled credentials upstream; the client token from
  Claude Code is accepted (localhost) but not used to call Anthropic.
