# Configuration reference

cerber is configured by a single YAML file (default `config.yaml`, override with
`-config <path>`). The file is read once at startup, validated, and is the **only**
place that names hosts cerber may contact. Unknown fields are rejected.

```bash
./bin/cerber -config /etc/cerber/config.yaml
./bin/cerber -version
```

## Full example

```yaml
server:
  addr: ":8080"

access:
  keys:
    - "change-me-client-key"

providers:
  anthropic:
    base_url: "https://api.anthropic.com"
    version: "2023-06-01"
    timeout: "1800s"
    credentials:
      - type: api_key
        name: account-a
        key: "sk-ant-..."
      - type: oauth
        name: claude-code-account
        access_token: "..."
        refresh_token: "..."
        expires_at: 2026-01-01T00:00:00Z
```

## `server`

| Field | Type | Default | Description |
|---|---|---|---|
| `addr` | string | `:8080` | Listen address (`host:port`). |

## `access`

Controls which clients may call cerber. A client presents one of these keys as
`Authorization: Bearer <key>` or `x-api-key: <key>`.

| Field | Type | Required | Description |
|---|---|---|---|
| `keys` | list of string | yes (≥1) | Allowed client API keys. Empty/blank entries are rejected. |

These are **cerber's own** keys that you hand to your clients — they are not your
upstream provider keys.

## `providers.anthropic`

| Field | Type | Default | Description |
|---|---|---|---|
| `base_url` | string | `https://api.anthropic.com` | Anthropic origin. Must be an `http(s)` URL. The only host cerber will send these requests to. |
| `version` | string | `2023-06-01` | Value of the `anthropic-version` header. |
| `timeout` | duration | `1800s` | Upstream **silence** timeout, not a whole-request cap: applied as the wait for the first response byte AND a mid-stream idle-read timeout. A live stream that keeps sending bytes runs unbounded, so long LLM responses are never cut; only a silent/dead upstream is dropped. |
| `credentials` | list | yes (≥1) | Upstream accounts; cerber rotates across them. See below. |

### `credentials[]`

cerber round-robins across all credentials and sidelines (cooldown ~60s) any that
return `401`/`403`/`429` or a transport error, then tries the next.

| Field | Type | Applies to | Description |
|---|---|---|---|
| `type` | `api_key` \| `oauth` | both | Auth mechanism. Required. |
| `name` | string | both | Label for logs (defaults to `cred-<index>`). Never logged with secrets. |
| `key` | string | `api_key` | The `sk-ant-...` API key. Required for `api_key`. |
| `access_token` | string | `oauth` | Claude Code OAuth access token. Required for `oauth`. |
| `refresh_token` | string | `oauth` | OAuth refresh token; used to renew `access_token`. |
| `expires_at` | RFC 3339 time | `oauth` | When `access_token` expires. If set, cerber refreshes ~60s before. If omitted, no proactive refresh. |

See [Provider: Anthropic / Claude Code](providers/anthropic.md) for how to obtain
these values.

## Validation errors

cerber refuses to start (with a clear message) if: no access keys; a blank access
key; no providers; `base_url` is not `http(s)`; no credentials; an `api_key`
credential without `key`; an `oauth` credential without `access_token`; an unknown
or missing credential `type`; or an invalid `timeout`.
