package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/diegoaleyvag/relay/internal/core"
)

// ToolTracer is a core.ToolPort decorator that emits one relay.tool_call child
// span per invocation, carrying only allowlisted attributes. It composes with
// the fault harness (wrap the FaultyToolPort) and the MCP client adapter.
type ToolTracer struct {
	Inner  core.ToolPort
	Tracer trace.Tracer
}

var _ core.ToolPort = (*ToolTracer)(nil)

// Invoke traces the wrapped call. It records the tool, step, attempt and
// idempotency-key hash before the call, then the outcome, duration and (on
// failure) the machine-readable error code and whether the failure was injected.
func (t *ToolTracer) Invoke(ctx context.Context, call core.ToolCall) (core.ToolResult, *core.RelayError) {
	ctx, span := t.Tracer.Start(ctx, "relay.tool_call", trace.WithAttributes(
		attrTool(call.Tool),
		attrStep(int(call.Step)),
		attrAttempt(call.Attempt),
		attrKeyHash(call.Idem),
	))
	defer span.End()

	res, rerr := t.Inner.Invoke(ctx, call)
	span.SetAttributes(attrDuration(res.Duration))

	switch {
	case rerr != nil:
		span.SetAttributes(
			attrOutcome(rerr.Class.String()),
			attrErrorCode(rerr.Code),
			attrInjected(rerr.Injected()),
		)
		span.SetStatus(codes.Error, string(rerr.Code))
	case res.NeedsInput:
		span.SetAttributes(attrOutcome("needs_input"))
	default:
		span.SetAttributes(attrOutcome("ok"))
	}
	return res, rerr
}
