package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/diegoaleyvag/relay/internal/core"
)

// This file implements every route registered by Server.Handler. Each handler
// follows the same shape: load state fresh from core.Repository (never trust
// anything about a run's current phase from the request itself), build a safe
// view model, and render exactly one named template. POST action handlers
// additionally re-validate the loaded phase against the action being
// attempted and answer 409 Conflict if it is not legal — defense in depth, so
// a stale page or a hand-crafted request can never drive an illegal
// transition even if the UI itself would never have offered the control.

// render executes the named template into a buffer first, so a template
// execution failure can still produce a clean 500 rather than a
// partially-written response body with a 200 status already sent.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("web: render %s: %v", name, err)
		http.Error(w, "internal error rendering page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// writeLoadErr maps a core.Repository read error to the right HTTP status:
// core.ErrNotFound becomes 404, anything else is an unexpected 500.
func writeLoadErr(w http.ResponseWriter, err error) {
	if errors.Is(err, core.ErrNotFound) {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	log.Printf("web: load run: %v", err)
	http.Error(w, "failed to load run", http.StatusInternalServerError)
}

// loadTimeline reads a run's event history and findings and projects them,
// together with the run's own state, into a ViewTimeline.
func (s *Server) loadTimeline(ctx context.Context, run core.RunState) (ViewTimeline, error) {
	events, err := s.repo.Events(ctx, run.ID)
	if err != nil {
		return ViewTimeline{}, fmt.Errorf("events: %w", err)
	}
	findings, err := s.repo.Findings(ctx, run.ID)
	if err != nil {
		return ViewTimeline{}, fmt.Errorf("findings: %w", err)
	}
	return buildViewTimeline(run, events, findings), nil
}

// scenarioFor returns the best-effort scenario label recorded for id, or
// "unknown" if this process never recorded one (see Server.scenarioOf).
func (s *Server) scenarioFor(id core.RunID) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name, ok := s.scenarioOf[id]; ok {
		return name
	}
	return "unknown"
}

// setScenario records the scenario a newly created run was started with.
func (s *Server) setScenario(id core.RunID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarioOf[id] = name
}

// validScenario reports whether name names one of the scenarios the server
// was configured with. The create-run handler uses this as a defense-in-depth
// check: the <select> only ever offers these values, but a request is never
// trusted to have honored that.
func (s *Server) validScenario(name string) bool {
	for _, sc := range s.scenarios {
		if sc.Name == name {
			return true
		}
	}
	return false
}

// handleIndex renders the run list and the new-run form.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	runs, err := s.repo.ListRuns(r.Context())
	if err != nil {
		log.Printf("web: list runs: %v", err)
		http.Error(w, "failed to list runs", http.StatusInternalServerError)
		return
	}

	views := make([]ViewRun, 0, len(runs))
	for _, run := range runs {
		views = append(views, buildViewRun(run, s.scenarioFor(run.ID)))
	}

	s.render(w, http.StatusOK, "index", IndexPage{Runs: views, Scenarios: s.scenarios})
}

// handleCreateRun reads the scenario and require-review fields from the
// new-run form, starts the run through Runner, and redirects to its page.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	scenario := r.PostFormValue("scenario")
	if !s.validScenario(scenario) {
		http.Error(w, "unknown scenario", http.StatusBadRequest)
		return
	}
	requireReview := r.PostFormValue("require_review") != ""

	id, err := s.runner.Start(r.Context(), scenario, requireReview)
	if err != nil {
		log.Printf("web: start run: %v", err)
		http.Error(w, "failed to start run", http.StatusInternalServerError)
		return
	}
	s.setScenario(id, scenario)

	http.Redirect(w, r, "/runs/"+string(id), http.StatusSeeOther)
}

// handleRunPage renders the full single-run timeline page.
func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	id := core.RunID(r.PathValue("id"))
	run, err := s.repo.LoadState(r.Context(), id)
	if err != nil {
		writeLoadErr(w, err)
		return
	}

	tl, err := s.loadTimeline(r.Context(), run)
	if err != nil {
		log.Printf("web: load timeline: %v", err)
		http.Error(w, "failed to load run", http.StatusInternalServerError)
		return
	}

	page := RunPage{
		RunID:    string(id),
		State:    s.buildViewState(run),
		Timeline: tl,
	}
	s.render(w, http.StatusOK, "run", page)
}

