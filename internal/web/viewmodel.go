package web

import (
	"encoding/json"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// This file builds the safe, metadata-only view models the templates render.
// Every type here is a deliberate allowlist of fields copied out of the
// domain's RunState / Event / FindingRow: nothing that could carry source
// content or finding claims is ever copied in. In particular:
//
//   - Findings are rendered from FindingRef-shaped data only (source id,
//     idempotency key, finding id) — never FindingRow.Claim or
//     FindingRow.Evidence, which hold the actual recorded content.
//   - Event evidence is decoded only far enough to pull out a small, known
//     set of redacted metadata fields (tool, code, injected, ...); the
//     summary string is already redacted, human-facing copy from the
//     reducer, safe to display verbatim.

// timeLayout is the format every displayed timestamp uses.
const timeLayout = "2006-01-02T15:04:05Z07:00"

// formatTime renders t in a stable, sortable, human-readable form, or "" for
// the zero value (so optional timestamps render as blank rather than
// "0001-01-01...").
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

// phaseLabels gives each phase a short, human-facing label for badges.
var phaseLabels = map[core.Phase]string{
	core.PhasePending:       "Pending",
	core.PhaseRunning:       "Running",
	core.PhaseBackoff:       "Backoff",
	core.PhaseAwaitingHuman: "Awaiting human review",
	core.PhaseSucceeded:     "Succeeded",
	core.PhaseFailed:        "Failed",
	core.PhaseCancelled:     "Cancelled",
}

// phaseLabel returns p's display label, falling back to the raw phase string
// for any value phaseLabels does not know about (defensive; every current
// core.Phase is covered).
func phaseLabel(p core.Phase) string {
	if l, ok := phaseLabels[p]; ok {
		return l
	}
	return string(p)
}

// resumeLegal reports whether Runner.Resume is a legal action for a run
// currently in phase p. Resume nudges a parked-but-not-escalated run
// (pending or backoff) to continue; it is never legal while a run is already
// actively running, parked on a human decision, or terminal.
func resumeLegal(p core.Phase) bool {
	return p == core.PhasePending || p == core.PhaseBackoff
}

// cancelLegal reports whether Runner.Cancel is legal for phase p: any
// non-terminal phase.
func cancelLegal(p core.Phase) bool {
	return !p.Terminal()
}

// reviewLegal reports whether Runner.ResolveHuman (approve or reject) is
// legal for phase p: only while parked awaiting a human decision.
func reviewLegal(p core.Phase) bool {
	return p == core.PhaseAwaitingHuman
}

// IndexPage is the view model for the run-list / new-run-form page.
type IndexPage struct {
	Runs      []ViewRun
	Scenarios []Scenario
}

// ViewRun is one row of the run-list table: id, scenario label, phase badge
// and findings count only.
type ViewRun struct {
	ID         string
	Scenario   string
	Phase      string
	PhaseLabel string
	Findings   int
	CreatedAt  string
}

// buildViewRun projects a RunState into its safe index-row view model.
// scenario is the best-effort scenario label the control room recorded when
// the run was created (see Server.scenarioOf); it is "unknown" for runs the
// current process did not itself create.
func buildViewRun(run core.RunState, scenario string) ViewRun {
	return ViewRun{
		ID:         string(run.ID),
		Scenario:   scenario,
		Phase:      string(run.Phase),
		PhaseLabel: phaseLabel(run.Phase),
		Findings:   len(run.Findings),
		CreatedAt:  formatTime(run.CreatedAt),
	}
}

// RunPage is the view model for the full single-run timeline page.
type RunPage struct {
	RunID    string
	State    ViewState
	Timeline ViewTimeline
}

// ViewState is the safe view model for the state partial: current phase,
// retry budget, the next scripted plan step (when running), any escalation,
// and which controls are legal right now.
type ViewState struct {
	RunID       string
	Phase       string
	PhaseLabel  string
	Terminal    bool
	Step        int
	Attempt     int
	RetriesUsed int
	Version     int64

	NextStep *ViewNextStep

	LastErrorCode  string
	LastErrorClass string

	Escalation *ViewEscalation

	CanResume  bool
	CanCancel  bool
	CanApprove bool
	CanReject  bool
}

// ViewNextStep describes the next permitted scripted plan step, as computed
// by the deterministic planner. Label is always the fixed, non-anthropomorphic
// phrase "scripted plan step" — the planner is never described as AI or
// reasoning.
type ViewNextStep struct {
	Label string
	Kind  string // core.ActionKind: call_tool, complete, fail, wait
	Tool  string // core.ToolName, set only when Kind == call_tool
}

// ViewEscalation is the safe view of a run parked awaiting human review: the
// reason and severity the planner recorded, and the review's id and status.
type ViewEscalation struct {
	Reason   string
	Severity string
	ReviewID string
	Status   string
}

// buildViewState projects a RunState (plus the scripted planner's opinion of
// what comes next) into the state partial's view model.
func (s *Server) buildViewState(run core.RunState) ViewState {
	vs := ViewState{
		RunID:       string(run.ID),
		Phase:       string(run.Phase),
		PhaseLabel:  phaseLabel(run.Phase),
		Terminal:    run.Phase.Terminal(),
		Step:        int(run.Step),
		Attempt:     run.Attempt,
		RetriesUsed: run.RetriesUsed,
		Version:     int64(run.Version),
		CanResume:   resumeLegal(run.Phase),
		CanCancel:   cancelLegal(run.Phase),
		CanApprove:  reviewLegal(run.Phase),
		CanReject:   reviewLegal(run.Phase),
	}

	if run.LastError != nil {
		vs.LastErrorCode = string(run.LastError.Code)
		vs.LastErrorClass = run.LastError.Class.String()
	}

	if run.Review != nil {
		vs.Escalation = &ViewEscalation{
			Reason:   run.Review.Reason,
			Severity: run.Review.Severity,
			ReviewID: run.Review.ReviewID,
			Status:   string(run.Review.Status),
		}
	}

	vs.NextStep = s.nextScriptedStep(run)

	return vs
}

// nextScriptedStep asks the scripted, deterministic planner what it would do
// next, but only while the run is actively running: the planner is a pure
// function of state and has no opinion about backoff timers, human decisions
// or terminal outcomes, all of which are handled by the engine (and the UI)
// around it, not by the plan itself. A planner error (never expected from the
// scripted planner in practice) simply suppresses the next-step display
// rather than failing the page.
func (s *Server) nextScriptedStep(run core.RunState) *ViewNextStep {
	if run.Phase != core.PhaseRunning {
		return nil
	}
	a, err := s.planner.Next(run)
	if err != nil {
		return nil
	}
	step := &ViewNextStep{Label: "scripted plan step", Kind: string(a.Kind)}
	if a.Kind == core.ActionCallTool {
		step.Tool = string(a.Tool)
	}
	return step
}

// ViewTimeline is the safe view model for the timeline partial: the run's
// tool-attempt history (from redacted events) and its preserved findings
// (metadata only), plus its current checkpoint version.
type ViewTimeline struct {
	RunID    string
	Terminal bool
	Version  int64

	SourceCount  int
	SkippedCount int

	Attempts []ViewAttempt
	Findings []ViewFinding
}

// ViewAttempt is one redacted event in a run's history, with a handful of
// well-known evidence fields (tool, error code, whether the failure was a
// deliberately injected fault) pulled out for display.
type ViewAttempt struct {
	Seq     int64
	Version int64
	Kind    string
	Summary string
	At      string

	Tool     string
	Code     string
	Injected bool
}

// ViewFinding is a preserved finding's metadata only: which source it came
// from and the durable finding id it was recorded under. It deliberately has
// no field for the finding's claim or evidence content.
type ViewFinding struct {
	SourceID  string
	Key       string
	FindingID string
}

// eventEvidence is the small, known subset of an event's redacted evidence
// this package understands. Unknown or absent fields simply decode to their
// zero value; a malformed or missing Evidence payload is not an error here
// (best-effort — the timeline still renders using Summary/Kind alone).
type eventEvidence struct {
	Tool     string `json:"tool"`
	Code     string `json:"code"`
	Injected bool   `json:"injected"`
}

// buildViewAttempt projects one redacted Event into its display row.
func buildViewAttempt(ev core.Event) ViewAttempt {
	var ee eventEvidence
	_ = json.Unmarshal(ev.Evidence, &ee) // best-effort; see eventEvidence doc

	return ViewAttempt{
		Seq:      ev.Seq,
		Version:  int64(ev.Version),
		Kind:     string(ev.Kind),
		Summary:  ev.Summary,
		At:       formatTime(ev.At),
		Tool:     ee.Tool,
		Code:     ee.Code,
		Injected: ee.Injected,
	}
}

// buildViewTimeline projects a run's current state, its full redacted event
// history and its durably recorded findings into the timeline partial's view
// model. findingRows is the repository's authoritative, chronological
// findings list (core.Repository.Findings); each row's FindingID is looked up
// from the run snapshot's own FindingRef list (state.Findings), which is the
// only place the domain records that id.
func buildViewTimeline(run core.RunState, events []core.Event, findingRows []core.FindingRow) ViewTimeline {
	findingIDByKey := make(map[core.IdempotencyKey]string, len(run.Findings))
	for _, f := range run.Findings {
		findingIDByKey[f.Key] = f.FindingID
	}

	findings := make([]ViewFinding, 0, len(findingRows))
	for _, row := range findingRows {
		findings = append(findings, ViewFinding{
			SourceID:  row.SourceID,
			Key:       string(row.Key),
			FindingID: findingIDByKey[row.Key],
		})
	}

	attempts := make([]ViewAttempt, 0, len(events))
	for _, ev := range events {
		attempts = append(attempts, buildViewAttempt(ev))
	}

	return ViewTimeline{
		RunID:        string(run.ID),
		Terminal:     run.Phase.Terminal(),
		Version:      int64(run.Version),
		SourceCount:  len(run.Sources),
		SkippedCount: len(run.Skipped),
		Attempts:     attempts,
		Findings:     findings,
	}
}
