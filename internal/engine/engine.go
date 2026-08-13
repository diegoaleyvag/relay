// Package engine is the application orchestrator: it drives a run through the
// pure core.Reduce state machine, performing the I/O (tool calls, persistence,
// sleeping) that the reducer describes. It depends only on the domain ports, so
// the same engine runs against the SQLite store and the MCP tool client in
// production and against fakes in tests.
//
// The loop is deliberately uniform for normal execution and resume:
//   - a side-effecting step first commits an INTENT (durable), then, on the next
//     iteration, executes the tool and commits the CONFIRM;
//   - on restart a run whose checkpoint has a Pending effect re-enters exactly at
//     the execute step, re-issues the tool call with the same idempotency key,
//     and confirms — the receiver dedupes, so the effect is exactly-once.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// Config holds the failure-contract knobs.
type Config struct {
	Backoff        core.Backoff
	MaxAttempts    int           // attempts per step before RETRIES_EXHAUSTED
	MaxRetries     int           // global retry budget across the run
	Policy         core.Policy   // malformed/permission policy
	PerCallTimeout time.Duration // per tool-call deadline (0 = none)
}

// DefaultConfig is the standard failure contract.
func DefaultConfig() Config {
	return Config{
		Backoff:     core.DefaultBackoff(),
		MaxAttempts: 3,
		MaxRetries:  5,
		Policy:      core.DefaultPolicy(),
	}
}

// Hooks are optional test seams. BeforeCommit fires just before each durable
// write; a restart test can os.Exit there to simulate a crash.
type Hooks struct {
	BeforeCommit func(next core.RunState)
	AfterCommit  func(next core.RunState)
}

// Params configures a new Engine.
type Params struct {
	Repo     core.Repository
	Tools    core.ToolPort
	Planner  core.Planner
	Clock    core.Clock
	Redactor core.Redactor // optional; defaults to LimitRedactor
	Config   Config
	Hooks    Hooks
}

// Engine drives runs.
type Engine struct {
	repo     core.Repository
	tools    core.ToolPort
	planner  core.Planner
	clock    core.Clock
	redactor core.Redactor
	cfg      Config
	hooks    Hooks
}

// New builds an Engine.
func New(p Params) *Engine {
	red := p.Redactor
	if red == nil {
		red = core.LimitRedactor{MaxString: 160}
	}
	return &Engine{
		repo: p.Repo, tools: p.Tools, planner: p.Planner, clock: p.Clock,
		redactor: red, cfg: p.Config, hooks: p.Hooks,
	}
}

func (e *Engine) env() core.Env {
	return core.Env{
		Now:         e.clock.Now(),
		Backoff:     e.cfg.Backoff,
		MaxAttempts: e.cfg.MaxAttempts,
		MaxRetries:  e.cfg.MaxRetries,
		Policy:      e.cfg.Policy,
	}
}

// StartRun creates a fresh run and drives it to a terminal or parked state.
func (e *Engine) StartRun(ctx context.Context, s core.RunState) (core.RunState, error) {
	if err := e.repo.CreateRun(ctx, s); err != nil {
		return s, fmt.Errorf("engine: create run: %w", err)
	}
	return e.drive(ctx, s)
}

// Run resumes an existing run from its latest checkpoint and drives it.
func (e *Engine) Run(ctx context.Context, id core.RunID) (core.RunState, error) {
	s, err := e.load(ctx, id)
	if err != nil {
		return core.RunState{}, err
	}
	return e.drive(ctx, s)
}

// ResumeAll drives every non-terminal run once (e.g. on process startup).
func (e *Engine) ResumeAll(ctx context.Context) error {
	ids, err := e.repo.ResumableRuns(ctx)
	if err != nil {
		return fmt.Errorf("engine: resumable runs: %w", err)
	}
	for _, id := range ids {
		if _, err := e.Run(ctx, id); err != nil {
			return fmt.Errorf("engine: resume %s: %w", id, err)
		}
	}
	return nil
}

