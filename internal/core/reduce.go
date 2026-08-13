package core

import (
	"encoding/json"
	"fmt"
	"time"
)

// Policy captures the two tunable failure decisions.
type Policy struct {
	// MalformedSkips: MALFORMED_RESPONSE skips the source and continues (true)
	// rather than failing the run (false).
	MalformedSkips bool
	// PermissionEscalates: PERMISSION_DENIED parks the run in awaiting_human
	// (true) rather than failing closed to a terminal state (false).
	PermissionEscalates bool
}

// DefaultPolicy preserves partial results on malformed data and fails closed on
// permission denial.
func DefaultPolicy() Policy { return Policy{MalformedSkips: true, PermissionEscalates: false} }

// Env injects all nondeterminism into the reducer as plain data. Identical
// (state, input, env) always yields an identical Result.
type Env struct {
	Now         time.Time
	Backoff     Backoff
	MaxAttempts int // attempts per step before it is a terminal RETRIES_EXHAUSTED
	MaxRetries  int // global retry budget across the whole run
	Policy      Policy
}

// Input is the discriminated union of everything that can drive the machine.
type Input interface{ isInput() }

// InStart begins a pending run.
type InStart struct{}

// InAction feeds the planner's chosen action.
type InAction struct{ Action Action }

// InToolResult feeds a tool outcome (success when Err is nil).
type InToolResult struct {
	Call   ToolCall
	Result ToolResult
	Err    *RelayError
}

// InHuman feeds a reviewer's resolution of an awaiting_human run.
type InHuman struct{ Decision HumanDecision }

// InCancel cancels a non-terminal run (ctx cancel / deadline).
type InCancel struct{ Reason ErrorCode }

// InWake wakes a run from backoff back to running once its delay has elapsed.
// It is committed as a durable transition so the checkpoint chain records the
// legal backoff→running edge rather than jumping from backoff to the retry's
// result phase.
type InWake struct{}

func (InStart) isInput()      {}
func (InAction) isInput()     {}
func (InToolResult) isInput() {}
func (InHuman) isInput()      {}
func (InCancel) isInput()     {}
func (InWake) isInput()       {}

// Effect is a side action the engine must perform after persisting the result.
type Effect interface{ isEffect() }

// EffCallTool asks the engine to invoke a tool.
type EffCallTool struct{ Call ToolCall }

// EffScheduleWake asks the engine to wake the run at a time (backoff).
type EffScheduleWake struct{ At time.Time }

func (EffCallTool) isEffect()     {}
func (EffScheduleWake) isEffect() {}

// Result is the reducer's output. When Durable is true the engine must persist a
// CommitBundle built from State/Events/rows and the transition metadata; when it
// is false the engine only performs Effects (e.g. dispatching a read-only tool).
type Result struct {
	State   RunState
	Events  []Event
	Effects []Effect
	Durable bool

	// Optional atomic writes for this transition.
	Effect    *SideEffectRow
	Finding   *FindingRow
	Review    *ReviewRow
	ReviewFix *ReviewResolution

	// Transition metadata; the engine adds versions, phases and timestamp.
	ActionKind ActionKind
	Tool       ToolName
	IdemKey    IdempotencyKey
	InputHash  string
	Evidence   Redacted
}

// Reduce is the pure, total state-machine reducer. It never performs I/O, never
// panics, and rejects any input that is illegal for the current state by
// returning the state unchanged plus an illegal_transition event.
func Reduce(s RunState, in Input, env Env) Result {
	switch v := in.(type) {
	case InStart:
		return reduceStart(s, env)
	case InAction:
		return reduceAction(s, v.Action, env)
	case InToolResult:
		if v.Err != nil {
			return reduceToolError(s, v.Call, v.Err, env)
		}
		return reduceToolSuccess(s, v.Call, v.Result, env)
	case InHuman:
		return reduceHuman(s, v.Decision, env)
	case InCancel:
		return reduceCancel(s, v.Reason, env)
	case InWake:
		return reduceWake(s, env)
	default:
		return illegal(s, env, "unknown input")
	}
}

// --- helpers -------------------------------------------------------------

func illegal(s RunState, env Env, why string) Result {
	ev := Event{
		RunID: s.ID, Version: s.Version, Kind: EventIllegalTransition,
		Summary: "illegal transition: " + why, At: env.Now,
		Evidence: evidence(map[string]any{"phase": s.Phase, "reason": why}),
	}
	return Result{State: s, Events: []Event{ev}, Durable: false}
}

