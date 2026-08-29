# Relay

**A reliability lab for checkpointed, observable agent workflows.**
Decision explored: *What should survive a failure?*

Relay is a small, deterministic agent workflow that proves useful work can
survive timeouts, malformed tool responses, duplicate requests, process
restarts and human escalation. It is **not** a general autonomous agent — it is
a bounded reliability laboratory with explicit failure semantics.

The workflow is a synthetic public-research task over a small local corpus,
driven by a **scripted, deterministic planner** (never a model — the planner is
labelled "scripted plan step" everywhere, never "AI reasoning"). It exposes four
MCP-facing tools:

| Tool | Kind | Purpose |
|------|------|---------|
| `list_sources` | read | list synthetic corpus sources (metadata only) |
| `read_source` | read | read one source with a size cap |
| `record_finding` | side effect | durably record a finding (exactly-once) |
| `request_human_review` | side effect | escalate to an explicit human-review state |

## What survives a failure

Six failure scenarios are injected deterministically and made visible in the UI
and telemetry:

1. **Tool timeout** — bounded exponential backoff, then success or a terminal
   `RETRIES_EXHAUSTED`; partial findings retained.
2. **Malformed response** — the source is skipped, the run completes degraded,
   partials preserved.
3. **Duplicate delivery** — the second delivery is a no-op; exactly one finding.
4. **Process termination after a side effect** — restart resumes without
   duplicating `record_finding`.
5. **Permission denial** — fails closed into an explicit, recoverable state;
   partials preserved.
6. **Human review required** — an explicit `awaiting_human` state, resumable on
   approve/reject.

See [`docs/failure-semantics.md`](docs/failure-semantics.md) for the full
state / retries / preserved-artifacts / outcome table.

## Architecture

Hexagonal (ports & adapters). The domain core (`internal/core`) and the planner
(`internal/planner`) depend only on the standard library — never on HTTP, HTMX,
SQLite or MCP types. Adapters translate at the boundaries:

- `internal/store/sqlite` — durable state, checkpoints and the idempotency
  ledger (pure-Go `modernc.org/sqlite`, WAL, optimistic version CAS).
- `internal/mcp` — the four tools + a `ToolPort` adapter over the official Go
  MCP SDK.
- `internal/faults` — deterministic fault injection for the six scenarios.
- `internal/telemetry` — OpenTelemetry spans with redaction (IDs/types/durations
  only, never payloads).
- `internal/web` — a server-rendered `html/template` + HTMX control room.

The boundary is enforced mechanically by `make boundary` and `depguard`.

## Quickstart

Requires Go 1.26.x (`brew install go`).

```bash
make build        # build both binaries
make test-race    # unit + integration tests with the race detector
make boundary     # assert the hexagonal boundary holds
make run          # start the control room
```

Then open the control room at http://127.0.0.1:8080 and start a run for each
scenario. See [`docs/DEMO.md`](docs/DEMO.md) for the five-minute walkthrough.

## Static Five Decisions preview

`preview/` is a deterministic, static guided sample for portfolio review. It
does not connect to Relay, show live state, start runs, or send POST requests.
Build-check it with `make preview-build` and validate its local brand snapshot
and project manifest with `make brand-check`. The interactive demonstration
remains local-only: run `make run` and follow [`docs/DEMO.md`](docs/DEMO.md).

## Documentation

- [Failure semantics](docs/failure-semantics.md) — the six scenarios as a table
- [State machine](docs/architecture/state-machine.md) — Mermaid lifecycle diagram
- [Retry](docs/architecture/sequence-retry.md) and [resume](docs/architecture/sequence-resume.md) sequence diagrams
- [MCP boundary](docs/mcp-boundary.md) — tools, port, transports, protocol version
- [Threat model](docs/threat-model.md) — trust boundaries and redaction policy
- ADRs: [deterministic planner](docs/adr/0001-deterministic-planner.md),
  [SQLite checkpoints](docs/adr/0002-sqlite-checkpoints.md),
  [idempotency](docs/adr/0003-idempotency.md),
  [pure-Go driver](docs/adr/0004-pure-go-sqlite-driver.md)
- [HANDOFF.md](HANDOFF.md) — commands, tests, changed files, known risks

## Status

The foundation vertical slice is complete: the happy path and all six injected
failure scenarios are deterministic, a real process restart resumes without
duplicating side effects, and the HTMX control room drives everything. No live
model, external browser or additional tools are added until the slice is
reliable.

## Safety & privacy

Tools are allowlisted and fail closed on unknown names. Inputs are validated and
size-limited at the adapters. SQLite and logs contain no credentials. Telemetry
records IDs, types, durations and outcomes — not source content. The UI escapes
all displayed payloads. Model/tool output never directly authorizes a privileged
action. See [`docs/threat-model.md`](docs/threat-model.md).

## License

The official Go MCP SDK is Apache-2.0. A project license will be added before
any publication.
