# Sequence: crash and resume (exactly-once)

A process killed after a side effect executes but before its confirm checkpoint
resumes without duplicating the effect.

```mermaid
sequenceDiagram
    participant E1 as Engine (process 1)
    participant T as ToolPort (MCP)
    participant S as Store (SQLite)
    participant E2 as Engine (process 2)

    E1->>S: Commit(INTENT: record_finding key K, Pending set) + checkpoint
    E1->>T: Invoke(record_finding, key K)
    T-->>E1: finding_id (recorded)
    Note over E1: CRASH before the confirm checkpoint

    Note over E2: process restart → ResumeAll
    E2->>S: LatestCheckpoint → state with Pending{key K}
    E2->>S: FindingExists(K)? → no (confirm never landed)
    E2->>T: Invoke(record_finding, key K) — re-execute
    T-->>E2: duplicate=true (receiver deduped on K)
    E2->>S: Commit(CONFIRM + FindingRow ON CONFLICT DO NOTHING) + checkpoint
    Note over S: exactly one finding row for the source
    E2->>S: continue plan → succeeded
```

Correctness rests on three invariants:

1. the checkpoint and the `runs.version` are written in one transaction, so the
   latest checkpoint is always consistent;
2. the planner and the idempotency key are deterministic, so re-planning after a
   restart reproduces the identical step and key; and
3. the `side_effects` ledger plus the `UNIQUE(idempotency_key)` constraint make
   re-execution a no-op.
