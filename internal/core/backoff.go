package core

import (
	"math"
	"time"
)

// Backoff is a deterministic bounded exponential-backoff policy. Delays are a
// pure function of (seed, attempt), so a run's retry timing is fully
// reproducible under a virtual test clock.
type Backoff struct {
	Base   time.Duration // delay for attempt 0 (e.g. 100ms)
	Factor float64       // growth factor per attempt (e.g. 2.0)
	Max    time.Duration // cap (e.g. 2s)
	Jitter bool          // apply deterministic full jitter
}

// DefaultBackoff is the standard policy for Relay runs.
func DefaultBackoff() Backoff {
	return Backoff{Base: 100 * time.Millisecond, Factor: 2.0, Max: 2 * time.Second, Jitter: true}
}

// Delay returns the backoff duration for the given zero-based attempt. With
// Jitter enabled it applies full jitter in [0, capped] seeded by (seed,
// attempt); without jitter it returns the capped exponential value exactly.
func (b Backoff) Delay(seed uint64, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := float64(b.Base) * math.Pow(b.Factor, float64(attempt))
	capped := math.Min(base, float64(b.Max))
	if capped < 0 || math.IsInf(capped, 0) {
		capped = float64(b.Max)
	}
	if !b.Jitter {
		return time.Duration(capped)
	}
	// Full jitter in [0, capped], deterministic in (seed, attempt).
	h := hash64(seed, uint64(uint32(attempt)))
	frac := float64(h%1_000_001) / 1_000_000.0
	return time.Duration(frac * capped)
}

// mix64 is splitmix64: a fast, well-distributed integer hash used to derive
// deterministic jitter without a stateful RNG.
func mix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func hash64(seed, n uint64) uint64 { return mix64(seed ^ mix64(n)) }
