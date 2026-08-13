package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
	"github.com/diegoaleyvag/relay/internal/planner"
)

// --- fakes --------------------------------------------------------------

// fakeClock advances virtual time on Sleep so backoff is instant/deterministic.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.Now().Add(d)
	return ch
}

// memRepo is an in-memory core.Repository with CAS, checkpoints, event seq and
// exactly-once dedupe on finding/review keys.
type memRepo struct {
	mu          sync.Mutex
	runs        map[core.RunID]core.RunState
	checkpoints map[core.RunID][]core.Checkpoint
	events      map[core.RunID][]core.Event
	findings    map[core.IdempotencyKey]core.FindingRow
	reviews     map[core.IdempotencyKey]core.ReviewRow
	side        map[core.IdempotencyKey]core.SideEffectRow
	seq         map[core.RunID]int64
}

func newMemRepo() *memRepo {
	return &memRepo{
		runs:        map[core.RunID]core.RunState{},
		checkpoints: map[core.RunID][]core.Checkpoint{},
		events:      map[core.RunID][]core.Event{},
		findings:    map[core.IdempotencyKey]core.FindingRow{},
		reviews:     map[core.IdempotencyKey]core.ReviewRow{},
		side:        map[core.IdempotencyKey]core.SideEffectRow{},
		seq:         map[core.RunID]int64{},
	}
}

func (m *memRepo) CreateRun(_ context.Context, s core.RunState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[s.ID] = s
	return nil
}

func (m *memRepo) Commit(_ context.Context, b core.CommitBundle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.runs[b.Next.ID]
	if !ok || cur.Version != b.Expected {
		return core.ErrVersionConflict
	}
	m.runs[b.Next.ID] = b.Next
	m.checkpoints[b.Next.ID] = append(m.checkpoints[b.Next.ID], b.Checkpoint)
	for _, ev := range b.Events {
		m.seq[b.Next.ID]++
		ev.Seq = m.seq[b.Next.ID]
		m.events[b.Next.ID] = append(m.events[b.Next.ID], ev)
	}
	if b.Effect != nil {
		m.side[b.Effect.Key] = *b.Effect
	}
	if b.Finding != nil {
		if _, dup := m.findings[b.Finding.Key]; !dup {
			m.findings[b.Finding.Key] = *b.Finding
		}
	}
	if b.Review != nil {
		if _, dup := m.reviews[b.Review.Key]; !dup {
			m.reviews[b.Review.Key] = *b.Review
		}
	}
	if b.ReviewFix != nil {
		if r, ok := m.reviews[b.ReviewFix.Key]; ok {
			r.Status = b.ReviewFix.Status
			m.reviews[b.ReviewFix.Key] = r
		}
	}
	return nil
}

func (m *memRepo) LoadState(_ context.Context, id core.RunID) (core.RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.runs[id]
	if !ok {
		return core.RunState{}, core.ErrNotFound
	}
	return s, nil
}

func (m *memRepo) LatestCheckpoint(_ context.Context, id core.RunID) (core.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cps := m.checkpoints[id]
	if len(cps) == 0 {
		return core.Checkpoint{}, core.ErrNotFound
	}
	return cps[len(cps)-1], nil
}

func (m *memRepo) ResumableRuns(_ context.Context) ([]core.RunID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []core.RunID
	for id, s := range m.runs {
		if !s.Phase.Terminal() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m *memRepo) PendingIntent(_ context.Context, id core.RunID) (*core.SideEffectRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, se := range m.side {
		if se.RunID == id && se.Status == core.EffectIntent {
			cp := se
			return &cp, true, nil
		}
	}
	return nil, false, nil
}

func (m *memRepo) FindingExists(_ context.Context, key core.IdempotencyKey) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.findings[key]
	return ok, nil
}

func (m *memRepo) ReviewExists(_ context.Context, key core.IdempotencyKey) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.reviews[key]
	return ok, nil
}

func (m *memRepo) ListRuns(_ context.Context) ([]core.RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]core.RunState, 0, len(m.runs))
	for _, s := range m.runs {
		out = append(out, s)
	}
	return out, nil
}

