package core

import (
	"encoding/json"
	"testing"
	"time"
)

func testEnv(now time.Time) Env {
	return Env{
		Now:         now,
		Backoff:     DefaultBackoff(),
		MaxAttempts: 3,
		MaxRetries:  5,
		Policy:      DefaultPolicy(),
	}
}

func mustJSONt(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func hasEvent(evs []Event, kind EventKind) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestReduceHappyPath(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := NewRun("run1", 7, false, time.Time{}, now)

	// pending -> running
	r := Reduce(s, InStart{}, env)
	if !r.Durable || r.State.Phase != PhaseRunning || r.State.Version != 1 {
		t.Fatalf("start: durable=%v phase=%v version=%d", r.Durable, r.State.Phase, r.State.Version)
	}
	s = r.State

	// dispatch list_sources (read-only -> not durable)
	list := CallTool(s.ID, StepList(), ToolListSources, mustJSONt(t, ListSourcesInput{Limit: 100}))
	r = Reduce(s, InAction{Action: list}, env)
	if r.Durable {
		t.Fatal("read-only dispatch must not be durable")
	}
	if len(r.Effects) != 1 {
		t.Fatalf("expected one EffCallTool, got %d", len(r.Effects))
	}

	sources := []SourceRef{{ID: "s1", Title: "A", Bytes: 10}, {ID: "s2", Title: "B", Bytes: 20}}
	res := ToolResult{Tool: ToolListSources, Output: mustJSONt(t, ListSourcesOutput{Sources: sources})}
	r = Reduce(s, InToolResult{Call: ToolCall{Tool: ToolListSources, Step: 0}, Result: res}, env)
	s = r.State
	if !r.Durable || len(s.Sources) != 2 || s.Step != 1 {
		t.Fatalf("list result: durable=%v sources=%d step=%d", r.Durable, len(s.Sources), s.Step)
	}

	for i := 0; i < 2; i++ {
		if s.Step != StepRead(i) {
			t.Fatalf("source %d: expected read step %d, got %d", i, StepRead(i), s.Step)
		}
		// read (not durable dispatch, then durable result)
		read := CallTool(s.ID, s.Step, ToolReadSource, mustJSONt(t, ReadSourceInput{ID: sources[i].ID}))
		r = Reduce(s, InAction{Action: read}, env)
		if r.Durable {
			t.Fatal("read dispatch must not be durable")
		}
		rres := ToolResult{Tool: ToolReadSource, Output: mustJSONt(t, ReadSourceOutput{ID: sources[i].ID, Bytes: 1})}
		r = Reduce(s, InToolResult{Call: ToolCall{Tool: ToolReadSource, Step: s.Step}, Result: rres}, env)
		s = r.State
		if s.Step != StepRecord(i) {
			t.Fatalf("after read %d: expected record step %d, got %d", i, StepRecord(i), s.Step)
		}

		// record finding: intent then confirm
		key := DeriveKey(s.ID, s.Step, ToolRecordFinding)
		fi := RecordFindingInput{IdempotencyKey: string(key), SourceID: sources[i].ID, Claim: "c", Confidence: 0.9}
		rec := CallTool(s.ID, s.Step, ToolRecordFinding, mustJSONt(t, fi))
		if rec.Idem != key {
			t.Fatalf("record action idem %q != derived %q", rec.Idem, key)
		}
		r = Reduce(s, InAction{Action: rec}, env)
		if !r.Durable || r.State.Pending == nil || r.Effect == nil || r.Effect.Status != EffectIntent {
			t.Fatalf("intent: durable=%v pending=%v effect=%+v", r.Durable, r.State.Pending, r.Effect)
		}
		if !hasEvent(r.Events, EventSideEffectIntent) {
			t.Fatal("intent event missing")
		}
		s = r.State

		confirmCall := ToolCall{Tool: ToolRecordFinding, Step: s.Step, Idem: key, Input: mustJSONt(t, fi)}
		fres := ToolResult{Tool: ToolRecordFinding, Output: mustJSONt(t, RecordFindingOutput{FindingID: "f" + sources[i].ID, Recorded: true})}
		r = Reduce(s, InToolResult{Call: confirmCall, Result: fres}, env)
		s = r.State
		if !r.Durable || s.Pending != nil {
			t.Fatalf("confirm: durable=%v pending=%v", r.Durable, s.Pending)
		}
		if r.Finding == nil || r.Finding.SourceID != sources[i].ID {
			t.Fatalf("confirm finding row wrong: %+v", r.Finding)
		}
		if r.Effect == nil || r.Effect.Status != EffectConfirmed {
			t.Fatalf("confirm effect wrong: %+v", r.Effect)
		}
		if len(s.Findings) != i+1 || s.Findings[i].SourceID != sources[i].ID {
			t.Fatalf("findings after %d: %+v", i, s.Findings)
		}
		if s.Step != StepRead(i+1) {
			t.Fatalf("after record %d: expected step %d, got %d", i, StepRead(i+1), s.Step)
		}
	}

	// complete
	r = Reduce(s, InAction{Action: Complete()}, env)
	s = r.State
	if !r.Durable || s.Phase != PhaseSucceeded || len(s.Findings) != 2 {
		t.Fatalf("complete: phase=%v findings=%d", s.Phase, len(s.Findings))
	}
	if !hasEvent(r.Events, EventRunSucceeded) {
		t.Fatal("run_succeeded event missing")
	}
}

func runningAtRead(now time.Time) RunState {
	s := NewRun("r", 1, false, time.Time{}, now)
	s.Phase = PhaseRunning
	s.Version = 1
	s.Sources = []SourceRef{{ID: "s1", Title: "A", Bytes: 10}, {ID: "s2", Title: "B", Bytes: 20}}
	s.Step = StepRead(0)
	return s
}

func TestReduceRetryThenExhaust(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := runningAtRead(now)
	call := ToolCall{Tool: ToolReadSource, Step: s.Step}

	// attempt 0 -> backoff
	r := Reduce(s, InToolResult{Call: call, Err: NewError(CodeTimeout, "slow")}, env)
	if r.State.Phase != PhaseBackoff || r.State.Attempt != 1 || r.State.RetriesUsed != 1 {
		t.Fatalf("attempt0: phase=%v attempt=%d used=%d", r.State.Phase, r.State.Attempt, r.State.RetriesUsed)
	}
	if r.State.NextWakeAt.IsZero() {
		t.Fatal("next wake not set")
	}
	if !hasEvent(r.Events, EventBackoffScheduled) {
		t.Fatal("backoff event missing")
	}
	s = r.State
	s.Phase = PhaseRunning // simulate wake

	// attempt 1 -> backoff
	r = Reduce(s, InToolResult{Call: call, Err: NewError(CodeTimeout, "slow")}, env)
	if r.State.Phase != PhaseBackoff || r.State.Attempt != 2 {
		t.Fatalf("attempt1: phase=%v attempt=%d", r.State.Phase, r.State.Attempt)
	}
	s = r.State
	s.Phase = PhaseRunning

	// attempt 2 -> exhausted (terminal)
	r = Reduce(s, InToolResult{Call: call, Err: NewError(CodeTimeout, "slow")}, env)
	if r.State.Phase != PhaseFailed || r.State.LastError == nil || r.State.LastError.Code != CodeRetriesExhausted {
		t.Fatalf("exhaust: phase=%v err=%+v", r.State.Phase, r.State.LastError)
	}
	if !hasEvent(r.Events, EventRunFailed) {
		t.Fatal("run_failed event missing")
	}
}

func TestReduceMalformedSkips(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now) // MalformedSkips = true by default
	s := runningAtRead(now)
	call := ToolCall{Tool: ToolReadSource, Step: s.Step}

	r := Reduce(s, InToolResult{Call: call, Err: NewError(CodeMalformed, "garbage")}, env)
	s = r.State
	if s.Phase != PhaseRunning {
		t.Fatalf("skip should stay running, got %v", s.Phase)
	}
	if len(s.Skipped) != 1 || s.Skipped[0].ID != "s1" {
		t.Fatalf("expected s1 skipped, got %+v", s.Skipped)
	}
	if s.Step != StepRead(1) {
		t.Fatalf("skip should advance to next source read step %d, got %d", StepRead(1), s.Step)
	}
	if !hasEvent(r.Events, EventSourceSkipped) {
		t.Fatal("source_skipped event missing")
	}
}

