package core

import (
	"encoding/json"
	"time"
)

// Redacted is machine-readable evidence that has already passed through a
// Redactor. It is safe to persist and display. The reducer only ever builds
// evidence from metadata (ids, types, counts, codes) — never source content.
type Redacted json.RawMessage

// MarshalJSON emits the raw redacted bytes (or null when empty).
func (r Redacted) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// EventKind names a point in a run's redacted history.
type EventKind string

const (
	EventRunCreated        EventKind = "run_created"
	EventStarted           EventKind = "started"
	EventActionChosen      EventKind = "action_chosen"
	EventToolStarted       EventKind = "tool_started"
	EventToolSucceeded     EventKind = "tool_succeeded"
	EventToolFailed        EventKind = "tool_failed"
	EventBackoffScheduled  EventKind = "backoff_scheduled"
	EventSideEffectIntent  EventKind = "side_effect_intent"
	EventSideEffectConfirm EventKind = "side_effect_confirmed"
	EventDuplicateSuppress EventKind = "duplicate_suppressed"
	EventSourceSkipped     EventKind = "source_skipped"
	EventHumanRequested    EventKind = "human_requested"
	EventHumanResolved     EventKind = "human_resolved"
	EventResumed           EventKind = "resumed"
	EventRunSucceeded      EventKind = "run_succeeded"
	EventRunFailed         EventKind = "run_failed"
	EventRunCancelled      EventKind = "run_cancelled"
	EventIllegalTransition EventKind = "illegal_transition"
)

// Event is one entry in a run's append-only, redacted history.
type Event struct {
	RunID    RunID     `json:"run_id"`
	Seq      int64     `json:"seq"`     // monotonic per run
	Version  Version   `json:"version"` // state version at emission
	Kind     EventKind `json:"kind"`
	Summary  string    `json:"summary"`            // redacted, human-facing
	Evidence Redacted  `json:"evidence,omitempty"` // redacted machine-readable payload
	At       time.Time `json:"at"`
}

// Redactor scrubs machine-readable evidence before it becomes an Event or a
// telemetry attribute. Redaction is a domain policy: it lives here, not in the
// adapters.
type Redactor interface {
	Redact(kind EventKind, raw json.RawMessage) Redacted
}

// NopRedactor passes evidence through unchanged. Use it only where the caller
// guarantees the evidence is already metadata-only.
type NopRedactor struct{}

func (NopRedactor) Redact(_ EventKind, raw json.RawMessage) Redacted { return Redacted(raw) }

// LimitRedactor is a defense-in-depth redactor: it walks the JSON evidence and
// replaces any string longer than MaxString with a short fingerprint, and drops
// values it cannot parse. Because the reducer already emits only small metadata,
// this normally passes evidence through untouched.
type LimitRedactor struct {
	MaxString int // strings longer than this are fingerprinted (default 160)
}

func (lr LimitRedactor) Redact(_ EventKind, raw json.RawMessage) Redacted {
	max := lr.MaxString
	if max <= 0 {
		max = 160
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return Redacted(mustJSON(map[string]string{"redacted": "unparseable"}))
	}
	scrubbed := scrub(v, max)
	return Redacted(mustJSON(scrubbed))
}

func scrub(v any, max int) any {
	switch t := v.(type) {
	case string:
		if len(t) > max {
			return "sha256:" + Fingerprint([]byte(t))
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = scrub(val, max)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = scrub(val, max)
		}
		return out
	default:
		return v
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"redacted":"marshal_error"}`)
	}
	return b
}

// evidence marshals a small metadata struct into Redacted evidence. It never
// receives source content by construction.
func evidence(v any) Redacted {
	return Redacted(mustJSON(v))
}
