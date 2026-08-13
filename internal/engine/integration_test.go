package engine_test

// End-to-end integration of the whole vertical slice: the real engine driving
// the real SQLite store and the real MCP server (over the in-memory transport)
// against the real synthetic corpus, with deterministic fault injection for each
// of the six required failure scenarios.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/diegoaleyvag/relay/internal/clock"
	"github.com/diegoaleyvag/relay/internal/core"
	"github.com/diegoaleyvag/relay/internal/corpus"
	"github.com/diegoaleyvag/relay/internal/engine"
	"github.com/diegoaleyvag/relay/internal/faults"
	"github.com/diegoaleyvag/relay/internal/mcp"
	"github.com/diegoaleyvag/relay/internal/planner"
	"github.com/diegoaleyvag/relay/internal/store/sqlite"
)

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newPort(t *testing.T) core.ToolPort {
	t.Helper()
	crp, err := corpus.Load()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	port, closeFn, err := mcp.InMemory(context.Background(), crp)
	if err != nil {
		t.Fatalf("mcp in-memory: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	return port
}

func fastCfg() engine.Config {
	c := engine.DefaultConfig()
	c.Backoff = core.Backoff{Base: time.Millisecond, Factor: 2, Max: 5 * time.Millisecond, Jitter: false}
	return c
}

func corpusCount(t *testing.T) int {
	t.Helper()
	crp, err := corpus.Load()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	srcs, _, err := crp.List("", 100, "")
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	return len(srcs)
}

func newEngine(repo core.Repository, tools core.ToolPort, cfg engine.Config) *engine.Engine {
	return engine.New(engine.Params{
		Repo: repo, Tools: tools, Planner: planner.New(), Clock: clock.SystemClock{}, Config: cfg,
	})
}

func run(seed uint64, requireReview bool) core.RunState {
	return core.NewRun(core.RunID("run"), seed, requireReview, time.Time{}, time.Now())
}

func countFindings(t *testing.T, store *sqlite.Store, id core.RunID) int {
	t.Helper()
	fs, err := store.Findings(context.Background(), id)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	// Exactly-once invariant: at most one finding per source.
	seen := map[string]bool{}
	for _, f := range fs {
		if seen[f.SourceID] {
			t.Fatalf("duplicate finding for source %s", f.SourceID)
		}
		seen[f.SourceID] = true
	}
	return len(fs)
}

func hasEventKind(t *testing.T, store *sqlite.Store, id core.RunID, kind core.EventKind) bool {
	t.Helper()
	evs, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// --- Scenario 0: happy path --------------------------------------------------

func TestIntegrationHappyPath(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	tools := &faults.FaultyToolPort{Inner: newPort(t)}
	e := newEngine(store, tools, fastCfg())

	final, err := e.StartRun(ctx, run(1, false))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded, got %v (%v)", final.Phase, final.LastError)
	}
	if got, want := countFindings(t, store, "run"), corpusCount(t); got != want {
		t.Fatalf("expected %d findings, got %d", want, got)
	}
}

// --- Scenario 1: tool timeout (retry then success) ---------------------------

func TestIntegrationTimeoutRetrySucceeds(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	plan := faults.FaultPlan{Faults: []faults.FaultSpec{
		{Step: int(core.StepRead(0)), Tool: core.ToolReadSource, Kind: faults.FaultTimeout, Times: 2},
	}}
	tools := &faults.FaultyToolPort{Inner: newPort(t), Plan: plan}
	cfg := fastCfg()
	cfg.MaxAttempts = 5
	cfg.PerCallTimeout = 100 * time.Millisecond
	e := newEngine(store, tools, cfg)

	final, err := e.StartRun(ctx, run(1, false))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded after timeouts, got %v", final.Phase)
	}
	if final.RetriesUsed < 2 {
		t.Fatalf("expected >=2 retries, got %d", final.RetriesUsed)
	}
	if got, want := countFindings(t, store, "run"), corpusCount(t); got != want {
		t.Fatalf("expected %d findings, got %d", want, got)
	}
}

// --- Scenario 2: malformed response (skip + degraded success) ----------------

func TestIntegrationMalformedSkips(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	plan := faults.FaultPlan{Faults: []faults.FaultSpec{
		{Step: int(core.StepRead(0)), Tool: core.ToolReadSource, Kind: faults.FaultMalformed},
	}}
	tools := &faults.FaultyToolPort{Inner: newPort(t), Plan: plan}
	e := newEngine(store, tools, fastCfg())

	final, err := e.StartRun(ctx, run(1, false))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected degraded success, got %v", final.Phase)
	}
	if len(final.Skipped) != 1 {
		t.Fatalf("expected 1 skipped source, got %d", len(final.Skipped))
	}
	if got, want := countFindings(t, store, "run"), corpusCount(t)-1; got != want {
		t.Fatalf("expected %d findings (one skipped), got %d", want, got)
	}
	if !hasEventKind(t, store, "run", core.EventSourceSkipped) {
		t.Fatal("expected a source_skipped event")
	}
}

