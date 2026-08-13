# Sequence: retry with bounded backoff

A retryable tool failure (e.g. a timeout) is retried with the same idempotency
key after a deterministic backoff, bounded by the retry budget. Each durable
transition is checkpointed.

```mermaid
sequenceDiagram
    participant P as Planner (scripted)
    participant E as Engine
    participant T as ToolPort (MCP client)
    participant S as Store (SQLite)

    E->>P: Next(state)
    P-->>E: call_tool read_source(src) @ step k
    E->>T: Invoke(read_source, attempt 0)
    T-->>E: TIMEOUT (retryable, injected=true)
    E->>S: Commit(phase=backoff, attempt=1, next_wake_at) + checkpoint
    Note over E: Clock.Sleep(backoff delay = 100ms·2⁰ ± jitter)
    E->>P: Next(state) — identical, step k unchanged
    P-->>E: call_tool read_source(src) @ step k
    E->>T: Invoke(read_source, attempt 1)
    T-->>E: ok
    E->>S: Commit(advance to step k+1) + checkpoint
```

If the budget is exhausted before a retry wins, the run commits a terminal
`failed` with code `RETRIES_EXHAUSTED`, and all findings recorded before the
failing step remain inspectable.
