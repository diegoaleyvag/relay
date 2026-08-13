package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/diegoaleyvag/relay/internal/core"
)

// newTestPort wires an in-memory server+client pair over fakeSources and
// registers a cleanup that closes it, so individual tests only deal with the
// resulting port.
func newTestPort(t *testing.T) *MCPToolPort {
	t.Helper()
	ctx := context.Background()
	port, closeFn, err := InMemory(ctx, newFakeSources())
	if err != nil {
		t.Fatalf("InMemory: %v", err)
	}
	t.Cleanup(func() {
		if err := closeFn(); err != nil {
			t.Fatalf("closeFn: %v", err)
		}
	})
	return port
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestListSourcesReturnsFakeSources(t *testing.T) {
	port := newTestPort(t)

	call := core.ToolCall{
		Tool:  core.ToolListSources,
		Input: mustJSON(t, core.ListSourcesInput{Limit: 10}),
	}
	res, rerr := port.Invoke(context.Background(), call)
	if rerr != nil {
		t.Fatalf("Invoke: %v", rerr)
	}
	if res.NeedsInput {
		t.Fatal("list_sources should not need input")
	}

	var out core.ListSourcesOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal output %s: %v", res.Output, err)
	}
	if len(out.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d: %+v", len(out.Sources), out.Sources)
	}
}

func TestReadSourceReturnsContentForKnownID(t *testing.T) {
	port := newTestPort(t)

	call := core.ToolCall{
		Tool:  core.ToolReadSource,
		Input: mustJSON(t, core.ReadSourceInput{ID: "s1"}),
	}
	res, rerr := port.Invoke(context.Background(), call)
	if rerr != nil {
		t.Fatalf("Invoke: %v", rerr)
	}

	var out core.ReadSourceOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal output %s: %v", res.Output, err)
	}
	if out.Content != "alpha" {
		t.Fatalf("expected content %q, got %q", "alpha", out.Content)
	}
	if out.ID != "s1" {
		t.Fatalf("expected id %q, got %q", "s1", out.ID)
	}
}

func TestReadSourceUnknownIDIsToolError(t *testing.T) {
	port := newTestPort(t)

	call := core.ToolCall{
		Tool:  core.ToolReadSource,
		Input: mustJSON(t, core.ReadSourceInput{ID: "does-not-exist"}),
	}
	_, rerr := port.Invoke(context.Background(), call)
	if rerr == nil {
		t.Fatal("expected an error for an unknown source id")
	}
	if !rerr.Terminal() {
		t.Fatalf("expected a terminal error class, got %v", rerr.Class)
	}
	if rerr.Code != core.CodeToolNotFound {
		t.Fatalf("expected CodeToolNotFound, got %v (%s)", rerr.Code, rerr.Message)
	}
}

// TestRecordFindingIsIdempotent is the core exactly-once assertion: the same
// idempotency_key must produce the same finding_id, and only the first call
// reports Recorded=true/Duplicate=false.
func TestRecordFindingIsIdempotent(t *testing.T) {
	port := newTestPort(t)
	ctx := context.Background()

	const key = "test-idem-key-1"
	call := core.ToolCall{
		Tool: core.ToolRecordFinding,
		Idem: core.IdempotencyKey(key),
		Input: mustJSON(t, core.RecordFindingInput{
			IdempotencyKey: key,
			SourceID:       "s1",
			Claim:          "source s1 says something interesting",
			Confidence:     0.9,
		}),
	}

	res1, rerr := port.Invoke(ctx, call)
	if rerr != nil {
		t.Fatalf("first Invoke: %v", rerr)
	}
	var out1 core.RecordFindingOutput
	if err := json.Unmarshal(res1.Output, &out1); err != nil {
		t.Fatalf("unmarshal first output %s: %v", res1.Output, err)
	}
	if !out1.Recorded || out1.Duplicate {
		t.Fatalf("expected first call Recorded=true, Duplicate=false, got %+v", out1)
	}
	wantID := "fnd-" + core.Fingerprint([]byte(key))
	if out1.FindingID != wantID {
		t.Fatalf("finding_id %q != deterministic fingerprint %q", out1.FindingID, wantID)
	}

	// Repeat with the identical idempotency key.
	res2, rerr := port.Invoke(ctx, call)
	if rerr != nil {
		t.Fatalf("second Invoke: %v", rerr)
	}
	var out2 core.RecordFindingOutput
	if err := json.Unmarshal(res2.Output, &out2); err != nil {
		t.Fatalf("unmarshal second output %s: %v", res2.Output, err)
	}
	if out2.Recorded || !out2.Duplicate {
		t.Fatalf("expected second call Recorded=false, Duplicate=true, got %+v", out2)
	}
	if out2.FindingID != out1.FindingID {
		t.Fatalf("finding_id changed across duplicate calls: %q vs %q", out1.FindingID, out2.FindingID)
	}
}