func TestReduceMalformedTerminalPolicy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	env.Policy.MalformedSkips = false
	s := runningAtRead(now)
	call := ToolCall{Tool: ToolReadSource, Step: s.Step}

	r := Reduce(s, InToolResult{Call: call, Err: NewError(CodeMalformed, "garbage")}, env)
	if r.State.Phase != PhaseFailed {
		t.Fatalf("malformed-as-terminal should fail, got %v", r.State.Phase)
	}
}

func TestReduceMalformedListIsTerminal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := NewRun("r", 1, false, time.Time{}, now)
	s.Phase = PhaseRunning
	s.Version = 1
	s.Step = StepList()
	call := ToolCall{Tool: ToolListSources, Step: 0}
	r := Reduce(s, InToolResult{Call: call, Err: NewError(CodeMalformed, "garbage")}, env)
	if r.State.Phase != PhaseFailed {
		t.Fatalf("malformed list has nothing to skip; should fail, got %v", r.State.Phase)
	}
}

func TestReducePermissionDeniedTerminal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := runningAtRead(now)
	call := ToolCall{Tool: ToolReadSource, Step: s.Step}
	r := Reduce(s, InToolResult{Call: call, Err: NewError(CodePermissionDenied, "no")}, env)
	if r.State.Phase != PhaseFailed || r.State.LastError.Code != CodePermissionDenied {
		t.Fatalf("permission denied should fail closed, got %v %v", r.State.Phase, r.State.LastError)
	}
}

