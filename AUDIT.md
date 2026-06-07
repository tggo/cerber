# Upstream audit

Purpose: understand the three upstream projects well enough to reimplement them, and confirm there is nothing in them that exfiltrates credentials. We rewrite from scratch; this file is the running record of what the upstream actually does and what we deliberately exclude.

Reference checkouts (read-only): `~/code/CLIProxyAPI`, `~/code/cpa-usage-keeper`, `~/code/CPA-Manager-Plus`.

## Scope at a glance

| Repo | Lang | LOC | Role |
|---|---|---|---|
| CLIProxyAPI | Go | ~181k | core multi-provider proxy + translators + OAuth logins |
| cpa-usage-keeper | Go | ~44k | usage/quota poller + API |
| CPA-Manager-Plus | TS/Vue + Go | ~101k | web management UI + manager-server |

## Egress findings (2026-06-07)

Enumerated every `http(s)://` literal in the Go sources. After removing test placeholders (`example.com`, `*.example.*`, `proxy.local`, `127.0.0.1`, `localhost`), the real outbound hosts are **all legitimate provider endpoints**:

- Google: `www.googleapis.com`, `cloudcode-pa.googleapis.com`, `oauth2.googleapis.com`, `aiplatform.googleapis.com`, `serviceusage.googleapis.com`
- Anthropic: `api.anthropic.com`
- OpenAI / Codex: `api.openai.com`, `auth.openai.com`, `chatgpt.com`, `openai.com`
- xAI / Grok: `auth.x.ai`, `vidgen.x.ai`, `cli-chat-proxy.grok.com`
- Others: `openrouter.ai`, `api.kimi.com`, `ampcode.com` (Amp CLI)

**No telemetry/analytics SDK is wired in** (no posthog/sentry/mixpanel/amplitude integration; keyword matches were test files and unrelated identifiers).

### Things we deliberately DO NOT replicate

1. **`internal/managementasset/updater.go`** (CLIProxyAPI) — pulls assets from `github.com`. Auto-fetching remote content is exactly the trust risk we reject. Dropped.
2. **`internal/updatecheck/checker.go`** (cpa-usage-keeper) — version ping to `api.github.com`. Dropped; `internal/version` is build-info only and never touches the network.
3. Any `github.com` calls in login flows are OAuth/asset conveniences — to be re-examined per provider when we implement that slice; reimplemented only if strictly required for the OAuth handshake, otherwise dropped.

> Audit is ongoing per vertical slice: before each provider is reimplemented, its upstream auth + request path is re-read and any non-provider egress is documented here.

## Per-slice notes

(filled in as each provider slice is built)
