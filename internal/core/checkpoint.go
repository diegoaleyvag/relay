package core

import "time"

// Checkpoint is a full, redacted snapshot of a run at a specific version. It is
// written in the same transaction as its transition, so the invariant
// checkpoint.Version == runs.version always holds. On restart the latest
// checkpoint is the sole source of truth for resume.
type Checkpoint struct {
	RunID     RunID     `json:"run_id"`
	Version   Version   `json:"version"`
	State     RunState  `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}
