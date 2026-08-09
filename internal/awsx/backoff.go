package awsx

import (
	"math/rand/v2"
	"time"
)

// Backoff is exponential backoff with full jitter: each delay is drawn
// uniformly from [0, min(Max, Base·2^attempt)). Reset after a success.
type Backoff struct {
	Base, Max time.Duration
	// Rand returns a value in [0,1); nil uses math/rand/v2.
	Rand    func() float64
	attempt int
}

// NewReceiveBackoff returns the §9.2 receive-error policy: 1s → 60s.
func NewReceiveBackoff() *Backoff { return &Backoff{Base: time.Second, Max: time.Minute} }

// Next returns the delay before the next retry and advances the attempt.
func (b *Backoff) Next() time.Duration {
	ceil := b.Max
	if b.attempt < 32 {
		if c := b.Base << b.attempt; c > 0 && c < b.Max {
			ceil = c
		}
	}
	b.attempt++
	r := b.Rand
	if r == nil {
		r = rand.Float64
	}
	return time.Duration(r() * float64(ceil))
}

// Ceiling returns the upper bound of the next delay without advancing.
func (b *Backoff) Ceiling() time.Duration {
	if b.attempt < 32 {
		if c := b.Base << b.attempt; c > 0 && c < b.Max {
			return c
		}
	}
	return b.Max
}

// Reset returns the backoff to its first attempt.
func (b *Backoff) Reset() { b.attempt = 0 }
