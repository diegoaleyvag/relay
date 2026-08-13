package scenarios

import "testing"

func TestAllCoversTheSixScenarios(t *testing.T) {
	got := All()
	if len(got) < 6 {
		t.Fatalf("expected at least 6 scenarios, got %d", len(got))
	}
	for _, name := range []string{"happy", "timeout", "malformed", "duplicate", "permission", "review"} {
		if _, ok := Get(name); !ok {
			t.Errorf("missing scenario %q", name)
		}
	}
}

func TestGetDefaultsToHappy(t *testing.T) {
	s, ok := Get("")
	if !ok || s.Name != "happy" {
		t.Fatalf("empty name should resolve to happy, got %q (ok=%v)", s.Name, ok)
	}
	if _, ok := Get("does-not-exist"); ok {
		t.Fatal("unknown scenario must not resolve")
	}
}

func TestScenarioShapes(t *testing.T) {
	if r, _ := Get("review"); !r.RequireReview {
		t.Error("review scenario must require review")
	}
	for _, name := range []string{"timeout", "malformed", "duplicate", "permission"} {
		if s, _ := Get(name); len(s.Plan().Faults) == 0 {
			t.Errorf("scenario %q should carry a fault", name)
		}
	}
	if h, _ := Get("happy"); len(h.Plan().Faults) != 0 || h.RequireReview {
		t.Error("happy scenario should be fault-free and not require review")
	}
}