func TestReducePermissionEscalates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	env.Policy.PermissionEscalates = true
	s := runningAtRead(now)
	call := ToolCall{Tool: ToolReadSource, Step: s.Step, Input: mustJSONt(t, ReadSourceInput{ID: "s1"})}
	r := Reduce(s, InToolResult{Call: call, Err: NewError(CodePermissionDenied, "no")}, env)
	if r.State.Phase != PhaseAwaitingHuman || r.State.Review == nil {
		t.Fatalf("permission escalate should await human, got %v review=%v", r.State.Phase, r.State.Review)
	}
	if r.Review == nil {
		t.Fatal("expected a review row for the escalation")
	}
}

func TestReduceNonAllowlistedToolFailsClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := runningAtRead(now)
	bogus := Action{Kind: ActionCallTool, Tool: "delete_everything", Step: s.Step}
	r := Reduce(s, InAction{Action: bogus}, env)
	if r.State.Phase != PhaseFailed || r.State.LastError.Code != CodePermissionDenied {
		t.Fatalf("non-allowlisted tool should fail closed, got %v", r.State.Phase)
	}
}

func TestReduceHumanReviewApprove(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := NewRun("r", 1, true, time.Time{}, now)
	s.Phase = PhaseRunning
	s.Version = 1
	s.Sources = []SourceRef{{ID: "s1"}}
	s.Step = StepReview(1)

	// request_human_review intent + confirm
	key := DeriveKey(s.ID, s.Step, ToolRequestReview)
	in := RequestHumanReviewInput{IdempotencyKey: string(key), Reason: "policy", Severity: "medium"}
	act := CallTool(s.ID, s.Step, ToolRequestReview, mustJSONt(t, in))
	r := Reduce(s, InAction{Action: act}, env)
	s = r.State
	if s.Pending == nil {
		t.Fatal("review intent should set pending")
	}
	confirmCall := ToolCall{Tool: ToolRequestReview, Step: s.Step, Idem: key, Input: mustJSONt(t, in)}
	out := RequestHumanReviewOutput{ReviewID: "rev1", State: "awaiting_human_review"}
	r = Reduce(s, InToolResult{Call: confirmCall, Result: ToolResult{Tool: ToolRequestReview, Output: mustJSONt(t, out)}}, env)
	s = r.State
	if s.Phase != PhaseAwaitingHuman || s.Review == nil || s.Review.Status != ReviewPending {
		t.Fatalf("after review request: phase=%v review=%+v", s.Phase, s.Review)
	}

	// approve
	r = Reduce(s, InHuman{Decision: DecisionApprove}, env)
	s = r.State
	if s.Phase != PhaseRunning || s.Review.Status != ReviewApproved {
		t.Fatalf("approve: phase=%v status=%v", s.Phase, s.Review.Status)
	}
	if r.ReviewFix == nil || r.ReviewFix.Status != ReviewApproved {
		t.Fatalf("expected review resolution, got %+v", r.ReviewFix)
	}

	// planner would now complete
	r = Reduce(s, InAction{Action: Complete()}, env)
	if r.State.Phase != PhaseSucceeded {
		t.Fatalf("after approve+complete: %v", r.State.Phase)
	}
}

