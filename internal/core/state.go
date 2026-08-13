package core

import "time"

// SourceRef is corpus-source metadata (never content). It is safe to persist in
// the run snapshot and to display.
type SourceRef struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	MediaType string   `json:"media_type"`
	Bytes     int      `json:"bytes"`
	Tags      []string `json:"tags,omitempty"`
}

// FindingRef is a pointer to a durably recorded finding. The claim/evidence body
// lives in the store; the run snapshot keeps only metadata.
type FindingRef struct {
	SourceID  string         `json:"source_id"`
	Key       IdempotencyKey `json:"idempotency_key"`
	FindingID string         `json:"finding_id"`
}

// ReviewStatus is the lifecycle of a human-review request.
type ReviewStatus string

const (
	ReviewPending  ReviewStatus = "pending"
	ReviewApproved ReviewStatus = "approved"
	ReviewRejected ReviewStatus = "rejected"
)

// HumanReviewRef records an escalation. It is set when request_human_review is
// confirmed and resolved when a human approves or rejects.
type HumanReviewRef struct {
	Key      IdempotencyKey `json:"idempotency_key"`
	ReviewID string         `json:"review_id"`
	Reason   string         `json:"reason"`
	Severity string         `json:"severity"`
	Status   ReviewStatus   `json:"status"`
	Step     StepIndex      `json:"step"`
}

// HumanDecision is a reviewer's resolution of an awaiting_human run.
type HumanDecision string

const (
	DecisionApprove HumanDecision = "approve"
	DecisionReject  HumanDecision = "reject"
)

// PendingEffect marks an in-flight side effect: its intent is durable but its
// confirmation is not yet. This is the crux of exactly-once — on resume it tells
// the engine to reconcile rather than blindly re-execute.
type PendingEffect struct {
	Key       IdempotencyKey `json:"idempotency_key"`
	Tool      ToolName       `json:"tool"`
	Step      StepIndex      `json:"step"`
	InputHash string         `json:"input_hash"`
}

// RunState is the run aggregate: everything needed to deterministically decide
// the next action and to resume after a restart. It is self-contained and
// carries only redacted metadata.
type RunState struct {
	ID      RunID     `json:"id"`
	Version Version   `json:"version"`
	Phase   Phase     `json:"phase"`
	Step    StepIndex `json:"step"`
	Attempt int       `json:"attempt"` // retry attempt for the current step
	Seed    uint64    `json:"seed"`    // deterministic jitter/plan seed

	NextWakeAt time.Time `json:"next_wake_at,omitempty"` // valid when Phase==backoff
	Deadline   time.Time `json:"deadline,omitempty"`     // run-wide deadline; zero == none

	RequireReview bool `json:"require_review"` // run must escalate before completing

	Listed   bool         `json:"listed"`             // list_sources has completed (distinct from an empty result)
	Sources  []SourceRef  `json:"sources,omitempty"`  // populated after list_sources
	Findings []FindingRef `json:"findings,omitempty"` // preserved partial results
	Skipped  []SourceRef  `json:"skipped,omitempty"`  // sources abandoned (e.g. malformed)

	Pending   *PendingEffect  `json:"pending,omitempty"`
	Review    *HumanReviewRef `json:"review,omitempty"`
	LastError *RelayError     `json:"last_error,omitempty"`

	RetriesUsed int `json:"retries_used"` // global retry budget consumed

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewRun builds a fresh pending run.
func NewRun(id RunID, seed uint64, requireReview bool, deadline time.Time, now time.Time) RunState {
	return RunState{
		ID:            id,
		Version:       0,
		Phase:         PhasePending,
		Seed:          seed,
		RequireReview: requireReview,
		Deadline:      deadline,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// SourceDone reports whether a source has been recorded or skipped.
func (s RunState) SourceDone(sourceID string) bool {
	for _, f := range s.Findings {
		if f.SourceID == sourceID {
			return true
		}
	}
	for _, sk := range s.Skipped {
		if sk.ID == sourceID {
			return true
		}
	}
	return false
}

// clone deep-copies slices and pointer fields so the reducer can treat its input
// as immutable and never alias the caller's state.
func (s RunState) clone() RunState {
	out := s
	out.Sources = append([]SourceRef(nil), s.Sources...)
	out.Findings = append([]FindingRef(nil), s.Findings...)
	out.Skipped = append([]SourceRef(nil), s.Skipped...)
	if s.Pending != nil {
		p := *s.Pending
		out.Pending = &p
	}
	if s.Review != nil {
		r := *s.Review
		out.Review = &r
	}
	if s.LastError != nil {
		e := *s.LastError
		out.LastError = &e
	}
	return out
}

// phaseEdges is the legal phase-transition table. It is consulted by the reducer
// and mirrored by the UI so controls are enabled only for legal transitions.
var phaseEdges = map[Phase]map[Phase]bool{
	PhasePending: {PhaseRunning: true, PhaseCancelled: true},
	PhaseRunning: {
		PhaseRunning: true, PhaseBackoff: true, PhaseAwaitingHuman: true,
		PhaseSucceeded: true, PhaseFailed: true, PhaseCancelled: true,
	},
	PhaseBackoff:       {PhaseRunning: true, PhaseFailed: true, PhaseCancelled: true},
	PhaseAwaitingHuman: {PhaseRunning: true, PhaseFailed: true, PhaseCancelled: true},
	PhaseSucceeded:     {},
	PhaseFailed:        {},
	PhaseCancelled:     {},
}

// CanTransition reports whether a run may move from one phase to another.
// Self-loops on running are allowed (advancing the plan cursor).
func CanTransition(from, to Phase) bool {
	return phaseEdges[from][to]
}
