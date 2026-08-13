package mcp

import (
	"context"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/diegoaleyvag/relay/internal/core"
)

// defaultMaxResponseBytes caps a tool's structured response when
// MCPToolPort.MaxResponseBytes is left at its zero value.
const defaultMaxResponseBytes = 1 << 20

// MCPToolPort implements core.ToolPort over a connected MCP client session.
// It is the only adapter in this module that translates between the domain's
// tool-agnostic core.ToolCall/core.ToolResult shapes and the MCP wire
// protocol.
type MCPToolPort struct {
	// Session is the connected client session tool calls are dispatched
	// through. It must already be initialized (see Client.Connect / InMemory).
	Session *mcpsdk.ClientSession

	// MaxResponseBytes caps the size of a tool's structured response payload.
	// Zero means defaultMaxResponseBytes (1 MiB).
	MaxResponseBytes int

	// Now returns the current time. Overridable for deterministic tests; a
	// nil value defaults to time.Now.
	Now func() time.Time

	// Allow, if non-nil, is consulted before every call: a tool not present
	// (or present but false) is rejected with CodePermissionDenied WITHOUT
	// ever reaching the session. This is the fail-closed path — it does not
	// rely on the server also rejecting the name, though the server's
	// registered-tool set is itself an allowlist (see server.go).
	Allow map[core.ToolName]bool
}

func (p *MCPToolPort) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *MCPToolPort) maxResponseBytes() int {
	if p.MaxResponseBytes > 0 {
		return p.MaxResponseBytes
	}
	return defaultMaxResponseBytes
}

// Invoke calls the named tool over the MCP session and translates the result
// (or failure) into the domain's core.ToolResult / *core.RelayError shapes.
//
// Failure classification:
//   - Allow set and call.Tool not allowlisted: CodePermissionDenied,
//     terminal, WITHOUT calling the session (fail closed).
//   - ctx.Err() == context.DeadlineExceeded: CodeTimeout, retryable.
//   - ctx.Err() == context.Canceled: CodeCancelled, terminal.
//   - a transport-level error whose text names an unknown/unregistered tool
//     (see server.go's callTool: `unknown tool "<name>"`): CodeToolNotFound,
//     terminal.
//   - any other transport-level error: CodeTransport, retryable.
//   - a successful call whose result NeedsInput: no error, ToolResult with
//     NeedsInput set (used for the human-review elicitation path).
//   - a structured response over MaxResponseBytes: CodeResponseTooLarge,
//     terminal.
//   - a tool-level error (CallToolResult.IsError, i.e. the handler itself
//     returned a Go error — see tools.go's readSource): best-effort text
//     match maps a "not found"/"unknown" message to CodeToolNotFound,
//     otherwise CodeInternal. Both are terminal.
//
// Invoke always returns a non-nil core.ToolResult even on error, with Tool
// and Duration populated, so callers/telemetry can log the elapsed time of a
// failed call.
func (p *MCPToolPort) Invoke(ctx context.Context, call core.ToolCall) (core.ToolResult, *core.RelayError) {
	if p.Allow != nil && !p.Allow[call.Tool] {
		return core.ToolResult{Tool: call.Tool},
			core.NewError(core.CodePermissionDenied, "tool not allowlisted: "+string(call.Tool))
	}

	params := &mcpsdk.CallToolParams{
		Name:      string(call.Tool),
		Arguments: call.Input,
		Meta: mcpsdk.Meta{
			"relay/idemKey": string(call.Idem),
			"relay/step":    int(call.Step),
		},
	}

	t0 := p.now()
	res, err := p.Session.CallTool(ctx, params)
	dur := p.now().Sub(t0)

	if err != nil {
		return core.ToolResult{Tool: call.Tool, Duration: dur}, classifyTransportError(ctx, call.Tool, err)
	}

	if res.NeedsInput() {
		return core.ToolResult{Tool: call.Tool, NeedsInput: true, Duration: dur}, nil
	}

	raw, perr := decodeStructured(res.StructuredContent)
	if perr != nil {
		return core.ToolResult{Tool: call.Tool, Duration: dur},
			core.NewError(core.CodeMalformed, "malformed tool response").WithCause(perr)
	}

	if len(raw) > p.maxResponseBytes() {
		return core.ToolResult{Tool: call.Tool, Duration: dur},
			core.NewError(core.CodeResponseTooLarge, "tool response exceeds max bytes").
				WithDetail("bytes", strconv.Itoa(len(raw)))
	}

	if res.IsError {
		msg := toolErrorText(res)
		code := core.CodeInternal
		if looksLikeNotFound(msg) {
			code = core.CodeToolNotFound
		}
		return core.ToolResult{Tool: call.Tool, Duration: dur}, core.NewError(code, msg)
	}

	return core.ToolResult{Tool: call.Tool, Output: raw, Duration: dur}, nil
}

// classifyTransportError maps a transport/protocol-level error (CallTool
// itself returned err != nil, as opposed to a tool handler error reported via
// CallToolResult.IsError) into a *core.RelayError.
//
// The classification prefers ctx.Err() over inspecting err's text: the
// caller's context is the authoritative signal for "this was a timeout" vs
// "this was a cancellation", regardless of how the SDK happens to wrap the
// underlying error.
func classifyTransportError(ctx context.Context, tool core.ToolName, err error) *core.RelayError {
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return core.NewError(core.CodeTimeout, "tool call timed out").WithCause(err)
	case ctx.Err() == context.Canceled:
		return core.NewError(core.CodeCancelled, "tool call cancelled").WithCause(err)
	case looksLikeNotFound(err.Error()):
		return core.NewError(core.CodeToolNotFound, "tool not found: "+string(tool)).WithCause(err)
	default:
		return core.NewError(core.CodeTransport, "tool transport error").WithCause(err)
	}
}

// toolErrorText extracts the human-readable error message a tool handler set
// via CallToolResult.SetError, which the SDK surfaces as the first
// TextContent block (see the SDK's toolForErr wrapper). It falls back to a
// generic message if no text content is present.
func toolErrorText(res *mcpsdk.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return "tool call failed"
}

// looksLikeNotFound is a best-effort, case-insensitive text match used to
// classify both transport-level "unknown tool" errors (the exact message the
// SDK's server uses, see server.go's callTool: `unknown tool "<name>"`) and
// tool-level errors such as an unknown source id. It is intentionally
// permissive: false positives only change a CodeInternal into a
// CodeToolNotFound, and both are terminal, so misclassification here does not
// change retry behavior.
func looksLikeNotFound(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "not found") || strings.Contains(lower, "unknown tool")
}
