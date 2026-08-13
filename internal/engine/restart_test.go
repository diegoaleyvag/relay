package engine_test

// Process-level restart proof: a child process is killed (os.Exit) immediately
// after record_finding executes but before its confirm checkpoint; a fresh
// process then resumes from the durable SQLite state and completes with exactly
// one finding per source. This is the strongest form of the exactly-once
// guarantee — a real process boundary, not a simulated one.

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

const (
	crashEnv  = "RELAY_RESTART_CHILD"
	dbEnv     = "RELAY_RESTART_DB"
	crashExit = 7
)

// TestMain re-enters as the "crash child" when the environment asks it to;
// otherwise it runs the test suite normally.
func TestMain(m *testing.M) {
	if os.Getenv(crashEnv) == "1" {
		crashChild()
		return // unreachable: crashChild always exits
	}
	os.Exit(m.Run())
}

// crashChild starts a run wired to kill the process right after the first
// record_finding side effect executes. It never returns.
func crashChild() {
	dbPath := os.Getenv(dbEnv)
	store, err := sqlite.Open(dbPath)
	if err != nil {
		os.Exit(2)
	}
	crp, err := corpus.Load()
	if err != nil {
		os.Exit(3)
	}
	port, _, err := mcp.InMemory(context.Background(), crp)
	if err != nil {
		os.Exit(4)
	}
	plan := faults.FaultPlan{Faults: []faults.FaultSpec{
		{Step: int(core.StepRecord(0)), Tool: core.ToolRecordFinding, Kind: faults.FaultKillAfterEffect},
	}}
	killer := &faults.FaultyToolPort{
		Inner:  port,
		Plan:   plan,
		OnKill: func() { os.Exit(crashExit) }, // die after the effect, before confirm
	}
	e := engine.New(engine.Params{
		Repo: store, Tools: killer, Planner: planner.New(),
		Clock: clock.SystemClock{}, Config: engine.DefaultConfig(),
	})
	_, _ = e.StartRun(context.Background(), core.NewRun("restart-run", 7, false, time.Time{}, time.Now()))
	os.Exit(0) // reached only if the kill fault did not fire
}

func TestProcessRestartResumesExactlyOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "relay.db")

	// Phase 1: run the crash child in a real subprocess.
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), crashEnv+"=1", dbEnv+"="+dbPath)
	err := child.Run()

	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != crashExit {
		t.Fatalf("expected the child to crash with exit %d, got %v", crashExit, err)
	}

	// The durable store must show no confirmed finding yet.
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store.Close() }()

	before, err := store.Findings(context.Background(), "restart-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("expected 0 durable findings after the crash, got %d", len(before))
	}

	// Phase 2: a fresh process (this one) resumes from the durable checkpoint.
	crp, err := corpus.Load()
	if err != nil {
		t.Fatal(err)
	}
	port, closeFn, err := mcp.InMemory(context.Background(), crp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeFn() }()

	e := engine.New(engine.Params{
		Repo: store, Tools: &faults.FaultyToolPort{Inner: port}, Planner: planner.New(),
		Clock: clock.SystemClock{}, Config: engine.DefaultConfig(),
	})
	final, err := e.Run(context.Background(), "restart-run")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded after resume, got %v (%v)", final.Phase, final.LastError)
	}

	after, err := store.Findings(context.Background(), "restart-run")
	if err != nil {
		t.Fatal(err)
	}
	srcs, _, _ := crp.List("", 100, "")
	if len(after) != len(srcs) {
		t.Fatalf("expected exactly %d findings after resume, got %d", len(srcs), len(after))
	}
	seen := map[string]bool{}
	for _, f := range after {
		if seen[f.SourceID] {
			t.Fatalf("duplicate finding for source %s after restart", f.SourceID)
		}
		seen[f.SourceID] = true
	}
}