// begin bumps the version and stamps the update time for a durable transition.
func begin(s RunState, env Env) RunState {
	n := s.clone()
	n.Version = s.Version + 1
	n.UpdatedAt = env.Now
	return n
}

func (r *Result) event(s RunState, kind EventKind, summary string, ev any) {
	r.Events = append(r.Events, Event{
		RunID: s.ID, Version: r.State.Version, Kind: kind,
		Summary: summary, Evidence: evidence(ev), At: s.UpdatedAt,
	})
}

// failPending resolves any in-flight side-effect intent on n: it clears Pending
// and returns a FAILED ledger row (or nil if there was none), so an abandoned
// intent never lingers as a dangling INTENT in the ledger.
func failPending(n *RunState, env Env) *SideEffectRow {
	if n.Pending == nil {
		return nil
	}
	row := &SideEffectRow{
		Key: n.Pending.Key, RunID: n.ID, Tool: n.Pending.Tool, Status: EffectFailed,
		RequestHash: n.Pending.InputHash, Attempt: n.Attempt, At: env.Now,
	}
	n.Pending = nil
	return row
}

// classify applies policy overrides on top of the code's default class.
func classify(err *RelayError, p Policy) ErrorClass {
	switch err.Code {
	case CodeMalformed:
		if p.MalformedSkips {
			return ClassSkippable
		}
		return ClassTerminal
	default:
		return err.Class
	}
}

// --- reducers ------------------------------------------------------------

func reduceStart(s RunState, env Env) Result {
	if s.Phase != PhasePending {
		return illegal(s, env, "start requires pending")
	}
	n := begin(s, env)
	n.Phase = PhaseRunning
	r := Result{State: n, Durable: true, ActionKind: ActionWait}
	r.event(s, EventStarted, "run started", map[string]any{"planner": PlannerKind})
	return r
}

func reduceAction(s RunState, a Action, env Env) Result {
	if s.Phase != PhaseRunning || s.Pending != nil {
		return illegal(s, env, "action requires running with no pending effect")
	}
	switch a.Kind {
	case ActionComplete:
		n := begin(s, env)
		n.Phase = PhaseSucceeded
		r := Result{State: n, Durable: true, ActionKind: ActionComplete}
		r.event(s, EventRunSucceeded, "run completed",
			map[string]any{"findings": len(n.Findings), "skipped": len(n.Skipped), "planner": PlannerKind})
		return r
	case ActionFail:
		n := begin(s, env)
		n.Phase = PhaseFailed
		if n.LastError == nil {
			n.LastError = NewError(CodeInternal, "planner requested failure")
		}
		r := Result{State: n, Durable: true, ActionKind: ActionFail}
		r.event(s, EventRunFailed, "run failed", map[string]any{"code": n.LastError.Code})
		return r
	case ActionCallTool:
		return reduceDispatch(s, a, env)
	default:
		return illegal(s, env, "unknown action kind")
	}
}

func reduceDispatch(s RunState, a Action, env Env) Result {
	// Fail closed on non-allowlisted tools.
	if !a.Tool.Allowlisted() {
		n := begin(s, env)
		n.Phase = PhaseFailed
		n.LastError = NewError(CodePermissionDenied, "tool not allowlisted: "+string(a.Tool))
		r := Result{State: n, Durable: true, ActionKind: ActionCallTool, Tool: a.Tool}
		r.event(s, EventToolFailed, "tool not allowlisted",
			map[string]any{"tool": a.Tool, "code": CodePermissionDenied})
		r.event(s, EventRunFailed, "run failed", map[string]any{"code": CodePermissionDenied})
		return r
	}

	call := ToolCall{Tool: a.Tool, Input: a.Input, Idem: a.Idem, Step: a.Step, Attempt: s.Attempt}

	if !a.Tool.SideEffecting() {
		// Read-only dispatch is not durable: the engine invokes and feeds the
		// result back. No checkpoint, no ledger.
		return Result{State: s, Durable: false, Effects: []Effect{EffCallTool{Call: call}}}
	}

	// Side-effecting dispatch: write the INTENT durably, then execute.
	inputHash := HashInput(a.Input)
	n := begin(s, env)
	n.Pending = &PendingEffect{Key: a.Idem, Tool: a.Tool, Step: a.Step, InputHash: inputHash}
	r := Result{
		State: n, Durable: true,
		Effects:    []Effect{EffCallTool{Call: call}},
		ActionKind: ActionCallTool, Tool: a.Tool, IdemKey: a.Idem, InputHash: inputHash,
		Effect: &SideEffectRow{
			Key: a.Idem, RunID: s.ID, Tool: a.Tool, Status: EffectIntent,
			RequestHash: inputHash, Attempt: s.Attempt, At: env.Now,
		},
	}
	r.event(s, EventSideEffectIntent, "side-effect intent recorded",
		map[string]any{"tool": a.Tool, "step": a.Step, "key": Fingerprint([]byte(a.Idem))})
	return r
}

