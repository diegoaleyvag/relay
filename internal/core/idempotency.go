package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// unitSep separates the components of an idempotency-key preimage. It is a byte
// that never appears in JSON text or the other components, so the concatenation
// is unambiguous.
const unitSep = 0x1f

// CanonicalJSON re-encodes arbitrary JSON into a canonical form: object keys are
// sorted and insignificant whitespace is removed. Logically identical inputs
// therefore hash identically. (encoding/json marshals map[string]any keys in
// sorted order, which is what gives us the canonical ordering.)
func CanonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// DeriveKey computes the deterministic idempotency key for a side effect:
//
//	hex(sha256(runID ␟ step ␟ tool))
//
// The key is a function of the logical operation identity — the run, the plan
// step and the tool — and deliberately excludes both the attempt number and the
// input. In the fixed deterministic plan a given (run, step, tool) always means
// the same side effect, so every retry of the step and every re-execution after
// a restart produce the same key. That input-independence is also why the key
// can be embedded in the tool input (as idempotency_key) without any
// self-reference. The full input is still recorded separately as an audit hash
// (see HashInput).
func DeriveKey(runID RunID, step StepIndex, tool ToolName) IdempotencyKey {
	h := sha256.New()
	h.Write([]byte(runID))
	h.Write([]byte{unitSep})
	h.Write([]byte(strconv.Itoa(int(step))))
	h.Write([]byte{unitSep})
	h.Write([]byte(tool))
	return IdempotencyKey(hex.EncodeToString(h.Sum(nil)))
}

// Fingerprint returns the first 12 hex characters of the SHA-256 of b. It is
// used to record the shape of a value (e.g. a redacted string, an idempotency
// key) in telemetry without revealing the value itself.
func Fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// HashInput returns the full hex SHA-256 of the canonicalized input, used as the
// transition/ledger request hash.
func HashInput(input json.RawMessage) string {
	canon, err := CanonicalJSON(input)
	if err != nil {
		canon = []byte(input)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}
