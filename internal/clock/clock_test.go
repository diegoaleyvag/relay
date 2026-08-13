package clock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitForPending polls (real time, briefly) until c has exactly n waiters
// registered, or fails the test. It exists so tests can deterministically
// know a Sleep/After call has registered its waiter before calling Advance,
// without reaching into timing assumptions about goroutine scheduling.
func waitForPending(t *testing.T, c *ManualClock, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.waiters)
		c.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending waiter(s)", n)
}

func TestManualClockSleepUnblocksOnAdvance(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	done := make(chan error, 1)
	go func() {
		done <- c.Sleep(context.Background(), time.Second)
	}()

	waitForPending(t, c, 1)
	c.Advance(time.Second)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sleep returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sleep did not unblock after Advance")
	}
}

func TestManualClockSleepUnblocksOnPartialAdvances(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	done := make(chan error, 1)
	go func() {
		done <- c.Sleep(context.Background(), time.Second)
	}()

	waitForPending(t, c, 1)
	c.Advance(400 * time.Millisecond)

	select {
	case err := <-done:
		t.Fatalf("Sleep unblocked early with %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	c.Advance(600 * time.Millisecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sleep returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sleep did not unblock once cumulative Advance reached the deadline")
	}
}

func TestManualClockSleepZeroOrNegativeDurationReturnsImmediately(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	if err := c.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v, want nil", err)
	}
	if err := c.Sleep(context.Background(), -time.Second); err != nil {
		t.Fatalf("Sleep(-1s) = %v, want nil", err)
	}
	c.mu.Lock()
	n := len(c.waiters)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("Sleep with non-positive duration registered %d waiter(s), want 0", n)
	}
}

func TestManualClockAfterFiresOnAdvance(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	ch := c.After(500 * time.Millisecond)

	select {
	case <-ch:
		t.Fatal("After fired before any Advance")
	default:
	}

	c.Advance(200 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("After fired before its deadline was reached")
	default:
	}

	c.Advance(300 * time.Millisecond)
	select {
	case got := <-ch:
		want := c.Now()
		if !got.Equal(want) {
			t.Fatalf("After delivered %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("After did not fire once Advance crossed the deadline")
	}
}

func TestManualClockAfterFiresImmediatelyForPastDeadline(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	ch := c.After(0)
	select {
	case <-ch:
	default:
		t.Fatal("After(0) did not fire immediately")
	}
}

// TestManualClockMultipleWaitersAllFireOnAdvance covers several pending
// waiters with distinct deadlines released by one Advance call. (The
// implementation additionally fires them in deadline order internally, but
// that ordering is not asserted here via goroutine scheduling: once each
// waiter's buffered channel is signalled, the order in which independent
// consumer goroutines observe and act on it is up to the Go scheduler, not a
// property the clock contract makes — or could make — externally
// observable.)
func TestManualClockMultipleWaitersAllFireOnAdvance(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	ch1 := c.After(100 * time.Millisecond)
	ch2 := c.After(200 * time.Millisecond)
	ch3 := c.After(300 * time.Millisecond)

	waitForPending(t, c, 3)
	c.Advance(300 * time.Millisecond)

	for i, ch := range []<-chan time.Time{ch1, ch2, ch3} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d did not fire after Advance", i+1)
		}
	}
}

func TestManualClockSleepReleasedByContextCancel(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.Sleep(ctx, time.Minute)
	}()

	waitForPending(t, c, 1)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sleep returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sleep did not unblock after ctx cancel")
	}

	// The cancelled waiter must not linger in the pending set.
	c.mu.Lock()
	n := len(c.waiters)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending waiters after cancel = %d, want 0", n)
	}
}

func TestManualClockSleepAlreadyCancelledContext(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Sleep(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep returned %v, want context.Canceled", err)
	}
}

func TestSystemClockSleepRespectsShortTimeout(t *testing.T) {
	c := SystemClock{}
	start := time.Now()
	if err := c.Sleep(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("Sleep returned %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("Sleep returned after %v, want >= 20ms", elapsed)
	}
}

func TestSystemClockSleepReleasedByContextCancel(t *testing.T) {
	c := SystemClock{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := c.Sleep(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep returned %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Sleep took %v to observe cancellation, want well under 2s", elapsed)
	}
}

func TestSystemClockNowAndAfter(t *testing.T) {
	c := SystemClock{}
	before := time.Now()
	if got := c.Now(); got.Before(before) {
		t.Fatalf("Now() = %v, want >= %v", got, before)
	}
	select {
	case <-c.After(10 * time.Millisecond):
	case <-time.After(2 * time.Second):
		t.Fatal("After did not fire")
	}
}
