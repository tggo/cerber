# Definition of Done — living spec of cerber

This is a **point-in-time snapshot** of how cerber is supposed to behave *right now*.
It is the source of truth for "is it working correctly?".

**Rule:** every change that adds or alters observable behaviour MUST add or update its
entry here, **in the same commit** as the code. One entry per feature:
- **What** — one line: the feature / behaviour.
- **DoD** — observable acceptance criteria (what a human/QA can check), not impl detail.
- **Verified** — how it was confirmed (measurement) + date; "—" if not yet verified.

Keep entries terse. When behaviour changes, edit the entry (don't append a second one).
**Never invent a DoD.** If acceptance criteria aren't clear, ASK and record the answer.

---

## Build & quality gates
**What:** the project builds and meets its own quality bars.
**DoD:**
- `make build` produces `bin/cerber`.
- `make test` passes with total coverage **≥ 85%** (excluding `cmd/`).
- `make lint` (gofmt + go vet) is clean.
- `make mocks` regenerates all mocks via mockery; no hand-written mocks exist.
**Verified:** scaffold builds, coverage gate green at 100% — 2026-06-07.

## Config — YAML load & validation
**What:** cerber loads a YAML config (server addr, client access keys, Anthropic provider + credentials) with defaults and strict validation.
**DoD:**
- Missing file or malformed YAML → clear error, no panic.
- Unknown YAML fields are rejected.
- Defaults applied: addr `:8080`, base_url `https://api.anthropic.com`, version `2023-06-01`, timeout `120s`.
- Rejects: no access keys, empty key, no providers, non-http(s) base_url, no credentials, api_key without key, oauth without access_token, unknown/missing credential type, bad duration.
**Verified:** `internal/config` tests, 98.2% coverage — 2026-06-07.

## Credentials — store, rotation, cooldown
**What:** thread-safe store of Anthropic credentials handed out round-robin, with per-credential cooldown after upstream failures.
**DoD:**
- Round-robin order is stable; `Next()` cycles through all credentials.
- A failing credential is sidelined with **exponential backoff** (`Penalize`: 60s→120s→…→30m cap); a successful use (`MarkSuccess`) resets the streak and clears the cooldown. So a persistently-broken account (e.g. unpaid → 403) is parked instead of retried every minute, and `auto` stops picking it until it recovers.
- When all credentials are cooling down, `Next()` returns `ErrNoneAvailable`.
- Secrets are never present in `String()`/logs; readable only via explicit accessors.
**Verified:** `internal/credential` tests, 100% coverage — 2026-06-07.

## Access control — client API keys
**What:** clients authenticate to cerber with an API key via `Authorization: Bearer` or `x-api-key`; only allow-listed keys pass.
**DoD:**
- Valid configured key (either header) → allowed; bearer takes precedence; non-bearer Authorization falls back to x-api-key.
- Unknown/empty/wrong-case/wrong-length key → denied.
- Key comparison is constant-time and scans all keys (no timing/which-key leak).
**Verified:** `internal/access` tests, 100% coverage — 2026-06-07.

## Anthropic upstream client
**What:** sends Anthropic Messages requests to the configured base URL with correct per-credential auth headers.
**DoD:**
- POSTs to `{base_url}/v1/messages` with `anthropic-version` and JSON body intact.
- api_key credential → `x-api-key` header, no Authorization.
- oauth credential → `Authorization: Bearer …` + `anthropic-beta`, no x-api-key.
- `Accept: text/event-stream` when streaming, else `application/json`.
- Only ever contacts the configured Anthropic base URL.
- OAuth requests carry the Claude Code system prefix (see OAuth entry below).
**Verified:** `internal/provider/anthropic` tests (mockery HTTPDoer) + live OAuth smoke test — 2026-06-07.

## OAuth refresh — singleflight + 401 self-heal
**What:** concurrent requests to an OAuth credential must not corrupt its rotating refresh token, and a stale-but-valid-looking access token (401) must self-heal.
**DoD:**
- `credential.Store.EnsureFresh(cred, force, now, skew, refreshFn)` serializes refresh per credential (per-cred mutex) and dedups via a generation counter: only one concurrent caller spends the single-use refresh token; the others reuse the winner's result. `force` refreshes regardless of expiry. Proven: 12 concurrent `EnsureFresh` → refresh runs exactly once.
- dispatch refreshes proactively near expiry via EnsureFresh; on an upstream `401/403` for an OAuth cred it forces one refresh and retries the SAME credential before sidelining it (the access token was likely invalidated by a rotation elsewhere). Proven: 401-then-retry → 200, upstream called twice.
- The OpenAI-compatible provider's OAuth path (grok) uses EnsureFresh too.
- Refresh lifecycle logs at Info (`oauth token near expiry, refreshing`; `oauth token refreshed`; `retrying after forced oauth refresh`; `upstream credential failure, sidelining`).
**Verified:** `internal/credential` (singleflight, force vs proactive) + `internal/server` (401 force-refresh+retry) tests; live: 10 concurrent requests pinned to one OAuth account → all 200 (previously flapped to 503) — 2026-06-09.

## OAuth — token refresh & Claude Code spoofing
**What:** Claude Code OAuth credentials stay usable over time: their access token is refreshed before expiry, and requests carry the system prefix Anthropic requires for OAuth.
**DoD:**
- An OAuth credential within 60s of (or past) expiry is refreshed via `POST {base_url}/v1/oauth/token` (`grant_type=refresh_token`, client_id `9d1c250a-…`) before the request is sent; the rotated refresh_token and new expiry are stored.
- A valid (not-near-expiry) token is not refreshed.
- Refresh failure sidelines the credential (cooldown) and rotation continues; all failing → 502.
- Every OAuth request's `system` begins with "You are Claude Code, Anthropic's official CLI for Claude." (idempotent; caller system content preserved); api_key requests are unmodified.
- Known gaps: refreshed tokens are in-memory only (not persisted across restart); no reactive refresh on a 401 (only proactive-by-expiry); full Claude Code fingerprint (billing headers, static prompt, tool renaming) intentionally not replicated.
**Verified:** `internal/provider/anthropic` (refresher+spoof) + `internal/server` refresh tests + live smoke test (expired token → Bearer REFRESHED-TOKEN, sysok=True) — 2026-06-07.

## Translator — OpenAI ↔ Anthropic
**What:** converts OpenAI chat-completions requests/responses to and from Anthropic Messages, including streaming.
**DoD:**
- Request: OpenAI → Anthropic merges system messages into `system`; defaults `max_tokens` to 4096; maps temperature/top_p/stop(string|array)/stream; text + image content parts (data: URIs → base64 source, others → url source).
- Request errors: bad JSON, missing model, no messages, unsupported role/part, only-system, bad stop/content → clear error.
- Response (non-stream): Anthropic → OpenAI concatenates text blocks; maps stop_reason→finish_reason (end_turn/stop_sequence→stop, max_tokens→length, tool_use→tool_calls); maps usage; derives `chatcmpl-<id>`.
- Streaming: Anthropic SSE → OpenAI `chat.completion.chunk` SSE — role chunk first, content deltas, final finish_reason chunk, then `data: [DONE]`; tolerates pings/non-JSON; finishes on EOF even without message_stop.
- Known gaps (slice #1): OpenAI `tools`/function-calling not translated (use native endpoint).
**Verified:** `internal/translator` tests, 94.8% coverage — 2026-06-07.

## HTTP API — endpoints, auth, rotation (Anthropic slice)
**What:** cerber serves a native Anthropic passthrough and an OpenAI-compatible endpoint, authenticating clients and rotating across upstream credentials.
**DoD:**
- `GET /healthz` → 200 `ok`.
- Missing/invalid client key on any provider endpoint → 401.
- `POST /v1/messages` → relays the Anthropic request/response verbatim (streaming preserved), injecting a credential; upstream response headers (incl. `Anthropic-Ratelimit-Unified-*`) are forwarded to the client, hop-by-hop headers dropped.
- `POST /v1/chat/completions` → translates OpenAI→Anthropic→OpenAI (stream and non-stream); malformed OpenAI request → 400; upstream non-200 relayed as-is; untranslatable upstream body → 502.
- On upstream 401/403/429 (or transport error), the credential is sidelined (cooldown) and the next is tried; all failing → 502; none available → 503.
**Verified:** `internal/server` tests (92.9%) + live smoke test (healthz, 401, native passthrough, OpenAI translation against a fake upstream) — 2026-06-07.

## Logging (zap)
**What:** structured logging via zap to a dated file and stdout, at a configurable level.
**DoD:**
- Logs written to `<logging.dir>/<YYYY-MM-DD>.log` (JSON) and stdout (console), both at `logging.level` (debug/info/warn/error; default info, dir `./logs`).
- One info line per HTTP request: method, path, status, latency (streaming still flushes).
- Credential rotation, OAuth refresh, upstream send failures, and upstream error responses (status + body snippet) are logged; **secrets are never logged** (only credential names).
- No stdlib `log` in the app; invalid log level fails startup with a clear error.
**Verified:** `internal/logging` tests + live logs observed during integration — 2026-06-07.

## Config — secrets via .env / ${ENV}
**What:** secrets live outside config.yaml: a `.env` file is loaded and `${VAR}`/`$VAR` in the YAML are expanded from the environment.
**DoD:**
- `.env` (default `./.env`, `-env` flag) loaded at startup; `KEY=VALUE`, `export`, quotes, comments handled; real env wins; missing file is not an error.
- `${PLAYGROUND_API_KEY}` in config.yaml resolves to the env value; a missing var → empty → validation error.
- `.env`, `logs/`, `config.yaml` are gitignored.
**Verified:** `internal/config` tests + live run with `.env` PLAYGROUND_API_KEY — 2026-06-07.

## Header passthrough (Claude Code compatibility)
**What:** cerber forwards the client's `anthropic-beta` header upstream so faithful clients (Claude Code) work.
**DoD:**
- Client `anthropic-beta` is forwarded to Anthropic (required for `context_management` etc.); for OAuth it is merged with `oauth-2025-04-20` (deduped).
- Browser-context headers (`Origin`, `Referer`, `Cookie`, `Sec-Fetch-*`, `Sec-Ch-*`) are NOT forwarded, and for **OAuth** credentials a non-`claude-cli` User-Agent is replaced with the Claude Code UA — Anthropic ties OAuth tokens to the Claude Code client and 401/403s browser/SDK/curl UAs (this is why the web chat 503'd on Claude). A genuine 401 force-refreshes+retries once; a 403 is treated as a policy/permission/rate decision and not retried.
- `Accept-Encoding` is NOT forwarded — Go's transport negotiates + transparently decompresses, so cerber always reads a plain body (else a client `Accept-Encoding: gzip` left the body gzipped and the OpenAI→Anthropic response translator failed with 502 "translate upstream response").
- Real `claude -p` pointed at cerber (`ANTHROPIC_BASE_URL`) completes a prompt through cerber to Anthropic.
**Verified:** `internal/provider/anthropic` beta tests + `scripts/verify-claude.sh` (real `claude -p` → "pong") — 2026-06-07.

## Live integration testing
**What:** end-to-end tests against the real Anthropic API through a full cerber server.
**DoD:**
- `make integration` (build tag `integration`) runs native, OpenAI-compat, and streaming calls against real Anthropic using `PLAYGROUND_API_KEY`; skips (not fails) if the key is unset; excluded from the unit coverage gate.
- `scripts/verify-claude.sh` verifies the real `claude -p` CLI through cerber.
**Verified:** `make integration` → 3/3 PASS; verify-claude.sh → PASS — 2026-06-07.

## Usage & stats
**What:** cerber tracks request/error/token counts per credential and per model, exposed as JSON.
**DoD:**
- `GET /admin/stats` (requires a client key) returns totals + by_credential + by_model (requests, errors, input/output tokens, last_used), sorted by requests.
- Tokens are recorded for non-streaming responses (parsed from Anthropic usage) AND for native streaming responses (parsed from `message_start`/`message_delta` SSE events as they pass through). OpenAI-compat streaming still records request counts only.
- Errors (4xx/5xx, transport, refresh, none-available) increment the error count.
**Verified:** `internal/usage` (100%) + `internal/server` stats tests + live (`input 9/output 6` after one real call) — 2026-06-07.

## Prometheus metrics
**What:** usage exposed in Prometheus format for scraping.
**DoD:**
- `GET /metrics` (unauthenticated; counts + credential/model names only, no secrets) emits `cerber_requests_total`, `cerber_errors_total`, `cerber_input_tokens_total`, `cerber_output_tokens_total` (by credential) and `cerber_requests_by_model_total` (by model).
**Verified:** `internal/metrics` (100%) + live `/metrics` scrape — 2026-06-07.

## Web dashboard
**What:** a self-contained usage dashboard (no external/CDN assets).
**DoD:**
- `GET /dashboard` serves an HTML page that, given a client key, polls `/admin/stats` and renders totals + per-credential/per-model tables with auto-refresh.
**Verified:** served 200 text/html; live stats render — 2026-06-07.

## Multi-provider routing + OpenAI provider
**What:** the OpenAI-compatible endpoint routes by model name to a provider; OpenAI is supported as a real upstream (passthrough).
**DoD:**
- `route(model)`: configured `providers.routing` prefixes win, then discovered models, then built-in prefixes `gpt*/o1*/o3*/o4*/chatgpt*→openai`, `gemini*→gemini`, `grok*→grok`, `claude*→anthropic`. An unknown model matches nothing and `/v1/chat/completions` rejects it with 400 (no silent Anthropic fallback).
- `/v1/chat/completions` with an OpenAI model → forwarded to OpenAI (Bearer key, rotation across credentials), response relayed unchanged (stream + non-stream); tokens recorded from OpenAI usage.
- Model routed to an unconfigured provider → 501; native `/v1/messages` remains Anthropic-only.
- Anthropic is currently required as the base provider; OpenAI/Gemini are optional.
**Verified:** `internal/provider/openai` (93%) + `internal/provider` Rotate (96%) + server routing tests + live `make integration` (OpenAI route → "pong" via real api.openai.com) — 2026-06-07.

## Gemini provider
**What:** Gemini supported as an upstream on the OpenAI-compatible endpoint via OpenAI↔Gemini translation.
**DoD:**
- `/v1/chat/completions` with a `gemini*` model → translated to Gemini generateContent (`x-goog-api-key`, credential rotation), response translated back to OpenAI (text, finish_reason, usage); stream → `:streamGenerateContent?alt=sse` translated to OpenAI chunks + `[DONE]`.
- System messages → `systemInstruction`; user/assistant → `user`/`model`; text + base64(data:) images supported; http image URLs/tools rejected (400).
- Untranslatable request → 400; Gemini upstream errors relayed.
**Verified:** `internal/translator` Gemini tests (93%) + `internal/provider/gemini` (92%) + live `make integration` (Gemini route → "pong" via real generativelanguage API) — 2026-06-07.

## Claude Code login (`--claude-login`) + token persistence
**What:** an interactive OAuth flow that logs into Claude Code and saves the tokens to disk, loaded at startup and refreshed in place.
**DoD:**
- `cerber --claude-login` runs the PKCE flow: starts a local callback server (default port `54545`, `--login-port` overrides), opens the browser (or prints the URL with `--no-browser`), and exchanges the code for tokens.
- State is verified (CSRF); auth errors / timeout / port-in-use produce clear errors.
- Tokens are written to `<auth_dir>/<name>.json` (mode `0600`, dir `0700`; default `./auths`, gitignored).
- On startup, tokens in `auth_dir` are loaded and merged with config Anthropic credentials; an empty merged set fails with a hint to run `--claude-login`.
- Refreshed OAuth tokens are persisted back to `auth_dir`, so logins survive restarts.
**Verified:** `internal/auth/claude` + `internal/auth/login` + `internal/tokenstore` tests + server persister test + live smoke (`--claude-login --no-browser` prints the real claude.ai authorize URL and serves the callback) — 2026-06-07.

## Credential selection by header (X-Cerber-Cred)
**What:** clients can pick which Anthropic credential type handles a request.
**DoD:**
- `X-Cerber-Cred: oauth` → only OAuth (auth_dir) credentials are used; `key`/`api_key` → only API-key credentials; absent/other → any (round-robin), unchanged default.
- Applies to `/v1/messages` and the Anthropic-routed `/v1/chat/completions`; rotation/cooldown still honored within the chosen kind; none of the requested kind available → 503.
**Verified:** `internal/credential` NextOf tests (100%) + server header tests + live (oauth header → OAuth login cred, key header → api key) — 2026-06-07.

## Grok (xAI) provider
**What:** Grok supported as an upstream on the OpenAI-compatible endpoint.
**DoD:**
- `/v1/chat/completions` with a `grok*` model → forwarded to xAI (`https://api.x.ai`, Bearer key, credential rotation), response relayed unchanged (xAI is OpenAI-compatible — reuses the OpenAI provider named "grok").
- `providers.grok` config (base_url default `https://api.x.ai`); `grok` valid in `routing`.
**Verified:** reuses `internal/provider/openai` (93%) + config grok tests + live `make integration` (grok route → "pong" via real api.x.ai, model grok-4.3) — 2026-06-07.

## Access — allow_localhost
**What:** optional open access for loopback clients, so a local Claude Code (which sends its own token) can use cerber without a matching key.
**DoD:**
- `access.allow_localhost: true` → requests from `127.0.0.1`/`::1` are accepted with any or no key; non-loopback clients still require a configured key.
- Config validation allows empty `access.keys` when `allow_localhost` is true.
**Verified:** `internal/server` allow-localhost + isLoopback tests + live (no-key/any-key localhost → 200) — 2026-06-07.

## TLS impersonation (Docker only)
**What:** in a container, cerber impersonates `api.anthropic.com` so Claude Code treats it as first-party and enables 1M context + tool-search.
**DoD:**
- `cerber --gen-cert` writes a CA + leaf cert for the impersonated host(s) (default `api.anthropic.com`).
- With `tls.enabled`, cerber serves HTTPS on `tls.addr` using the generated cert; with `tls.use_doh`, it resolves the real upstream via DNS-over-HTTPS, bypassing the container's `/etc/hosts` redirect.
- `docker compose -f docker-compose.tls.yml up` runs cerber + Claude Code in a container with `extra_hosts` redirect and `NODE_EXTRA_CA_CERTS`; the host is untouched.
- 1M context + tool-search require Claude Code logged into Max in the container (mount `~/.claude`).
**Verified:** `internal/tlscert` + `internal/upstreamdial` tests + live in-container: `https://api.anthropic.com/healthz`→ok via cerber, real `/v1/messages`→Anthropic via DoH, `claude -p`→"pong" through the impersonation — 2026-06-08.

## Account orchestration (management API)
**What:** list and enable/disable upstream accounts at runtime, without editing files or restarting.
**DoD:**
- `GET /admin/accounts` (authed) lists each credential: name, kind, enabled, cooling_down, and its usage (requests/errors/tokens).
- `POST /admin/accounts/{name}/disable` removes it from rotation; `…/enable` restores it; unknown name → 404.
- Disabled credentials are skipped by selection (`Next`/`NextOf`); change takes effect immediately.
**Verified:** `internal/credential` (SetEnabled/List, 100%) + `internal/server` accounts tests — 2026-06-08.

## Usage persistence, cost, quota, strategy, management key
**What:** usage survives restarts, has cost, shows per-account quota; credential strategy and admin auth are configurable.
**DoD:**
- Usage aggregates persist to `usage.file` (load on start, save every 30s + on SIGINT/SIGTERM); `usage.pricing` (per-1M-token) yields per-model + total cost in `/admin/stats` and the dashboard.
- `/admin/accounts` includes each account's quota (5h/7d utilization/status/reset) captured passively from Anthropic rate-limit headers.
- `providers.strategy: fill-first` drains one credential before the next (default round-robin).
- `access.management_key`, when set, gates `/admin/*` (Bearer/x-api-key/X-Cerber-Management) instead of client keys.
- Dashboard shows a cost card + accounts table with enable/disable buttons and 5h quota.
**Verified:** `internal/usage` (Save/Load/cost), `internal/quota` (100%), `internal/credential` (fill-first), `internal/server` (management key) tests — 2026-06-08.

## Analytics (time-series) + embedded UI
**What:** usage over time (hourly) with a chart in the binary-embedded dashboard.
**DoD:**
- The usage tracker keeps hourly buckets (~30-day retention, persisted with the rest); `/admin/stats` returns `series` (chronological hourly requests/errors/tokens).
- The embedded dashboard (`go:embed`, no external/CDN assets) renders a requests/hour SVG chart (last 48h, errors overlaid, hover details) plus cost card and accounts table.
**Verified:** `internal/usage` series test + live (`series` populated, cost computed, dashboard 200) — 2026-06-08.

## Ollama / vLLM provider (local, OpenAI-compatible)
**What:** route selected models to a local ollama/vLLM server (e.g. on a GPU box) over the same OpenAI-compatible passthrough as Grok.
**DoD:**
- `providers.ollama` (base_url, optional credentials; default `http://localhost:11434`) registers an OpenAI-compatible chatter named `ollama`; credentials are optional (keyless local server → a dummy key is injected so rotation works).
- `providers.routing` prefixes select it (local model names are arbitrary); `ollama` is a valid routing provider.
**Verified:** `internal/config` (defaults/no-creds/bad-url) + `internal/server` route tests; live against gpu0 ollama — 2026-06-08.

## Per-credential health probe + model discovery (all providers)
**What:** cerber periodically validates every credential of every provider and collects the models each provider serves, then uses that for key-health reporting and discovery routing.
**DoD:**
- A background probe runs at startup and every `providers.ollama.probe_interval` (default 60s) calling `ProbeAll`: for each provider, each credential is validated against the upstream — OpenAI-compatible (openai/grok/ollama) and Anthropic-API-key via `GET /v1/models`; Gemini via `GET /v1beta/models?key=` (only models advertising `generateContent` are kept — embeddings, Imagen, Veo, AQA and live/native-audio models are dropped so /v1/models never offers a model that 4xxes on the chat path); Anthropic-OAuth via a minimal `POST /v1/messages` auth check (OAuth can't list models). A `401/403` marks the credential invalid (`ErrInvalidCredential`); other errors record the error string; success marks it healthy and contributes its models.
- Per-credential health (`healthy`, `health_error`, `health_checked_at`) is stored and returned by `GET /admin/accounts` (every provider's creds, tagged with `provider`; enable/disable works across all stores).
- `GET /admin/providers` lists each provider: base_url, credential count, healthy-credential count, and the discovered model union.
- Routing: after configured prefixes, a request whose model exactly matches a discovered model (any provider) goes there — arbitrary ollama names (`supergemma4-…`, `hf.co/…`) route to ollama with no prefix config.
- The embedded dashboard shows a providers section (keys-ok ratio + model count) and the accounts table shows a per-key valid/invalid column.
**Verified:** `internal/provider/{openai,anthropic,gemini}` ProbeCredential tests + `internal/server` (ProbeAll key health, discovery routing, providers view, cross-provider accounts) tests; live against gpu0 + cloud keys — 2026-06-08.

## Client keys — dashboard-managed (dynamic, persisted)
**What:** client API keys can be minted, enabled/disabled and deleted at runtime from the dashboard, in addition to the static config keys.
**DoD:**
- A persisted store (`access.keys_file`, default `./data/keys.json`) holds `cer_`-prefixed keys; its enabled keys are accepted alongside the static config keys (so an env-seeded key always works).
- `GET /admin/keys` lists keys redacted (name, enabled, last4, created, last-used — never the secret); `POST /admin/keys {name}` mints one and returns the full secret exactly once (409 on duplicate name, 400 on empty); `POST /admin/keys/{name}/{enable,disable}` toggles; `DELETE /admin/keys/{name}` (or `…/delete`) removes; unknown name → 404; all gated by the admin auth (management key if set, else client key); 503 if no store configured.
- Mutations persist atomically; last-used is stamped on auth and flushed by the periodic saver. The embedded dashboard has a client-keys section (create + reveal-once, enable/disable, delete).
**Verified:** `internal/access` store tests + `internal/server` CRUD/not-configured tests — 2026-06-08.

## Client-key governance — per-key budgets & rate limits
**What:** each managed (dashboard) client key may carry a rolling cost budget plus rolling request/token rate limits, enforced per request. Static config keys and loopback callers are the operator's own and bypass governance (unlimited).
**DoD:**
- A key's limits are `max_cost_usd` over `budget_period` (default `month`) and `max_requests` / `max_tokens` over `rate_period` (default `minute`); periods are one of `minute|hour|day|week|month` (rolling spans, not calendar-aligned). A zero/absent value for a dimension means unlimited for that dimension.
- Per request, after the key is identified: if its cost window is at/over `max_cost_usd` → **402**; if its request or token window is at/over the limit → **429**; otherwise one request is reserved and the request proceeds. Cost is computed from the configured model pricing (`usage.pricing`); cost+tokens are charged back to the key after the response.
- Counters reset when their rolling window elapses, and are **persisted with the key** (survive restart) and flushed by the periodic saver (not on every request).
- `POST /admin/keys/{name}/limits` (admin-authed) sets a key's limits from an `access.Limits` JSON body (empty body clears them → unlimited); unknown name → 404, unrecognised period → 400. `GET /admin/keys` and the key store expose each key's `limits` and current-window `usage` (redacted, no secret).
- `access.default_key_limits` in config seeds the limits of **newly-created** dashboard keys; existing keys keep their own. Invalid default periods fail config validation.
**Verified:** `internal/access` limits tests (Admit/Charge/window-reset/Validate/SetLimits/defaults) + `internal/server` governance tests (402/429/charge-from-pricing/admin set) — 2026-06-10.

## Model catalog — aliases & multi-provider lookup
**What:** a stable client-facing model name can be aliased to the canonical model a provider actually serves, resolved before routing and before the request reaches upstream; the server can also report every provider that serves a given model (for fallback chains).
**DoD:**
- `providers.model_aliases` (config) maps `alias -> canonical`; resolution is single-hop and case-sensitive. A request whose `model` matches an alias is rewritten so the **canonical** name is used for routing, usage attribution, and the upstream body; all other body fields are preserved. A model with no alias leaves the body byte-for-byte untouched.
- Empty alias or empty target → config validation error.
- `providersForModel(model)` returns the sorted set of providers whose discovered models include the exact name (basis for fallback).
**Verified:** `internal/catalog` tests (resolve/single-hop/nil/copy) + `internal/server` alias tests (upstream body rewrite, untouched-when-no-alias, setModelField) — 2026-06-10.

## Cross-provider/model fallback (OpenAI endpoint)
**What:** on `/v1/chat/completions`, a request whose primary model fails with a retryable error is automatically retried against an ordered list of fallback models (which may live on other providers), before any response bytes are written.
**DoD:**
- Targets are: the requested model first, then either the per-request `X-Cerber-Fallback` header (comma-separated model names, overrides config) or, absent the header, the first `providers.fallbacks` entry whose `model` matches the request model (exact or prefix) — its `to` list in order. Each target is a model name, routed like any other (so a target can resolve to a different provider).
- A target is retried (fall through to the next) only on a **retryable** failure: no credential available / transport error, or an upstream **5xx**. A **4xx** client error and a **2xx** success are terminal (no fallback). Governance 402/429 happen before routing and never trigger fallback.
- Fallback only occurs **before** the response is committed; once streaming/relay has started it is never re-attempted. An unroutable or unconfigured fallback target is skipped; if every target fails, the **last** target's error is surfaced.
- Scope: the OpenAI-compatible endpoint only (output is always OpenAI-format, so cross-provider is coherent). Native `/v1/messages` is unchanged — its resilience remains per-credential rotation (an Anthropic response can't be re-expressed from another provider). Empty `fallbacks[].model`/`to` → config validation error.
**Verified:** `internal/server` fallback tests (anthropic-5xx→chatter, 4xx terminal, header override, all-exhausted→last error, skip-unroutable) + `internal/config` validation — 2026-06-10.

## Self-describing usage doc (`GET /llm.md`)
**What:** a live markdown guide so an agent given only the base URL + a key learns how to connect, which endpoints/dialects exist, how models route, and exactly which models each provider serves.
**DoD:**
- `GET /llm.md` (alias `GET /llms.txt`) returns `text/markdown`, **public** (no key) so a plain browser/agent can read it — it exposes no secrets, only how to use the API (which still needs a key from the public side).
- Content is dynamic: base URL is derived from the request host/scheme (honours `X-Forwarded-Proto`); the model list reflects discovered models per provider (e.g. ollama), with routing rules and curl/SDK examples.
**Verified:** `internal/server` TestLLMDoc (auth, content-type, endpoints, discovered models, host) — 2026-06-08.

## Model list, token counting, usage export
**What:** OpenAI-style model listing, Anthropic token counting, and a CSV usage export.
**DoD:**
- `GET /v1/models` (authed like the API) returns `{"object":"list","data":[{id,object:"model",owned_by:<provider>}]}` aggregated from the per-provider discovered models (ProbeAll); sorted by provider.
- `POST /v1/messages/count_tokens` proxies Anthropic's count-tokens endpoint through the pooled credentials (rotation/refresh via the shared dispatch), forwarding the client body unchanged (no Claude Code system injection); Anthropic-only.
- `GET /admin/usage.csv` (admin-authed) exports a section-tagged CSV: total, per-credential, per-model (requests/errors/tokens/cost) and the hourly series (RFC3339 UTC).
**Verified:** `internal/server` TestModelsEndpoint / TestCountTokens / TestUsageCSV; mocks regenerated (Upstream.CountTokens) — 2026-06-08.

## Image generation (`/v1/images/generations`)
**What:** OpenAI-compatible image-generation passthrough to the provider that serves the model (xAI/Grok `grok-imagine-*`, OpenAI `gpt-image-*`/`dall-e-*`).
**DoD:**
- `POST /v1/images/generations` (authed like the API) routes by model via `route()`; the target provider must implement `provider.ImageGenerator` (the OpenAI provider does — grok/openai/ollama). The request body is forwarded unchanged with the provider's Bearer credential (rotation/cooldown via `provider.Rotate`); the response (e.g. `{"data":[{"url"}],"usage"}`) is relayed unchanged.
- Unknown model or `anthropic` (no image gen) → 400; a routed provider that can't generate images → 501. Usage records request/error counts (no token cost for images).
**Verified:** `internal/provider/openai` TestImages_Passthrough + `internal/server` TestImages_RoutedToProvider / ProviderWithoutImageSupport; live grok image via cerber — 2026-06-09.

## xAI / Grok OAuth login (`--xai-login`, device flow)
**What:** log in with a Grok Build / SuperGrok / X Premium+ subscription and use it for Grok inference (chat + images) instead of a pay-per-token API key — the xAI analogue of `--claude-login`.
**DoD:**
- `cerber --xai-login` runs the xAI OAuth2 **device** flow (`auth.x.ai`, public Grok CLI client, scopes incl. `offline_access grok-cli:access`): prints a verification URL + user code (opens a browser unless `--no-browser`), polls the token endpoint, and saves the token under `auth_dir/xai/`. Works headless/remote (no localhost callback).
- At startup the Grok provider merges `auth_dir/xai` OAuth tokens with config API keys into one rotating store; if only OAuth tokens exist (no `providers.grok` block) the provider is still enabled (`config.DefaultGrok`, base `https://api.x.ai`).
- The OpenAI provider sends `Bearer <access_token>` for OAuth credentials (api keys unchanged) and proactively refreshes them before expiry (`offline_access` refresh token), persisting the result. Subscription tokens drive chat + `/v1/images/generations`.
- Trust: only xAI hosts (`auth.x.ai`, `api.x.ai`) are contacted; tokens live in `auth_dir/xai` (0600), never logged.
**Verified:** `internal/auth/xai` (device/poll/refresh) + `internal/auth/login` (Grok device flow) + `internal/provider/openai` (oauth bearer + refresh) tests; device-code start confirmed live against auth.x.ai — 2026-06-09.

## Chat playground (`/chat`)
**What:** an embedded web chat to try a provider/account from the browser — left = conversation, right = raw request/response JSON.
**DoD:**
- `GET /chat` serves an embedded page (no external assets). Model picker is populated from `/v1/models` (filtered to `?provider=` when present, free-text allowed); a credential picker (auto / oauth / key, or a pinned `?cred=<name>`) sets `X-Cerber-Cred`.
- It calls `/v1/chat/completions` with the running conversation; the assistant reply renders on the left and every request + response (or error) is shown as pretty JSON on the right.
- The dashboard links into it: each provider → `/chat?provider=<p>`, each account → `/chat?provider=<p>&cred=<name>` (so you can chat on a specific subscription/key).
- The OpenAI-compatible providers (openai/grok/ollama) honour `X-Cerber-Cred` to pin a credential (`provider.RotateFiltered` + `credential.MatchHeader`, shared with the server), so "chat with this subscription" actually uses it.
**Verified:** `internal/server` TestChatPage + `internal/provider/openai` TestChat_PinsCredentialByHeader; live — 2026-06-09.

## Trust: no phone-home
**What:** cerber's only outbound network destinations are provider APIs being routed to (or hosts explicitly in config).
**DoD:**
- No telemetry/analytics/update-check/auto-asset-download code exists.
- `internal/version` never makes network calls.
**Verified:** `internal/version` is build-info only — 2026-06-07.
