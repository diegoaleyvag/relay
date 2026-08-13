package mcp

import (
	"encoding/json"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// decodeStructured turns a CallToolResult.StructuredContent value into a
// canonical json.RawMessage.
//
// StructuredContent is typed `any` by the SDK: on the client side it is
// whatever encoding/json produced when decoding the wire "structuredContent"
// field into an `any` — typically nil (absent), map[string]any, []any, a
// string, a number (float64), or a bool. decodeStructured never panics on any
// such value: json.Marshal on the plain-data shapes produced by
// encoding/json's own decoder cannot fail, but this function still surfaces
// any error rather than assume that, so callers (and the fuzz target below)
// get a typed error instead of a crash for any input we haven't anticipated.
//
// A nil value (no structured content on the wire) decodes to the JSON literal
// null rather than an error: callers that don't care about structured output
// (e.g. a NeedsInput or plain-text result) should not have to special-case
// "no structured content" as a failure.
func decodeStructured(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	// Defensive fast path: some callers (tests, or future SDK versions) may
	// hand us an already-encoded json.RawMessage. Pass it through unchanged
	// rather than re-marshaling (which would double-encode a string).
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return json.RawMessage("null"), nil
		}
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal structured content: %w", err)
	}
	return json.RawMessage(b), nil
}

// ParseToolResponse is the fuzz entry point for this package's response
// parsing path: it decodes raw bytes as an MCP CallToolResult (exercising the
// SDK's own CallToolResult.UnmarshalJSON) and then extracts the structured
// content via decodeStructured, exactly as client.go does after a real
// CallTool round trip.
//
// For any input, ParseToolResponse either returns a valid json.RawMessage and
// a nil error, or a nil json.RawMessage and a non-nil error. It never panics:
// that invariant is what FuzzParseToolResponse (parse_fuzz_test.go) checks
// across arbitrary byte sequences.
func ParseToolResponse(data []byte) (json.RawMessage, error) {
	var res mcpsdk.CallToolResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("mcp: parse tool response: %w", err)
	}
	return decodeStructured(res.StructuredContent)
}
