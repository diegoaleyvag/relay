# ADR 0001 — A scripted, deterministic planner

## Status
Accepted.

## Context
Relay's thesis is about reliability, not autonomy. To demonstrate that work
survives failures we must be able to reproduce a run exactly — including which
tool is called at which step — across retries and restarts.

## Decision
The initial planner is **scripted and deterministic**: `Planner.Next(RunState)`
is a pure function with no I/O and no randomness, so identical state always
yields the identical action. It is implemented in
[`internal/planner`](../../internal/planner) and is labelled
"scripted-deterministic" everywhere — never "AI" or "reasoning".

A future model-based planner may be added **only behind the same `Planner`
port** and never as a foundation dependency.

## Consequences
- Re-planning after a restart reproduces the same step and the same
  idempotency key, which is what makes safe resume possible.
- The reducer's totality and the planner's output are unit-testable as pure
  functions with golden expectations.
- Findings are derived deterministically from source metadata plus the run seed,
  so a run's output is a function of `(corpus, seed, fault plan)`.
