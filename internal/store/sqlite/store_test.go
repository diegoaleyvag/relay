package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// newTestStore opens a fresh Store backed by a file in t.TempDir() and
// arranges for it to be closed when the test ends.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

// mustCommit builds a CommitBundle from its parts and commits it, failing the
// test on any error. The checkpoint always mirrors Next at the same version,
// matching the invariant the domain requires (checkpoint.Version ==
// runs.version after a successful Commit).
func mustCommit(t *testing.T, ctx context.Context, st *Store, b core.CommitBundle) {
	t.Helper()
	if err := st.Commit(ctx, b); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// bumpedState returns a copy of base advanced to the next version and phase,
// stamped with now. It is the "bump Version yourself" helper the task
// describes for building minimal CommitBundles by hand.
func bumpedState(base core.RunState, phase core.Phase, step core.StepIndex, now time.Time) core.RunState {
	next := base
	next.Version++
	next.Phase = phase
	next.Step = step
	next.UpdatedAt = now
	return next
}

func TestOpenMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")

	st1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	// Re-opening the same file re-runs migrate(); every statement in
	// schema.sql is CREATE TABLE/INDEX IF NOT EXISTS so this must succeed
	// without error and without disturbing existing data.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = st2.Close() }()

	// And a third time, without ever closing st2, to exercise two live
	// *sql.DB pairs against the same file concurrently.
	st3, err := Open(path)
	if err != nil {
		t.Fatalf("third Open (concurrent): %v", err)
	}
	defer func() { _ = st3.Close() }()
}

func TestCreateRunLoadStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	now := time.Now().UTC()
	run := core.NewRun(core.RunID("run-1"), 42, true, now.Add(time.Hour), now)

	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := st.LoadState(ctx, run.ID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.ID != run.ID || got.Version != run.Version || got.Phase != run.Phase ||
		got.Seed != run.Seed || got.RequireReview != run.RequireReview {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, run)
	}
	if !got.Deadline.Equal(run.Deadline) {
		t.Fatalf("deadline mismatch: got %v, want %v", got.Deadline, run.Deadline)
	}
	if !got.CreatedAt.Equal(run.CreatedAt) || !got.UpdatedAt.Equal(run.UpdatedAt) {
		t.Fatalf("timestamps mismatch: got created=%v updated=%v, want created=%v updated=%v",
			got.CreatedAt, got.UpdatedAt, run.CreatedAt, run.UpdatedAt)
	}
}

func TestLoadStateNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := st.LoadState(ctx, core.RunID("missing")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("LoadState: got %v, want core.ErrNotFound", err)
	}
	if _, err := st.LatestCheckpoint(ctx, core.RunID("missing")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("LatestCheckpoint: got %v, want core.ErrNotFound", err)
	}
}

func TestCommitDurableTransition(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	now := time.Now().UTC()
	run := core.NewRun(core.RunID("run-1"), 1, false, time.Time{}, now)
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	next := bumpedState(run, core.PhaseRunning, 1, now.Add(time.Second))
	b := core.CommitBundle{
		Expected: run.Version, // 0
		Next:     next,
		Transition: core.Transition{
			RunID:        run.ID,
			PriorVersion: run.Version,
			NewVersion:   next.Version,
			ActionKind:   core.ActionCallTool,
			Tool:         core.ToolListSources,
			InputHash:    "hash-1",
			FromPhase:    run.Phase,
			ToPhase:      next.Phase,
			Evidence:     core.Redacted(`{"sources":2}`),
			At:           next.UpdatedAt,
		},
		Checkpoint: core.Checkpoint{
			RunID:     next.ID,
			Version:   next.Version,
			State:     next,
			CreatedAt: next.UpdatedAt,
		},
		Events: []core.Event{
			{RunID: run.ID, Version: next.Version, Kind: core.EventStarted, Summary: "run started", At: next.UpdatedAt},
		},
	}
	mustCommit(t, ctx, st, b)

	state, err := st.LoadState(ctx, run.ID)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Phase != core.PhaseRunning || state.Version != 1 || state.Step != 1 {
		t.Fatalf("LoadState after commit: phase=%v version=%d step=%d", state.Phase, state.Version, state.Step)
	}

	cp, err := st.LatestCheckpoint(ctx, run.ID)
	if err != nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	if cp.Version != 1 || cp.State.Phase != core.PhaseRunning {
		t.Fatalf("LatestCheckpoint: version=%d phase=%v", cp.Version, cp.State.Phase)
	}
}

