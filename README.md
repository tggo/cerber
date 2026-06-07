# cerber

A trust-first AI provider proxy in Go. Exposes an **OpenAI-compatible** API and a
**native Anthropic** API, backed by one or more upstream Anthropic accounts
(API key or Claude Code OAuth), with round-robin rotation and automatic OAuth
token refresh.

Built from scratch (no upstream code) so it can be fully audited: cerber has **no
telemetry, no update checks, and never auto-downloads code or assets**. It only
contacts the provider base URL in your config. See [`AUDIT.md`](AUDIT.md).

## Quick start

```bash
make build
cp config.example.yaml config.yaml   # edit access.keys + an Anthropic credential
./bin/cerber -config config.yaml
```

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer <your-access-key>" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}'
```

## Documentation

- [docs/](docs/README.md) — overview & endpoints
- [Configuration reference](docs/configuration.md)
- [Anthropic / Claude Code provider](docs/providers/anthropic.md)
- [Client usage](docs/usage.md)

## Development

```bash
make hooks    # install the commit-msg hook (once per clone)
make test     # unit tests + coverage gate (>=85%)
make mocks    # regenerate mockery mocks
make lint     # gofmt + go vet
```

See [`CLAUDE.md`](CLAUDE.md) for engineering conventions and
[`DEFINITION_OF_DONE.md`](DEFINITION_OF_DONE.md) for the living spec.

## Providers

On `/v1/chat/completions`, cerber routes by model name:

| Model prefix | Provider | How |
|---|---|---|
| `claude*` (default) | Anthropic | OpenAI↔Anthropic translation; also native `/v1/messages` |
| `gpt* o1* o3* o4* chatgpt*` | OpenAI | passthrough |
| `gemini*` | Gemini | OpenAI↔Gemini translation |
| `grok*` | xAI / Grok | passthrough (OpenAI-compatible) |

Override routing with `providers.routing` in the config. Each provider pools
multiple credentials with rotation; Anthropic adds OAuth refresh + Claude Code spoofing.

## Observability

- `GET /admin/stats` — JSON usage (requests/errors/tokens per credential + model)
- `GET /metrics` — Prometheus counters
- `GET /dashboard` — self-contained usage UI
- Structured zap logs to `./logs/<date>.log` + stdout

## Status

All three providers work end-to-end (live-tested). Planned: interactive
`--claude-login`, OAuth token persistence across restart, Grok, and tools/function
calling on the OpenAI-compatible endpoint for non-OpenAI providers.
