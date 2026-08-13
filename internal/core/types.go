// Package core is the domain heart of Relay: the run state machine, its pure
// reducer, and the ports (interfaces) through which adapters plug in.
//
// The dependency rule is absolute: this package and internal/planner import
// only the Go standard library. They never reach for HTTP, HTMX, SQLite or MCP
// types. Adapters translate at the boundaries. `make boundary` enforces it.
package core

// RunID is an opaque, sortable run identifier (a ULID-style text value). It
// carries no external meaning.
type RunID string

// Version is a run's optimistic-concurrency token. It equals the number of
// committed transitions and is used for compare-and-swap writes so stale
// writers are rejected.
type Version int64

// StepIndex is the deterministic planner cursor within a run. Identical state
// yields an identical step, which is what makes re-planning after a restart
// reproduce the same action.
type StepIndex int

// ToolName identifies one of the four allowlisted MCP tools. Any other name
// fails closed.
type ToolName string

const (
	ToolListSources   ToolName = "list_sources"
	ToolReadSource    ToolName = "read_source"
	ToolRecordFinding ToolName = "record_finding"       // side-effecting
	ToolRequestReview ToolName = "request_human_review" // side-effecting
)

// SideEffecting reports whether a tool mutates durable state and therefore
// requires an idempotency key and the intent→execute→confirm ledger.
func (t ToolName) SideEffecting() bool {
	return t == ToolRecordFinding || t == ToolRequestReview
}

// Allowlisted reports whether the name is one of the four permitted tools.
func (t ToolName) Allowlisted() bool {
	switch t {
	case ToolListSources, ToolReadSource, ToolRecordFinding, ToolRequestReview:
		return true
	default:
		return false
	}
}

// Phase is the run's lifecycle state. succeeded, failed and cancelled are
// terminal.
type Phase string

const (
	PhasePending       Phase = "pending"        // created, not started
	PhaseRunning       Phase = "running"        // planning / dispatching / awaiting a tool result
	PhaseBackoff       Phase = "backoff"        // waiting to retry after a retryable error
	PhaseAwaitingHuman Phase = "awaiting_human" // parked on human review (durable, non-terminal)
	PhaseSucceeded     Phase = "succeeded"      // terminal
	PhaseFailed        Phase = "failed"         // terminal
	PhaseCancelled     Phase = "cancelled"      // terminal (ctx cancel / deadline)
)

// Terminal reports whether the phase admits no further transitions.
func (p Phase) Terminal() bool {
	return p == PhaseSucceeded || p == PhaseFailed || p == PhaseCancelled
}

// Valid reports whether p is a known phase.
func (p Phase) Valid() bool {
	switch p {
	case PhasePending, PhaseRunning, PhaseBackoff, PhaseAwaitingHuman,
		PhaseSucceeded, PhaseFailed, PhaseCancelled:
		return true
	default:
		return false
	}
}

// ActionKind classifies the planner's chosen step.
type ActionKind string

const (
	ActionCallTool ActionKind = "call_tool"
	ActionComplete ActionKind = "complete"
	ActionFail     ActionKind = "fail"
	ActionWait     ActionKind = "wait"
)

// PlannerKind is a constant label attached to every event and shown in the UI.
// The planner is scripted and deterministic — it is never labelled as AI or
// "reasoning".
const PlannerKind = "scripted-deterministic"