func TestCommitVersionConflict(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	now := time.Now().UTC()
	run := core.NewRun(core.RunID("run-1"), 1, false, time.Time{}, now)
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	buildBundle := func(hash string) core.CommitBundle {
		next := bumpedState(run, core.PhaseRunning, 1, now.Add(time.Second))
		return core.CommitBundle{
			Expected: run.Version, // both attempts race on the same stale Expected=0
			Next:     next,
			Transition: core.Transition{
				RunID:        run.ID,
				PriorVersion: run.Version,
				NewVersion:   next.Version,
				ActionKind:   core.ActionCallTool,
				Tool:         core.ToolListSources,
				InputHash:    hash,
				FromPhase:    run.Phase,
				ToPhase:      next.Phase,
				At:           next.UpdatedAt,
			},
			Checkpoint: core.Checkpoint{RunID: next.ID, Version: next.Version, State: next, CreatedAt: next.UpdatedAt},
		}
	}

	// First writer wins.
	if err := st.Commit(ctx, buildBundle("writer-a")); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	// Second writer is stale: the run is already at version 1, but this
	// bundle still expects version 0.
	err := st.Commit(ctx, buildBundle("writer-b"))
	if !errors.Is(err, core.ErrVersionConflict) {
		t.Fatalf("second Commit: got %v, want core.ErrVersionConflict", err)
	}

	// The rejected commit must not have left partial writes behind.
	state, loadErr := st.LoadState(ctx, run.ID)
	if loadErr != nil {
		t.Fatalf("LoadState: %v", loadErr)
	}
	if state.Version != 1 {
		t.Fatalf("state.Version = %d, want 1 (rejected commit must not apply)", state.Version)
	}
}

func TestFindingDedupeAcrossCommits(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	now := time.Now().UTC()
	run := core.NewRun(core.RunID("run-1"), 1, false, time.Time{}, now)
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	key := core.DeriveKey(run.ID, core.StepIndex(0), core.ToolRecordFinding)
	finding := &core.FindingRow{
		RunID:    run.ID,
		Key:      key,
		SourceID: "src-1",
		Claim:    "claim text",
		Evidence: core.Redacted(`{"confidence":0.9}`),
		At:       now,
	}

	// First transition: intent + finding write.
	next1 := bumpedState(run, core.PhaseRunning, 1, now.Add(time.Second))
	mustCommit(t, ctx, st, core.CommitBundle{
		Expected: run.Version,
		Next:     next1,
		Transition: core.Transition{
			RunID: run.ID, PriorVersion: run.Version, NewVersion: next1.Version,
			ActionKind: core.ActionCallTool, Tool: core.ToolRecordFinding, IdemKey: key,
			InputHash: "hash-1", FromPhase: run.Phase, ToPhase: next1.Phase, At: next1.UpdatedAt,
		},
		Checkpoint: core.Checkpoint{RunID: next1.ID, Version: next1.Version, State: next1, CreatedAt: next1.UpdatedAt},
		Finding:    finding,
	})

	// Second, later transition replays the very same FindingRow (same key):
	// this simulates a resumed run re-issuing the confirm step after a crash.
	next2 := bumpedState(next1, core.PhaseRunning, 2, now.Add(2*time.Second))
	mustCommit(t, ctx, st, core.CommitBundle{
		Expected: next1.Version,
		Next:     next2,
		Transition: core.Transition{
			RunID: run.ID, PriorVersion: next1.Version, NewVersion: next2.Version,
			ActionKind: core.ActionCallTool, Tool: core.ToolRecordFinding, IdemKey: key,
			InputHash: "hash-2", FromPhase: next1.Phase, ToPhase: next2.Phase, At: next2.UpdatedAt,
		},
		Checkpoint: core.Checkpoint{RunID: next2.ID, Version: next2.Version, State: next2, CreatedAt: next2.UpdatedAt},
		Finding:    finding,
	})

	rows, err := st.Findings(ctx, run.ID)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Findings: got %d rows, want exactly 1 (dedupe on idempotency_key)", len(rows))
	}
	if rows[0].Key != key || rows[0].SourceID != "src-1" {
		t.Fatalf("Findings[0] = %+v", rows[0])
	}

	exists, err := st.FindingExists(ctx, key)
	if err != nil {
		t.Fatalf("FindingExists: %v", err)
	}
	if !exists {
		t.Fatal("FindingExists: want true")
	}
}

