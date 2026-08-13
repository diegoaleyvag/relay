package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// This file is the hexagonal seam. Every interface here is owned by the domain
// and implemented by an adapter (store, mcp, clock, telemetry, web). Treat the
// signatures as a frozen contract: changing them re-synchronizes all adapter
// work, so they are deliberately reviewed before the parallel milestones begin.

// Sentinel errors returned by Repository implementations.
var (
	// ErrVersionConflict is returned by Commit when the run's stored version no
	// longer matches the expected version — a stale writer is rejected.
	ErrVersionConflict = errors.New("relay: optimistic version conflict")
	// ErrNotFound is returned when a run or checkpoint does not exist.
	ErrNotFound = errors.New("relay: not found")
)

// Clock is the single seam for time. SystemClock wraps the wall clock;
// ManualClock (tests) advances virtual time deterministically. Backoff delays,
// run deadlines and per-attempt tool timeouts all resolve through the injected
// Clock.
type Clock interface {
	Now() time.Time
	// Sleep blocks for d or until ctx is done, returning ctx.Err() if cancelled.
	Sleep(ctx context.Context, d time.Duration) error
	// After returns a channel that receives after d (virtual time for tests).
	After(d time.Duration) <-chan time.Time
}

// ToolCall is a domain request to invoke a tool. Idem is set for side-effecting
// tools; the adapter passes it downstream so the receiver can dedupe.
type ToolCall struct {
	Tool    ToolName
	Input   ToolInput
	Idem    IdempotencyKey
	Step    StepIndex
	Attempt int
}

// ToolResult is the structured outcome of a successful tool call. Output is the
// tool-specific structured payload; NeedsInput signals a human-review/elicitation
// result rather than an error.
type ToolResult struct {
	Tool       ToolName
	Output     json.RawMessage
	NeedsInput bool
	Duration   time.Duration
}

// ToolPort is the MCP-agnostic port through which the engine invokes tools. It
// MUST honor ctx cancellation and deadlines: a deadline surfaces as
// *RelayError{Code: TIMEOUT}. A nil error means success.
type ToolPort interface {
	Invoke(ctx context.Context, c ToolCall) (ToolResult, *RelayError)
}

// Planner chooses the next action from the run state. It is PURE and
// DETERMINISTIC: identical RunState yields an identical Action with no I/O. That
// is what makes re-planning after a restart reproduce the same step and the same
// idempotency key.
type Planner interface {
	Next(s RunState) (Action, error)
}

// EffectStatus is the state of a side effect in the idempotency ledger.
type EffectStatus string

const (
	EffectIntent    EffectStatus = "INTENT"
	EffectConfirmed EffectStatus = "CONFIRMED"
	EffectFailed    EffectStatus = "FAILED"
)

// Transition is the audit row for one durable state change. Persisted with its
// checkpoint and events in a single transaction.
type Transition struct {
	RunID        RunID
	PriorVersion Version
	NewVersion   Version
	ActionKind   ActionKind
	Tool         ToolName
	IdemKey      IdempotencyKey
	InputHash    string
	FromPhase    Phase
	ToPhase      Phase
	Evidence     Redacted
	At           time.Time
}

// SideEffectRow is a ledger entry for a side effect (record_finding /
// request_human_review). The intent→confirm progression is what survives a kill
// between the effect and its checkpoint.
type SideEffectRow struct {
	Key         IdempotencyKey
	RunID       RunID
	Tool        ToolName
	Status      EffectStatus
	RequestHash string
	Response    Redacted
	Attempt     int
	At          time.Time
	ConfirmedAt time.Time
}

// FindingRow is a durably recorded finding. Deduped on Key (exactly-once).
type FindingRow struct {
	RunID    RunID
	Key      IdempotencyKey
	SourceID string
	Claim    string
	Evidence Redacted
	At       time.Time
}

// ReviewRow is a human-review request. Deduped on Key (exactly-once).
type ReviewRow struct {
	RunID    RunID
	Key      IdempotencyKey
	ReviewID string
	Reason   string
	Severity string
	Status   ReviewStatus
	At       time.Time
}

// ReviewResolution updates a review row when a human approves or rejects.
type ReviewResolution struct {
	Key    IdempotencyKey
	Status ReviewStatus
	At     time.Time
}

// CommitBundle is everything that must land atomically for one durable
// transition: the version compare-and-swap, the new state, the audit
// transition, the checkpoint, the appended events, and any ledger/finding/review
// writes. The store performs all of it in one SQL transaction.
type CommitBundle struct {
	Expected   Version
	Next       RunState
	Transition Transition
	Checkpoint Checkpoint
	Events     []Event
	Effect     *SideEffectRow    // INTENT or CONFIRMED ledger write
	Finding    *FindingRow       // idempotent finding insert
	Review     *ReviewRow        // idempotent review insert
	ReviewFix  *ReviewResolution // review status update on human resolution
}

// Repository is the durable state port. Commit is the only mutating method and
// is atomic + version-guarded. The read methods back the engine's resume path
// and the control-room UI.
type Repository interface {
	CreateRun(ctx context.Context, s RunState) error
	// Commit applies the bundle in one transaction, guarded by a version CAS.
	// Returns ErrVersionConflict if the stored version != bundle.Expected.
	Commit(ctx context.Context, b CommitBundle) error

	LoadState(ctx context.Context, id RunID) (RunState, error)
	LatestCheckpoint(ctx context.Context, id RunID) (Checkpoint, error)
	ResumableRuns(ctx context.Context) ([]RunID, error)

	// Reconciliation reads for resume.
	PendingIntent(ctx context.Context, id RunID) (*SideEffectRow, bool, error)
	FindingExists(ctx context.Context, key IdempotencyKey) (bool, error)
	ReviewExists(ctx context.Context, key IdempotencyKey) (bool, error)

	// UI read model.
	ListRuns(ctx context.Context) ([]RunState, error)
	Events(ctx context.Context, id RunID) ([]Event, error)
	Findings(ctx context.Context, id RunID) ([]FindingRow, error)
}