func reduceToolSuccess(s RunState, call ToolCall, res ToolResult, env Env) Result {
	if s.Phase != PhaseRunning {
		return illegal(s, env, "tool result requires running")
	}
	switch call.Tool {
	case ToolListSources:
		var out ListSourcesOutput
		if err := json.Unmarshal(res.Output, &out); err != nil {
			return reduceToolError(s, call, NewError(CodeMalformed, "list_sources output unparseable"), env)
		}
		n := begin(s, env)
		n.Listed = true
		n.Sources = append([]SourceRef(nil), out.Sources...)
		n.Step = s.Step + 1
		n.Attempt = 0
		r := Result{State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool}
		r.event(s, EventToolSucceeded, "listed sources",
			map[string]any{"tool": call.Tool, "count": len(out.Sources), "duration_ms": res.Duration.Milliseconds()})
		return r

	case ToolReadSource:
		n := begin(s, env)
		n.Step = s.Step + 1
		n.Attempt = 0
		r := Result{State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool}
		r.event(s, EventToolSucceeded, "read source",
			map[string]any{"tool": call.Tool, "step": call.Step, "duration_ms": res.Duration.Milliseconds()})
		return r

	case ToolRecordFinding:
		return confirmFinding(s, call, res, env)

	case ToolRequestReview:
		return confirmReview(s, call, res, env)

	default:
		return illegal(s, env, "success for unknown tool")
	}
}

func confirmFinding(s RunState, call ToolCall, res ToolResult, env Env) Result {
	if s.Pending == nil || s.Pending.Key != call.Idem {
		return illegal(s, env, "finding confirm without matching pending intent")
	}
	var in RecordFindingInput
	_ = json.Unmarshal(call.Input, &in)
	var out RecordFindingOutput
	_ = json.Unmarshal(res.Output, &out)

	n := begin(s, env)
	n.Pending = nil
	n.Attempt = 0
	n.Step = s.Step + 1
	n.Findings = append(n.Findings, FindingRef{SourceID: in.SourceID, Key: call.Idem, FindingID: out.FindingID})

	r := Result{
		State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool, IdemKey: call.Idem,
		InputHash: HashInput(call.Input),
		Effect: &SideEffectRow{
			Key: call.Idem, RunID: s.ID, Tool: call.Tool, Status: EffectConfirmed,
			RequestHash: HashInput(call.Input), Attempt: s.Attempt, At: s.CreatedAt, ConfirmedAt: env.Now,
			Response: evidence(map[string]any{"finding_id": out.FindingID, "duplicate": out.Duplicate}),
		},
		Finding: &FindingRow{
			RunID: s.ID, Key: call.Idem, SourceID: in.SourceID, Claim: in.Claim,
			Evidence: evidence(map[string]any{"confidence": in.Confidence, "source_id": in.SourceID}),
			At:       env.Now,
		},
	}
	if out.Duplicate {
		r.event(s, EventDuplicateSuppress, "duplicate finding suppressed",
			map[string]any{"tool": call.Tool, "source_id": in.SourceID, "key": Fingerprint([]byte(call.Idem))})
	}
	r.event(s, EventSideEffectConfirm, "finding recorded",
		map[string]any{"tool": call.Tool, "source_id": in.SourceID, "finding_id": out.FindingID, "duplicate": out.Duplicate})
	return r
}

