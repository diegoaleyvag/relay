# HANDOFF

Relay — a bounded reliability lab. Foundation vertical slice is complete: the
happy path and all six injected failure scenarios are deterministic, a real
process restart resumes without duplicating side effects, and a live HTMX control
room drives everything.

## Commands

Go is expected on PATH (`brew install go`). If it is not (e.g. a non-login
shell), prefix with `make GO=/opt/homebrew/bin/go <target>`.

```bash
make build        # static, cgo-free binaries into bin/
make test         # unit + integration tests
make test-race    # the above with the race detector (requires cgo — enabled for tests)
make boundary     # assert internal/core and internal/planner import no adapters
make lint         # golangci-lint (installs the pinned v2.12.2 on first run)
make doc-lint     # reject template.HTML/JS and "AI reasoning" wording
make fuzz         # short fuzz smoke over the parsing/decoding targets
make run          # start the control room (http://127.0.0.1:8080)
make tools        # run the MCP tool server over stdio
```

Environment for `cmd/relay`: `RELAY_ADDR` (default 127.0.0.1:8080), `RELAY_DB`
(default relay.db), `RELAY_TRACES` (`off`|`stdout`).

## Layout (hexagonal)

```
cmd/relay          composition root: control room + engine host + scenario runner
cmd/relay-tools    MCP stdio server exposing the four tools
internal/core      DOMAIN — pure, stdlib only: state machine, reducer, ports, errors
internal/planner   scripted deterministic planner (imports core only)
internal/engine    orchestrator: drive loop, retry, resume, exactly-once; + tests
internal/store/sqlite   core.Repository on modernc.org/sqlite (WAL, CAS, atomic Commit)
internal/mcp       four typed tools + allowlist server + MCPToolPort client
internal/corpus    embedded synthetic 6-source corpus + validated loader
internal/clock     SystemClock + deterministic virtual ManualClock
internal/faults    FaultyToolPort: the six scenarios (+ bounded Times faults)
internal/telemetry OpenTelemetry adapter, allowlisted attributes, redaction
internal/web       server-rendered html/template + HTMX control room
internal/scenarios named scenario -> fault plan mapping
docs/              failure semantics, diagrams, MCP boundary, threat model, ADRs
```

## Key tests to trust

- `internal/engine/integration_test.go` — all six scenarios end-to-end.
- `internal/engine/restart_test.go` — real subprocess kill + resume, exactly one
  finding per source.
- `internal/core/reduce_test.go` — reducer totality + every scenario at the unit
  level.
- `internal/store/sqlite/store_test.go` — version CAS conflict + finding dedupe.
- `internal/telemetry/telemetry_test.go` — no attribute outside the allowlist,
  no payload leak.

## Known risks / follow-ups

- `synchronous=NORMAL` is durable across process kill (the modelled failure) but
  not a power cut; flip to `FULL` if power-loss durability is ever in scope.
- The per-call timeout uses a real `context.WithTimeout` (300ms); backoff uses
  the injected clock. Fully virtualizing tool timeouts would need the
  `ctxWithClockTimeout` bridge noted in the design.
- The control room tracks a run's scenario label in memory (best-effort), since
  `RunState` has no scenario field; after a restart older runs show "unknown".
- Run-wide `Deadline` is checked once per loop iteration (not mid-sleep/-call),
  so a run can overshoot by up to one backoff/`PerCallTimeout` before cancelling;
  the production runner passes a zero deadline, so this is latent.
- Concurrent runs share one in-memory MCP session; fine for the lab, revisit if
  scaling out.
- CI (`.github/workflows/ci.yml`) is authored but intentionally not pushed.

## Review prompts (for an independent/adversarial reviewer)

- Can any interleaving of crash + resume duplicate a `record_finding`, or skip
  one? Attack the intent→execute→confirm window and the version CAS.
- Can a stale writer ever win a commit? Check `UPDATE ... WHERE version = ?` +
  `UNIQUE(run_id, new_version)`.
- Is the reducer total — does any (phase, input) pair panic or make an illegal
  transition durable? See `TestReduceTotality`.
- Can source content reach telemetry, the event log, or the UI? Trace every path
  from a tool response to a span attribute / template.
- Are retry budgets and terminal states enforced exactly (off-by-one on
  `MaxAttempts`/`MaxRetries`)?
