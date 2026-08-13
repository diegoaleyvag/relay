package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// seedRun builds a fresh RunState in the given phase, applying mutate (if
// non-nil) to customize it before the caller seeds it into a fakeRepo.
func seedRun(id core.RunID, phase core.Phase, mutate func(*core.RunState)) core.RunState {
	now := time.Now().UTC()
	s := core.NewRun(id, 1, false, time.Time{}, now)
	s.Phase = phase
	if mutate != nil {
		mutate(&s)
	}
	return s
}

// getBody performs a GET and returns the response's status and body text.
func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

// postForm performs a POST with an empty body (the action endpoints read no
// form fields) and returns the response's status and body text.
func postForm(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

func TestIndexListsSeededRunsAndForm(t *testing.T) {
	repo := newFakeRepo()
	run := seedRun("run-1", core.PhaseRunning, func(s *core.RunState) {
		s.Findings = []core.FindingRef{{SourceID: "src-1", Key: "k1", FindingID: "f1"}}
	})
	repo.seed(run)

	scenarios := []Scenario{{Name: "demo", Label: "Demo Scenario", Description: "a demo"}}
	srv := New(repo, &fakeRunner{}, scenarios)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := getBody(t, ts.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", status)
	}
	for _, want := range []string{
		"run-1",                 // run id listed
		"/runs/run-1",           // link to the run
		"Running",               // phase label
		"<td>1</td>",            // findings count column
		"Demo Scenario",         // scenario option label from the new-run form
		`<select id="scenario"`, // the new-run form itself
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q; body:\n%s", want, body)
		}
	}
}

