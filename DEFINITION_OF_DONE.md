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
- Known gaps (slice #1): OAuth token refresh and Claude-Code system-prompt spoofing not yet implemented.
**Verified:** `internal/provider/anthropic` tests (mockery HTTPDoer), 100% coverage — 2026-06-07.

## Trust: no phone-home
**What:** cerber's only outbound network destinations are provider APIs being routed to (or hosts explicitly in config).
**DoD:**
- No telemetry/analytics/update-check/auto-asset-download code exists.
- `internal/version` never makes network calls.
**Verified:** `internal/version` is build-info only — 2026-06-07.