// --- Scenario 3: duplicate delivery (exactly-once) ---------------------------

func TestIntegrationDuplicateDelivery(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	plan := faults.FaultPlan{Faults: []faults.FaultSpec{
		{Step: int(core.StepRecord(0)), Tool: core.ToolRecordFinding, Kind: faults.FaultDuplicate},
	}}
	tools := &faults.FaultyToolPort{Inner: newPort(t), Plan: plan}
	e := newEngine(store, tools, fastCfg())

	final, err := e.StartRun(ctx, run(1, false))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded, got %v", final.Phase)
	}
	if got, want := countFindings(t, store, "run"), corpusCount(t); got != want {
		t.Fatalf("expected %d findings with no duplicates, got %d", want, got)
	}
	if !hasEventKind(t, store, "run", core.EventDuplicateSuppress) {
		t.Fatal("expected a duplicate_suppressed event")
	}
}

// --- Scenario 4: process kill after side effect, before checkpoint -----------

// crashStore wraps a real Repository and fails the first finding-confirm commit,
// simulating a process kill after the tool executed but before the durable
// checkpoint landed.
type crashStore struct {
	core.Repository
	crashed bool
}

func (c *crashStore) Commit(ctx context.Context, b core.CommitBundle) error {
	if b.Finding != nil && !c.crashed {
		c.crashed = true
		return context.Canceled // any error; simulates the crash
	}
	return c.Repository.Commit(ctx, b)
}

func TestIntegrationKillAfterEffectResumesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	port := newPort(t)

	// First "process": crashes at the first finding confirm.
	crashing := &crashStore{Repository: store}
	e1 := newEngine(crashing, &faults.FaultyToolPort{Inner: port}, fastCfg())
	if _, err := e1.StartRun(ctx, run(1, false)); err == nil {
		t.Fatal("expected the simulated crash to surface as an error")
	}
	if n := countFindings(t, store, "run"); n != 0 {
		t.Fatalf("expected 0 durable findings before resume, got %d", n)
	}

	// Second "process": resumes from the durable checkpoint.
	e2 := newEngine(store, &faults.FaultyToolPort{Inner: port}, fastCfg())
	final, err := e2.Run(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded after resume, got %v", final.Phase)
	}
	if got, want := countFindings(t, store, "run"), corpusCount(t); got != want {
		t.Fatalf("expected exactly %d findings after resume (no duplication), got %d", want, got)
	}
}

// --- Scenario 5: permission denial (fail closed, partials preserved) ---------

func TestIntegrationPermissionDenied(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	// Deny the third read, so the first two sources are already recorded.
	plan := faults.FaultPlan{Faults: []faults.FaultSpec{
		{Step: int(core.StepRead(2)), Tool: core.ToolReadSource, Kind: faults.FaultPermissionDenied},
	}}
	tools := &faults.FaultyToolPort{Inner: newPort(t), Plan: plan}
	e := newEngine(store, tools, fastCfg())

	final, err := e.StartRun(ctx, run(1, false))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseFailed || final.LastError == nil || final.LastError.Code != core.CodePermissionDenied {
		t.Fatalf("expected failed/PERMISSION_DENIED, got %v/%v", final.Phase, final.LastError)
	}
	if got := countFindings(t, store, "run"); got != 2 {
		t.Fatalf("expected 2 preserved findings, got %d", got)
	}
}

// --- Scenario 6: human review required ---------------------------------------

func TestIntegrationHumanReview(t *testing.T) {
	ctx := context.Background()

	// Approve path.
	store := openStore(t)
	e := newEngine(store, &faults.FaultyToolPort{Inner: newPort(t)}, fastCfg())
	parked, err := e.StartRun(ctx, run(1, true))
	if err != nil {
		t.Fatal(err)
	}
	if parked.Phase != core.PhaseAwaitingHuman {
		t.Fatalf("expected awaiting_human, got %v", parked.Phase)
	}
	if !hasEventKind(t, store, "run", core.EventHumanRequested) {
		t.Fatal("expected a human_requested event")
	}
	final, err := e.ResolveHuman(ctx, "run", core.DecisionApprove)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded after approval, got %v", final.Phase)
	}

	// Reject path (separate DB).
	store2 := openStore(t)
	e2 := newEngine(store2, &faults.FaultyToolPort{Inner: newPort(t)}, fastCfg())
	if _, err := e2.StartRun(ctx, run(1, true)); err != nil {
		t.Fatal(err)
	}
	rej, err := e2.ResolveHuman(ctx, "run", core.DecisionReject)
	if err != nil {
		t.Fatal(err)
	}
	if rej.Phase != core.PhaseFailed {
		t.Fatalf("expected failed after rejection, got %v", rej.Phase)
	}
}