func TestRunPageRendersPhaseAndFindingsCount(t *testing.T) {
	repo := newFakeRepo()
	run := seedRun("run-2", core.PhaseAwaitingHuman, func(s *core.RunState) {
		s.Findings = []core.FindingRef{
			{SourceID: "src-1", Key: "k1", FindingID: "f1"},
			{SourceID: "src-2", Key: "k2", FindingID: "f2"},
		}
		s.Review = &core.HumanReviewRef{
			Key: "k3", ReviewID: "rev-1",
			Reason:   "policy requires human review before completion",
			Severity: "medium", Status: core.ReviewPending,
		}
	})
	repo.seed(run)
	repo.findings["run-2"] = []core.FindingRow{
		{RunID: "run-2", Key: "k1", SourceID: "src-1", Claim: "TOP SECRET CLAIM CONTENT", At: time.Now()},
		{RunID: "run-2", Key: "k2", SourceID: "src-2", Claim: "TOP SECRET CLAIM CONTENT", At: time.Now()},
	}

	srv := New(repo, &fakeRunner{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := getBody(t, ts.URL+"/runs/run-2")
	if status != http.StatusOK {
		t.Fatalf("GET /runs/run-2: status = %d, want 200", status)
	}
	if !strings.Contains(body, "Awaiting human review") {
		t.Errorf("run page missing phase label; body:\n%s", body)
	}
	if !strings.Contains(body, "2 findings preserved") {
		t.Errorf("run page missing findings count; body:\n%s", body)
	}
	if !strings.Contains(body, "policy requires human review before completion") {
		t.Errorf("run page missing escalation reason; body:\n%s", body)
	}
	if strings.Contains(body, "TOP SECRET CLAIM CONTENT") {
		t.Errorf("run page leaked finding claim content; body:\n%s", body)
	}
}

func TestRunPageNotFound(t *testing.T) {
	srv := New(newFakeRepo(), &fakeRunner{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, _ := getBody(t, ts.URL+"/runs/does-not-exist")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestApproveIllegalWhenRunningReturns409(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-3", core.PhaseRunning, nil))
	runner := &fakeRunner{}
	srv := New(repo, runner, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, _ := postForm(t, ts.URL+"/runs/run-3/review/approve")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if len(runner.resolveCalls) != 0 {
		t.Fatalf("runner.ResolveHuman should not have been called for an illegal action, got %d calls", len(runner.resolveCalls))
	}
}

func TestResumeIllegalWhenRunningReturns409(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-3b", core.PhaseRunning, nil))
	runner := &fakeRunner{}
	srv := New(repo, runner, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, _ := postForm(t, ts.URL+"/runs/run-3b/actions/resume")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if len(runner.resumeCalls) != 0 {
		t.Fatalf("runner.Resume should not have been called for an illegal action")
	}
}

func TestCancelLegalCallsRunnerAndReturnsStatePartial(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-4", core.PhaseRunning, nil))
	runner := &fakeRunner{
		onCancel: func(id core.RunID) {
			r := repo.mustLoad(id)
			r.Phase = core.PhaseCancelled
			r.Version++
			repo.seed(r)
		},
	}
	srv := New(repo, runner, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := postForm(t, ts.URL+"/runs/run-4/actions/cancel")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", status, body)
	}
	if !strings.Contains(body, `id="state"`) {
		t.Errorf("expected #state partial in response; body:\n%s", body)
	}
	if !strings.Contains(body, "Cancelled") {
		t.Errorf("expected updated (cancelled) phase in response; body:\n%s", body)
	}
	if len(runner.cancelCalls) != 1 || runner.cancelCalls[0] != "run-4" {
		t.Fatalf("runner.Cancel calls = %v, want exactly [run-4]", runner.cancelCalls)
	}
}

func TestApproveLegalWhenAwaitingHuman(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-5", core.PhaseAwaitingHuman, func(s *core.RunState) {
		s.Review = &core.HumanReviewRef{Key: "k1", ReviewID: "rev-1", Reason: "r", Severity: "low", Status: core.ReviewPending}
	}))
	runner := &fakeRunner{
		onResolve: func(id core.RunID, d core.HumanDecision) {
			r := repo.mustLoad(id)
			r.Phase = core.PhaseRunning
			r.Version++
			repo.seed(r)
		},
	}
	srv := New(repo, runner, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := postForm(t, ts.URL+"/runs/run-5/review/approve")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", status, body)
	}
	if len(runner.resolveCalls) != 1 || runner.resolveCalls[0] != core.DecisionApprove {
		t.Fatalf("runner.ResolveHuman calls = %v, want exactly [approve]", runner.resolveCalls)
	}
}

func TestCreateRunRedirectsToNewRun(t *testing.T) {
	repo := newFakeRepo()
	runner := &fakeRunner{nextID: "run-6"}
	scenarios := []Scenario{{Name: "demo", Label: "Demo"}}
	srv := New(repo, runner, scenarios)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	form := url.Values{"scenario": {"demo"}, "require_review": {"true"}}
	resp, err := client.PostForm(ts.URL+"/runs", form)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/runs/run-6" {
		t.Fatalf("Location = %q, want /runs/run-6", loc)
	}
	if runner.startCalls != 1 {
		t.Fatalf("runner.Start calls = %d, want 1", runner.startCalls)
	}
}

func TestCreateRunUnknownScenarioIsBadRequest(t *testing.T) {
	repo := newFakeRepo()
	runner := &fakeRunner{nextID: "run-7"}
	scenarios := []Scenario{{Name: "demo", Label: "Demo"}}
	srv := New(repo, runner, scenarios)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	form := url.Values{"scenario": {"not-a-real-scenario"}}
	resp, err := client.PostForm(ts.URL+"/runs", form)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if runner.startCalls != 0 {
		t.Fatalf("runner.Start should not have been called for an unknown scenario")
	}
}

func TestTimelineShowsInjectedFaultMarkerAndCode(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-8", core.PhaseBackoff, nil))
	repo.events["run-8"] = []core.Event{
		{
			RunID: "run-8", Seq: 1, Version: 1, Kind: core.EventToolFailed,
			Summary:  "tool failed (retryable)",
			Evidence: core.Redacted(`{"tool":"read_source","code":"TIMEOUT","injected":true,"attempt":0}`),
			At:       time.Now(),
		},
	}

	srv := New(repo, &fakeRunner{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := getBody(t, ts.URL+"/runs/run-8/timeline")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "fault injected here") {
		t.Errorf("expected injected-fault marker; body:\n%s", body)
	}
	if !strings.Contains(body, "TIMEOUT") {
		t.Errorf("expected error code TIMEOUT; body:\n%s", body)
	}
}

func TestTimelinePartialOmitsPollingTriggerWhenTerminal(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-9", core.PhaseSucceeded, nil))
	srv := New(repo, &fakeRunner{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := getBody(t, ts.URL+"/runs/run-9/timeline")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(body, "hx-trigger") {
		t.Errorf("terminal run's timeline partial should omit hx-trigger polling; body:\n%s", body)
	}
}

func TestTimelinePartialKeepsPollingTriggerWhenNonTerminal(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-10", core.PhaseRunning, nil))
	srv := New(repo, &fakeRunner{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := getBody(t, ts.URL+"/runs/run-10/timeline")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, `hx-trigger="every 1s"`) {
		t.Errorf("non-terminal run's timeline partial should self-poll; body:\n%s", body)
	}
}

func TestHealthzAndReadyz(t *testing.T) {
	srv := New(newFakeRepo(), &fakeRunner{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/healthz", "/readyz"} {
		status, _ := getBody(t, ts.URL+path)
		if status != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, status)
		}
	}
}

func TestStaticServesVendoredHTMX(t *testing.T) {
	srv := New(newFakeRepo(), &fakeRunner{}, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status, body := getBody(t, ts.URL+"/static/htmx.min.js")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "htmx") {
		t.Errorf("expected htmx source to mention htmx; got %d bytes", len(body))
	}
}
