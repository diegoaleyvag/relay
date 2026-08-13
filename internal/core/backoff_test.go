package core

import (
	"testing"
	"time"
)

func TestBackoffNoJitter(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Factor: 2.0, Max: 2 * time.Second, Jitter: false}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 100 * time.Millisecond},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{4, 1600 * time.Millisecond},
		{5, 2 * time.Second},  // 3200ms capped
		{10, 2 * time.Second}, // capped
	}
	for _, c := range cases {
		if got := b.Delay(0, c.attempt); got != c.want {
			t.Errorf("attempt %d: got %v want %v", c.attempt, got, c.want)
		}
	}
}

func TestBackoffJitterDeterministic(t *testing.T) {
	b := DefaultBackoff()
	for attempt := 0; attempt < 8; attempt++ {
		a := b.Delay(42, attempt)
		again := b.Delay(42, attempt)
		if a != again {
			t.Fatalf("attempt %d not deterministic: %v vs %v", attempt, a, again)
		}
		capped := b.Delay(0, attempt) // no jitter path would exceed; recompute bound
		_ = capped
		if a < 0 || a > b.Max {
			t.Fatalf("attempt %d jitter out of [0,Max]: %v", attempt, a)
		}
	}
}

func TestBackoffJitterVariesBySeed(t *testing.T) {
	b := DefaultBackoff()
	// Different seeds at the same attempt should (almost always) differ; assert
	// at least one attempt differs so we know the seed is actually used.
	diff := false
	for attempt := 0; attempt < 8; attempt++ {
		if b.Delay(1, attempt) != b.Delay(2, attempt) {
			diff = true
			break
		}
	}
	if !diff {
		t.Fatal("jitter appears independent of the seed")
	}
}
