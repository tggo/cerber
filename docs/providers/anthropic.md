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

### Obtaining the OAuth tokens

The OAuth values come from a Claude Code authorization (`claude.ai/oauth/authorize`
→ `api.anthropic.com/v1/oauth/token`, PKCE). For now, supply tokens you already
have (e.g. from an existing Claude Code login) in the config.

> **Planned:** an interactive `cerber --claude-login` command that runs the OAuth
> PKCE flow (local callback on port `54545`, with a `--no-browser` option to print
> the URL) and writes the tokens for you. Until then, OAuth is configured by hand.

### Known limitations (current)

- Refreshed tokens are kept **in memory only** — they are not yet written back to
  disk, so after a restart cerber refreshes again from the `refresh_token` in your
  config. If that token has since been rotated away, re-supply a current one.
- Refresh is **proactive (by expiry)** only; there is no reactive refresh on a
  `401` yet.
- cerber sends the **minimal** Claude Code system prefix, not the full Claude Code
  fingerprint (billing headers, full static prompt, tool renaming).

## Rotation & failure handling

For both modes, on an upstream `401`/`403`/`429` or a network error cerber puts
that credential in a short cooldown and tries the next one. If every credential is
unavailable it returns `503`; other upstream errors are surfaced as `502` (or
relayed as-is for the OpenAI endpoint).
