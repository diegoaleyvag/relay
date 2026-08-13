package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/diegoaleyvag/relay/internal/core"
)

// Commit applies b atomically: it compare-and-swaps the run row on
// b.Expected, then writes the audit transition, the checkpoint, every
// appended event, and the optional side-effect ledger / finding / review
// writes — all in one transaction on the single-connection writer. Either the
// whole bundle lands or none of it does; a failed CAS rolls back and returns
// core.ErrVersionConflict without touching any other table.
func (s *Store) Commit(ctx context.Context, b core.CommitBundle) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit below succeeds

	if err := casRun(ctx, tx, b); err != nil {
		return err
	}
	if err := insertTransition(ctx, tx, b.Transition); err != nil {
		return err
	}
	if err := insertCheckpoint(ctx, tx, b.Checkpoint); err != nil {
		return err
	}
	if err := insertEvents(ctx, tx, b.Events); err != nil {
		return err
	}
	if b.Effect != nil {
		if err := upsertSideEffect(ctx, tx, *b.Effect); err != nil {
			return err
		}
	}
	if b.Finding != nil {
		if err := insertFinding(ctx, tx, *b.Finding); err != nil {
			return err
		}
	}
	if b.Review != nil {
		if err := insertReview(ctx, tx, *b.Review); err != nil {
			return err
		}
	}
	if b.ReviewFix != nil {
		if err := applyReviewFix(ctx, tx, *b.ReviewFix); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}

// casRun performs the version compare-and-swap on the run row: it writes
// b.Next in full (denormalized columns plus state_json) but only if the
// stored version still equals b.Expected. A RowsAffected of anything other
// than 1 means a concurrent writer already advanced the run past b.Expected,
// so the caller must reject with core.ErrVersionConflict rather than silently
// clobbering that writer's work.
func casRun(ctx context.Context, tx *sql.Tx, b core.CommitBundle) error {
	next := b.Next
	stateJSON, err := marshalState(next)
	if err != nil {
		return fmt.Errorf("sqlite: marshal next state: %w", err)
	}

	const q = `
UPDATE runs
SET version = ?, phase = ?, plan_step = ?, attempt = ?, next_wake_at = ?, deadline = ?, state_json = ?, updated_at = ?
WHERE id = ? AND version = ?`
	res, err := tx.ExecContext(ctx, q,
		int64(next.Version), string(next.Phase), int(next.Step), next.Attempt,
		nullTime(next.NextWakeAt), nullTime(next.Deadline), stateJSON, formatTime(next.UpdatedAt),
		string(next.ID), int64(b.Expected),
	)
	if err != nil {
		return fmt.Errorf("sqlite: cas run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: cas run rows affected: %w", err)
	}
	if n != 1 {
		return core.ErrVersionConflict
	}
	return nil
}

// insertTransition writes the audit row for this commit.
func insertTransition(ctx context.Context, tx *sql.Tx, t core.Transition) error {
	const q = `
INSERT INTO transitions
	(run_id, prior_version, new_version, action_kind, tool, idempotency_key, input_hash, from_phase, to_phase, evidence_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := tx.ExecContext(ctx, q,
		string(t.RunID), int64(t.PriorVersion), int64(t.NewVersion), string(t.ActionKind), string(t.Tool),
		string(t.IdemKey), t.InputHash, string(t.FromPhase), string(t.ToPhase), nullRedacted(t.Evidence),
		formatTime(t.At),
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert transition: %w", err)
	}
	return nil
}

// insertCheckpoint writes the full state snapshot for the new version. Its
// (run_id, version) primary key is the same pair the transition just wrote to
// runs.version, keeping the "checkpoint.Version == runs.version" invariant.
func insertCheckpoint(ctx context.Context, tx *sql.Tx, c core.Checkpoint) error {
	stateJSON, err := marshalState(c.State)
	if err != nil {
		return fmt.Errorf("sqlite: marshal checkpoint state: %w", err)
	}
	const q = `INSERT INTO checkpoints (run_id, version, state_json, created_at) VALUES (?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q, string(c.RunID), int64(c.Version), stateJSON, formatTime(c.CreatedAt)); err != nil {
		return fmt.Errorf("sqlite: insert checkpoint: %w", err)
	}
	return nil
}

