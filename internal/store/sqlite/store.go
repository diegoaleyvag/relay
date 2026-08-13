// Package sqlite implements core.Repository on top of modernc.org/sqlite, a
// CGo-free (pure Go) SQLite driver. It is the durable-state adapter for the
// Relay engine: every RunState mutation the engine performs is expressed as a
// core.CommitBundle and lands here in a single ACID transaction (see
// Store.Commit in tx.go).
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/diegoaleyvag/relay/internal/core"
)

// Store is a core.Repository backed by a single SQLite database file. It
// deliberately opens two independent *sql.DB handles against that one file:
//
//   - writer has exactly one connection (SetMaxOpenConns(1)). SQLite allows
//     only one writer transaction at a time; funnelling every mutation
//     through a single connection means Go's database/sql pool — not SQLite's
//     SQLITE_BUSY/retry machinery — serializes writers, so busy_timeout only
//     ever has to absorb contention from the reader pool.
//   - reader is an ordinary (multi-connection) pool used for every read-only
//     Repository method. Under WAL journaling, readers never block behind the
//     writer and vice versa, so read traffic (the UI's ListRuns/Events/
//     Findings) can be fully concurrent with commits.
type Store struct {
	writer *sql.DB
	reader *sql.DB
}

// Open opens (creating if it does not already exist) the SQLite database at
// path, applies the pragmas required for a durable single-writer/many-reader
// workload, and applies the schema migration. It is safe to call Open
// concurrently against the same path from multiple processes: migrate is
// idempotent and SQLite itself arbitrates the resulting file locks.
func Open(path string) (*Store, error) {
	dsn := buildDSN(path)

	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)

	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("sqlite: open reader: %w", err)
	}

	ctx := context.Background()
	if err := writer.PingContext(ctx); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("sqlite: ping writer: %w", err)
	}
	if err := reader.PingContext(ctx); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("sqlite: ping reader: %w", err)
	}

	if err := migrate(ctx, writer); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, err
	}

	return &Store{writer: writer, reader: reader}, nil
}

// buildDSN appends the pragma query parameters modernc.org/sqlite applies on
// every new connection (see its dsn_test.go for the `_pragma=name=value`
// syntax). Ordering here does not matter: the driver itself always applies
// busy_timeout first and the rest in a fixed order, regardless of the order
// pragmas appear in the DSN.
func buildDSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout=5000")
	q.Add("_pragma", "journal_mode=WAL")
	q.Add("_pragma", "foreign_keys=ON")
	q.Add("_pragma", "synchronous=NORMAL")
	return path + "?" + q.Encode()
}

