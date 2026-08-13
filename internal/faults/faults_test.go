package faults

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// fakeToolPort is a minimal core.ToolPort that records every call it
// receives (guarded by a mutex so it is safe under -race) and delegates the
// actual response to an injectable invoke function, or a fixed canned result
// when invoke is nil.
type fakeToolPort struct {
	mu     sync.Mutex
	calls  []core.ToolCall
	invoke func(n int, call core.ToolCall) (core.ToolResult, *core.RelayError)
}

func (f *fakeToolPort) Invoke(_ context.Context, call core.ToolCall) (core.ToolResult, *core.RelayError) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	n := len(f.calls)
	f.mu.Unlock()
	if f.invoke != nil {
		return f.invoke(n, call)
	}
	return core.ToolResult{Tool: call.Tool}, nil
}

func (f *fakeToolPort) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var _ core.ToolPort = (*fakeToolPort)(nil)

func TestFaultyToolPortTimesBoundedThenRecovers(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{Inner: inner, Plan: FaultPlan{Faults: []FaultSpec{
		{Step: 3, Kind: FaultMalformed, Times: 2},
	}}}
	call := core.ToolCall{Tool: core.ToolReadSource, Step: 3}

	// First two calls fire the fault (malformed, inner never touched).
	for i := 0; i < 2; i++ {
		if _, err := p.Invoke(context.Background(), call); err == nil || err.Code != core.CodeMalformed {
			t.Fatalf("call %d: expected injected malformed, got %v", i, err)
		}
	}
	// After Times is exhausted the fault passes through to Inner (success).
	for i := 0; i < 2; i++ {
		if _, err := p.Invoke(context.Background(), call); err != nil {
			t.Fatalf("recovered call %d: expected passthrough success, got %v", i, err)
		}
	}
	if inner.callCount() != 2 {
		t.Fatalf("expected inner called only on the 2 passthroughs, got %d", inner.callCount())
	}
}

func TestFaultyToolPortPassthroughOnNoMatch(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{Inner: inner, Plan: FaultPlan{}}

	call := core.ToolCall{Tool: core.ToolListSources, Step: 0}
	res, err := p.Invoke(context.Background(), call)
	if err != nil {
		t.Fatalf("Invoke returned %v, want nil", err)
	}
	if res.Tool != core.ToolListSources {
		t.Fatalf("res.Tool = %q, want %q", res.Tool, core.ToolListSources)
	}
	if inner.callCount() != 1 {
		t.Fatalf("inner called %d times, want 1", inner.callCount())
	}
}

func TestFaultPlanAtMatchesStepAndOptionalTool(t *testing.T) {
	plan := FaultPlan{Faults: []FaultSpec{
		{Step: 1, Tool: core.ToolReadSource, Kind: FaultMalformed},
		{Step: 2, Kind: FaultTimeout}, // no Tool: matches any tool at step 2
	}}

	if _, hit := plan.At(0, core.ToolReadSource); hit {
		t.Fatal("step 0 unexpectedly matched")
	}
	if _, hit := plan.At(1, core.ToolListSources); hit {
		t.Fatal("step 1 with wrong tool unexpectedly matched")
	}
	spec, hit := plan.At(1, core.ToolReadSource)
	if !hit || spec.Kind != FaultMalformed {
		t.Fatalf("plan.At(1, read_source) = %+v, %v, want FaultMalformed, true", spec, hit)
	}
	spec, hit = plan.At(2, core.ToolRecordFinding)
	if !hit || spec.Kind != FaultTimeout {
		t.Fatalf("plan.At(2, record_finding) = %+v, %v, want FaultTimeout, true", spec, hit)
	}
}

