package web

import (
	"context"
	"sort"
	"sync"

	"github.com/diegoaleyvag/relay/internal/core"
)

// fakeRepo is a minimal, in-memory core.Repository for tests. It implements
// only enough behavior for the control room's read paths and a simple,
// version-checked Commit; it is not a substitute for the real store's tests
// (internal/store/sqlite has those) — it exists purely so this package's HTTP
// tests can seed exact RunState/Event/FindingRow fixtures without a database.
type fakeRepo struct {
	mu       sync.Mutex
	runs     map[core.RunID]core.RunState
	events   map[core.RunID][]core.Event
	findings map[core.RunID][]core.FindingRow
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		runs:     make(map[core.RunID]core.RunState),
		events:   make(map[core.RunID][]core.Event),
		findings: make(map[core.RunID][]core.FindingRow),
	}
}

// seed installs a run's state directly, bypassing CreateRun/Commit, so tests
// can construct any RunState fixture they need (phase, findings, review, ...).
func (f *fakeRepo) seed(s core.RunState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[s.ID] = s
}

// mustLoad returns a seeded run's current state for test assertions/mutation;
// it panics (via zero value) if the id was never seeded, which a test author
// would notice immediately.
func (f *fakeRepo) mustLoad(id core.RunID) core.RunState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[id]
}

func (f *fakeRepo) CreateRun(_ context.Context, s core.RunState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[s.ID] = s
	return nil
}

func (f *fakeRepo) Commit(_ context.Context, b core.CommitBundle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.runs[b.Next.ID]; ok && cur.Version != b.Expected {
		return core.ErrVersionConflict
	}
	f.runs[b.Next.ID] = b.Next
	f.events[b.Next.ID] = append(f.events[b.Next.ID], b.Events...)
	if b.Finding != nil {
		f.findings[b.Next.ID] = append(f.findings[b.Next.ID], *b.Finding)
	}
	return nil
}

func (f *fakeRepo) LoadState(_ context.Context, id core.RunID) (core.RunState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.runs[id]
	if !ok {
		return core.RunState{}, core.ErrNotFound
	}
	return s, nil
}

func (f *fakeRepo) LatestCheckpoint(ctx context.Context, id core.RunID) (core.Checkpoint, error) {
	s, err := f.LoadState(ctx, id)
	if err != nil {
		return core.Checkpoint{}, err
	}
	return core.Checkpoint{RunID: id, Version: s.Version, State: s}, nil
}

func (f *fakeRepo) ResumableRuns(_ context.Context) ([]core.RunID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []core.RunID
	for id, s := range f.runs {
		if !s.Phase.Terminal() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (f *fakeRepo) PendingIntent(_ context.Context, _ core.RunID) (*core.SideEffectRow, bool, error) {
	return nil, false, nil
}

func (f *fakeRepo) FindingExists(_ context.Context, _ core.IdempotencyKey) (bool, error) {
	return false, nil
}

func (f *fakeRepo) ReviewExists(_ context.Context, _ core.IdempotencyKey) (bool, error) {
	return false, nil
}

func (f *fakeRepo) ListRuns(_ context.Context) ([]core.RunState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.RunState, 0, len(f.runs))
	for _, s := range f.runs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeRepo) Events(_ context.Context, id core.RunID) ([]core.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Event(nil), f.events[id]...), nil
}

func (f *fakeRepo) Findings(_ context.Context, id core.RunID) ([]core.FindingRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.FindingRow(nil), f.findings[id]...), nil
}

var _ core.Repository = (*fakeRepo)(nil)

// fakeRunner is a test double for Runner. Each hook (onResume, onCancel,
// onResolve) lets a test simulate the side effect a real engine-backed Runner
// would have on the repository (e.g. flipping the run's phase) so the
// handler's "reload after invoke" path can be exercised.
type fakeRunner struct {
	mu sync.Mutex

	nextID   core.RunID
	startErr error

	startCalls   int
	resumeCalls  []core.RunID
	cancelCalls  []core.RunID
	resolveCalls []core.HumanDecision

	onResume  func(id core.RunID)
	onCancel  func(id core.RunID)
	onResolve func(id core.RunID, d core.HumanDecision)
}

func (f *fakeRunner) Start(_ context.Context, _ string, _ bool) (core.RunID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.startErr != nil {
		return "", f.startErr
	}
	return f.nextID, nil
}

func (f *fakeRunner) Resume(_ context.Context, id core.RunID) error {
	f.mu.Lock()
	f.resumeCalls = append(f.resumeCalls, id)
	f.mu.Unlock()
	if f.onResume != nil {
		f.onResume(id)
	}
	return nil
}

func (f *fakeRunner) ResolveHuman(_ context.Context, id core.RunID, d core.HumanDecision) error {
	f.mu.Lock()
	f.resolveCalls = append(f.resolveCalls, d)
	f.mu.Unlock()
	if f.onResolve != nil {
		f.onResolve(id, d)
	}
	return nil
}

func (f *fakeRunner) Cancel(_ context.Context, id core.RunID) error {
	f.mu.Lock()
	f.cancelCalls = append(f.cancelCalls, id)
	f.mu.Unlock()
	if f.onCancel != nil {
		f.onCancel(id)
	}
	return nil
}

var _ Runner = (*fakeRunner)(nil)
