# Feature parity vs upstream

Tracking cerber against the three upstreams it replaces: **CLIProxyAPI** (proxy),
**cpa-usage-keeper** (usage/quota), **CPA-Manager-Plus** (management UI). Honest
status — what's done, partial, and not yet. Updated 2026-06-08.

Legend: ✅ done · 🟡 partial · ❌ not yet

## Core proxy

| Feature | Status | Notes |
|---|---|---|
| OpenAI `/v1/chat/completions` (stream + non-stream) | ✅ | routes by model |
| Anthropic native `/v1/messages` (stream) | ✅ | transparent passthrough |
| `/v1/models` (list models) | ✅ | aggregated from per-provider discovery |
| `/v1/messages/count_tokens` | ✅ | proxied to Anthropic via pooled creds |
| `/v1/images/generations` (image gen) | ✅ | passthrough (grok-imagine-*, openai gpt-image/dall-e) |
| `/v1/embeddings`, `/v1/completions`, `/v1/responses` | ✅ | OpenAI passthrough to routed provider (Forwarder); not Anthropic |
| video gen | ❌ | |
| Streaming SSE + flush | ✅ | |
| Request/response header passthrough | ✅ | incl. anthropic-ratelimit-* |
| Tools / function calling | 🟡 | native passthrough yes; OpenAI→Anthropic translate no |
| Multimodal (images) | 🟡 | text+image in translators; image GEN via /v1/images/generations (grok); no video |
| Model aliases (alias→canonical) | ✅ | `providers.model_aliases`; resolved pre-routing + pre-upstream |
| Cross-provider/model fallback chains | ✅ | OpenAI endpoint; `providers.fallbacks` + `X-Cerber-Fallback`; retryable-only (5xx/no-cred), pre-commit |

## Providers

| Provider | Status | Auth |
|---|---|---|
| Anthropic / Claude | ✅ | api_key + OAuth (refresh + Claude Code spoof) |
| OpenAI | ✅ | api_key (passthrough) |
| Gemini | ✅ | api_key (OpenAI↔Gemini translate) |
| Grok / xAI | ✅ | api_key + OAuth (Grok Build / SuperGrok subscription, device flow) |
| Ollama / vLLM (local) | ✅ | keyless (OpenAI-compatible passthrough) |
| Codex, Kimi, Vertex, Antigravity, Gemini-CLI OAuth, OpenRouter | ❌ | |

## Credentials & orchestration

| Feature | Status | Notes |
|---|---|---|
| Multiple accounts per provider | ✅ | auth_dir + config, per-org names |
| Round-robin rotation | ✅ | |
| Fill-first strategy | ✅ | `providers.strategy: fill-first` |
| Cooldown on 401/403/429 + transport error | ✅ | exponential backoff (60s→…→30m), reset on success |
| Exponential backoff | ✅ | per-credential, capped 30m; success resets |
| Pick credential per request | ✅ | `X-Cerber-Cred: oauth\|key\|<name>` |
| **Runtime enable/disable account** | ✅ | `POST /admin/accounts/{name}/{enable,disable}` |
| **List accounts + state + usage** | ✅ | `GET /admin/accounts` |
| OAuth token refresh (proactive) | ✅ | persisted to auth_dir |
| Reactive refresh on 401 | ❌ | |
| Add/remove account via API | ❌ | login is CLI (`--claude-login`); no runtime add/remove |
| Session affinity / sticky routing | ❌ | |

## Auth & access

| Feature | Status |
|---|---|
| Client API keys | ✅ |
| Per-key budgets (cost $) + rate-limits (requests/tokens per window) | ✅ `POST /admin/keys/{name}/limits` · `access.default_key_limits` |
| allow_localhost | ✅ |
| Separate management key for /admin | ✅ `access.management_key` |
| Interactive `--claude-login` (PKCE) | ✅ |
| Interactive `--xai-login` (OAuth device flow, SuperGrok) | ✅ |
| Per-provider OAuth logins (codex/gemini/kimi) | ❌ |

## Observability (cpa-usage-keeper territory)

| Feature | Status | Notes |
|---|---|---|
| Per-credential + per-model usage (requests/errors/tokens) | ✅ | in-memory |
| `/admin/stats` JSON | ✅ | |
| Prometheus `/metrics` | ✅ | |
| Web dashboard | ✅ | embedded; usage + accounts + hourly chart + cost |
| **Persistent usage (survives restart)** | ✅ | JSON aggregates (not per-event SQLite) |
| **Quota inspection per account** (5h/7d windows) | ✅ | passive, from Anthropic rate-limit headers |
| **Cost calculation** (per-model pricing) | ✅ | `usage.pricing`; `total_cost` = cumulative $ through cerber |
| **Recent-request log** (who/IP/UA/model/tokens/cost) | ✅ | `GET /admin/requests` + dashboard table; in-memory ring (last N, holds IPs → not persisted) |
| Usage event log / export / filtering | 🟡 | recent ring + CSV export `/admin/usage.csv` (aggregates); no durable per-event history (SQLite) |
| Cost/usage history & analytics | 🟡 | hourly time-series + chart (no per-event) |

## Management UI (CPA-Manager-Plus territory)

| Feature | Status |
|---|---|
| Usage dashboard | ✅ (basic) |
| Self-describing docs (`/llm.md` agent guide + `/docs` full HTML reference) | ✅ | public, dynamic (providers/models/aliases/fallback) |
| Account management UI (enable/disable) | ✅ | dashboard accounts table |
| Config editing via UI/API | ❌ |
| Quota/cost dashboard | ✅ | cost card + requests/hour chart + 5h quota |

## Deployment & ops

| Feature | Status |
|---|---|
| YAML config + .env + ${ENV} | ✅ |
| zap logging to dated files | ✅ |
| Docker image + compose | ✅ |
| TLS impersonation (Docker) for first-party Claude Code | ✅ |
| Config hot-reload | ❌ |
| Storage backends (Postgres/Git/S3) | ❌ |
| Plugins / SDK | ❌ |

## Deliberately excluded (trust-first)

- Management-asset auto-download / update checks (the upstream pulls a UI from
  GitHub at runtime — dropped; see AUDIT.md).
- Telemetry / analytics.

## Suggested next priorities

Done: persistent usage+cost ✅, quota ✅, accounts view ✅, ollama/vLLM ✅,
per-credential key-health + model discovery ✅, dashboard client-key mgmt ✅,
`/llm.md` ✅, favicon ✅, `GET /v1/models` ✅, `/v1/messages/count_tokens` ✅,
usage CSV export ✅, origin restricted to Cloudflare+LAN ✅.

Remaining, roughly by value/effort:

1. **`cerber -healthcheck` flag** — local GET /healthz → exit 0/1, so a distroless
   docker `healthcheck` works (prereq for the Traefik zero-downtime plan).
2. **Zero-downtime deploy** — Traefik sidecar (see `~/obsidian/notes/cerber-zero-downtime-proxy.md`); deferred.
3. **OpenAI→Anthropic tools/function-calling translation** — finish the 🟡.
4. **Resilience** — exponential backoff; reactive refresh on 401.
5. **More providers** — Codex, Vertex, OpenRouter, Kimi.
6. **Per-event usage** (SQLite) — enables filtering/per-event export & true history.