func TestFaultTimeoutAlreadyCancelledContext(t *testing.T) {
	inner := &fakeToolPort{}
	var emitted []FaultEvent
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 3, Tool: core.ToolReadSource, Kind: FaultTimeout},
		}},
		Emit: func(e FaultEvent) { emitted = append(emitted, e) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	res, err := p.Invoke(ctx, core.ToolCall{Tool: core.ToolReadSource, Step: 3})
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("Invoke took %v with an already-cancelled ctx, want prompt return", elapsed)
	}
	if err == nil {
		t.Fatal("Invoke returned nil error, want injected TIMEOUT")
	}
	if err.Code != core.CodeTimeout {
		t.Fatalf("err.Code = %v, want %v", err.Code, core.CodeTimeout)
	}
	if !err.Injected() {
		t.Fatal("err.Injected() = false, want true")
	}
	if res.Tool != "" || res.Output != nil || res.NeedsInput || res.Duration != 0 {
		t.Fatalf("res = %+v, want zero value", res)
	}
	if inner.callCount() != 0 {
		t.Fatalf("inner called %d times, want 0 (FaultTimeout must not call Inner)", inner.callCount())
	}
	if len(emitted) != 1 || emitted[0].Kind != FaultTimeout {
		t.Fatalf("emitted = %+v, want one FaultTimeout event", emitted)
	}
}

func TestFaultTimeoutHonorsParamDuration(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 1, Kind: FaultTimeout, Param: map[string]any{"duration": 20 * time.Millisecond}},
		}},
	}

	start := time.Now()
	_, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolReadSource, Step: 1})
	elapsed := time.Since(start)

	if err == nil || err.Code != core.CodeTimeout {
		t.Fatalf("Invoke returned %v, want injected TIMEOUT", err)
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("Invoke returned after %v, want >= 20ms", elapsed)
	}
	if inner.callCount() != 0 {
		t.Fatalf("inner called %d times, want 0", inner.callCount())
	}
}

func TestFaultTimeoutUnblocksOnContextCancelBeforeDuration(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 1, Kind: FaultTimeout, Param: map[string]any{"duration": time.Minute}},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := p.Invoke(ctx, core.ToolCall{Tool: core.ToolReadSource, Step: 1})
	elapsed := time.Since(start)

	if err == nil || err.Code != core.CodeTimeout {
		t.Fatalf("Invoke returned %v, want injected TIMEOUT", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Invoke took %v, want release well before the 1-minute duration", elapsed)
	}
}

func TestFaultMalformedDoesNotCallInner(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 0, Kind: FaultMalformed},
		}},
	}

	_, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolReadSource, Step: 0})
	if err == nil || err.Code != core.CodeMalformed {
		t.Fatalf("Invoke returned %v, want injected MALFORMED_RESPONSE", err)
	}
	if !err.Injected() {
		t.Fatal("err.Injected() = false, want true")
	}
	if inner.callCount() != 0 {
		t.Fatalf("inner called %d times, want 0", inner.callCount())
	}
}

func TestFaultDuplicateCallsInnerExactlyTwiceAndReturnsSecond(t *testing.T) {
	inner := &fakeToolPort{invoke: func(n int, call core.ToolCall) (core.ToolResult, *core.RelayError) {
		return core.ToolResult{Tool: call.Tool, Duration: time.Duration(n)}, nil
	}}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 5, Kind: FaultDuplicate},
		}},
	}

	res, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolRecordFinding, Step: 5})
	if err != nil {
		t.Fatalf("Invoke returned %v, want nil", err)
	}
	if inner.callCount() != 2 {
		t.Fatalf("inner called %d times, want exactly 2", inner.callCount())
	}
	if res.Duration != 2 {
		t.Fatalf("res.Duration = %v, want 2 (the second call's marker)", res.Duration)
	}
}

