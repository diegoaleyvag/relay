package mcp

import (
	"encoding/json"
	"testing"
)

// FuzzParseToolResponse fuzzes ParseToolResponse (parse.go) over arbitrary
// bytes. The only invariant under test is: it must never panic, and it must
// always return either a valid json.RawMessage or a non-nil error — never
// both nil, and never an invalid-JSON RawMessage.
func FuzzParseToolResponse(f *testing.F) {
	seeds := []string{
		``,
		`not json at all`,
		`null`,
		`{}`,
		`[]`,
		`{"content":[{"type":"text","text":"hi"}]}`,
		`{"structuredContent":{"a":1,"b":[1,2,3]},"content":[{"type":"text","text":"{}"}]}`,
		`{"structuredContent":null}`,
		`{"structuredContent":[1,2,3]}`,
		`{"structuredContent":"just a string"}`,
		`{"structuredContent":42}`,
		`{"structuredContent":true}`,
		`{"isError":true,"content":[{"type":"text","text":"boom"}]}`,
		`{"content":null}`,
		`{"resultType":"input_required","inputRequests":{}}`,
		`{"structuredContent":{"nested":{"deep":{"deeper":[1,2,{"x":"y"}]}}}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		raw, err := ParseToolResponse(data)
		if err != nil {
			if raw != nil {
				t.Fatalf("expected nil json.RawMessage alongside a non-nil error, got %q (err: %v)", raw, err)
			}
			return
		}
		if raw == nil {
			t.Fatalf("expected a non-nil json.RawMessage when err is nil (input %q)", data)
		}
		if !json.Valid(raw) {
			t.Fatalf("decodeStructured produced invalid JSON for input %q: %s", data, raw)
		}
	})
}
