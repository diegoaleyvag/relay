// Package clock provides the two implementations of core.Clock used across
// Relay: SystemClock, a thin wrapper over the wall clock for production, and
// ManualClock, a virtual clock that lets tests advance time deterministically
// without sleeping in real time.
package clock

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
)

// Compile-time assertions that both clocks satisfy core.Clock — the seam
// every other package (planner, engine, MCP adapter) programs against.
var (
	_ core.Clock = SystemClock{}
	_ core.Clock = (*ManualClock)(nil)
)

// SystemClock implements core.Clock over the real wall clock. It has no
// state and is safe for concurrent use by many goroutines (all methods
// simply delegate to the standard time package).
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep blocks for d or until ctx is done, whichever happens first. It
// returns nil if the timer fired and ctx.Err() if ctx was cancelled or its
// deadline elapsed first. A zero or negative d still goes through the timer,
// which the standard library fires as soon as possible.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// After returns a channel that receives the current time after d elapses.
// It is a direct pass-through to time.After.
func (SystemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// waiter is one pending ManualClock timer: it fires (sends the virtual time
// that satisfied it, exactly once, into a buffered channel of size 1) when
// Advance moves the virtual clock at or past deadline. The buffering means
// the firing goroutine (Advance) never blocks on a receiver that has gone
// away (e.g. a Sleep call released early by ctx cancellation).
type waiter struct {
	deadline time.Time
	ch       chan time.Time
	id       uint64 // insertion order, used only to break deadline ties
}

// ManualClock is a virtual implementation of core.Clock for deterministic
// tests. Time never advances on its own; callers move it forward explicitly
// with Advance, which releases every pending Sleep/After waiter whose
// deadline has been reached, in deadline order (earliest first, ties broken
// by registration order).
//
// All state is guarded by mu so ManualClock is safe for concurrent use: one
// goroutine may block in Sleep while another calls Advance to release it.
type ManualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	nextID  uint64
}

// NewManualClock returns a ManualClock whose virtual time starts at start.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start}
}

// Now returns the current virtual time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the virtual clock forward by d (d may be zero or negative;
// a negative d moves time backward, which is occasionally useful in tests
// but will not by itself release any waiter). After moving now, Advance
// fires — in deadline order — every pending waiter whose deadline is now at
// or before the new virtual time, then removes them from the pending set.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now

	var fired []*waiter
	kept := c.waiters[:0:0] // fresh backing array; never alias the fired entries
	for _, w := range c.waiters {
		if !w.deadline.After(now) {
			fired = append(fired, w)
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
	c.mu.Unlock()

	sort.Slice(fired, func(i, j int) bool {
		if fired[i].deadline.Equal(fired[j].deadline) {
			return fired[i].id < fired[j].id
		}
		return fired[i].deadline.Before(fired[j].deadline)
	})
	for _, w := range fired {
		w.ch <- now
	}
}

// register creates a waiter for deadline. If deadline has already been
// reached (<=  the current virtual time), the waiter fires immediately
// (without being added to the pending set) so Sleep/After callers never
// block on a deadline that is already in the past.
func (c *ManualClock) register(deadline time.Time) *waiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	w := &waiter{deadline: deadline, ch: make(chan time.Time, 1), id: c.nextID}
	if !deadline.After(c.now) {
		w.ch <- c.now
		return w
	}
	c.waiters = append(c.waiters, w)
	return w
}

// removeWaiter drops target from the pending set, if it is still present.
// It is a no-op if Advance has already fired (and removed) the waiter — that
// race is expected and harmless: whichever of "fired" or "cancelled" the
// select in Sleep observes first is the outcome that wins.
func (c *ManualClock) removeWaiter(target *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.waiters {
		if w == target {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			break
		}
	}
}

// Sleep blocks until d virtual-time has elapsed (as observed through a call
// to Advance) or ctx is done, whichever happens first. A zero or negative d
// returns immediately with a nil error, without registering a waiter.
func (c *ManualClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	c.mu.Lock()
	deadline := c.now.Add(d)
	c.mu.Unlock()

	w := c.register(deadline)
	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		c.removeWaiter(w)
		return ctx.Err()
	}
}

// After returns a channel that receives the virtual time once Advance moves
// the clock to or past now+d. Like time.After, the channel is buffered so a
// caller that never reads it does not block anything else.
func (c *ManualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	deadline := c.now.Add(d)
	c.mu.Unlock()
	w := c.register(deadline)
	return w.ch
}
