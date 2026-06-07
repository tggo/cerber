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

## Trust: no phone-home
**What:** cerber's only outbound network destinations are provider APIs being routed to (or hosts explicitly in config).
**DoD:**
- No telemetry/analytics/update-check/auto-asset-download code exists.
- `internal/version` never makes network calls.
**Verified:** `internal/version` is build-info only — 2026-06-07.
