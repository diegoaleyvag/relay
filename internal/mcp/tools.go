package mcp

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/diegoaleyvag/relay/internal/core"
)

// listSources implements the list_sources tool. It is read-only: it never
// mutates handlers state and carries no idempotency key.
func (h *handlers) listSources(
	_ context.Context, _ *mcpsdk.CallToolRequest, in core.ListSourcesInput,
) (*mcpsdk.CallToolResult, core.ListSourcesOutput, error) {
	refs, next, err := h.src.List(in.Tag, in.Limit, in.Cursor)
	if err != nil {
		return nil, core.ListSourcesOutput{}, err
	}
	return nil, core.ListSourcesOutput{Sources: refs, NextCursor: next}, nil
}

// readSource implements the read_source tool. It is read-only. A bad id is
// reported as a Go error; the SDK's generic AddTool wrapper packs that into a
// CallToolResult with IsError set (see the toolForErr wrapper in the SDK), so
// this never surfaces as a transport-level failure — only as a per-call
// tool error the client can inspect.
func (h *handlers) readSource(
	_ context.Context, _ *mcpsdk.CallToolRequest, in core.ReadSourceInput,
) (*mcpsdk.CallToolResult, core.ReadSourceOutput, error) {
	out, err := h.src.Read(in.ID, in.MaxBytes)
	if err != nil {
		return nil, core.ReadSourceOutput{}, err
	}
	return nil, out, nil
}

// recordFinding implements the record_finding tool: a side effect that must
// be exactly-once per idempotency_key.
//
// finding_id is DETERMINISTIC: it is derived solely from the idempotency key
// via "fnd-"+core.Fingerprint([]byte(key)), never from a counter or random
// source. That means the same idempotency_key always yields the same
// finding_id, whether this is the first call or a retried duplicate — which
// is exactly what lets a caller (or a resumed run) safely re-issue the same
// call and get back a stable answer instead of a second finding.
//
// The first call for a given key is recorded (Recorded=true, Duplicate=false)
// and stored in the in-memory ledger. Every subsequent call with the same key
// is recognized as a duplicate (Recorded=false, Duplicate=true) and produces
// no further effect: the ledger is only ever written once per key, guarded by
// h.mu.
func (h *handlers) recordFinding(
	_ context.Context, _ *mcpsdk.CallToolRequest, in core.RecordFindingInput,
) (*mcpsdk.CallToolResult, core.RecordFindingOutput, error) {
	findingID := "fnd-" + core.Fingerprint([]byte(in.IdempotencyKey))

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, seen := h.findings[in.IdempotencyKey]; seen {
		return nil, core.RecordFindingOutput{
			FindingID: findingID,
			Recorded:  false,
			Duplicate: true,
		}, nil
	}
	h.findings[in.IdempotencyKey] = findingID
	return nil, core.RecordFindingOutput{
		FindingID: findingID,
		Recorded:  true,
		Duplicate: false,
	}, nil
}

// requestHumanReview implements the request_human_review tool: a side effect
// that must be exactly-once per idempotency_key.
//
// review_id is DETERMINISTIC in the same way as finding_id:
// "rev-"+core.Fingerprint([]byte(key)). A repeated call with the same key
// returns the same review_id and the same awaiting_human_review state without
// creating a second ledger entry.
func (h *handlers) requestHumanReview(
	_ context.Context, _ *mcpsdk.CallToolRequest, in core.RequestHumanReviewInput,
) (*mcpsdk.CallToolResult, core.RequestHumanReviewOutput, error) {
	reviewID := "rev-" + core.Fingerprint([]byte(in.IdempotencyKey))

	h.mu.Lock()
	if _, seen := h.reviews[in.IdempotencyKey]; !seen {
		h.reviews[in.IdempotencyKey] = reviewID
	}
	h.mu.Unlock()

	return nil, core.RequestHumanReviewOutput{
		ReviewID: reviewID,
		State:    "awaiting_human_review",
	}, nil
}