// insertEvents appends each event, assigning its seq as one past the current
// per-run maximum. Because this runs inside the commit transaction on the
// single-connection writer, no other writer can interleave a seq assignment
// between the SELECT and the INSERT, so seq is both gap-free and strictly
// increasing across commits.
func insertEvents(ctx context.Context, tx *sql.Tx, events []core.Event) error {
	const seqQ = `SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE run_id = ?`
	const insQ = `
INSERT INTO events (run_id, seq, version, kind, summary, evidence_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`

	for _, e := range events {
		var seq int64
		if err := tx.QueryRowContext(ctx, seqQ, string(e.RunID)).Scan(&seq); err != nil {
			return fmt.Errorf("sqlite: next event seq: %w", err)
		}
		_, err := tx.ExecContext(ctx, insQ,
			string(e.RunID), seq, int64(e.Version), string(e.Kind), e.Summary, nullRedacted(e.Evidence), formatTime(e.At),
		)
		if err != nil {
			return fmt.Errorf("sqlite: insert event: %w", err)
		}
	}
	return nil
}

// upsertSideEffect writes or advances the intent->confirm ledger row for a
// side effect. The first write (INTENT) inserts; a later write for the same
// key (CONFIRMED or FAILED) updates status/response/attempt/confirmed_at in
// place, since idempotency_key is the primary key.
func upsertSideEffect(ctx context.Context, tx *sql.Tx, e core.SideEffectRow) error {
	const q = `
INSERT INTO side_effects (idempotency_key, run_id, tool, status, request_hash, response_json, attempt, created_at, confirmed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(idempotency_key) DO UPDATE SET
	status = excluded.status,
	response_json = excluded.response_json,
	attempt = excluded.attempt,
	confirmed_at = excluded.confirmed_at`
	_, err := tx.ExecContext(ctx, q,
		string(e.Key), string(e.RunID), string(e.Tool), string(e.Status), e.RequestHash,
		nullRedacted(e.Response), e.Attempt, formatTime(e.At), nullTime(e.ConfirmedAt),
	)
	if err != nil {
		return fmt.Errorf("sqlite: upsert side effect: %w", err)
	}
	return nil
}

// insertFinding durably records a finding, deduped by idempotency_key: a
// retried or replayed record_finding call for the same key is a silent
// no-op, preserving exactly-once semantics.
func insertFinding(ctx context.Context, tx *sql.Tx, f core.FindingRow) error {
	const q = `
INSERT INTO findings (run_id, idempotency_key, source_id, claim, evidence_json, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(idempotency_key) DO NOTHING`
	_, err := tx.ExecContext(ctx, q,
		string(f.RunID), string(f.Key), f.SourceID, f.Claim, nullRedacted(f.Evidence), formatTime(f.At),
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert finding: %w", err)
	}
	return nil
}

// insertReview durably records a human-review request, deduped by
// idempotency_key for the same exactly-once reason as insertFinding.
func insertReview(ctx context.Context, tx *sql.Tx, r core.ReviewRow) error {
	const q = `
INSERT INTO human_reviews (run_id, idempotency_key, review_id, reason, severity, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(idempotency_key) DO NOTHING`
	_, err := tx.ExecContext(ctx, q,
		string(r.RunID), string(r.Key), r.ReviewID, r.Reason, r.Severity, string(r.Status), formatTime(r.At),
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert review: %w", err)
	}
	return nil
}

// applyReviewFix updates a human-review row's status and resolution time when
// a human approves or rejects. It targets the row by idempotency_key, the
// same key insertReview used to create it.
func applyReviewFix(ctx context.Context, tx *sql.Tx, f core.ReviewResolution) error {
	const q = `UPDATE human_reviews SET status = ?, resolved_at = ? WHERE idempotency_key = ?`
	_, err := tx.ExecContext(ctx, q, string(f.Status), nullTime(f.At), string(f.Key))
	if err != nil {
		return fmt.Errorf("sqlite: apply review fix: %w", err)
	}
	return nil
}