// ResolveHuman applies a reviewer decision to a parked run and continues it.
func (e *Engine) ResolveHuman(ctx context.Context, id core.RunID, d core.HumanDecision) (core.RunState, error) {
	s, err := e.repo.LoadState(ctx, id)
	if err != nil {
		return core.RunState{}, fmt.Errorf("engine: load %s: %w", id, err)
	}
	ns, err := e.commit(ctx, s, core.Reduce(s, core.InHuman{Decision: d}, e.env()))
	if err != nil {
		return s, err
	}
	return e.drive(ctx, ns)
}

// Cancel cancels a non-terminal run.
func (e *Engine) Cancel(ctx context.Context, id core.RunID) (core.RunState, error) {
	s, err := e.repo.LoadState(ctx, id)
	if err != nil {
		return core.RunState{}, fmt.Errorf("engine: load %s: %w", id, err)
	}
	if s.Phase.Terminal() {
		return s, nil
	}
	return e.commit(ctx, s, core.Reduce(s, core.InCancel{Reason: core.CodeCancelled}, e.env()))
}

// load reads the resume point: the latest checkpoint, falling back to the run
// row for a freshly-created run with no checkpoint yet.
func (e *Engine) load(ctx context.Context, id core.RunID) (core.RunState, error) {
	cp, err := e.repo.LatestCheckpoint(ctx, id)
	if err == nil {
		return cp.State, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return core.RunState{}, fmt.Errorf("engine: checkpoint %s: %w", id, err)
	}
	s, err := e.repo.LoadState(ctx, id)
	if err != nil {
		return core.RunState{}, fmt.Errorf("engine: load %s: %w", id, err)
	}
	return s, nil
}

// drive runs the loop until the run is terminal or parked in awaiting_human.
func (e *Engine) drive(ctx context.Context, s core.RunState) (core.RunState, error) {
	for {
		if s.Phase.Terminal() {
			return s, nil
		}
		// Cancellation and deadline propagation.
		if ctx.Err() != nil {
			return e.commit(ctx, s, core.Reduce(s, core.InCancel{Reason: core.CodeCancelled}, e.env()))
		}
		if !s.Deadline.IsZero() && e.clock.Now().After(s.Deadline) {
			return e.commit(ctx, s, core.Reduce(s, core.InCancel{Reason: core.CodeDeadlineExceeded}, e.env()))
		}

		switch s.Phase {
		case core.PhasePending:
			ns, err := e.commit(ctx, s, core.Reduce(s, core.InStart{}, e.env()))
			if err != nil {
				return s, err
			}
			s = ns

		case core.PhaseAwaitingHuman:
			return s, nil // parked; ResolveHuman resumes it out-of-band

		case core.PhaseBackoff:
			if d := s.NextWakeAt.Sub(e.clock.Now()); d > 0 {
				if err := e.clock.Sleep(ctx, d); err != nil {
					return e.commit(ctx, s, core.Reduce(s, core.InCancel{Reason: core.CodeCancelled}, e.env()))
				}
			}
			// Commit the wake as a durable backoff→running transition so the
			// checkpoint chain records a legal edge (not backoff→<result>).
			ns, err := e.commit(ctx, s, core.Reduce(s, core.InWake{}, e.env()))
			if err != nil {
				return s, err
			}
			s = ns

		case core.PhaseRunning:
			ns, err := e.step(ctx, s)
			if err != nil {
				return s, err
			}
			s = ns

		default:
			return s, fmt.Errorf("engine: unknown phase %q", s.Phase)
		}
	}
}

