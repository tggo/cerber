# cerber documentation

cerber is a trust-first AI provider proxy. It exposes an OpenAI-compatible API and
a native Anthropic API, backed by one or more upstream Anthropic accounts (API key
or Claude Code OAuth), and rotates across them.

> **Trust model.** cerber only ever makes outbound calls to the provider base URL
> in your config (and the Anthropic OAuth token endpoint for refresh). It has no
> telemetry, no update checks, and never auto-downloads code or assets. Your
> credentials are only sent as auth headers to the provider that owns them.
> See [`../AUDIT.md`](../AUDIT.md).

## Contents

- [Configuration reference](configuration.md) — every `config.yaml` field.
- [Provider: Anthropic / Claude Code](providers/anthropic.md) — API key and OAuth setup.
- [Client usage](usage.md) — how to call cerber from OpenAI SDKs and the Anthropic API.

## Quick start

```bash
# 1. build
make build

# 2. create your config
cp config.example.yaml config.yaml
$EDITOR config.yaml         # set access.keys and at least one Anthropic credential

# 3. run
./bin/cerber -config config.yaml
# cerber listening on :8080 (anthropic, 1 credential(s))

# 4. test
curl localhost:8080/healthz
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer <one-of-your-access-keys>" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}'
```

## Endpoints

| Method & path | Dialect | Description |
|---|---|---|
| `GET /healthz` | — | Liveness probe, returns `ok`. |
| `POST /v1/messages` | Anthropic native | Pass-through to Anthropic Messages (streaming preserved). |
| `POST /v1/chat/completions` | OpenAI-compatible | Translated to/from Anthropic (stream + non-stream). |

All provider endpoints require a client API key — see [Configuration](configuration.md#access).
