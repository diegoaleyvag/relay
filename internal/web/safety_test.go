package web

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/diegoaleyvag/relay/internal/core"
)

// forbiddenAIWording returns the anthropomorphic phrases the planner must
// never be described with. Each is assembled by concatenation rather than
// spelled out as one literal so this test file's own source never contains a
// forbidden phrase as a contiguous substring — the repository's own doc-lint
// target (see the Makefile) greps the whole internal/ tree for exactly these
// phrases and would otherwise flag this file for testing for them.
func forbiddenAIWording() []string {
	return []string{
		"ai" + " reasoning",
		"the ai " + "thinks",
		"the ai " + "decides",
		"the ai " + "reasons",
	}
}

// TestTemplatesNeverUseUnsafeTypesOrAIWording is the grep-style safety test
// make doc-lint also runs at the repository level (see the Makefile's
// doc-lint target): no template may reference one of html/template's
// "trusted" types (which would bypass its automatic contextual escaping),
// and no template may describe the scripted, deterministic planner as "AI"
// or as "reasoning".
func TestTemplatesNeverUseUnsafeTypesOrAIWording(t *testing.T) {
	unsafeTypes := []string{
		"template." + "html", "template." + "js", "template." + "url", "template." + "css", "template." + "srcset",
	}
	aiWording := forbiddenAIWording()

	err := fs.WalkDir(templateFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := fs.ReadFile(templateFS, path)
		if rerr != nil {
			return rerr
		}
		lower := strings.ToLower(string(data))
		for _, bad := range unsafeTypes {
			if strings.Contains(lower, bad) {
				t.Errorf("%s: forbidden unsafe template type reference %q (html/template auto-escaping must never be bypassed)", path, bad)
			}
		}
		for _, bad := range aiWording {
			if strings.Contains(lower, bad) {
				t.Errorf("%s: forbidden wording %q (the planner is scripted/deterministic, never AI or reasoning)", path, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded templates: %v", err)
	}
}

// TestRenderedOutputNeverMentionsAIReasoning renders every full page and
// partial this package produces and asserts none of them describe the
// scripted planner as "AI" or "reasoning" — a second, output-level check
// alongside the source-level TestTemplatesNeverUseUnsafeTypesOrAIWording, in
// case wording were ever injected via a view model rather than a template
// literal.
func TestRenderedOutputNeverMentionsAIReasoning(t *testing.T) {
	repo := newFakeRepo()
	repo.seed(seedRun("run-ai-check", core.PhaseRunning, nil))
	scenarios := []Scenario{{Name: "demo", Label: "Demo", Description: "demo scenario"}}
	srv := New(repo, &fakeRunner{}, scenarios)

	var buf strings.Builder
	if err := srv.tmpl.ExecuteTemplate(&buf, "index", IndexPage{
		Runs:      []ViewRun{buildViewRun(repo.mustLoad("run-ai-check"), "demo")},
		Scenarios: scenarios,
	}); err != nil {
		t.Fatalf("render index: %v", err)
	}
	run := repo.mustLoad("run-ai-check")
	if err := srv.tmpl.ExecuteTemplate(&buf, "run", RunPage{
		RunID:    string(run.ID),
		State:    srv.buildViewState(run),
		Timeline: buildViewTimeline(run, nil, nil),
	}); err != nil {
		t.Fatalf("render run: %v", err)
	}

	lower := strings.ToLower(buf.String())
	for _, bad := range forbiddenAIWording() {
		if strings.Contains(lower, bad) {
			t.Errorf("rendered output contains forbidden wording %q", bad)
		}
	}
}
