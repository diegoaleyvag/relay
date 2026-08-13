package core

import "encoding/json"

// ToolInput is a tool-specific JSON argument object. The planner marshals it;
// the MCP adapter validates it against the tool's inferred schema.
type ToolInput = json.RawMessage

// IdempotencyKey deduplicates a side effect across retries and restarts. It is
// derived deterministically (see DeriveKey) and is never random.
type IdempotencyKey string

// Action is the planner's chosen next step. For ActionCallTool it names the tool
// and carries its input; for side-effecting tools Idem is set to the derived
// idempotency key.
type Action struct {
	Kind  ActionKind
	Tool  ToolName
	Step  StepIndex
	Input ToolInput
	Idem  IdempotencyKey
}

// CallTool builds a tool-call action, deriving the idempotency key for
// side-effecting tools from (runID, step, tool, input).
func CallTool(runID RunID, step StepIndex, tool ToolName, input ToolInput) Action {
	a := Action{Kind: ActionCallTool, Tool: tool, Step: step, Input: input}
	if tool.SideEffecting() {
		a.Idem = DeriveKey(runID, step, tool)
	}
	return a
}

// Complete is the terminal "plan finished successfully" action.
func Complete() Action { return Action{Kind: ActionComplete} }

// Fail is the terminal "plan cannot continue" action.
func Fail() Action { return Action{Kind: ActionFail} }
