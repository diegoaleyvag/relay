# Failure semantics

Every failure Relay handles falls into one of three classes, which fully
determine the engine's reaction:

| Class | Meaning | Engine reaction |
|-------|---------|-----------------|
| **retryable** | transient | bounded exponential backoff, retry with the same idempotency key |
| **skippable** | deterministic bad data | record the source as skipped, preserve partials, continue (degraded success) |
| **terminal** | unrecoverable | fail the run, preserving all partial results |

Error codes and their default classes live in
[`internal/core/error.go`](../internal/core/error.go). Policy can reclassify
`MALFORMED_RESPONSE` (skip vs fail) and `PERMISSION_DENIED` (fail vs escalate).

## The six required scenarios

Retry budget defaults: **3 attempts per step**, **5 retries per run**; backoff is
100ms × 2ⁿ capped at 2s with deterministic full jitter.

| # | Scenario | Trigger → code | Class | Retries | End state | Preserved artifacts | User-visible outcome |
|---|----------|----------------|-------|---------|-----------|---------------------|----------------------|
| 1 | **Tool timeout** | per-call deadline → `TIMEOUT` | retryable | ≤3/step, ≤5/run | `succeeded` if a retry wins, else `failed` (`RETRIES_EXHAUSTED`) | all prior findings, full event + transition log | "Completed with N findings" or "Failed after retries reading source X; N partial findings retained" |
| 2 | **Malformed response** | unparseable/oversized → `MALFORMED_RESPONSE` | skippable | 0 (skip) | `succeeded` (degraded) | good-source findings; the bad source recorded in `Skipped` | "Completed; 1 source unreadable (malformed), skipped" |
| 3 | **Duplicate delivery** | same request/idempotency key delivered twice | n/a | n/a | unchanged (`succeeded`) | exactly one finding per source | history shows one confirm + a `duplicate_suppressed` event |
| 4 | **Process termination after a side effect** | crash between tool execution and the confirm checkpoint | n/a | 0 re-executions of the *durable* effect | resume → `succeeded` | the finding **exactly once**, plus a resume trail | run finishes as if the crash never happened |
| 5 | **Permission denial** | disallowed/blocked tool → `PERMISSION_DENIED` | terminal (default; escalate optional) | 0 | `failed` (or `awaiting_human` under the escalation policy) | prior findings + an event naming the denied action | "Stopped: permission denied for &lt;action&gt;; N findings retained" |
| 6 | **Human review required** | `request_human_review` (or the `RequireReview` policy) | n/a | n/a (bounded by the run deadline) | `running`→`succeeded` on approve; `failed` on reject; `cancelled` if the deadline passes while parked | everything; exactly one review row | "Awaiting human review: &lt;reason&gt;" then the resolved outcome |

## Why exactly-once holds across a crash (scenario 4)

A side effect is committed in two durable steps and reconciled on resume:

1. **INTENT** — the engine commits `Pending` + a `side_effects` ledger row with
   status `INTENT`, atomically with a checkpoint. "We intend effect *K*" is now
   durable.
2. **EXECUTE** — the tool runs (`record_finding` with idempotency key *K*).
3. **CONFIRM** — the engine commits the durable `FindingRow` (via
   `INSERT ... ON CONFLICT(idempotency_key) DO NOTHING`), flips the ledger to
   `CONFIRMED`, clears `Pending`, and advances — all in one transaction.

A crash between EXECUTE and CONFIRM leaves the checkpoint with `Pending` set and
no durable finding. On restart the engine re-plans the identical step (the
planner is pure and the idempotency key is `sha256(runID | step | tool)`,
input-independent), re-executes, and confirms. The MCP receiver dedupes on the
same key, and the `UNIQUE(idempotency_key)` constraint makes the durable insert
a no-op if it already landed. The effect is therefore applied **exactly once**.

The invariant is proven end-to-end by
`TestIntegrationKillAfterEffectResumesExactlyOnce` in
[`internal/engine/integration_test.go`](../internal/engine/integration_test.go)
and by the process-level restart test in the same package.
