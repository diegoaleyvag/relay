# Five-minute demo

## 0. Prerequisites (30s)

Go 1.26.x. On macOS: `brew install go`. Everything runs offline against the
module cache; no network or external services are needed.

## 1. Prove it works (90s)

```bash
make build         # static, cgo-free binaries
make test-race     # unit + integration + the process-restart proof, under -race
make boundary      # the domain core imports no adapter technology
```

`make test-race` runs, among others:
- `TestIntegration*` — all six failure scenarios end-to-end (real SQLite store,
  real MCP server over the in-memory transport, real corpus, fault injection);
- `TestProcessRestartResumesExactlyOnce` — a **real subprocess** is killed right
  after `record_finding` executes but before its checkpoint, and a fresh process
  resumes to exactly one finding per source.

## 2. Drive it in the control room (150s)

```bash
make run           # http://127.0.0.1:8080
```

Open the control room and start a run for each scenario from the "Start a new
run" form. Watch the timeline update live (HTMX polls once a second and stops on
its own when the run is terminal):

| Scenario | What you'll see |
|----------|-----------------|
| **Happy path** | 6 sources listed, read and recorded; `Succeeded` with 6 findings |
| **Tool timeout (recovers)** | the first read times out twice → `backoff` → a retry wins → `Succeeded` (Retries used ≥ 2) |
| **Malformed response (skips)** | the first source is skipped; `Succeeded` (degraded) with 5 findings and a `source_skipped` event |
| **Duplicate delivery** | the finding is delivered twice; one durable finding and a `duplicate_suppressed` event |
| **Permission denied** | a later read is denied; `Failed` with the earlier findings preserved |
| **Human review** | the run parks in `Awaiting human`; click **Approve** → `Succeeded`, or **Reject** → `Failed` |

Note the controls (Resume / Cancel / Approve / Reject) are enabled only for the
transitions that are legal in the run's current state, and the planner is always
labelled a "scripted plan step" — never AI.

## 3. See the traces (30s, optional)

```bash
RELAY_TRACES=stdout make run
```

Each run emits a `relay.run` span with `relay.tool_call` children carrying only
IDs, types, durations, outcomes, error codes and hashes — never source content.

## Notes

- The "process kill after a side effect" scenario is demonstrated by the restart
  test (a live server cannot kill itself mid-request); the control room shows the
  companion recovery paths (backoff→retry, awaiting-human→approve).
- State is durable: stop `make run` mid-flight and start it again — non-terminal
  runs resume automatically.