func TestRequestHumanReviewReturnsAwaitingState(t *testing.T) {
	port := newTestPort(t)
	ctx := context.Background()

	const key = "test-review-key-1"
	call := core.ToolCall{
		Tool: core.ToolRequestReview,
		Idem: core.IdempotencyKey(key),
		Input: mustJSON(t, core.RequestHumanReviewInput{
			IdempotencyKey: key,
			Reason:         "needs a second pair of eyes",
			Severity:       "medium",
		}),
	}

	res, rerr := port.Invoke(ctx, call)
	if rerr != nil {
		t.Fatalf("Invoke: %v", rerr)
	}
	var out core.RequestHumanReviewOutput
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal output %s: %v", res.Output, err)
	}
	if out.State != "awaiting_human_review" {
		t.Fatalf("expected state %q, got %q", "awaiting_human_review", out.State)
	}
	wantID := "rev-" + core.Fingerprint([]byte(key))
	if out.ReviewID != wantID {
		t.Fatalf("review_id %q != deterministic fingerprint %q", out.ReviewID, wantID)
	}

	// Repeat: same review_id, still awaiting_human_review, no crash from a
	// second ledger write.
	res2, rerr := port.Invoke(ctx, call)
	if rerr != nil {
		t.Fatalf("second Invoke: %v", rerr)
	}
	var out2 core.RequestHumanReviewOutput
	if err := json.Unmarshal(res2.Output, &out2); err != nil {
		t.Fatalf("unmarshal second output %s: %v", res2.Output, err)
	}
	if out2.ReviewID != out.ReviewID {
		t.Fatalf("review_id changed across repeated calls: %q vs %q", out.ReviewID, out2.ReviewID)
	}
}

// TestInvokeFailsClosedForNonAllowlistedTool exercises the client-side
// allowlist: when Allow is set, a tool missing from it is rejected before the
// session is ever called.
func TestInvokeFailsClosedForNonAllowlistedTool(t *testing.T) {
	port := newTestPort(t)
	port.Allow = map[core.ToolName]bool{core.ToolListSources: true}

	call := core.ToolCall{Tool: core.ToolRecordFinding, Input: json.RawMessage(`{}`)}
	_, rerr := port.Invoke(context.Background(), call)
	if rerr == nil {
		t.Fatal("expected an error for a tool missing from Allow")
	}
	if rerr.Code != core.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got %v", rerr.Code)
	}
	if !rerr.Terminal() {
		t.Fatalf("expected a terminal error class, got %v", rerr.Class)
	}
}

// TestInvokeMapsUnregisteredServerToolToTerminalNotFound calls a tool name
// the server never registered at all (as opposed to one merely missing from
// the client-side Allow map): the server itself fails closed, and Invoke must
// map that into a terminal RelayError.
func TestInvokeMapsUnregisteredServerToolToTerminalNotFound(t *testing.T) {
	port := newTestPort(t)

	call := core.ToolCall{Tool: core.ToolName("delete_everything"), Input: json.RawMessage(`{}`)}
	_, rerr := port.Invoke(context.Background(), call)
	if rerr == nil {
		t.Fatal("expected an error for a tool name the server never registered")
	}
	if !rerr.Terminal() {
		t.Fatalf("expected a terminal error class, got %v", rerr.Class)
	}
	if rerr.Code != core.CodeToolNotFound {
		t.Fatalf("expected CodeToolNotFound, got %v (%s)", rerr.Code, rerr.Message)
	}
}

// TestSessionCallToolErrorsForUnregisteredName is the raw-session variant of
// the assertion above: independent of MCPToolPort's mapping, the underlying
// ClientSession.CallTool call itself must error for a name the server never
// registered.
func TestSessionCallToolErrorsForUnregisteredName(t *testing.T) {
	port := newTestPort(t)

	_, err := port.Session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "no_such_tool"})
	if err == nil {
		t.Fatal("expected CallTool to error for an unregistered tool name")
	}
}
