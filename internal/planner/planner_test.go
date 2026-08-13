package planner

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

func state(sources []core.SourceRef, step core.StepIndex, requireReview bool) core.RunState {
	s := core.NewRun("run1", 7, requireReview, time.Time{}, time.Unix(0, 0))
	s.Phase = core.PhaseRunning
	s.Version = 1
	s.Listed = true
	s.Sources = sources
	s.Step = step
	return s
}

func TestPlannerListsFirst(t *testing.T) {
	p := New()
	s := core.NewRun("run1", 7, false, time.Time{}, time.Unix(0, 0))
	s.Phase = core.PhaseRunning
	a, err := p.Next(s)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != core.ActionCallTool || a.Tool != core.ToolListSources || a.Step != core.StepList() {
		t.Fatalf("expected list_sources at step 0, got %+v", a)
	}
}

func TestPlannerPerSourceSequence(t *testing.T) {
	p := New()
	sources := []core.SourceRef{{ID: "s1", Title: "A", Bytes: 10}, {ID: "s2", Title: "B", Bytes: 20}}

	// read s1
	if a, _ := p.Next(state(sources, core.StepRead(0), false)); a.Tool != core.ToolReadSource {
		t.Fatalf("step %d expected read_source, got %+v", core.StepRead(0), a)
	}
	// record s1: idempotency key must match and be embedded in the input
	a, _ := p.Next(state(sources, core.StepRecord(0), false))
	if a.Tool != core.ToolRecordFinding {
		t.Fatalf("step %d expected record_finding, got %+v", core.StepRecord(0), a)
	}
	wantKey := core.DeriveKey("run1", core.StepRecord(0), core.ToolRecordFinding)
	if a.Idem != wantKey {
		t.Fatalf("action idem %q != derived %q", a.Idem, wantKey)
	}
	var in core.RecordFindingInput
	if err := json.Unmarshal(a.Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.IdempotencyKey != string(wantKey) {
		t.Fatalf("embedded idempotency_key %q != %q", in.IdempotencyKey, wantKey)
	}
	if in.SourceID != "s1" {
		t.Fatalf("record for wrong source: %q", in.SourceID)
	}
	// read s2
	if a, _ := p.Next(state(sources, core.StepRead(1), false)); a.Tool != core.ToolReadSource {
		t.Fatalf("step %d expected read_source s2, got %+v", core.StepRead(1), a)
	}
}

func TestPlannerCompletesWithoutReview(t *testing.T) {
	p := New()
	sources := []core.SourceRef{{ID: "s1"}, {ID: "s2"}}
	a, _ := p.Next(state(sources, core.StepReview(2), false))
	if a.Kind != core.ActionComplete {
		t.Fatalf("expected complete, got %+v", a)
	}
}

func TestPlannerRequestsReviewWhenRequired(t *testing.T) {
	p := New()
	sources := []core.SourceRef{{ID: "s1"}}
	a, _ := p.Next(state(sources, core.StepReview(1), true))
	if a.Tool != core.ToolRequestReview || a.Step != core.StepReview(1) {
		t.Fatalf("expected request_human_review, got %+v", a)
	}
}

func TestPlannerAfterReviewResolution(t *testing.T) {
	p := New()
	sources := []core.SourceRef{{ID: "s1"}}

	s := state(sources, core.StepReview(1), true)
	s.Review = &core.HumanReviewRef{Status: core.ReviewApproved}
	if a, _ := p.Next(s); a.Kind != core.ActionComplete {
		t.Fatalf("approved review should complete, got %+v", a)
	}

	s.Review = &core.HumanReviewRef{Status: core.ReviewRejected}
	if a, _ := p.Next(s); a.Kind != core.ActionFail {
		t.Fatalf("rejected review should fail, got %+v", a)
	}
}

func TestPlannerDeterministic(t *testing.T) {
	p := New()
	sources := []core.SourceRef{{ID: "s1", Title: "A", Bytes: 10}}
	s := state(sources, core.StepRecord(0), false)
	a1, _ := p.Next(s)
	a2, _ := p.Next(s)
	if string(a1.Input) != string(a2.Input) || a1.Idem != a2.Idem || a1.Tool != a2.Tool {
		t.Fatal("planner is not deterministic for identical state")
	}
}
