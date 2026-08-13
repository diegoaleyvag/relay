package core

import (
	"encoding/json"
	"testing"
)

func TestDeriveKeyStableAndDistinct(t *testing.T) {
	k1 := DeriveKey("run-a", 2, ToolRecordFinding)
	k2 := DeriveKey("run-a", 2, ToolRecordFinding)
	if k1 != k2 {
		t.Fatalf("same inputs must yield same key: %q vs %q", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(k1))
	}
	// Distinct by run, step and tool.
	if DeriveKey("run-b", 2, ToolRecordFinding) == k1 {
		t.Fatal("different run must change the key")
	}
	if DeriveKey("run-a", 3, ToolRecordFinding) == k1 {
		t.Fatal("different step must change the key")
	}
	if DeriveKey("run-a", 2, ToolRequestReview) == k1 {
		t.Fatal("different tool must change the key")
	}
}

func TestCanonicalJSONKeyOrderIndependent(t *testing.T) {
	a, err := CanonicalJSON(json.RawMessage(`{"b":1,"a":2,"c":{"z":1,"y":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalJSON(json.RawMessage(`{"c":{"y":2,"z":1},"a":2,"b":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("canonical forms differ:\n%s\n%s", a, b)
	}
}

func TestHashInputStable(t *testing.T) {
	h1 := HashInput(json.RawMessage(`{"a":1,"b":2}`))
	h2 := HashInput(json.RawMessage(`{"b":2,"a":1}`))
	if h1 != h2 {
		t.Fatalf("hash must be canonical-order independent: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h1))
	}
}

func TestFingerprintLength(t *testing.T) {
	if got := Fingerprint([]byte("hello")); len(got) != 12 {
		t.Fatalf("expected 12-char fingerprint, got %q", got)
	}
}
