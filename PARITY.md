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
| `/v1/completions`, `/v1/responses`, images, videos | ❌ | |
| Streaming SSE + flush | ✅ | |
| Request/response header passthrough | ✅ | incl. anthropic-ratelimit-* |
| Tools / function calling | 🟡 | native passthrough yes; OpenAI→Anthropic translate no |
| Multimodal (images) | 🟡 | text+image in translators; no image/video gen |

## Providers

| Provider | Status | Auth |
|---|---|---|
| Anthropic / Claude | ✅ | api_key + OAuth (refresh + Claude Code spoof) |
| OpenAI | ✅ | api_key (passthrough) |
| Gemini | ✅ | api_key (OpenAI↔Gemini translate) |
| Grok / xAI | ✅ | api_key (OpenAI-compatible passthrough) |
| Ollama / vLLM (local) | ✅ | keyless (OpenAI-compatible passthrough) |
| Codex, Kimi, Vertex, Antigravity, Gemini-CLI OAuth, OpenRouter | ❌ | |

## Credentials & orchestration

| Feature | Status | Notes |
|---|---|---|
| Multiple accounts per provider | ✅ | auth_dir + config, per-org names |
| Round-robin rotation | ✅ | |
| Fill-first strategy | ✅ | `providers.strategy: fill-first` |
| Cooldown on 401/403/429 + transport error | ✅ | fixed duration (no exponential backoff yet) |
| Exponential backoff | ❌ | |
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
| allow_localhost | ✅ |
| Separate management key for /admin | ✅ `access.management_key` |
| Interactive `--claude-login` (PKCE) | ✅ |
| Per-provider OAuth logins (codex/gemini/xai/kimi) | ❌ |

## Observability (cpa-usage-keeper territory)

| Feature | Status | Notes |
|---|---|---|
| Per-credential + per-model usage (requests/errors/tokens) | ✅ | in-memory |
| `/admin/stats` JSON | ✅ | |
| Prometheus `/metrics` | ✅ | |
| Web dashboard | ✅ | embedded; usage + accounts + hourly chart + cost |
| **Persistent usage (survives restart)** | ✅ | JSON aggregates (not per-event SQLite) |
| **Quota inspection per account** (5h/7d windows) | ✅ | passive, from Anthropic rate-limit headers |
| **Cost calculation** (per-model pricing) | ✅ | `usage.pricing` |
| Usage event log / export / filtering | 🟡 | CSV export `/admin/usage.csv` (aggregates, no per-event) |
| Cost/usage history & analytics | 🟡 | hourly time-series + chart (no per-event) |

## Management UI (CPA-Manager-Plus territory)

| Feature | Status |
|---|---|
| Usage dashboard | ✅ (basic) |
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
