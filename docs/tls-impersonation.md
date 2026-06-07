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
- **Transparent login** — the entrypoint runs `cerber --seed-claude-creds`, writing
  `~/.claude/.credentials.json` from `auth_dir` so Claude Code starts as a normal
  Max login (no `ANTHROPIC_API_KEY`, no prompt). cerber is the sole token owner: it
  injects its pooled OAuth on `/v1/messages` *and* on the catch-all proxy, so the
  seeded token is never actually used upstream and never needs refreshing.

With a Max login seen on a first-party host, Claude Code enables
`advanced-tool-use` (tool-search) — so it stops dumping every MCP tool into each
request, which is what caused the instant auto-compact via a custom base URL.

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

## Tool-search / 1M context

These activate when Claude Code believes it has a Max login on the first-party
host — which the seeded credentials + impersonation provide automatically. No API
key and no manual login are needed: the entrypoint seeds the login from `auth_dir`
(run `cerber --claude-login` once on the host to populate it). Verified: through
impersonation Claude Code sends `advanced-tool-use-2025-11-20` (tool-search), which
it never sends via a custom `ANTHROPIC_BASE_URL`.

## Notes / limits

- **Docker only.** Never add `api.anthropic.com -> 127.0.0.1` to your host
  `/etc/hosts` or trust the CA on the host — it would reroute all Anthropic traffic.
- `127.0.0.1:8088 -> 8080` exposes the dashboard/stats/metrics on the host.
- cerber still injects its own pooled credentials upstream; the client token from
  Claude Code is accepted (localhost) but not used to call Anthropic.