func confirmReview(s RunState, call ToolCall, res ToolResult, env Env) Result {
	if s.Pending == nil || s.Pending.Key != call.Idem {
		return illegal(s, env, "review confirm without matching pending intent")
	}
	var in RequestHumanReviewInput
	_ = json.Unmarshal(call.Input, &in)
	var out RequestHumanReviewOutput
	_ = json.Unmarshal(res.Output, &out)

	n := begin(s, env)
	n.Pending = nil
	n.Attempt = 0
	n.Phase = PhaseAwaitingHuman
	n.Review = &HumanReviewRef{
		Key: call.Idem, ReviewID: out.ReviewID, Reason: in.Reason,
		Severity: in.Severity, Status: ReviewPending, Step: call.Step,
	}
	r := Result{
		State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool, IdemKey: call.Idem,
		InputHash: HashInput(call.Input),
		Effect: &SideEffectRow{
			Key: call.Idem, RunID: s.ID, Tool: call.Tool, Status: EffectConfirmed,
			RequestHash: HashInput(call.Input), Attempt: s.Attempt, At: s.CreatedAt, ConfirmedAt: env.Now,
			Response: evidence(map[string]any{"review_id": out.ReviewID}),
		},
		Review: &ReviewRow{
			RunID: s.ID, Key: call.Idem, ReviewID: out.ReviewID, Reason: in.Reason,
			Severity: in.Severity, Status: ReviewPending, At: env.Now,
		},
	}
	r.event(s, EventHumanRequested, "human review requested",
		map[string]any{"tool": call.Tool, "severity": in.Severity, "review_id": out.ReviewID})
	return r
}

func reduceToolError(s RunState, call ToolCall, err *RelayError, env Env) Result {
	if s.Phase != PhaseRunning {
		return illegal(s, env, "tool error requires running")
	}
	switch classify(err, env.Policy) {
	case ClassRetryable:
		return retryOrExhaust(s, call, err, env)
	case ClassSkippable:
		return skipSource(s, call, err, env)
	default: // terminal
		if err.Code == CodePermissionDenied && env.Policy.PermissionEscalates {
			return escalatePermission(s, call, err, env)
		}
		return terminalFail(s, err, env)
	}
}

func retryOrExhaust(s RunState, call ToolCall, err *RelayError, env Env) Result {
	if s.Attempt+1 >= env.MaxAttempts || s.RetriesUsed >= env.MaxRetries {
		n := begin(s, env)
		n.Phase = PhaseFailed
		n.LastError = NewError(CodeRetriesExhausted,
			fmt.Sprintf("retries exhausted after %s", err.Code)).WithDetail("last_code", string(err.Code))
		effect := failPending(&n, env)
		r := Result{State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool, Effect: effect}
		r.event(s, EventToolFailed, "tool failed (retryable)",
			map[string]any{"tool": call.Tool, "code": err.Code, "injected": err.Injected(), "attempt": s.Attempt})
		r.event(s, EventRunFailed, "run failed: retries exhausted", map[string]any{"code": CodeRetriesExhausted})
		return r
	}
	n := begin(s, env)
	n.Attempt = s.Attempt + 1
	n.RetriesUsed = s.RetriesUsed + 1
	n.Phase = PhaseBackoff
	delay := env.Backoff.Delay(s.Seed, s.Attempt)
	n.NextWakeAt = env.Now.Add(delay)
	n.LastError = err
	r := Result{
		State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool,
		Effects: []Effect{EffScheduleWake{At: n.NextWakeAt}},
	}
	r.event(s, EventToolFailed, "tool failed (retryable)",
		map[string]any{"tool": call.Tool, "code": err.Code, "injected": err.Injected(), "attempt": s.Attempt})
	r.event(s, EventBackoffScheduled, "backoff scheduled",
		map[string]any{"attempt": n.Attempt, "delay_ms": delay.Milliseconds(), "retries_used": n.RetriesUsed})
	return r
}

func skipSource(s RunState, call ToolCall, err *RelayError, env Env) Result {
	i := StepSourceIndex(s.Step)
	if s.Step < 1 || i < 0 || i >= len(s.Sources) {
		// Nothing to skip (e.g. a malformed list step) — this is terminal.
		return terminalFail(s, err, env)
	}
	src := s.Sources[i]
	n := begin(s, env)
	effect := failPending(&n, env) // resolve any in-flight side-effect intent
	n.Attempt = 0
	n.Skipped = append(n.Skipped, src)
	n.Step = StepRead(i + 1)
	n.LastError = err
	r := Result{State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool, Effect: effect}
	r.event(s, EventToolFailed, "tool failed (skippable)",
		map[string]any{"tool": call.Tool, "code": err.Code, "injected": err.Injected()})
	r.event(s, EventSourceSkipped, "source skipped",
		map[string]any{"source_id": src.ID, "code": err.Code})
	return r
}

