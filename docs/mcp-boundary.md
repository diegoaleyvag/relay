# MCP boundary and protocol version

Relay talks to its four research tools across a real Model Context Protocol
boundary, using the official Go MCP SDK.

- **SDK**: `github.com/modelcontextprotocol/go-sdk` **v1.7.0** (Apache-2.0),
  requires Go ≥ 1.25.
- **Protocol**: the SDK's latest supported MCP protocol version is
  **`2026-07-28`**; it also negotiates down to `2025-11-25`, `2025-06-18`,
  `2025-03-26` and `2024-11-05`. Relay pins the SDK, not a protocol string — the
  SDK negotiates the effective version per session.

## The four tools

Defined once as domain payloads in
[`internal/core/payloads.go`](../internal/core/payloads.go) and registered with
the SDK's typed `mcp.AddTool` in [`internal/mcp`](../internal/mcp):

| Tool | Kind | Notes |
|------|------|-------|
| `list_sources` | read (`ReadOnlyHint`) | corpus metadata only, sorted by id, cursor-paginated |
| `read_source` | read (`ReadOnlyHint`) | `io.LimitReader`-capped content; `SOURCE_NOT_FOUND`/`SOURCE_TOO_LARGE` |
| `record_finding` | side effect (`IdempotentHint`) | dedupes on `idempotency_key`; deterministic `finding_id` |
| `request_human_review` | side effect (`IdempotentHint`) | escalates to an explicit review state |

The **registered set is the allowlist**: only these four names exist and there
is no catch-all handler, so any other tool name fails closed. The client-side
adapter (`MCPToolPort`) additionally holds an independent allowlist and rejects a
disallowed tool *before* issuing a `CallTool`.

## The port

The domain defines a transport-agnostic `ToolPort` in
[`internal/core/ports.go`](../internal/core/ports.go). The MCP adapter
(`internal/mcp/client.go`) implements it over an `*mcp.ClientSession`, translating
a domain `Action` → an MCP `CallTool` → a structured `ToolResult` or a classified
`RelayError` (timeout, transport, malformed, oversized, permission, tool-not-found).

## Transports

- **In-memory** (`mcp.NewInMemoryTransports`) — deterministic, socket-free wiring
  used by the integration tests and by the single-process demo.
- **stdio** (`mcp.StdioTransport`) — the `cmd/relay-tools` server binary.
- **Streamable HTTP** (`mcp.NewStreamableHTTPHandler`) is available; when used it
  must be wrapped with `http.NewCrossOriginProtection().Handler(...)` because the
  SDK's cross-origin protection is off by default before v1.8.0.

## Idempotency across the boundary

`record_finding` and `request_human_review` carry an explicit `idempotency_key`
field (not only transport metadata), so the key round-trips regardless of `_meta`
propagation and the receiver can dedupe reliably. The MCP server derives its
`finding_id`/`review_id` deterministically from the key, so a re-execution after
a restart returns the identical id.