func (m *memRepo) Events(_ context.Context, id core.RunID) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.Event(nil), m.events[id]...), nil
}

func (m *memRepo) Findings(_ context.Context, id core.RunID) ([]core.FindingRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.FindingRow
	for _, f := range m.findings {
		if f.RunID == id {
			out = append(out, f)
		}
	}
	return out, nil
}

// fakeTools is a scripted core.ToolPort. failTimes injects timeouts per step.
type fakeTools struct {
	mu          sync.Mutex
	sources     []core.SourceRef
	failTimes   map[int]int
	recordCalls int
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (f *fakeTools) Invoke(_ context.Context, call core.ToolCall) (core.ToolResult, *core.RelayError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.failTimes[int(call.Step)]; n > 0 {
		f.failTimes[int(call.Step)] = n - 1
		return core.ToolResult{Tool: call.Tool}, core.NewError(core.CodeTimeout, "injected").MarkInjected("timeout")
	}
	switch call.Tool {
	case core.ToolListSources:
		return core.ToolResult{Tool: call.Tool, Output: mustJSON(core.ListSourcesOutput{Sources: f.sources})}, nil
	case core.ToolReadSource:
		return core.ToolResult{Tool: call.Tool, Output: mustJSON(core.ReadSourceOutput{ID: "x", Bytes: 1})}, nil
	case core.ToolRecordFinding:
		f.recordCalls++
		var in core.RecordFindingInput
		_ = json.Unmarshal(call.Input, &in)
		return core.ToolResult{Tool: call.Tool, Output: mustJSON(core.RecordFindingOutput{FindingID: "fnd-" + in.SourceID, Recorded: true})}, nil
	case core.ToolRequestReview:
		return core.ToolResult{Tool: call.Tool, Output: mustJSON(core.RequestHumanReviewOutput{ReviewID: "rev-1", State: "awaiting_human_review"})}, nil
	default:
		return core.ToolResult{}, core.NewError(core.CodeInternal, "unknown tool")
	}
}

// crashRepo wraps a repo and fails the first finding-confirm commit, simulating
// a process kill after the side effect executes but before its checkpoint.
type crashRepo struct {
	core.Repository
	crashed bool
}

func (c *crashRepo) Commit(ctx context.Context, b core.CommitBundle) error {
	if b.Finding != nil && !c.crashed {
		c.crashed = true
		return errors.New("simulated crash before confirm checkpoint")
	}
	return c.Repository.Commit(ctx, b)
}

func newEngine(repo core.Repository, tools core.ToolPort, cfg Config) *Engine {
	return New(Params{Repo: repo, Tools: tools, Planner: planner.New(), Clock: newClock(), Config: cfg})
}

func twoSources() []core.SourceRef {
	return []core.SourceRef{{ID: "s1", Title: "A", Bytes: 10}, {ID: "s2", Title: "B", Bytes: 20}}
}

// --- tests --------------------------------------------------------------

func TestEngineZeroSourcesTerminates(t *testing.T) {
	// An empty source list must complete immediately, not loop forever. A context
	// deadline turns a regression (infinite re-list) into a fast failure.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	repo := newMemRepo()
	tools := &fakeTools{sources: []core.SourceRef{}, failTimes: map[int]int{}}
	e := newEngine(repo, tools, DefaultConfig())

	final, err := e.StartRun(ctx, core.NewRun("run0", 7, false, time.Time{}, time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("empty corpus should complete, got %v", final.Phase)
	}
	if fs, _ := repo.Findings(ctx, "run0"); len(fs) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(fs))
	}
}

func TestEngineHappyPath(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	tools := &fakeTools{sources: twoSources(), failTimes: map[int]int{}}
	e := newEngine(repo, tools, DefaultConfig())

	run := core.NewRun("run1", 7, false, time.Time{}, time.Unix(0, 0))
	final, err := e.StartRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded, got %v (err=%v)", final.Phase, final.LastError)
	}
	fs, _ := repo.Findings(ctx, "run1")
	if len(fs) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(fs))
	}
}

