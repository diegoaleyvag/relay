package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/diegoaleyvag/relay/internal/clock"
	"github.com/diegoaleyvag/relay/internal/core"
	"github.com/diegoaleyvag/relay/internal/engine"
	"github.com/diegoaleyvag/relay/internal/faults"
	"github.com/diegoaleyvag/relay/internal/planner"
	"github.com/diegoaleyvag/relay/internal/scenarios"
	"github.com/diegoaleyvag/relay/internal/telemetry"
)

// relayRunner implements web.Runner. It maps a named scenario to its fault plan,
// creates the run durably, and drives it in the background (so the HTTP handler
// returns immediately and the control room polls for progress). Faults are a
// start-time concern; Resume and human resolution always drive with a plain,
// fault-free tool port so recovery is clean.
type relayRunner struct {
	repo     core.Repository
	toolPort core.ToolPort // shared MCP client (fault-free)
	prov     *telemetry.Provider
	cfg      engine.Config

	mu       sync.Mutex
	inFlight map[core.RunID]bool // single-driver lease per run
}

func newRunner(repo core.Repository, toolPort core.ToolPort, prov *telemetry.Provider, cfg engine.Config) *relayRunner {
	return &relayRunner{repo: repo, toolPort: toolPort, prov: prov, cfg: cfg, inFlight: map[core.RunID]bool{}}
}

// acquire grants a single-driver lease for a run; release frees it. Concurrent
// drivers of the same run are prevented, so a run's tool calls are never issued
// twice in parallel (durable exactly-once holds regardless, via the version CAS,
// but the lease avoids redundant work and noisy conflicts).
func (r *relayRunner) acquire(id core.RunID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[id] {
		return false
	}
	r.inFlight[id] = true
	return true
}

func (r *relayRunner) release(id core.RunID) {
	r.mu.Lock()
	delete(r.inFlight, id)
	r.mu.Unlock()
}

// engineWith builds an engine whose tool port is traced and (optionally) faulted.
func (r *relayRunner) engineWith(tools core.ToolPort) *engine.Engine {
	traced := &telemetry.ToolTracer{Inner: tools, Tracer: r.prov.Tracer}
	return engine.New(engine.Params{
		Repo: r.repo, Tools: traced, Planner: planner.New(),
		Clock: clock.SystemClock{}, Config: r.cfg,
	})
}

// Start creates a run for the named scenario and drives it in the background.
func (r *relayRunner) Start(ctx context.Context, scenario string, requireReview bool) (core.RunID, error) {
	sc, ok := scenarios.Get(scenario)
	if !ok {
		return "", fmt.Errorf("unknown scenario %q", scenario)
	}
	id := newRunID()
	run := core.NewRun(id, seedFor(id), requireReview || sc.RequireReview, time.Time{}, time.Now())
	if err := r.repo.CreateRun(ctx, run); err != nil {
		return "", err
	}
	faulty := &faults.FaultyToolPort{Inner: r.toolPort, Plan: sc.Plan()}
	go r.drive(r.engineWith(faulty), id, run, sc.Name)
	return id, nil
}

// Resume continues a run with a fault-free engine.
func (r *relayRunner) Resume(_ context.Context, id core.RunID) error {
	go func() {
		s, err := r.repo.LoadState(context.Background(), id)
		if err != nil {
			log.Printf("resume load %s: %v", id, err)
			return
		}
		r.drive(r.engineWith(r.toolPort), id, s, "resume")
	}()
	return nil
}

// ResolveHuman applies a reviewer decision and continues the run.
func (r *relayRunner) ResolveHuman(_ context.Context, id core.RunID, d core.HumanDecision) error {
	go func() {
		if _, err := r.engineWith(r.toolPort).ResolveHuman(context.Background(), id, d); err != nil {
			log.Printf("resolve %s: %v", id, err)
		}
	}()
	return nil
}

// Cancel cancels a run (synchronously — it is a single transition).
func (r *relayRunner) Cancel(ctx context.Context, id core.RunID) error {
	_, err := r.engineWith(r.toolPort).Cancel(ctx, id)
	return err
}

// drive runs an engine to completion under a root run span, holding the run's
// single-driver lease. If the run is already being driven, it returns at once.
func (r *relayRunner) drive(eng *engine.Engine, id core.RunID, run core.RunState, scenario string) {
	if !r.acquire(id) {
		return
	}
	defer r.release(id)
	ctx, end := telemetry.StartRunSpan(context.Background(), r.prov.Tracer, run, scenario)
	final, err := eng.Run(ctx, id)
	if err != nil {
		log.Printf("run %s: %v", id, err)
		if s, lerr := r.repo.LoadState(context.Background(), id); lerr == nil {
			final = s
		}
	}
	end(final)
}

// resumeAll re-drives any non-terminal runs left by a previous process.
func (r *relayRunner) resumeAll() {
	if err := r.engineWith(r.toolPort).ResumeAll(context.Background()); err != nil {
		log.Printf("resume all: %v", err)
	}
}

func newRunID() core.RunID {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return core.RunID("run-" + hex.EncodeToString(b[:]))
}

// seedFor derives a stable per-run seed from the id (FNV-1a).
func seedFor(id core.RunID) uint64 {
	var h uint64 = 1469598103934665603
	for _, b := range []byte(id) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}
