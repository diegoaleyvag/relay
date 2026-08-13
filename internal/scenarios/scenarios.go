// Package scenarios maps a named demo scenario to the deterministic fault plan
// (and review policy) that produces it. It is the bridge between the control
// room's "new run" form and the fault harness: each scenario is data, so the
// same six failure modes the tests exercise are reproducible from the UI.
package scenarios

import (
	"github.com/diegoaleyvag/relay/internal/core"
	"github.com/diegoaleyvag/relay/internal/faults"
)

// Scenario is a named, reproducible run configuration.
type Scenario struct {
	Name          string
	Label         string
	Description   string
	RequireReview bool
	Faults        []faults.FaultSpec
}

// Plan returns the scenario's fault plan.
func (s Scenario) Plan() faults.FaultPlan { return faults.FaultPlan{Faults: s.Faults} }

var all = []Scenario{
	{
		Name:        "happy",
		Label:       "Happy path",
		Description: "No faults injected; every source is read and recorded.",
	},
	{
		Name:        "timeout",
		Label:       "Tool timeout (recovers)",
		Description: "The first read times out twice, then a retry within budget succeeds.",
		Faults: []faults.FaultSpec{
			{Step: int(core.StepRead(0)), Tool: core.ToolReadSource, Kind: faults.FaultTimeout, Times: 2},
		},
	},
	{
		Name:        "malformed",
		Label:       "Malformed response (skips)",
		Description: "The first source returns an unparseable response and is skipped; the run finishes degraded.",
		Faults: []faults.FaultSpec{
			{Step: int(core.StepRead(0)), Tool: core.ToolReadSource, Kind: faults.FaultMalformed},
		},
	},
	{
		Name:        "duplicate",
		Label:       "Duplicate delivery",
		Description: "A finding is delivered twice; the idempotency key suppresses the duplicate.",
		Faults: []faults.FaultSpec{
			{Step: int(core.StepRecord(0)), Tool: core.ToolRecordFinding, Kind: faults.FaultDuplicate},
		},
	},
	{
		Name:        "permission",
		Label:       "Permission denied",
		Description: "A later tool call is denied; the run fails closed with earlier findings preserved.",
		Faults: []faults.FaultSpec{
			{Step: int(core.StepRead(2)), Tool: core.ToolReadSource, Kind: faults.FaultPermissionDenied},
		},
	},
	{
		Name:          "review",
		Label:         "Human review",
		Description:   "Policy requires human review; the run parks until a reviewer approves or rejects.",
		RequireReview: true,
	},
}

// All returns the available scenarios (a copy).
func All() []Scenario { return append([]Scenario(nil), all...) }

// Get resolves a scenario by name. An empty name resolves to the happy path.
func Get(name string) (Scenario, bool) {
	if name == "" {
		return all[0], true
	}
	for _, s := range all {
		if s.Name == name {
			return s, true
		}
	}
	return Scenario{}, false
}
