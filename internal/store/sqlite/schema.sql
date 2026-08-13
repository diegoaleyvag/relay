-- Schema for the Relay SQLite adapter.
--
-- Every statement is idempotent (CREATE TABLE/INDEX IF NOT EXISTS) so
-- migrate() can be run on every Open call, including against a database file
-- created by an earlier version of this schema.
--
-- Timestamps are stored as TEXT in RFC3339Nano. Columns documented as
-- "nullable" hold SQL NULL for an unset/zero time.Time (e.g. a run that has
-- never entered backoff has next_wake_at = NULL); all other timestamp columns
-- are NOT NULL.
--
-- The full RunState (and Checkpoint) is additionally stored as a JSON blob in
-- state_json so the engine's resume path never has to reconstruct a RunState
-- from denormalized columns. The denormalized runs columns exist purely to
-- make cheap, indexable reads (resumable run listing, UI dashboards) possible
-- without deserializing JSON.

CREATE TABLE IF NOT EXISTS runs (
    id           TEXT PRIMARY KEY,
    version      INTEGER NOT NULL,
    phase        TEXT NOT NULL,
    plan_step    INTEGER NOT NULL,
    attempt      INTEGER NOT NULL,
    next_wake_at TEXT,
    deadline     TEXT,
    state_json   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_runs_phase ON runs(phase);

-- transitions is the audit log: one row per durable state change. new_version
-- is unique per run because a version is only ever assigned to exactly one
-- committed transition (that is the compare-and-swap invariant Commit
-- enforces).
CREATE TABLE IF NOT EXISTS transitions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL REFERENCES runs(id),
    prior_version   INTEGER NOT NULL,
    new_version     INTEGER NOT NULL,
    action_kind     TEXT NOT NULL,
    tool            TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    input_hash      TEXT NOT NULL,
    from_phase      TEXT NOT NULL,
    to_phase        TEXT NOT NULL,
    evidence_json   TEXT,
    created_at      TEXT NOT NULL,
    UNIQUE (run_id, new_version)
);

CREATE INDEX IF NOT EXISTS idx_transitions_run ON transitions(run_id);

-- checkpoints holds one full redacted RunState snapshot per committed
-- version. (run_id, version) is the primary key: it mirrors runs.version so
-- "the latest checkpoint" and "the current run row" always agree.
CREATE TABLE IF NOT EXISTS checkpoints (
    run_id     TEXT NOT NULL REFERENCES runs(id),
    version    INTEGER NOT NULL,
    state_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, version)
);

-- events is the append-only, redacted history shown in the control-room UI.
-- seq is assigned by Commit as MAX(seq)+1 per run, so it is strictly
-- increasing and gap-free within a run.
CREATE TABLE IF NOT EXISTS events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT NOT NULL REFERENCES runs(id),
    seq           INTEGER NOT NULL,
    version       INTEGER NOT NULL,
    kind          TEXT NOT NULL,
    summary       TEXT NOT NULL,
    evidence_json TEXT,
    created_at    TEXT NOT NULL,
    UNIQUE (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id);

-- findings are deduped on idempotency_key: record_finding is a side-effecting
-- tool, so a retried or replayed call must not create a second row.
CREATE TABLE IF NOT EXISTS findings (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL REFERENCES runs(id),
    idempotency_key TEXT NOT NULL UNIQUE,
    source_id       TEXT NOT NULL,
    claim           TEXT NOT NULL,
    evidence_json   TEXT,
    created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_findings_run ON findings(run_id);

-- human_reviews are likewise deduped on idempotency_key. status starts at
-- "pending" and is later updated in place by a ReviewResolution (approve /
-- reject), which is why resolved_at is nullable.
CREATE TABLE IF NOT EXISTS human_reviews (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL REFERENCES runs(id),
    idempotency_key TEXT NOT NULL UNIQUE,
    review_id       TEXT NOT NULL,
    reason          TEXT NOT NULL,
    severity        TEXT NOT NULL,
    status          TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    resolved_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_human_reviews_run ON human_reviews(run_id);

-- side_effects is the intent -> confirm idempotency ledger for both
-- side-effecting tools. idempotency_key is the primary key: a side effect has
-- exactly one ledger row, which Commit upserts in place as it progresses from
-- INTENT to CONFIRMED (or FAILED).
CREATE TABLE IF NOT EXISTS side_effects (
    idempotency_key TEXT PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES runs(id),
    tool            TEXT NOT NULL,
    status          TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    response_json   TEXT,
    attempt         INTEGER NOT NULL,
    created_at      TEXT NOT NULL,
    confirmed_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_side_effects_run ON side_effects(run_id);
