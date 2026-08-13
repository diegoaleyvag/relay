package sqlite

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// timeLayout is the on-disk text encoding for every timestamp column:
// RFC3339Nano preserves sub-second precision and sorts lexicographically in
// the same order as chronologically, which is what makes "ORDER BY created_at"
// and "ORDER BY seq" agree.
const timeLayout = time.RFC3339Nano

// formatTime renders a required (never-NULL) timestamp column.
func formatTime(t time.Time) string {
	return t.Format(timeLayout)
}

// parseTime is the inverse of formatTime.
func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

// nullTime renders an optional timestamp: the zero time.Time (Go's "unset")
// maps to SQL NULL rather than to a formatted zero date. RunState.NextWakeAt,
// RunState.Deadline and SideEffectRow.ConfirmedAt are all "valid only in
// certain phases" fields that use this encoding.
func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.Format(timeLayout), Valid: true}
}

// parseNullTime is the inverse of nullTime: SQL NULL (or an empty string)
// decodes back to the zero time.Time.
func parseNullTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, ns.String)
}

// marshalState JSON-encodes a RunState for storage in runs.state_json /
// checkpoints.state_json.
func marshalState(s core.RunState) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalState is the inverse of marshalState.
func unmarshalState(raw string) (core.RunState, error) {
	var s core.RunState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return core.RunState{}, err
	}
	return s, nil
}

// nullRedacted encodes Redacted evidence for an optional (nullable)
// evidence/response JSON column. Redacted is already validated JSON produced
// by a Redactor, so it is stored verbatim; an empty payload maps to SQL NULL
// rather than to the literal string "null" so FindingRow/SideEffectRow/Event
// evidence round-trips to nil, not to a non-empty json.RawMessage("null").
func nullRedacted(r core.Redacted) sql.NullString {
	if len(r) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(r), Valid: true}
}

// parseNullRedacted is the inverse of nullRedacted.
func parseNullRedacted(ns sql.NullString) core.Redacted {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	return core.Redacted(ns.String)
}