// Close releases both underlying database handles. It is safe to call once
// after all in-flight Repository calls have returned.
func (s *Store) Close() error {
	werr := s.writer.Close()
	rerr := s.reader.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// CreateRun inserts the initial row for a freshly constructed run (as built by
// core.NewRun). The engine calls this exactly once per run, before the first
// Commit; no transition, checkpoint or event rows are written here — the
// run's row is its own version-0 snapshot, and the first real Commit produces
// the version-1 transition/checkpoint pair.
func (s *Store) CreateRun(ctx context.Context, run core.RunState) error {
	stateJSON, err := marshalState(run)
	if err != nil {
		return fmt.Errorf("sqlite: marshal run state: %w", err)
	}

	const q = `
INSERT INTO runs (id, version, phase, plan_step, attempt, next_wake_at, deadline, state_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = s.writer.ExecContext(ctx, q,
		string(run.ID), int64(run.Version), string(run.Phase), int(run.Step), run.Attempt,
		nullTime(run.NextWakeAt), nullTime(run.Deadline), stateJSON,
		formatTime(run.CreatedAt), formatTime(run.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert run: %w", err)
	}
	return nil
}

// LoadState returns the current RunState for id, decoded from runs.state_json.
// It returns core.ErrNotFound if no run with that id exists.
func (s *Store) LoadState(ctx context.Context, id core.RunID) (core.RunState, error) {
	const q = `SELECT state_json FROM runs WHERE id = ?`
	var stateJSON string
	err := s.reader.QueryRowContext(ctx, q, string(id)).Scan(&stateJSON)
	switch {
	case err == sql.ErrNoRows:
		return core.RunState{}, core.ErrNotFound
	case err != nil:
		return core.RunState{}, fmt.Errorf("sqlite: load state: %w", err)
	}
	return unmarshalState(stateJSON)
}

// LatestCheckpoint returns the highest-version checkpoint recorded for id. It
// returns core.ErrNotFound if the run has never been checkpointed.
func (s *Store) LatestCheckpoint(ctx context.Context, id core.RunID) (core.Checkpoint, error) {
	const q = `
SELECT run_id, version, state_json, created_at
FROM checkpoints WHERE run_id = ?
ORDER BY version DESC LIMIT 1`
	var (
		runID     string
		version   int64
		stateJSON string
		createdAt string
	)
	err := s.reader.QueryRowContext(ctx, q, string(id)).Scan(&runID, &version, &stateJSON, &createdAt)
	switch {
	case err == sql.ErrNoRows:
		return core.Checkpoint{}, core.ErrNotFound
	case err != nil:
		return core.Checkpoint{}, fmt.Errorf("sqlite: latest checkpoint: %w", err)
	}

	state, err := unmarshalState(stateJSON)
	if err != nil {
		return core.Checkpoint{}, fmt.Errorf("sqlite: decode checkpoint state: %w", err)
	}
	at, err := parseTime(createdAt)
	if err != nil {
		return core.Checkpoint{}, fmt.Errorf("sqlite: decode checkpoint time: %w", err)
	}
	return core.Checkpoint{
		RunID:     core.RunID(runID),
		Version:   core.Version(version),
		State:     state,
		CreatedAt: at,
	}, nil
}

// ResumableRuns returns the ids of every run not in a terminal phase
// (succeeded, failed, cancelled). The engine calls this on startup to decide
// which runs to re-plan and resume.
func (s *Store) ResumableRuns(ctx context.Context) ([]core.RunID, error) {
	const q = `SELECT id FROM runs WHERE phase NOT IN (?, ?, ?)`
	rows, err := s.reader.QueryContext(ctx, q,
		string(core.PhaseSucceeded), string(core.PhaseFailed), string(core.PhaseCancelled))
	if err != nil {
		return nil, fmt.Errorf("sqlite: resumable runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []core.RunID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan resumable run: %w", err)
		}
		ids = append(ids, core.RunID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: resumable runs: %w", err)
	}
	return ids, nil
}

// PendingIntent returns the side-effect ledger row for id that is still in the
// INTENT state, if any. On resume, an INTENT row with no matching CONFIRMED
// update means the engine crashed between issuing the side effect and
// confirming it, and must reconcile before proceeding.
func (s *Store) PendingIntent(ctx context.Context, id core.RunID) (*core.SideEffectRow, bool, error) {
	const q = `
SELECT idempotency_key, run_id, tool, status, request_hash, response_json, attempt, created_at, confirmed_at
FROM side_effects WHERE run_id = ? AND status = ? LIMIT 1`
	var (
		key         string
		runID       string
		tool        string
		status      string
		requestHash string
		response    sql.NullString
		attempt     int
		createdAt   string
		confirmedAt sql.NullString
	)
	err := s.reader.QueryRowContext(ctx, q, string(id), string(core.EffectIntent)).
		Scan(&key, &runID, &tool, &status, &requestHash, &response, &attempt, &createdAt, &confirmedAt)
	switch {
	case err == sql.ErrNoRows:
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("sqlite: pending intent: %w", err)
	}

	at, err := parseTime(createdAt)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: decode side effect time: %w", err)
	}
	confirmedAtTime, err := parseNullTime(confirmedAt)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: decode side effect confirmed_at: %w", err)
	}

	row := &core.SideEffectRow{
		Key:         core.IdempotencyKey(key),
		RunID:       core.RunID(runID),
		Tool:        core.ToolName(tool),
		Status:      core.EffectStatus(status),
		RequestHash: requestHash,
		Response:    parseNullRedacted(response),
		Attempt:     attempt,
		At:          at,
		ConfirmedAt: confirmedAtTime,
	}
	return row, true, nil
}

// FindingExists reports whether a finding with the given idempotency key has
// already been durably recorded.
func (s *Store) FindingExists(ctx context.Context, key core.IdempotencyKey) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM findings WHERE idempotency_key = ?)`
	var exists bool
	if err := s.reader.QueryRowContext(ctx, q, string(key)).Scan(&exists); err != nil {
		return false, fmt.Errorf("sqlite: finding exists: %w", err)
	}
	return exists, nil
}

// ReviewExists reports whether a human-review request with the given
// idempotency key has already been durably recorded.
func (s *Store) ReviewExists(ctx context.Context, key core.IdempotencyKey) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM human_reviews WHERE idempotency_key = ?)`
	var exists bool
	if err := s.reader.QueryRowContext(ctx, q, string(key)).Scan(&exists); err != nil {
		return false, fmt.Errorf("sqlite: review exists: %w", err)
	}
	return exists, nil
}

// ListRuns returns every run's current RunState, most recently created first.
// It backs the control-room UI's run list.
func (s *Store) ListRuns(ctx context.Context) ([]core.RunState, error) {
	const q = `SELECT state_json FROM runs ORDER BY created_at DESC`
	rows, err := s.reader.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.RunState
	for rows.Next() {
		var stateJSON string
		if err := rows.Scan(&stateJSON); err != nil {
			return nil, fmt.Errorf("sqlite: scan run: %w", err)
		}
		state, err := unmarshalState(stateJSON)
		if err != nil {
			return nil, fmt.Errorf("sqlite: decode run state: %w", err)
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list runs: %w", err)
	}
	return out, nil
}

// Events returns a run's full redacted event history in emission order (by
// strictly increasing seq).
func (s *Store) Events(ctx context.Context, id core.RunID) ([]core.Event, error) {
	const q = `
