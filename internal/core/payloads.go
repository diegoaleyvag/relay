package core

// The four tools' input/output payloads. They are plain data with json and
// jsonschema struct tags. The jsonschema tags are inert strings here; the MCP
// adapter feeds them to the SDK, which infers each tool's schema. Defining them
// in the domain lets the planner build inputs, the reducer read them, and the
// MCP adapter register them from a single shared shape.

// ListSourcesInput filters and paginates the corpus listing.
type ListSourcesInput struct {
	Tag    string `json:"tag,omitempty"    jsonschema:"filter sources by tag"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"maximum results (1-100)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"opaque pagination cursor"`
}

// ListSourcesOutput returns source metadata only — never content.
type ListSourcesOutput struct {
	Sources    []SourceRef `json:"sources"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ReadSourceInput selects one source and optionally caps the returned bytes.
type ReadSourceInput struct {
	ID       string `json:"id"                  jsonschema:"source id"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"cap on returned content bytes"`
}

// ReadSourceOutput carries the (size-limited) source content.
type ReadSourceOutput struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	MediaType string `json:"media_type"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// RecordFindingInput is a side-effecting write. IdempotencyKey is an explicit
// field (not only transport metadata) so it round-trips reliably and the
// receiver can dedupe.
type RecordFindingInput struct {
	IdempotencyKey string  `json:"idempotency_key" jsonschema:"idempotency key for exactly-once recording"`
	SourceID       string  `json:"source_id"       jsonschema:"the source this finding is about"`
	Claim          string  `json:"claim"           jsonschema:"the finding claim"`
	Evidence       string  `json:"evidence,omitempty" jsonschema:"supporting evidence"`
	Confidence     float64 `json:"confidence"      jsonschema:"confidence in [0,1]"`
}

// RecordFindingOutput reports the recorded (or deduplicated) finding.
type RecordFindingOutput struct {
	FindingID string `json:"finding_id"`
	Recorded  bool   `json:"recorded"`
	Duplicate bool   `json:"duplicate"`
}

// RequestHumanReviewInput escalates a run to an explicit human-review state.
type RequestHumanReviewInput struct {
	IdempotencyKey string `json:"idempotency_key" jsonschema:"idempotency key for exactly-once escalation"`
	FindingID      string `json:"finding_id,omitempty"`
	Reason         string `json:"reason"   jsonschema:"why human review is needed"`
	Severity       string `json:"severity" jsonschema:"low|medium|high"`
}

// RequestHumanReviewOutput reports the created review request.
type RequestHumanReviewOutput struct {
	ReviewID string `json:"review_id"`
	State    string `json:"state"` // "awaiting_human_review"
}