func TestEngineTimeoutRetrySucceeds(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	// Fail the first read (step 1) twice, then succeed.
	tools := &fakeTools{sources: twoSources(), failTimes: map[int]int{int(core.StepRead(0)): 2}}
	cfg := DefaultConfig()
	cfg.MaxAttempts = 5
	e := newEngine(repo, tools, cfg)

	final, err := e.StartRun(ctx, core.NewRun("run2", 7, false, time.Time{}, time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded after retries, got %v", final.Phase)
	}
	if final.RetriesUsed != 2 {
		t.Fatalf("expected 2 retries used, got %d", final.RetriesUsed)
	}
	fs, _ := repo.Findings(ctx, "run2")
	if len(fs) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(fs))
	}
}

func TestEngineTimeoutExhaustsToFailed(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	tools := &fakeTools{sources: twoSources(), failTimes: map[int]int{int(core.StepRead(0)): 99}}
	e := newEngine(repo, tools, DefaultConfig()) // MaxAttempts 3

	final, err := e.StartRun(ctx, core.NewRun("run3", 7, false, time.Time{}, time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseFailed || final.LastError.Code != core.CodeRetriesExhausted {
		t.Fatalf("expected failed/retries-exhausted, got %v/%v", final.Phase, final.LastError)
	}
}

func TestEngineHumanReview(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	tools := &fakeTools{sources: twoSources(), failTimes: map[int]int{}}
	e := newEngine(repo, tools, DefaultConfig())

	parked, err := e.StartRun(ctx, core.NewRun("run4", 7, true /*requireReview*/, time.Time{}, time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if parked.Phase != core.PhaseAwaitingHuman {
		t.Fatalf("expected awaiting_human, got %v", parked.Phase)
	}
	final, err := e.ResolveHuman(ctx, "run4", core.DecisionApprove)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded after approval, got %v", final.Phase)
	}

	// Reject path on a fresh run.
	repo2 := newMemRepo()
	e2 := newEngine(repo2, &fakeTools{sources: twoSources(), failTimes: map[int]int{}}, DefaultConfig())
	if _, err := e2.StartRun(ctx, core.NewRun("run5", 7, true, time.Time{}, time.Unix(0, 0))); err != nil {
		t.Fatal(err)
	}
	rej, err := e2.ResolveHuman(ctx, "run5", core.DecisionReject)
	if err != nil {
		t.Fatal(err)
	}
	if rej.Phase != core.PhaseFailed {
		t.Fatalf("expected failed after rejection, got %v", rej.Phase)
	}
}

// TestEngineResumeExactlyOnce simulates a crash after a side effect executes but
// before its confirm checkpoint, then resumes with a fresh engine and asserts
// record_finding was re-executed yet exactly one finding per source survives.
func TestEngineResumeExactlyOnce(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	tools := &fakeTools{sources: twoSources(), failTimes: map[int]int{}}

	crashing := &crashRepo{Repository: repo}
	e1 := newEngine(crashing, tools, DefaultConfig())
	if _, err := e1.StartRun(ctx, core.NewRun("run6", 7, false, time.Time{}, time.Unix(0, 0))); err == nil {
		t.Fatal("expected the simulated crash to surface as an error")
	}

	// After the crash: the intent is durable, but no finding was confirmed.
	fs, _ := repo.Findings(ctx, "run6")
	if len(fs) != 0 {
		t.Fatalf("expected 0 confirmed findings before resume, got %d", len(fs))
	}

	// Resume with a healthy engine.
	e2 := newEngine(repo, tools, DefaultConfig())
	final, err := e2.Run(ctx, "run6")
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != core.PhaseSucceeded {
		t.Fatalf("expected succeeded after resume, got %v", final.Phase)
	}
	fs, _ = repo.Findings(ctx, "run6")
	if len(fs) != 2 {
		t.Fatalf("expected exactly 2 findings after resume, got %d", len(fs))
	}
	// record_finding was invoked more than twice (re-executed on resume) yet the
	// findings table holds exactly one row per source — exactly-once.
	if tools.recordCalls < 3 {
		t.Fatalf("expected a re-execution on resume (>=3 record calls), got %d", tools.recordCalls)
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if seen[f.SourceID] {
			t.Fatalf("duplicate finding for source %s", f.SourceID)
		}
		seen[f.SourceID] = true
	}
}