SELECT seq, version, kind, summary, evidence_json, created_at
FROM events WHERE run_id = ? ORDER BY seq ASC`
	rows, err := s.reader.QueryContext(ctx, q, string(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.Event
	for rows.Next() {
		var (
			seq       int64
			version   int64
			kind      string
			summary   string
			evidence  sql.NullString
			createdAt string
		)
		if err := rows.Scan(&seq, &version, &kind, &summary, &evidence, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		at, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("sqlite: decode event time: %w", err)
		}
		out = append(out, core.Event{
			RunID:    id,
			Seq:      seq,
			Version:  core.Version(version),
			Kind:     core.EventKind(kind),
			Summary:  summary,
			Evidence: parseNullRedacted(evidence),
			At:       at,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: events: %w", err)
	}
	return out, nil
}

// Findings returns every finding durably recorded for a run, oldest first.
func (s *Store) Findings(ctx context.Context, id core.RunID) ([]core.FindingRow, error) {
	const q = `
SELECT idempotency_key, source_id, claim, evidence_json, created_at
FROM findings WHERE run_id = ? ORDER BY created_at ASC`
	rows, err := s.reader.QueryContext(ctx, q, string(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: findings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.FindingRow
	for rows.Next() {
		var (
			key       string
			sourceID  string
			claim     string
			evidence  sql.NullString
			createdAt string
		)
		if err := rows.Scan(&key, &sourceID, &claim, &evidence, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan finding: %w", err)
		}
		at, err := parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("sqlite: decode finding time: %w", err)
		}
		out = append(out, core.FindingRow{
			RunID:    id,
			Key:      core.IdempotencyKey(key),
			SourceID: sourceID,
			Claim:    claim,
			Evidence: parseNullRedacted(evidence),
			At:       at,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: findings: %w", err)
	}
	return out, nil
}

var _ core.Repository = (*Store)(nil)
