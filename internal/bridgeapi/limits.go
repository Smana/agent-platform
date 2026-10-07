// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"sync"
	"time"
)

// Default per-principal limits, §4's per-principal numbers: 10 requests a
// second with a burst of 20, and 10 at once. A bridge sends one batch at a time,
// about once a second, so only a runaway or a stolen token reaches them.
const (
	defaultRate     = 10
	defaultBurst    = 20
	defaultInFlight = 10
	// maxPrincipals bounds the limiter's memory. Idle principals are swept once
	// it is reached; past it, a new principal waits for one to go idle.
	maxPrincipals = 10_000
)

// Limits bound each principal on the events endpoint and the system API. A
// zero field takes its default.
type Limits struct {
	Rate     float64 // requests a second, refilled continuously
	Burst    int     // requests at once after a quiet spell
	InFlight int     // requests being served at once
}

func (l Limits) withDefaults() Limits {
	if l.Rate <= 0 {
		l.Rate = defaultRate
	}
	if l.Burst <= 0 {
		l.Burst = defaultBurst
	}
	if l.InFlight <= 0 {
		l.InFlight = defaultInFlight
	}
	return l
}

// limiter is a token bucket and an in-flight count per principal.
type limiter struct {
	limits Limits
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	last     time.Time
	inFlight int
}

func newLimiter(l Limits, now func() time.Time) *limiter {
	return &limiter{limits: l.withDefaults(), now: now, buckets: map[string]*bucket{}}
}

// acquire admits one request of principal, or refuses it. release must be called
// once the request is served.
func (l *limiter) acquire(principal string) (release func(), ok bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[principal]
	if b == nil {
		if len(l.buckets) >= maxPrincipals && !l.sweep(now) {
			return nil, false
		}
		b = &bucket{tokens: float64(l.limits.Burst), last: now}
		l.buckets[principal] = b
	}
	l.refill(b, now)
	if b.inFlight >= l.limits.InFlight || b.tokens < 1 {
		return nil, false
	}
	b.tokens--
	b.inFlight++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		b.inFlight--
	}, true
}

func (l *limiter) refill(b *bucket, now time.Time) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(float64(l.limits.Burst), b.tokens+elapsed*l.limits.Rate)
		b.last = now
	}
}

// sweep forgets the principals a fresh bucket would stand in for: idle and full.
// It reports whether any room was made.
func (l *limiter) sweep(now time.Time) bool {
	before := len(l.buckets)
	for p, b := range l.buckets {
		l.refill(b, now)
		if b.inFlight == 0 && b.tokens >= float64(l.limits.Burst) {
			delete(l.buckets, p)
		}
	}
	return len(l.buckets) < before
}
