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
- A credential put on `Cooldown(d)` is skipped until `d` elapses, then returns to rotation.
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
- `POST /v1/messages` → relays the Anthropic request/response verbatim (streaming preserved), injecting a credential.
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
- Tokens are recorded for non-streaming responses (parsed from Anthropic usage); streaming records request counts only.
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

## Trust: no phone-home
**What:** cerber's only outbound network destinations are provider APIs being routed to (or hosts explicitly in config).
**DoD:**
- No telemetry/analytics/update-check/auto-asset-download code exists.
- `internal/version` never makes network calls.
**Verified:** `internal/version` is build-info only — 2026-06-07.