// handleTimelinePartial renders just the #timeline fragment, for both the
// initial page load and its HTMX self-refresh polling.
func (s *Server) handleTimelinePartial(w http.ResponseWriter, r *http.Request) {
	id := core.RunID(r.PathValue("id"))
	run, err := s.repo.LoadState(r.Context(), id)
	if err != nil {
		writeLoadErr(w, err)
		return
	}

	tl, err := s.loadTimeline(r.Context(), run)
	if err != nil {
		log.Printf("web: load timeline: %v", err)
		http.Error(w, "failed to load timeline", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "timeline", tl)
}

// handleStatePartial renders just the #state fragment, for both the initial
// page load and its HTMX self-refresh polling.
func (s *Server) handleStatePartial(w http.ResponseWriter, r *http.Request) {
	id := core.RunID(r.PathValue("id"))
	run, err := s.repo.LoadState(r.Context(), id)
	if err != nil {
		writeLoadErr(w, err)
		return
	}
	s.render(w, http.StatusOK, "state_panel", s.buildViewState(run))
}

// handleAction is the shared re-validate -> invoke -> reload -> re-render path
// behind every POST action endpoint (resume, cancel, approve, reject). legal
// re-checks the run's CURRENT phase — loaded fresh from the repository, never
// trusted from the request — against the action being attempted: the same
// gate that disabled (or enabled) the button in the last rendered HTML is
// enforced again here, so a stale page or a forged request can never drive an
// illegal transition. On success it reloads the run (the invoked action may
// have changed its phase) and re-renders the #state partial, matching what
// HTMX expects to swap in.
func (s *Server) handleAction(
	w http.ResponseWriter,
	r *http.Request,
	legal func(core.Phase) bool,
	invoke func(context.Context, core.RunID) error,
) {
	id := core.RunID(r.PathValue("id"))
	run, err := s.repo.LoadState(r.Context(), id)
	if err != nil {
		writeLoadErr(w, err)
		return
	}
	if !legal(run.Phase) {
		http.Error(w, fmt.Sprintf("action not legal for run in phase %q", run.Phase), http.StatusConflict)
		return
	}

	if err := invoke(r.Context(), id); err != nil {
		log.Printf("web: action on %s: %v", id, err)
		http.Error(w, "action failed", http.StatusInternalServerError)
		return
	}

	fresh, err := s.repo.LoadState(r.Context(), id)
	if err != nil {
		writeLoadErr(w, err)
		return
	}
	s.render(w, http.StatusOK, "state_panel", s.buildViewState(fresh))
}

// handleResume answers POST /runs/{id}/actions/resume: legal only while the
// run is pending or backed off.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, resumeLegal, s.runner.Resume)
}

// handleCancel answers POST /runs/{id}/actions/cancel: legal for any
// non-terminal run.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, cancelLegal, s.runner.Cancel)
}

// handleApprove answers POST /runs/{id}/review/approve: legal only while the
// run is parked awaiting human review.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, reviewLegal, func(ctx context.Context, id core.RunID) error {
		return s.runner.ResolveHuman(ctx, id, core.DecisionApprove)
	})
}

// handleReject answers POST /runs/{id}/review/reject: legal only while the
// run is parked awaiting human review.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, reviewLegal, func(ctx context.Context, id core.RunID) error {
		return s.runner.ResolveHuman(ctx, id, core.DecisionReject)
	})
}

// handleHealthz answers GET /healthz and GET /readyz: a plain, unconditional
// 200. The control room has no external dependency it would block startup on
// (core.Repository and Runner are supplied already-constructed), so liveness
// and readiness are the same trivial check.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// handleStatic serves the embedded static assets (vendored HTMX, app.css)
// under /static/. http.ServeFileFS guards against path traversal in the
// {file...} wildcard.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, s.static, r.PathValue("file"))
}
