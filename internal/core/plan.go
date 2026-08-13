package core

// The deterministic plan is a fixed step sequence over the listed sources:
//
//	step 0            -> list_sources
//	step 1+2i         -> read_source(sources[i])
//	step 2+2i         -> record_finding(sources[i])
//	step 1+2N         -> request_human_review (only if RequireReview) then complete
//
// StepIndex is the single cursor. These helpers are the shared source of truth
// for both the planner (which chooses the action) and the reducer (which
// advances the cursor), so the two can never drift.

// StepList is the cursor value for the initial list_sources call.
func StepList() StepIndex { return 0 }

// StepRead is the cursor value for reading source i.
func StepRead(i int) StepIndex { return StepIndex(1 + 2*i) }

// StepRecord is the cursor value for recording a finding for source i.
func StepRecord(i int) StepIndex { return StepIndex(2 + 2*i) }

// StepReview is the cursor value reached once all N sources are processed.
func StepReview(n int) StepIndex { return StepIndex(1 + 2*n) }

// StepSourceIndex returns the source index addressed by a per-source step
// (a read or record step). It is only meaningful for steps in [1, 1+2N).
func StepSourceIndex(step StepIndex) int { return (int(step) - 1) / 2 }

// StepIsRead reports whether a per-source step addresses a read (even relative
// offset) rather than a record.
func StepIsRead(step StepIndex) bool { return (int(step)-1)%2 == 0 }
