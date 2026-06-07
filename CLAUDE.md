# cerber

A self-written, trust-first Go reimplementation that **merges three upstream projects into one binary**:

- **CLIProxyAPI** — multi-provider AI proxy (OpenAI/Claude/Gemini/Grok/Codex translation, OAuth CLI logins, multi-account round-robin).
- **cpa-usage-keeper** — per-account usage/quota polling + API.
- **CPA-Manager-Plus** — web management UI + manager-server.

Reference checkouts live at `~/code/CLIProxyAPI`, `~/code/cpa-usage-keeper`, `~/code/CPA-Manager-Plus`. They are **read-only spec, not source**. See `AUDIT.md`.

## Why this exists (non-negotiable principles)

The whole point is that we do **not** trust the upstream with our OAuth tokens and API keys. So:

1. **From scratch.** We do not copy upstream code. We read it to understand protocol/behavior, then write our own. No vendored upstream packages.
2. **No phone-home.** cerber makes outbound calls to exactly one class of host: the legitimate upstream provider APIs that a request is being routed to. No telemetry, no analytics, no version pings, no auto-download of code/assets. The only network destinations allowed in the codebase are provider endpoints and hosts explicitly present in the user's config. Any new outbound host is a red flag in review.
3. **Credentials never leave the box** except as the documented auth header to the provider that owns them. The credential store (`internal/credential`) is the most security-critical package — it is fully ours and reviewed line by line.
4. **Pinned, opt-in, never silent.** No feature may fetch and execute or persist remote content automatically (this is the upstream `managementasset/updater` + `updatecheck` behavior we deliberately dropped).

## Architecture

Single binary, `cmd/cerber`. Built in **vertical slices** — one provider end-to-end (server → access control → router → credential store → translator → upstream) before adding the next.

```
cmd/cerber/            entrypoint
internal/
  config/              load + validate config (no remote fetch)
  server/              http server, routing, middleware
  access/              client-side access control (who may call cerber)
  credential/          provider credential store (TRUST-CRITICAL, ours)
  provider/            provider abstraction + per-provider impls
  translator/          request/response schema translation between API dialects
  usage/               quota + usage tracking (ex cpa-usage-keeper)
  version/             build info only — does NOT contact the network
web/                   management UI (final phase)
```

## Testing — hard rules

- **Coverage gate: >85%** across the module. CI/`make test` fails below it. New packages ship with their tests in the same change.
- **Mocks: mockery only. No hand-written mocks, ever.** Interfaces that need mocking are declared in our packages; mocks are generated via `make mocks` from `.mockery.yaml` into `*/mocks/`. Generated mock files are committed but never hand-edited.
- Prefer table-driven tests. Test behavior through the package's public interface, not internals.
- Network is never hit in unit tests — provider HTTP clients sit behind an interface and are mocked. Integration tests that hit real providers are build-tagged `//go:build integration` and excluded from the coverage gate run.

## Definition of Done + git-as-log

Two artifacts, never conflated: `DEFINITION_OF_DONE.md` = the living SPEC ("how it must
behave now", one entry per feature, edited in place); **git history = the LOG** — per-file
"why" via `git log --follow -- <file>` (no separate worklog).

- Behaviour change → update the feature's `DEFINITION_OF_DONE.md` entry **in the same commit**. **Never invent a DoD — if criteria are unclear, ASK.**
- `feat`/`fix` commit messages MUST carry `Why:` and `Expected:` lines; use only the
  canonical types `feat fix refactor perf docs test chore style build ci revert`
  (enforced by the `commit-msg` hook — `make hooks` installs it).
- Refactor/test/chore with no behaviour change may skip the DoD entry — say so.

## Commands

```
make hooks      # install git commit-msg hook (run once per clone)
make mocks      # regenerate all mocks via mockery (run after changing any mocked interface)
make test       # unit tests + coverage gate (>85%)
make cover      # write coverage.out and print per-package coverage
make build      # build ./cmd/cerber
make lint       # gofmt + go vet
```

## Conventions

- Module path is `cerber` (bare). Change with a single `go mod edit -module` + import rewrite if/when published.
- Go 1.25. `gofmt`'d, `go vet`-clean.
- Errors wrapped with `%w` and context; no naked `panic` in library code.
- Config is the single source of truth for which hosts cerber may talk to.
