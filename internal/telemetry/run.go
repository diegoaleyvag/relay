package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/diegoaleyvag/relay/internal/core"
)

// StartRunSpan opens the relay.run root span for a run and returns a context
// carrying it plus an end function. The end function stamps the final status,
// retry count and (if any) terminal error code, then closes the span — so a run
// correlates end-to-end in a trace without any payload ever being recorded.
//
// scenario is the demo scenario label (may be empty for an unlabelled run).
func StartRunSpan(ctx context.Context, tr trace.Tracer, run core.RunState, scenario string) (context.Context, func(core.RunState)) {
	ctx, span := tr.Start(ctx, "relay.run", trace.WithAttributes(
		attrRunID(run.ID),
		attrSeed(run.Seed),
		attrScenario(scenario),
	))
	return ctx, func(final core.RunState) {
		span.SetAttributes(
			attrStatus(final.Phase),
			attrRetries(final.RetriesUsed),
		)
		if final.LastError != nil {
			span.SetAttributes(attrErrorCode(final.LastError.Code))
			span.SetStatus(codes.Error, string(final.LastError.Code))
		} else if final.Phase == core.PhaseSucceeded {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// RecordTransition adds a redacted relay.transition event to the span in ctx,
// if any. It is safe to call when no span is recording (it becomes a no-op).
func RecordTransition(ctx context.Context, from, to core.Phase, action core.ActionKind, tool core.ToolName) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.AddEvent("relay.transition", trace.WithAttributes(
		attrFromState(from),
		attrToState(to),
		attrActionType(action),
		attrTool(tool),
	))
}
