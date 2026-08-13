// Package telemetry is the OpenTelemetry adapter. Its defining property is
// redaction by construction: the ONLY way to attach a span attribute is through
// the typed constructors in this file, each of which pairs an allowlisted key
// with a value derived from metadata (ids, types, counts, codes, durations,
// hashes). There is deliberately no SetString(key, rawValue) escape hatch, so
// source content, tool arguments, finding text and credentials are structurally
// unable to reach a span.
package telemetry

import (
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/diegoaleyvag/relay/internal/core"
)

// Allowlisted attribute keys. These are the only keys this package ever emits.
const (
	KeyRunID      = "relay.run.id"
	KeyScenario   = "relay.scenario"
	KeySeed       = "relay.seed"
	KeyStatus     = "relay.run.status"
	KeyRetries    = "relay.retries_used"
	KeyActionType = "relay.action.type"
	KeyTool       = "relay.tool.name"
	KeyStep       = "relay.plan.step"
	KeyAttempt    = "relay.attempt"
	KeyOutcome    = "relay.outcome"
	KeyErrorCode  = "relay.error.code"
	KeyDurationMs = "relay.duration_ms"
	KeyInjected   = "relay.fault.injected"
	KeyFromState  = "relay.from_state"
	KeyToState    = "relay.to_state"
	KeyKeyHash    = "relay.idempotency.key_hash"
)

// Allowlist is the closed set of attribute keys this package may emit. Tests
// assert that every emitted attribute key is a member.
var Allowlist = map[string]bool{
	KeyRunID: true, KeyScenario: true, KeySeed: true, KeyStatus: true,
	KeyRetries: true, KeyActionType: true, KeyTool: true, KeyStep: true,
	KeyAttempt: true, KeyOutcome: true, KeyErrorCode: true, KeyDurationMs: true,
	KeyInjected: true, KeyFromState: true, KeyToState: true, KeyKeyHash: true,
}

func attrRunID(id core.RunID) attribute.KeyValue { return attribute.String(KeyRunID, string(id)) }
func attrScenario(s string) attribute.KeyValue   { return attribute.String(KeyScenario, s) }
func attrSeed(seed uint64) attribute.KeyValue    { return attribute.Int64(KeySeed, int64(seed)) }
func attrStatus(p core.Phase) attribute.KeyValue { return attribute.String(KeyStatus, string(p)) }
func attrRetries(n int) attribute.KeyValue       { return attribute.Int(KeyRetries, n) }
func attrActionType(k core.ActionKind) attribute.KeyValue {
	return attribute.String(KeyActionType, string(k))
}
func attrTool(t core.ToolName) attribute.KeyValue { return attribute.String(KeyTool, string(t)) }
func attrStep(step int) attribute.KeyValue        { return attribute.Int(KeyStep, step) }
func attrAttempt(n int) attribute.KeyValue        { return attribute.Int(KeyAttempt, n) }
func attrOutcome(o string) attribute.KeyValue     { return attribute.String(KeyOutcome, o) }
func attrErrorCode(c core.ErrorCode) attribute.KeyValue {
	return attribute.String(KeyErrorCode, string(c))
}
func attrDuration(d time.Duration) attribute.KeyValue {
	return attribute.Int64(KeyDurationMs, d.Milliseconds())
}
func attrInjected(b bool) attribute.KeyValue { return attribute.Bool(KeyInjected, b) }
func attrFromState(p core.Phase) attribute.KeyValue {
	return attribute.String(KeyFromState, string(p))
}
func attrToState(p core.Phase) attribute.KeyValue { return attribute.String(KeyToState, string(p)) }

// attrKeyHash records the shape of an idempotency key without revealing it.
func attrKeyHash(k core.IdempotencyKey) attribute.KeyValue {
	if k == "" {
		return attribute.String(KeyKeyHash, "")
	}
	return attribute.String(KeyKeyHash, core.Fingerprint([]byte(k)))
}