// step advances one running transition.
func (e *Engine) step(ctx context.Context, s core.RunState) (core.RunState, error) {
	a, err := e.planner.Next(s)
	if err != nil {
		return e.failPlanner(ctx, s, err)
	}

	// An in-flight side effect (either just-committed intent or a resumed
	// crash) is executed and confirmed here — the single exactly-once path.
	if s.Pending != nil {
		return e.execute(ctx, s, a)
	}

	if a.Kind != core.ActionCallTool {
		return e.commit(ctx, s, core.Reduce(s, core.InAction{Action: a}, e.env()))
	}

	// Fail closed on a non-allowlisted tool BEFORE any invocation, for both
	// read-only and side-effecting tools. The reducer's dispatch gate produces
	// the terminal PERMISSION_DENIED.
	if !a.Tool.Allowlisted() {
		return e.commit(ctx, s, core.Reduce(s, core.InAction{Action: a}, e.env()))
	}

	if a.Tool.SideEffecting() {
		// Commit the INTENT; the next iteration (Pending != nil) executes it.
		return e.commit(ctx, s, core.Reduce(s, core.InAction{Action: a}, e.env()))
	}

	// Read-only tool: invoke and fold the result.
	return e.execute(ctx, s, a)
}

// execute invokes a tool (honoring a per-call deadline) and commits the result.
func (e *Engine) execute(ctx context.Context, s core.RunState, a core.Action) (core.RunState, error) {
	call := core.ToolCall{Tool: a.Tool, Input: a.Input, Idem: a.Idem, Step: a.Step, Attempt: s.Attempt}

	cctx := ctx
	if e.cfg.PerCallTimeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, e.cfg.PerCallTimeout)
		defer cancel()
	}

	res, rerr := e.tools.Invoke(cctx, call)
	return e.commit(ctx, s, core.Reduce(s, core.InToolResult{Call: call, Result: res, Err: rerr}, e.env()))
}

// failPlanner fails a run when the planner errors (does not happen for the
// scripted planner, but keeps the loop total).
func (e *Engine) failPlanner(ctx context.Context, s core.RunState, err error) (core.RunState, error) {
	s2 := s
	s2.LastError = core.NewError(core.CodeInternal, "planner error: "+err.Error())
	return e.commit(ctx, s2, core.Reduce(s2, core.InAction{Action: core.Fail()}, e.env()))
}

// commit builds a CommitBundle from a durable reducer Result, redacts event
// evidence as defense-in-depth, and persists it atomically.
func (e *Engine) commit(ctx context.Context, prev core.RunState, r core.Result) (core.RunState, error) {
	if !r.Durable {
		return prev, fmt.Errorf("engine: non-durable result in phase %s (events %d)", prev.Phase, len(r.Events))
	}
	next := r.State
	now := e.clock.Now()

	events := make([]core.Event, len(r.Events))
	for i, ev := range r.Events {
		ev.Evidence = e.redactor.Redact(ev.Kind, []byte(ev.Evidence))
		events[i] = ev
	}

	bundle := core.CommitBundle{
		Expected: prev.Version,
		Next:     next,
		Transition: core.Transition{
			RunID:        next.ID,
			PriorVersion: prev.Version,
			NewVersion:   next.Version,
			ActionKind:   r.ActionKind,
			Tool:         r.Tool,
			IdemKey:      r.IdemKey,
			InputHash:    r.InputHash,
			FromPhase:    prev.Phase,
			ToPhase:      next.Phase,
			Evidence:     e.redactor.Redact(core.EventKind(r.ActionKind), []byte(r.Evidence)),
			At:           now,
		},
		Checkpoint: core.Checkpoint{RunID: next.ID, Version: next.Version, State: next, CreatedAt: now},
		Events:     events,
		Effect:     r.Effect,
		Finding:    r.Finding,
		Review:     r.Review,
		ReviewFix:  r.ReviewFix,
	}

	if e.hooks.BeforeCommit != nil {
		e.hooks.BeforeCommit(next)
	}
	if err := e.repo.Commit(ctx, bundle); err != nil {
		return prev, fmt.Errorf("engine: commit v%d->v%d: %w", prev.Version, next.Version, err)
	}
	if e.hooks.AfterCommit != nil {
		e.hooks.AfterCommit(next)
	}
	return next, nil
}
