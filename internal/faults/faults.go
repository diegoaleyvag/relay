// Package faults provides deterministic fault injection for the Relay
// reliability lab. FaultyToolPort wraps a core.ToolPort and, according to a
// fixed FaultPlan keyed by (step, tool), substitutes a scripted failure (or
// escalation) instead of — or in addition to — delegating to the wrapped
// port. Every injected failure is marked via RelayError.MarkInjected so it is
// always visible in the event history, never silently indistinguishable from
// a "real" failure.
//
// This package imports core only: it knows nothing about the clock, the
// corpus, or MCP transport, so it can wrap any core.ToolPort implementation.
package faults

import (
	"context"
	"sync"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// FaultKind names one injectable failure mode.
type FaultKind int

const (
	// FaultTimeout blocks the call (honoring ctx) and then fails with
	// CodeTimeout, simulating a tool that never answers in time.
	FaultTimeout FaultKind = iota
	// FaultMalformed fails immediately with CodeMalformed, simulating a tool
	// that returned a response the adapter could not parse.
	FaultMalformed
	// FaultDuplicate invokes the wrapped port twice for one logical call,
	// simulating a retried request that reached the receiver more than
	// once — the scenario receiver-side idempotency must handle.
	FaultDuplicate
	// FaultKillAfterEffect lets the real call run to completion and then
	// invokes OnKill, simulating a process crash immediately after a side
	// effect took hold but before its result was durably recorded.
	FaultKillAfterEffect
	// FaultPermissionDenied fails immediately with CodePermissionDenied,
	// simulating a tool call rejected by an authorization layer.
	FaultPermissionDenied
	// FaultHumanReview short-circuits with a NeedsInput result, simulating a
	// tool that elicited a human-in-the-loop escalation instead of
	// completing normally.
	FaultHumanReview
)

// String renders the fault kind for logging and test failure messages.
func (k FaultKind) String() string {
	switch k {
	case FaultTimeout:
		return "timeout"
	case FaultMalformed:
		return "malformed"
	case FaultDuplicate:
		return "duplicate"
	case FaultKillAfterEffect:
		return "kill_after_effect"
	case FaultPermissionDenied:
		return "permission_denied"
	case FaultHumanReview:
		return "human_review"
	default:
		return "unknown"
	}
}

// FaultSpec pins one fault to a specific plan step and, optionally, a
// specific tool. Param carries fault-specific configuration (currently only
// "duration", read by FaultTimeout).
//
// Times bounds how many times this spec fires: 0 (the default) means it fires
// on every matching call, so a step-keyed fault recurs on each retry. A
// positive Times fires only that many times and then passes through to Inner —
// this is how a run rehearses "a transient failure that a retry recovers from"
// (e.g. Times: 2 with a retry budget of 3 fails twice then succeeds).
type FaultSpec struct {
	Step  int
	Tool  core.ToolName
	Kind  FaultKind
	Param map[string]any
	Times int
}

// FaultPlan is an ordered, fixed script of faults to inject during a run. It
// is deliberately data, not code, so a test (or a lab scenario file) can
// declare exactly which step/tool pairs misbehave and how.
type FaultPlan struct {
	Faults []FaultSpec
}

// At returns the first FaultSpec matching step (and tool, when the spec
// names one — an empty spec.Tool matches any tool at that step). The second
// return reports whether a match was found; a zero FaultSpec with false
// means "no fault, call through".
func (p FaultPlan) At(step int, tool core.ToolName) (FaultSpec, bool) {
	spec, _, ok := p.match(step, tool)
	return spec, ok
}

// match is At plus the index of the matched spec, used by FaultyToolPort to
// count per-spec hits for the Times bound.
func (p FaultPlan) match(step int, tool core.ToolName) (FaultSpec, int, bool) {
	for i, f := range p.Faults {
		if f.Step != step {
			continue
		}
		if f.Tool != "" && f.Tool != tool {
			continue
		}
		return f, i, true
	}
	return FaultSpec{}, -1, false
}

// FaultEvent is emitted whenever a FaultSpec is matched, regardless of what
// the fault subsequently does — so a plan's faults are always visible even
// if a caller only inspects emitted events rather than errors.
type FaultEvent struct {
	Step int
	Tool core.ToolName
	Kind FaultKind
}

// FaultyToolPort wraps Inner and, for any call matching Plan, substitutes
// the scripted fault behavior instead of (or, for FaultDuplicate and
// FaultKillAfterEffect, in addition to) delegating to Inner. It implements
// core.ToolPort, so it is a drop-in wrapper anywhere a ToolPort is expected.
//
// OnKill, if set, is invoked by FaultKillAfterEffect after the wrapped call
// completes — production callers wire this to something that ends the
// process (e.g. os.Exit) to rehearse a crash-after-effect restart; unit
// tests leave it nil, making FaultKillAfterEffect a safe passthrough.
//
// Emit, if set, is called once per matched fault (before the fault's own
// behavior runs) so a harness can record which faults actually fired during
// a run.
type FaultyToolPort struct {
	Inner  core.ToolPort
	Plan   FaultPlan
	OnKill func()
	Emit   func(FaultEvent)

	mu   sync.Mutex
	hits map[int]int // per-spec-index fire count, for the Times bound
}

// Compile-time assertion that FaultyToolPort satisfies core.ToolPort.
var _ core.ToolPort = (*FaultyToolPort)(nil)

// Invoke consults Plan for call.Step/call.Tool. On no match it delegates
// directly to Inner. On a match it emits a FaultEvent (if Emit is set) and
// then runs the fault's scripted behavior; see the FaultKind docs for what
// each one does.
func (p *FaultyToolPort) Invoke(ctx context.Context, call core.ToolCall) (core.ToolResult, *core.RelayError) {
	spec, idx, hit := p.Plan.match(int(call.Step), call.Tool)
	if !hit {
		return p.Inner.Invoke(ctx, call)
	}

	// A bounded fault (Times > 0) stops firing once exhausted and thereafter
	// passes through to Inner, so a run can recover after a fixed number of
	// transient failures.
	if spec.Times > 0 {
		p.mu.Lock()
		if p.hits == nil {
			p.hits = map[int]int{}
		}
		if p.hits[idx] >= spec.Times {
			p.mu.Unlock()
			return p.Inner.Invoke(ctx, call)
		}
		p.hits[idx]++
		p.mu.Unlock()
	}

	if p.Emit != nil {
		p.Emit(FaultEvent{Step: int(call.Step), Tool: call.Tool, Kind: spec.Kind})
	}

	switch spec.Kind {
	case FaultTimeout:
		return p.injectTimeout(ctx, spec)

	case FaultMalformed:
		return core.ToolResult{}, core.NewError(core.CodeMalformed, "injected malformed response").MarkInjected("malformed")

	case FaultDuplicate:
		// Always invoke Inner exactly twice with the identical call, then
		// decide which outcome to surface. This rehearses the case where a
		// retried request reaches the receiver a second time even though
		// the first attempt's result never made it back to the caller.
		res1, err1 := p.Inner.Invoke(ctx, call)
		res2, err2 := p.Inner.Invoke(ctx, call)
		if err1 != nil {
			return res1, err1
		}
		return res2, err2

	case FaultKillAfterEffect:
		res, err := p.Inner.Invoke(ctx, call)
		if p.OnKill != nil {
			p.OnKill()
		}
		return res, err

	case FaultPermissionDenied:
		return core.ToolResult{}, core.NewError(core.CodePermissionDenied, "injected permission denial").MarkInjected("permission")

	case FaultHumanReview:
		return core.ToolResult{Tool: call.Tool, NeedsInput: true}, nil

	default:
		return core.ToolResult{}, core.NewError(core.CodeInternal, "unknown injected fault kind").MarkInjected("unknown")
	}
}

// injectTimeout implements FaultKind FaultTimeout: it never calls Inner.
// It blocks until ctx is done, or — if spec.Param["duration"] names a valid
// duration — until that duration elapses, whichever happens first. If ctx
// is already done when this runs, it returns promptly. Either way it then
// returns an injected CodeTimeout error.
func (p *FaultyToolPort) injectTimeout(ctx context.Context, spec FaultSpec) (core.ToolResult, *core.RelayError) {
	if d, ok := paramDuration(spec.Param); ok {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	} else {
		<-ctx.Done()
	}
	return core.ToolResult{}, core.NewError(core.CodeTimeout, "injected timeout").MarkInjected("timeout")
}

// paramDuration reads an optional "duration" entry out of a FaultSpec.Param
// map and coerces it to a time.Duration. It accepts a time.Duration, any
// numeric type (interpreted as nanoseconds) or a string parseable by
// time.ParseDuration (e.g. "50ms"). It returns false if the map is nil, has
// no "duration" key, or the value is not in a recognized form — callers
// treat that as "no duration configured", not an error.
func paramDuration(m map[string]any) (time.Duration, bool) {
	v, ok := m["duration"]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case time.Duration:
		return t, true
	case int:
		return time.Duration(t), true
	case int32:
		return time.Duration(t), true
	case int64:
		return time.Duration(t), true
	case float64:
		return time.Duration(t), true
	case string:
		d, err := time.ParseDuration(t)
		if err != nil {
			return 0, false
		}
		return d, true
	default:
		return 0, false
	}
}
