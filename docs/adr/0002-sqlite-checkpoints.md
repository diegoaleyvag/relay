# ADR 0002 — SQLite checkpoints with optimistic versioning

## Status
Accepted.

## Context
A run must survive a process restart and resume from a consistent point without
duplicating side effects, and stale writers must never clobber newer state.

## Decision
Durable state lives in SQLite. Every durable transition is written in **one
transaction** that: compare-and-swaps `runs.version` (`WHERE version = expected`),
appends an audit `transitions` row, writes a full-state `checkpoints` row at the
new version, appends redacted `events`, and performs any side-effect/finding/
review writes. The `CommitBundle` in
[`internal/core/ports.go`](../../internal/core/ports.go) is the domain-owned unit
of this atomic write; the store adapter maps it to SQL.

- **Optimistic concurrency**: if the CAS affects zero rows, the writer is stale
  and the commit returns `ErrVersionConflict`. `UNIQUE(run_id, new_version)` is a
  second guard.
- **Checkpoint invariant**: because the checkpoint and `runs.version` are written
  together, the latest checkpoint is always consistent and is the sole source of
  truth for resume.
- **Durability**: WAL + `synchronous=NORMAL`, which survives process `SIGKILL`
  (the modelled failure). A single-writer connection pool serializes mutations.

## Consequences
- Resume loads the latest checkpoint and continues deterministically.
- Two racing writers cannot both advance a run.
- Power-loss durability is explicitly out of scope (documented in the threat
  model); `synchronous=FULL` would be the knob if it were needed.
