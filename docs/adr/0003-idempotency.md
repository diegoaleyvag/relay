# ADR 0003 — Idempotency and exactly-once side effects

## Status
Accepted.

## Context
`record_finding` and `request_human_review` are side-effecting. Retries and a
crash between executing an effect and checkpointing it must not duplicate the
effect.

## Decision
- **Deterministic keys.** The idempotency key is
  `hex(sha256(runID | step | tool))` — a function of the logical operation's
  identity, independent of the input and the attempt. In the fixed plan a given
  `(run, step, tool)` always means the same effect, so every retry and every
  post-restart re-execution produce the same key. Input-independence also lets
  the key be embedded in the tool input without self-reference; the full input is
  recorded separately as an audit hash.
- **Write-ahead intent → execute → confirm.** The engine commits an `INTENT`
  ledger row (with a checkpoint), executes the tool, then commits `CONFIRMED`
  plus the durable finding/review row — the latter via
  `INSERT ... ON CONFLICT(idempotency_key) DO NOTHING`.
- **Resume reconciliation.** On restart a run with a pending intent re-plans the
  identical step, re-executes (the receiver dedupes on the key), and confirms;
  the unique constraint makes an already-applied effect a no-op.

## Consequences
- Exactly-once for the durable effect, proven by
  `TestIntegrationKillAfterEffectResumesExactlyOnce` and the process-level restart
  test.
- Duplicate delivery (the same request twice) collapses to one effect and a
  `duplicate_suppressed` event.
- For a genuinely external, non-idempotent receiver, exactly-once is impossible;
  the fallback would be at-most-once plus an `UNKNOWN` escalation. Relay's
  receivers are its own tables, so true exactly-once is achievable.