func TestEventSeqIncreasesAcrossCommits(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	now := time.Now().UTC()
	run := core.NewRun(core.RunID("run-1"), 1, false, time.Time{}, now)
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	next1 := bumpedState(run, core.PhaseRunning, 1, now.Add(time.Second))
	mustCommit(t, ctx, st, core.CommitBundle{
		Expected: run.Version,
		Next:     next1,
		Transition: core.Transition{
			RunID: run.ID, PriorVersion: run.Version, NewVersion: next1.Version,
			ActionKind: core.ActionCallTool, Tool: core.ToolListSources,
			InputHash: "hash-1", FromPhase: run.Phase, ToPhase: next1.Phase, At: next1.UpdatedAt,
		},
		Checkpoint: core.Checkpoint{RunID: next1.ID, Version: next1.Version, State: next1, CreatedAt: next1.UpdatedAt},
		Events: []core.Event{
			{RunID: run.ID, Version: next1.Version, Kind: core.EventStarted, Summary: "e1", At: next1.UpdatedAt},
			{RunID: run.ID, Version: next1.Version, Kind: core.EventActionChosen, Summary: "e2", At: next1.UpdatedAt},
		},
	})

	next2 := bumpedState(next1, core.PhaseRunning, 2, now.Add(2*time.Second))
	mustCommit(t, ctx, st, core.CommitBundle{
		Expected: next1.Version,
		Next:     next2,
		Transition: core.Transition{
			RunID: run.ID, PriorVersion: next1.Version, NewVersion: next2.Version,
			ActionKind: core.ActionCallTool, Tool: core.ToolReadSource,
			InputHash: "hash-2", FromPhase: next1.Phase, ToPhase: next2.Phase, At: next2.UpdatedAt,
		},
		Checkpoint: core.Checkpoint{RunID: next2.ID, Version: next2.Version, State: next2, CreatedAt: next2.UpdatedAt},
		Events: []core.Event{
			{RunID: run.ID, Version: next2.Version, Kind: core.EventToolStarted, Summary: "e3", At: next2.UpdatedAt},
		},
	})

	events, err := st.Events(ctx, run.ID)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("Events: got %d, want 3", len(events))
	}
	for i, e := range events {
		wantSeq := int64(i + 1)
		if e.Seq != wantSeq {
			t.Fatalf("events[%d].Seq = %d, want %d", i, e.Seq, wantSeq)
		}
	}
	if events[0].Kind != core.EventStarted || events[1].Kind != core.EventActionChosen || events[2].Kind != core.EventToolStarted {
		t.Fatalf("unexpected event kinds: %+v", events)
	}
}

func TestResumableRunsExcludesTerminalPhases(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC()

	phases := []core.Phase{
		core.PhasePending,
		core.PhaseRunning,
		core.PhaseBackoff,
		core.PhaseAwaitingHuman,
		core.PhaseSucceeded,
		core.PhaseFailed,
		core.PhaseCancelled,
	}
	for i, phase := range phases {
		run := core.NewRun(core.RunID(phase), uint64(i), false, time.Time{}, now)
		run.Phase = phase
		if err := st.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun(%s): %v", phase, err)
		}
	}

	ids, err := st.ResumableRuns(ctx)
	if err != nil {
		t.Fatalf("ResumableRuns: %v", err)
	}

	want := map[core.RunID]bool{
		core.RunID(core.PhasePending):       true,
		core.RunID(core.PhaseRunning):       true,
		core.RunID(core.PhaseBackoff):       true,
		core.RunID(core.PhaseAwaitingHuman): true,
	}
	if len(ids) != len(want) {
		t.Fatalf("ResumableRuns: got %d ids %v, want %d", len(ids), ids, len(want))
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("ResumableRuns: unexpected terminal run id %q in result", id)
		}
	}
}
