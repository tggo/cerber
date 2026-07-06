# Provider: Anthropic / Claude Code

cerber talks to Anthropic in two credential modes. You can mix several of either
kind in one config; cerber rotates across them and sidelines any that fail.

- **API key** — a standard `sk-ant-...` key from the Anthropic Console.
- **Claude Code OAuth** — the OAuth token a Claude Code login produces. cerber
  refreshes it automatically and adds the Claude Code system prefix Anthropic
  requires for OAuth requests.

## Mode 1 — API key

The simplest setup. Get a key from <https://console.anthropic.com> → **API Keys**.

```yaml
providers:
  anthropic:
    credentials:
      - type: api_key
        name: console-key
        key: "sk-ant-api03-..."
```

cerber sends it upstream as the `x-api-key` header. Nothing else is required.

## Mode 2 — Claude Code OAuth

Use this to ride on a Claude Code (Pro/Max/Team) subscription instead of metered
API billing.

```yaml
providers:
  anthropic:
    credentials:
      - type: oauth
        name: claude-code
        access_token: "..."
        refresh_token: "..."
        expires_at: 2026-01-01T00:00:00Z
```

How cerber uses it:

- **Auth header.** Sent as `Authorization: Bearer <access_token>` with
  `anthropic-beta: oauth-2025-04-20` (and `x-api-key` is *not* sent).
- **System prefix.** Anthropic rejects OAuth requests whose system prompt does not
  begin with `You are Claude Code, Anthropic's official CLI for Claude.` cerber
  injects that block automatically and keeps your own system content after it.
- **Refresh.** When `access_token` is within ~60s of `expires_at` (or already
  expired), cerber exchanges `refresh_token` at
  `POST {base_url}/v1/oauth/token` (`grant_type=refresh_token`, public client id
  `9d1c250a-e61b-44d9-88ed-5944d1962f5e`) and uses the fresh token. Anthropic
  rotates the refresh token on each refresh.

### Obtaining the OAuth tokens — `cerber --claude-login`

Run the interactive login; it performs the OAuth PKCE flow and saves the tokens:

```bash
cerber --claude-login                 # opens your browser, callback on :54545
cerber --claude-login --no-browser    # prints the URL instead of opening a browser
cerber --claude-login --login-port 5555   # use a different callback port
cerber --claude-login --auth-dir ./auths   # where tokens are written (default ./auths)
```

After you authorize in the browser, tokens are written to `<auth_dir>/<email>.json`
(file mode `0600`). On the next `cerber` start they are **loaded automatically** and
merged with any credentials in `config.yaml` — you do not need to paste them into
the config. Refreshed tokens are written back to the same file, so logins survive
restarts.

You can also still supply OAuth tokens by hand in `config.yaml` (above) if you
obtained them elsewhere.

### Known limitations (current)

- Refresh is **proactive (by expiry)** only; there is no reactive refresh on a
  `401` yet.
- cerber sends the **minimal** Claude Code system prefix, not the full Claude Code
  fingerprint (billing headers, full static prompt, tool renaming).

## Rotation & failure handling

For both modes, on an upstream `401`/`403`/`429` or a network error cerber puts
that credential in a short cooldown and tries the next one. If every credential is
unavailable it returns `503`; other upstream errors are surfaced as `502` (or
relayed as-is for the OpenAI endpoint).

## Automatic prompt-cache injection (opt-in)

Anthropic [prompt caching](https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching)
lets you mark a stable request prefix (system prompt, tool definitions, long
context) with `cache_control` so it's cached upstream and re-read cheaply
instead of re-billed on every call. Many clients (n8n, Flowise, Make.com, most
SDK integrations, plain `curl`) never set it and pay full price each time.

cerber can inject the markers for you on the native `/v1/messages` path:

```yaml
providers:
  anthropic:
    cache:
      auto_inject: true
      strategy: moderate    # conservative | moderate | aggressive
      min_tokens: 1024      # est-token floor before a prefix earns a breakpoint
```

- **Off by default** — without this block the native path is a pure byte
  passthrough.
- **Strategy** sets the breakpoint budget and placement (Anthropic caches in the
  order tools → system → messages, max 4 markers per request):
  - `conservative` — tools + system prefix only (≤2 markers)
  - `moderate` (default) — the above + one message-history breakpoint (≤3)
  - `aggressive` — a second message-history breakpoint (≤4)
- **Non-destructive.** If your request already carries any `cache_control`,
  cerber leaves it completely untouched — you keep control of your own
  breakpoints. String `system` is converted to a single cached text block;
  message breakpoints are only added to content already in block form (string
  content is never reshaped). If a body can't be parsed it's forwarded as-is —
  injection never fails a request.
- Only prefixes whose estimated size reaches `min_tokens` are marked (a marker
  below Anthropic's minimum cacheable length is ignored upstream and would just
  waste one of the four slots). Note Anthropic's real minimum is 1024 tokens for
  most models, 2048 for Haiku.

Cache hits show up in the response `usage` as `cache_read_input_tokens`; the
first (writing) call reports `cache_creation_input_tokens`.