func TestReduceHumanReviewReject(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := NewRun("r", 1, true, time.Time{}, now)
	s.Phase = PhaseAwaitingHuman
	s.Version = 3
	s.Review = &HumanReviewRef{Key: "k", ReviewID: "rev1", Status: ReviewPending}

	r := Reduce(s, InHuman{Decision: DecisionReject}, env)
	if r.State.Phase != PhaseFailed || r.State.LastError.Code != CodeReviewRejected {
		t.Fatalf("reject: phase=%v err=%v", r.State.Phase, r.State.LastError)
	}
	if r.ReviewFix == nil || r.ReviewFix.Status != ReviewRejected {
		t.Fatalf("expected rejected resolution, got %+v", r.ReviewFix)
	}
}

func TestReduceCancel(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	s := runningAtRead(now)
	// pretend a side effect is in flight
	s.Pending = &PendingEffect{Key: "k", Tool: ToolRecordFinding, Step: s.Step}
	r := Reduce(s, InCancel{Reason: CodeDeadlineExceeded}, env)
	if r.State.Phase != PhaseCancelled {
		t.Fatalf("cancel phase=%v", r.State.Phase)
	}
	if r.State.Pending != nil {
		t.Fatal("cancel should clear pending")
	}
	if r.Effect == nil || r.Effect.Status != EffectFailed {
		t.Fatalf("cancel should mark the in-flight effect failed, got %+v", r.Effect)
	}
}

// TestReduceTotality feeds every input into every phase and asserts the reducer
// never panics and either performs a legal durable transition or reports an
// illegal_transition without changing the version.
func TestReduceTotality(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	env := testEnv(now)
	phases := []Phase{PhasePending, PhaseRunning, PhaseBackoff, PhaseAwaitingHuman, PhaseSucceeded, PhaseFailed, PhaseCancelled}
	inputs := []Input{
		InStart{},
		InAction{Action: Complete()},
		InToolResult{Call: ToolCall{Tool: ToolReadSource}, Result: ToolResult{Tool: ToolReadSource, Output: json.RawMessage(`{}`)}},
		InToolResult{Call: ToolCall{Tool: ToolReadSource}, Err: NewError(CodeTimeout, "x")},
		InHuman{Decision: DecisionApprove},
		InCancel{Reason: CodeCancelled},
	}
	for _, ph := range phases {
		for _, in := range inputs {
			s := NewRun("r", 1, false, time.Time{}, now)
			s.Phase = ph
			s.Version = 5
			s.Sources = []SourceRef{{ID: "s1"}}
			s.Step = StepRead(0)
			if ph == PhaseAwaitingHuman {
				s.Review = &HumanReviewRef{Key: "k", Status: ReviewPending}
			}
			r := Reduce(s, in, env) // must not panic
			if !r.Durable {
				if r.State.Version != s.Version {
					t.Fatalf("non-durable result changed version in phase %v", ph)
				}
			} else {
				if !CanTransition(ph, r.State.Phase) {
					t.Fatalf("durable transition %v->%v is not in the legal table", ph, r.State.Phase)
				}
				if r.State.Version != s.Version+1 {
					t.Fatalf("durable transition must bump version once (phase %v)", ph)
				}
			}
		}
	}
}
