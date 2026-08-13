// Package web is the server-rendered, HTMX-driven control room for a single
// Relay run. It is a thin, read-mostly adapter over the frozen domain core:
// every page and partial is built from core.RunState, core.Event and
// core.FindingRow values read through core.Repository, and every mutating
// action is delegated to a Runner the composition root (cmd/relay) supplies.
//
// The control room never reasons about a run itself — it only renders what
// the domain has already decided (its Phase, its retry budget, its redacted
// event history) and, for the "next permitted step", asks the scripted
// planner (internal/planner) what it would do next. Nothing here is AI or
// "reasoning": the planner is scripted and deterministic, and the UI labels
// it that way everywhere it appears.
//
// Every response is built with html/template, whose automatic contextual
// escaping is relied on for safety: this package never opts any value out of
// that escaping via one of the package's "trusted content" wrapper types, and
// the view models it builds carry only metadata (ids, phases, counts, codes,
// timestamps) — never source or finding content.
package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"sync"

	"github.com/diegoaleyvag/relay/internal/core"
	"github.com/diegoaleyvag/relay/internal/planner"
)

// templateFS embeds every HTML template the control room renders. Templates
// are parsed once, at New, so a malformed template fails the process fast at
// startup rather than on the first matching request.
//
//go:embed templates/layout.html templates/index.html templates/run.html templates/partials/*.html
var templateFS embed.FS

// staticFS embeds the control room's static assets: vendored HTMX and a small
// stylesheet. See static/htmx.min.js for provenance.
//
//go:embed static
var staticFS embed.FS

// Runner drives a run through the engine. The control room never touches the
// engine, the store or the planner's execution directly — it only asks a
// Runner to start, resume, resolve or cancel a run, by id, and then re-reads
// the resulting state through core.Repository. The composition root
// (cmd/relay) supplies the concrete implementation, wiring it to the real
// engine and store.
type Runner interface {
	// Start creates and begins a new run of the named scenario. requireReview
	// mirrors core.RunState.RequireReview: when true the run must be
	// approved by a human before it may succeed.
	Start(ctx context.Context, scenario string, requireReview bool) (core.RunID, error)
	// Resume nudges a parked run (pending or backoff) to continue driving
	// itself. It is a no-op error path (not a phase transition itself) — the
	// engine underneath decides what actually happens next.
	Resume(ctx context.Context, id core.RunID) error
	// ResolveHuman applies a reviewer's decision to a run parked in
	// awaiting_human and continues driving it.
	ResolveHuman(ctx context.Context, id core.RunID, d core.HumanDecision) error
	// Cancel cancels a non-terminal run.
	Cancel(ctx context.Context, id core.RunID) error
}

// Scenario is one selectable, named research-scenario preset offered on the
// new-run form. Label and Description are display copy; Name is the value
// passed back to Runner.Start.
type Scenario struct {
	Name        string
	Label       string
	Description string
}

// Server is the control room's HTTP surface: a thin read/render layer over
// core.Repository and Runner. Construct one with New and mount its Handler.
type Server struct {
	repo      core.Repository
	runner    Runner
	scenarios []Scenario
	tmpl      *template.Template
	static    fs.FS
	planner   *planner.Deterministic

	// scenarioOf remembers, best-effort and in-memory only, which scenario
	// name started each run. The domain's RunState (frozen, in internal/core)
	// deliberately carries no scenario field — a scenario is a launch-time
	// label, not durable run state — so the control room tracks it itself
	// purely for display on the index page. It is lost on process restart;
	// runs created before this process started (or in a previous process)
	// simply show as "unknown".
	mu         sync.RWMutex
	scenarioOf map[core.RunID]string
}

// New builds a Server. It parses the embedded template set once and panics if
// the templates fail to parse — a malformed template is a build-time defect,
// so the process fails fast at construction rather than on first request.
func New(repo core.Repository, runner Runner, scenarios []Scenario) *Server {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: static assets: " + err.Error())
	}
	tmpl := template.Must(template.ParseFS(templateFS,
		"templates/layout.html",
		"templates/index.html",
		"templates/run.html",
		"templates/partials/*.html",
	))
	return &Server{
		repo:       repo,
		runner:     runner,
		scenarios:  append([]Scenario(nil), scenarios...),
		tmpl:       tmpl,
		static:     sub,
		planner:    planner.New(),
		scenarioOf: make(map[core.RunID]string),
	}
}

// Handler returns the control room's full set of routes, mounted on an
// *http.ServeMux using Go 1.22 method+path patterns. See handlers.go for each
// handler's implementation.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /runs", s.handleCreateRun)

	mux.HandleFunc("GET /runs/{id}", s.handleRunPage)
	mux.HandleFunc("GET /runs/{id}/timeline", s.handleTimelinePartial)
	mux.HandleFunc("GET /runs/{id}/state", s.handleStatePartial)

	mux.HandleFunc("POST /runs/{id}/actions/resume", s.handleResume)
	mux.HandleFunc("POST /runs/{id}/actions/cancel", s.handleCancel)
	mux.HandleFunc("POST /runs/{id}/review/approve", s.handleApprove)
	mux.HandleFunc("POST /runs/{id}/review/reject", s.handleReject)

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleHealthz)

	mux.HandleFunc("GET /static/{file...}", s.handleStatic)

	return mux
}