func TestFaultDuplicateReturnsFirstErrorButStillCallsInnerTwice(t *testing.T) {
	wantErr := core.NewError(core.CodeTransport, "boom")
	inner := &fakeToolPort{invoke: func(n int, call core.ToolCall) (core.ToolResult, *core.RelayError) {
		if n == 1 {
			return core.ToolResult{}, wantErr
		}
		return core.ToolResult{Tool: call.Tool}, nil
	}}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 5, Kind: FaultDuplicate},
		}},
	}

	_, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolRecordFinding, Step: 5})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Invoke returned %v, want the first call's error", err)
	}
	if inner.callCount() != 2 {
		t.Fatalf("inner called %d times, want exactly 2 even though the first errored", inner.callCount())
	}
}

func TestFaultKillAfterEffectRunsInnerThenCallsOnKill(t *testing.T) {
	inner := &fakeToolPort{}
	killed := false
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 2, Kind: FaultKillAfterEffect},
		}},
		OnKill: func() { killed = true },
	}

	res, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolRecordFinding, Step: 2})
	if err != nil {
		t.Fatalf("Invoke returned %v, want nil", err)
	}
	if res.Tool != core.ToolRecordFinding {
		t.Fatalf("res.Tool = %q, want %q", res.Tool, core.ToolRecordFinding)
	}
	if inner.callCount() != 1 {
		t.Fatalf("inner called %d times, want 1", inner.callCount())
	}
	if !killed {
		t.Fatal("OnKill was not invoked")
	}
}

func TestFaultKillAfterEffectNilOnKillIsSafePassthrough(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 2, Kind: FaultKillAfterEffect},
		}},
	}

	if _, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolRecordFinding, Step: 2}); err != nil {
		t.Fatalf("Invoke returned %v, want nil", err)
	}
	if inner.callCount() != 1 {
		t.Fatalf("inner called %d times, want 1", inner.callCount())
	}
}

func TestFaultPermissionDeniedIsTerminalAndDoesNotCallInner(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 0, Kind: FaultPermissionDenied},
		}},
	}

	_, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolRequestReview, Step: 0})
	if err == nil || err.Code != core.CodePermissionDenied {
		t.Fatalf("Invoke returned %v, want injected PERMISSION_DENIED", err)
	}
	if !err.Terminal() {
		t.Fatal("err.Terminal() = false, want true")
	}
	if !err.Injected() {
		t.Fatal("err.Injected() = false, want true")
	}
	if inner.callCount() != 0 {
		t.Fatalf("inner called %d times, want 0", inner.callCount())
	}
}

func TestFaultHumanReviewReturnsNeedsInput(t *testing.T) {
	inner := &fakeToolPort{}
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 4, Kind: FaultHumanReview},
		}},
	}

	res, err := p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolRequestReview, Step: 4})
	if err != nil {
		t.Fatalf("Invoke returned %v, want nil", err)
	}
	if !res.NeedsInput {
		t.Fatal("res.NeedsInput = false, want true")
	}
	if res.Tool != core.ToolRequestReview {
		t.Fatalf("res.Tool = %q, want %q", res.Tool, core.ToolRequestReview)
	}
	if inner.callCount() != 0 {
		t.Fatalf("inner called %d times, want 0", inner.callCount())
	}
}

func TestFaultEventEmittedForEveryMatchedFault(t *testing.T) {
	inner := &fakeToolPort{}
	var mu sync.Mutex
	var emitted []FaultEvent
	p := &FaultyToolPort{
		Inner: inner,
		Plan: FaultPlan{Faults: []FaultSpec{
			{Step: 0, Kind: FaultPermissionDenied},
		}},
		Emit: func(e FaultEvent) {
			mu.Lock()
			emitted = append(emitted, e)
			mu.Unlock()
		},
	}

	_, _ = p.Invoke(context.Background(), core.ToolCall{Tool: core.ToolRequestReview, Step: 0})

	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 1 {
		t.Fatalf("emitted %d events, want 1", len(emitted))
	}
	want := FaultEvent{Step: 0, Tool: core.ToolRequestReview, Kind: FaultPermissionDenied}
	if emitted[0] != want {
		t.Fatalf("emitted[0] = %+v, want %+v", emitted[0], want)
	}
}
