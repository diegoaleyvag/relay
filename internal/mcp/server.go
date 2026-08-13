// Package mcp is Relay's MCP adapter: it exposes the domain's four
// research tools (list_sources, read_source, record_finding,
// request_human_review) over the Model Context Protocol, and it implements
// core.ToolPort so the engine can drive a real (or in-memory) MCP session
// without knowing anything about the wire protocol.
//
// This package is the only place in the module that imports the MCP SDK. The
// domain core (internal/core) and the planner (internal/planner) are frozen
// and never import it; that boundary is enforced by `make boundary` and by
// the depguard rule in .golangci.yml.
//
// The registered tool set IS the allowlist: NewServer registers exactly the
// four tools above and nothing else, so an unknown tool name always fails
// closed at the server (see tools.go and client.go).
package mcp

import (
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/diegoaleyvag/relay/internal/core"
)

// Sources is the read-only corpus port that backs list_sources and
// read_source. It is defined in this package (rather than imported from the
// corpus package) so the MCP adapter can be built and tested in isolation;
// the corpus package satisfies this interface without either package
// depending on the other's internals.
type Sources interface {
	// List returns source metadata (never content) matching tag, up to limit
	// results, resuming from cursor. An empty tag matches every source. It
	// returns the page of sources, the opaque cursor for the next page (empty
	// when there is no more data), and any error encountered.
	List(tag string, limit int, cursor string) ([]core.SourceRef, string, error)

	// Read returns the content of the source identified by id, capped at
	// maxBytes (0 means no cap). It returns an error if id does not name a
	// known source.
	Read(id string, maxBytes int) (core.ReadSourceOutput, error)
}

// handlers holds everything the four tool handlers need: the read-only
// Sources port, and an in-memory idempotency ledger for the two
// side-effecting tools (record_finding, request_human_review).
//
// The ledger here is intentionally minimal: it exists so this adapter can be
// exercised end-to-end (including exactly-once semantics) without a durable
// store. The durable, restart-safe ledger lives behind core.Repository and is
// the engine's responsibility, not this adapter's; this map only needs to
// survive for the lifetime of one server process/connection.
type handlers struct {
	src Sources

	mu       sync.Mutex
	findings map[string]string // idempotency_key -> finding_id
	reviews  map[string]string // idempotency_key -> review_id
}

// NewServer builds an MCP server exposing exactly the four Relay research
// tools, backed by src for the two read-only tools. The returned server has
// not been connected to any transport yet; call Server.Run (or use InMemory
// for tests/embedding) to start serving.
func NewServer(src Sources) *mcpsdk.Server {
	h := &handlers{
		src:      src,
		findings: make(map[string]string),
		reviews:  make(map[string]string),
	}

	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:        "relay-tools",
		Version:     "0.1.0",
		Description: "Relay's synthetic research toolset: list/read a corpus, record findings, and escalate to human review.",
	}, nil)

	readOnly := &mcpsdk.ToolAnnotations{ReadOnlyHint: true}

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        string(core.ToolListSources),
		Description: "List corpus sources (metadata only, never content), optionally filtered by tag and paginated by cursor.",
		Annotations: readOnly,
	}, h.listSources)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        string(core.ToolReadSource),
		Description: "Read the (optionally size-capped) content of one corpus source by id.",
		Annotations: readOnly,
	}, h.readSource)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        string(core.ToolRecordFinding),
		Description: "Durably record a finding about a source. Side-effecting: exactly-once per idempotency_key.",
	}, h.recordFinding)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        string(core.ToolRequestReview),
		Description: "Escalate the run to human review. Side-effecting: exactly-once per idempotency_key.",
	}, h.requestHumanReview)

	return server
}
