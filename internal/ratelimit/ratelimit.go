// Package ratelimit provides a per-tenant token bucket, used to stop one
// tenant's runaway agent from monopolising the server (and the Ollama calls
// behind it).
//
// ponytail: in-process state, so limits are per instance and reset on
// restart. That is exact for this deployment, which runs a single active
// instance — the failover design (deploy/failover-watch.sh) points DNS at one
// host at a time rather than load-balancing across several. If that ever
// changes, this needs shared state (Postgres counters or Redis) and every
// instance would otherwise allow the full rate independently.
package ratelimit

import (
	"sync"
	"time"
)

// bucket is one tenant's allowance. Tokens refill continuously rather than in
// fixed windows, so a tenant cannot burn a whole window's budget in the last
// second and another immediately at the start of the next.
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter hands out per-tenant token buckets. The zero value is not usable;
// call New.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	// idleTTL is how long an untouched bucket is kept before being swept.
	// Without this the map grows once per tenant that ever connects and never
	// shrinks — a slow leak on a server that stays up for months.
	idleTTL time.Duration
	now     func() time.Time // injectable for tests
}

// New returns a Limiter that forgets buckets untouched for idleTTL.
func New(idleTTL time.Duration) *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		idleTTL: idleTTL,
		now:     time.Now,
	}
}

// Allow reports whether the tenant may make a request now, consuming one
// token if so. perMinute <= 0 means unlimited, which is how the bootstrap
// tenant (and any plan configured with no limit) bypasses this entirely.
//
// burst is the bucket's capacity: a tenant that has been idle can spend up to
// this many requests at once before being held to the sustained rate. It is
// capped at perMinute so a limit of 10/min can never permit an instantaneous
// burst larger than its own minute's budget.
func (l *Limiter) Allow(tenant string, perMinute, burst int) bool {
	if perMinute <= 0 {
		return true
	}
	if burst <= 0 || burst > perMinute {
		burst = perMinute
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[tenant]
	if !ok {
		// A tenant's first request finds a full bucket, then spends one.
		l.buckets[tenant] = &bucket{tokens: float64(burst) - 1, last: now}
		l.sweepLocked(now)
		return true
	}

	// Refill for the time elapsed, capped at the bucket's capacity.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * (float64(perMinute) / 60.0)
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RetryAfter is how long the tenant should wait before a request would be
// admitted again. It is advisory — reported in the Retry-After header — and
// returns zero when a request would be allowed right now.
func (l *Limiter) RetryAfter(tenant string, perMinute int) time.Duration {
	if perMinute <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[tenant]
	if !ok {
		return 0
	}
	// Account for refill since the bucket was last touched, without mutating
	// it: this is a read-only question.
	tokens := b.tokens + l.now().Sub(b.last).Seconds()*(float64(perMinute)/60.0)
	if tokens >= 1 {
		return 0
	}
	need := 1 - tokens
	return time.Duration(need / (float64(perMinute) / 60.0) * float64(time.Second))
}

// sweepLocked drops buckets nobody has touched in idleTTL. Called on bucket
// creation rather than from a background goroutine: the map only grows when a
// new tenant appears, so that is exactly when it is worth checking, and it
// keeps the package free of lifecycle management (no Close, no leaked ticker).
func (l *Limiter) sweepLocked(now time.Time) {
	if l.idleTTL <= 0 {
		return
	}
	for tenant, b := range l.buckets {
		if now.Sub(b.last) > l.idleTTL {
			delete(l.buckets, tenant)
		}
	}
}

// Len reports how many buckets are currently tracked, for tests and
// diagnostics.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