func terminalFail(s RunState, err *RelayError, env Env) Result {
	n := begin(s, env)
	n.Phase = PhaseFailed
	n.LastError = err
	effect := failPending(&n, env)
	r := Result{State: n, Durable: true, ActionKind: ActionFail, Effect: effect}
	r.event(s, EventToolFailed, "tool failed (terminal)",
		map[string]any{"code": err.Code, "injected": err.Injected()})
	r.event(s, EventRunFailed, "run failed", map[string]any{"code": err.Code})
	return r
}

func escalatePermission(s RunState, call ToolCall, err *RelayError, env Env) Result {
	key := DeriveKey(s.ID, s.Step, ToolRequestReview)
	n := begin(s, env)
	effect := failPending(&n, env) // resolve the abandoned record intent, if any
	n.Phase = PhaseAwaitingHuman
	n.LastError = err
	n.Review = &HumanReviewRef{
		Key: key, Reason: "permission denied: " + err.Message, Severity: "high",
		Status: ReviewPending, Step: s.Step,
	}
	r := Result{
		State: n, Durable: true, ActionKind: ActionCallTool, Tool: call.Tool, Effect: effect,
		Review: &ReviewRow{
			RunID: s.ID, Key: key, Reason: n.Review.Reason, Severity: "high",
			Status: ReviewPending, At: env.Now,
		},
	}
	r.event(s, EventToolFailed, "tool failed (permission denied)",
		map[string]any{"code": err.Code, "injected": err.Injected()})
	r.event(s, EventHumanRequested, "escalated to human review after permission denial",
		map[string]any{"severity": "high"})
	return r
}

func reduceHuman(s RunState, d HumanDecision, env Env) Result {
	if s.Phase != PhaseAwaitingHuman || s.Review == nil {
		return illegal(s, env, "human decision requires awaiting_human")
	}
	switch d {
	case DecisionApprove:
		n := begin(s, env)
		n.Phase = PhaseRunning
		n.Review.Status = ReviewApproved
		n.LastError = nil
		r := Result{
			State: n, Durable: true, ActionKind: ActionWait,
			ReviewFix: &ReviewResolution{Key: n.Review.Key, Status: ReviewApproved, At: env.Now},
		}
		r.event(s, EventHumanResolved, "human approved", map[string]any{"review_id": n.Review.ReviewID})
		return r
	case DecisionReject:
		n := begin(s, env)
		n.Phase = PhaseFailed
		n.Review.Status = ReviewRejected
		n.LastError = NewError(CodeReviewRejected, "human rejected the run")
		r := Result{
			State: n, Durable: true, ActionKind: ActionFail,
			ReviewFix: &ReviewResolution{Key: n.Review.Key, Status: ReviewRejected, At: env.Now},
		}
		r.event(s, EventHumanResolved, "human rejected", map[string]any{"review_id": n.Review.ReviewID})
		r.event(s, EventRunFailed, "run failed: review rejected", map[string]any{"code": CodeReviewRejected})
		return r
	default:
		return illegal(s, env, "unknown human decision")
	}
}

func reduceCancel(s RunState, reason ErrorCode, env Env) Result {
	if s.Phase.Terminal() {
		return illegal(s, env, "cancel requires non-terminal run")
	}
	if reason == "" {
		reason = CodeCancelled
	}
	n := begin(s, env)
	n.Phase = PhaseCancelled
	n.LastError = NewError(reason, "run cancelled")
	effect := failPending(&n, env)
	r := Result{State: n, Durable: true, ActionKind: ActionWait, Effect: effect}
	r.event(s, EventRunCancelled, "run cancelled", map[string]any{"code": reason})
	return r
}

func reduceWake(s RunState, env Env) Result {
	if s.Phase != PhaseBackoff {
		return illegal(s, env, "wake requires backoff")
	}
	n := begin(s, env)
	n.Phase = PhaseRunning
	n.NextWakeAt = time.Time{}
	r := Result{State: n, Durable: true, ActionKind: ActionWait}
	r.event(s, EventWoke, "woke from backoff to retry", map[string]any{"attempt": n.Attempt})
	return r
}
