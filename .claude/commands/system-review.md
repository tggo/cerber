---
description: Review the whole live cerber instance and compile a prioritized list of fixes + improvements
argument-hint: "[optional focus area, e.g. 'governance' or 'credentials']"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write
---

# /system-review — full-system health audit + improvement backlog

Review the **live** cerber instance end to end and produce a prioritized,
evidence-backed list of fixes and improvements. If `$ARGUMENTS` names a focus
area (e.g. `governance`, `credentials`, `fallback`, `cost`), go deep there but
still sweep the rest for regressions.

**This repo is open source — keep this command generic and secret-free.** Take
the instance URL from `$CERBER_URL` (default `http://localhost:8080`) and the
deploy target from `.env` (the same `DEPLOY_HOST` / `DEPLOY_USER` / `DEPLOY_DIR`
that `deploy/deploy.sh` reads). **Never hardcode hosts, IPs, or keys here.**
Admin endpoints need a key — present the operator's management/client key
(`Authorization: Bearer $KEY`, or `X-Cerber-Management`); on a LAN fronted by the
documented Caddy snippet the key is injected for you, so a local sweep is keyless.

## 1. Gather live signals (read-only, parallel where possible)

Set `U="${CERBER_URL:-http://localhost:8080}"`.

- `GET $U/healthz` — liveness (expect `ok`).
- `GET $U/admin/stats` — `totals` (requests/errors), `total_cost`, `by_credential`,
  `by_model`, hourly `series`. Note error spikes; `total_cost: 0` usually means no
  `usage.pricing`, not a fault.
- `GET $U/admin/accounts` — per pooled credential: `healthy` / `health_error`,
  `cooling_down`, `enabled`, 5h/7d quota. `✗ invalid` keys or one stuck cooling are
  the real problems.
- `GET $U/admin/providers` — discovered models per provider; a provider advertising
  **0 models** = bad key or a failed probe.
- `GET "$U/admin/requests?limit=200"` — who called what (client / IP / UA / provider /
  model / tokens / cost) and any `error:true`. All traffic under one shared key or one
  IP is an attribution gap, not a fault.
- `GET $U/metrics` — `cerber_errors_total`, `cerber_cost_usd_total`, request counts;
  cross-check against `/admin/stats`.
- **Container + edge** (when this instance is the deployed one): source `.env`, then over
  SSH to `$DEPLOY_HOST`: `docker compose ps` (are BOTH blue-green colours accounted for?),
  `docker compose logs --since 6h <active>` filtered for `WARN|ERROR|PANIC`, and confirm a
  recent OAuth-refresh log line. Check the fronting proxy's health if present.
- **Locally**: `make lint` && `make test` — the **>85% coverage gate** is part of "healthy".

## 2. Cross-check signals against code

For every anomaly, find the root cause in code before judging it — don't report a
symptom as a bug. Traps proven real in cerber:

- **`total_cost: 0` / cost column `—`** → no/partial `usage.pricing`. Pricing matches by
  exact name first, else the longest key that prefixes the model (`internal/usage`). Not a bug.
- **recent-request log empty right after a deploy** → it is an **in-memory ring**
  (`usage.recent_log`), reset on restart by design (it holds client IPs). Not a stall.
- **client IP shows a CDN edge / Docker-bridge IP** → `clientIP()` takes the first
  `X-Forwarded-For` hop; behind a CDN the real IP only arrives if the edge forwards it,
  else you log the CDN's IP. Attribution caveat, not a crash.
- **`/admin/*` answers without a key on the LAN** → the Caddy snippet injects the shared
  key for LAN/WG — intended. But with **no `access.management_key`**, any valid client key
  can reach `/admin`; that's operator-intent (§3 🟢), surface it.
- **"upstream credential failure, sidelining" / cooldown WARNs** → normal self-healing
  rotation + exponential backoff (`internal/credential`), not an error to fix.
- **upstream `5xx` / `529 overloaded` relayed** → the provider was down and cerber relayed
  it. Upstream, not cerber.
- **streamed cost `0` for a grok/gemini chatter** → known gap: non-Anthropic chatter
  streams aren't token-counted (would need `stream_options.include_usage`). Native
  `/v1/messages` and OpenAI→Anthropic streams ARE counted.

## 3. Categorize findings

- **🔴 Bugs / risks** — confirmed-broken or a missing safety net. Per `CLAUDE.md`,
  **fix obvious low-risk bugs in this same pass** — don't just list them: code → test
  (logic packages stay **>85%**, **mockery-only** mocks) → update the feature's
  `DEFINITION_OF_DONE.md` entry **in the same commit** → `make deploy` → verify on the live
  instance (`/admin/*`, `/metrics`, logs), quoting the evidence. **Branch first if on `main`.**
  Never weaken the trust principles: no new outbound host, no secret in logs/responses.
- **🟡 Resilience** — degrades under load/failure (a model with no fallback chain; an account
  stuck cooling; OAuth refresh storms; a single point of credential failure).
- **🟢 Operator-intent toggles** — security/policy switches that are OFF: no
  `access.management_key` (admin reachable by any client key), `allow_localhost`, absent
  per-key `default_key_limits` / budgets, no `providers.fallbacks`. **Surface + recommend,
  do not flip** — these are the operator's call, and some are public/irreversible.
- **🔵 Hygiene / observability** — drift between `DEFINITION_OF_DONE.md` / `PARITY.md` / `/docs`
  and actual behavior, noisy ERROR logs, a stale `usage.pricing` table, unbounded growth, or a
  signal that exists internally but isn't exposed.

## 4. Deliver

A skimmable, prioritized table **per category**, each row carrying its **evidence** — the log
line, the `/admin/*` number, the code path (`file:line`) — no bare claims. End with the
**top-3 recommended next steps**. State plainly what you already **fixed + deployed** vs what
needs the **operator's call**. Direct, no cheerleading.
