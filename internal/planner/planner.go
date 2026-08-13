// Package planner is the scripted, deterministic research planner. It is not a
// model and performs no I/O: Next is a pure function of RunState, so identical
// state always yields the identical next action. That determinism is what lets a
// run be re-planned safely after a restart. It imports only the domain core.
//
// The plan is fixed: list the sources, then for each source read it and record a
// finding, then optionally request human review, then complete.
package planner

import (
	"encoding/json"
	"fmt"

	"github.com/diegoaleyvag/relay/internal/core"
)

// Deterministic is the scripted planner. Limit bounds the initial listing.
type Deterministic struct {
	Limit int
}

// New returns a scripted planner with sensible defaults.
func New() *Deterministic { return &Deterministic{Limit: 100} }

// Kind labels the planner as scripted and deterministic — never as AI.
func (p *Deterministic) Kind() string { return core.PlannerKind }

// Next returns the next action for the given state. It never mutates the state
// and never performs I/O.
func (p *Deterministic) Next(s core.RunState) (core.Action, error) {
	// 1. List the sources (once).
	if s.Sources == nil {
		limit := p.Limit
		if limit <= 0 {
			limit = 100
		}
		in := mustJSON(core.ListSourcesInput{Limit: limit})
		return core.CallTool(s.ID, core.StepList(), core.ToolListSources, in), nil
	}

	n := len(s.Sources)

	// 2. Per-source phase: read then record for source i.
	if int(s.Step) >= 1 && int(s.Step) < 1+2*n {
		i := core.StepSourceIndex(s.Step)
		src := s.Sources[i]
		if core.StepIsRead(s.Step) {
			in := mustJSON(core.ReadSourceInput{ID: src.ID})
			return core.CallTool(s.ID, s.Step, core.ToolReadSource, in), nil
		}
		return recordFindingAction(s, src), nil
	}

	// 3. All sources processed: escalate if required, else complete.
	if s.RequireReview && s.Review == nil {
		return requestReviewAction(s, n), nil
	}
	if s.Review != nil {
		switch s.Review.Status {
		case core.ReviewApproved:
			return core.Complete(), nil
		case core.ReviewRejected:
			return core.Fail(), nil
		}
	}
	return core.Complete(), nil
}

// recordFindingAction builds a deterministic record_finding action. The claim
// and confidence are pure functions of the source metadata and the run seed —
// they never depend on the (unpersisted) read body, which is what keeps the
// planner pure and resumable.
func recordFindingAction(s core.RunState, src core.SourceRef) core.Action {
	key := core.DeriveKey(s.ID, s.Step, core.ToolRecordFinding)
	in := core.RecordFindingInput{
		IdempotencyKey: string(key),
		SourceID:       src.ID,
		Claim: fmt.Sprintf("Source %s (%q, %d bytes) reviewed and summarized.",
			src.ID, src.Title, src.Bytes),
		Evidence:   fmt.Sprintf("media_type=%s; tags=%v", src.MediaType, src.Tags),
		Confidence: confidence(s.Seed, src.ID),
	}
	return core.CallTool(s.ID, s.Step, core.ToolRecordFinding, mustJSON(in))
}

// requestReviewAction builds a deterministic request_human_review action.
func requestReviewAction(s core.RunState, n int) core.Action {
	step := core.StepReview(n)
	key := core.DeriveKey(s.ID, step, core.ToolRequestReview)
	in := core.RequestHumanReviewInput{
		IdempotencyKey: string(key),
		Reason:         "policy requires human review before completion",
		Severity:       "medium",
	}
	return core.CallTool(s.ID, step, core.ToolRequestReview, mustJSON(in))
}

// confidence maps a (seed, sourceID) pair to a stable value in [0.60, 0.99].
func confidence(seed uint64, sourceID string) float64 {
	h := seed
	for _, b := range []byte(sourceID) {
		h = h*1099511628211 ^ uint64(b) // FNV-ish mix
	}
	return 0.60 + float64(h%40)/100.0
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
