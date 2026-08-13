package telemetry

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/diegoaleyvag/relay/internal/core"
)

// recExporter records exported spans for inspection.
type recExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *recExporter) ExportSpans(_ context.Context, ss []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	e.spans = append(e.spans, ss...)
	e.mu.Unlock()
	return nil
}

func (e *recExporter) Shutdown(_ context.Context) error { return nil }

// secretPort returns a payload containing a marker that must NEVER reach a span.
type secretPort struct{}

const secretMarker = "SUPER_SECRET_SOURCE_TEXT"

func (secretPort) Invoke(_ context.Context, call core.ToolCall) (core.ToolResult, *core.RelayError) {
	return core.ToolResult{
		Tool:     call.Tool,
		Output:   []byte(`{"content":"` + secretMarker + `"}`),
		Duration: 5 * time.Millisecond,
	}, nil
}

func TestTelemetryOnlyAllowlistedAttributesAndNoLeak(t *testing.T) {
	rec := &recExporter{}
	p := NewWithExporter(rec)
	defer func() { _ = p.Shutdown(context.Background()) }()

	tt := &ToolTracer{Inner: secretPort{}, Tracer: p.Tracer}

	run := core.NewRun("run-x", 42, false, time.Time{}, time.Now())
	ctx, end := StartRunSpan(context.Background(), p.Tracer, run, "happy")
	RecordTransition(ctx, core.PhasePending, core.PhaseRunning, core.ActionWait, "")

	if _, rerr := tt.Invoke(ctx, core.ToolCall{Tool: core.ToolReadSource, Step: 1, Idem: "some-idem-key"}); rerr != nil {
		t.Fatalf("invoke: %v", rerr)
	}

	final := run
	final.Phase = core.PhaseSucceeded
	final.RetriesUsed = 1
	end(final)

	if len(rec.spans) < 2 {
		t.Fatalf("expected at least the tool_call and run spans, got %d", len(rec.spans))
	}

	sawToolOutcome := false
	for _, s := range rec.spans {
		for _, kv := range s.Attributes() {
			if !Allowlist[string(kv.Key)] {
				t.Fatalf("span %q emitted non-allowlisted attribute %q", s.Name(), kv.Key)
			}
			if strings.Contains(kv.Value.String(), secretMarker) {
				t.Fatalf("payload leaked into attribute %q: %q", kv.Key, kv.Value.String())
			}
			if s.Name() == "relay.tool_call" && string(kv.Key) == KeyOutcome && kv.Value.String() == "ok" {
				sawToolOutcome = true
			}
		}
		for _, ev := range s.Events() {
			for _, kv := range ev.Attributes {
				if !Allowlist[string(kv.Key)] {
					t.Fatalf("event %q emitted non-allowlisted attribute %q", ev.Name, kv.Key)
				}
				if strings.Contains(kv.Value.String(), secretMarker) {
					t.Fatalf("payload leaked into event attribute %q", kv.Key)
				}
			}
		}
	}
	if !sawToolOutcome {
		t.Fatal("expected the tool_call span to record an ok outcome")
	}
}

func TestProviderModes(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []Mode{ModeOff, ModeStdout} {
		var sb strings.Builder
		p, shutdown, err := NewProvider(ctx, Config{Mode: mode, Writer: &sb})
		if err != nil {
			t.Fatalf("mode %s: %v", mode, err)
		}
		_, end := StartRunSpan(ctx, p.Tracer, core.NewRun("r", 1, false, time.Time{}, time.Now()), string(mode))
		s := core.NewRun("r", 1, false, time.Time{}, time.Now())
		s.Phase = core.PhaseSucceeded
		end(s)
		if err := shutdown(ctx); err != nil {
			t.Fatalf("shutdown mode %s: %v", mode, err)
		}
		if mode == ModeStdout && sb.Len() == 0 {
			t.Fatal("stdout mode exported nothing")
		}
		if mode == ModeOff && sb.Len() != 0 {
			t.Fatal("off mode should export nothing")
		}
	}
}
