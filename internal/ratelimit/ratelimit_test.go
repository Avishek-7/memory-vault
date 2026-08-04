package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock lets these tests exercise refill and eviction without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestLimiter(idleTTL time.Duration) (*Limiter, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := New(idleTTL)
	l.now = clock.Now
	return l, clock
}

func TestUnlimitedWhenRateIsZeroOrNegative(t *testing.T) {
	l, _ := newTestLimiter(time.Hour)
	for i := 0; i < 1000; i++ {
		if !l.Allow("t", 0, 0) {
			t.Fatalf("request %d denied with perMinute=0, which must mean unlimited", i)
		}
	}
	// An unlimited tenant should not even allocate a bucket — this is the
	// path the bootstrap tenant takes on every single request.
	if l.Len() != 0 {
		t.Errorf("unlimited tenant allocated %d bucket(s), want 0", l.Len())
	}
}

func TestBurstThenSustainedRate(t *testing.T) {
	l, clock := newTestLimiter(time.Hour)
	const perMinute = 60 // one token per second

	// A fresh bucket is full, so the whole burst is available immediately.
	for i := 0; i < perMinute; i++ {
		if !l.Allow("t", perMinute, perMinute) {
			t.Fatalf("request %d of the initial burst was denied", i+1)
		}
	}
	if l.Allow("t", perMinute, perMinute) {
		t.Fatal("request past the burst was allowed; the bucket is not being drained")
	}

	// One second refills exactly one token at 60/min.
	clock.Advance(time.Second)
	if !l.Allow("t", perMinute, perMinute) {
		t.Error("a token should have refilled after one second")
	}
	if l.Allow("t", perMinute, perMinute) {
		t.Error("only one token should have refilled after one second")
	}
}

func TestRefillIsCappedAtBurst(t *testing.T) {
	l, clock := newTestLimiter(time.Hour)
	const perMinute, burst = 60, 10

	for i := 0; i < burst; i++ {
		if !l.Allow("t", perMinute, burst) {
			t.Fatalf("request %d within burst denied", i+1)
		}
	}
	// Idle for an hour: refill must not accumulate beyond the bucket size,
	// or a long-idle tenant could dump an unbounded flood in one go.
	clock.Advance(time.Hour)
	allowed := 0
	for i := 0; i < burst*10; i++ {
		if l.Allow("t", perMinute, burst) {
			allowed++
		}
	}
	if allowed != burst {
		t.Errorf("after a long idle period %d requests were allowed at once, want exactly the burst %d", allowed, burst)
	}
}

func TestBurstCannotExceedTheMinuteBudget(t *testing.T) {
	l, _ := newTestLimiter(time.Hour)
	const perMinute = 10
	// Ask for a burst far larger than the sustained budget; it must be capped
	// at perMinute, not honoured.
	allowed := 0
	for i := 0; i < 100; i++ {
		if l.Allow("t", perMinute, 1000) {
			allowed++
		}
	}
	if allowed != perMinute {
		t.Errorf("burst=1000 with perMinute=10 allowed %d requests, want %d", allowed, perMinute)
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	l, _ := newTestLimiter(time.Hour)
	const perMinute = 5

	for i := 0; i < perMinute; i++ {
		l.Allow("noisy", perMinute, perMinute)
	}
	if l.Allow("noisy", perMinute, perMinute) {
		t.Fatal("the noisy tenant was not actually exhausted, so this test proves nothing")
	}
	if !l.Allow("quiet", perMinute, perMinute) {
		t.Error("one tenant exhausting its bucket blocked another tenant")
	}
}

func TestIdleBucketsAreEvicted(t *testing.T) {
	l, clock := newTestLimiter(time.Minute)

	l.Allow("old", 60, 60)
	if l.Len() != 1 {
		t.Fatalf("Len = %d after one tenant, want 1", l.Len())
	}

	// Past the TTL, a new tenant's arrival sweeps the stale one.
	clock.Advance(2 * time.Minute)
	l.Allow("new", 60, 60)
	if l.Len() != 1 {
		t.Errorf("Len = %d, want 1: the idle bucket should have been swept when a new tenant appeared", l.Len())
	}
	// And the survivor must be the recent one, not the stale one.
	if _, stale := l.buckets["old"]; stale {
		t.Error("the idle tenant's bucket survived the sweep")
	}
}

func TestRetryAfterShrinksAsTokensRefill(t *testing.T) {
	l, clock := newTestLimiter(time.Hour)
	const perMinute = 60 // one token per second

	if d := l.RetryAfter("unknown", perMinute); d != 0 {
		t.Errorf("RetryAfter for a tenant with no bucket = %v, want 0", d)
	}

	for i := 0; i < perMinute; i++ {
		l.Allow("t", perMinute, perMinute)
	}
	full := l.RetryAfter("t", perMinute)
	if full <= 0 || full > time.Second+time.Millisecond {
		t.Fatalf("RetryAfter on an exhausted bucket = %v, want just under a second at 60/min", full)
	}

	clock.Advance(500 * time.Millisecond)
	half := l.RetryAfter("t", perMinute)
	if half >= full {
		t.Errorf("RetryAfter did not shrink as tokens refilled: %v then %v", full, half)
	}

	// RetryAfter must not consume tokens — it answers a question, and a
	// client polling it should not lose its allowance by asking.
	clock.Advance(time.Second)
	if d := l.RetryAfter("t", perMinute); d != 0 {
		t.Errorf("RetryAfter = %v once a token is available, want 0", d)
	}
	if !l.Allow("t", perMinute, perMinute) {
		t.Error("the token RetryAfter reported as available was not actually there")
	}
}

func TestConcurrentAccessIsSafe(t *testing.T) {
	l, _ := newTestLimiter(time.Hour)
	const goroutines, each = 16, 100

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if l.Allow("shared", 100, 100) {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	// The clock never advances here, so no tokens refill: exactly the burst
	// must get through, no matter how the goroutines interleave. Run with
	// -race to make this meaningful.
	if allowed != 100 {
		t.Errorf("%d requests allowed under concurrency, want exactly the burst of 100", allowed)
	}
}

// TestShortIdleTTLIsClamped guards against eviction becoming a way to earn
// tokens. Deleting a bucket is equivalent to refilling it instantly, since
// the next request from that tenant gets a fresh full one — so an idleTTL
// shorter than a bucket's natural refill time would let an exhausted tenant
// come back early just because another tenant's request triggered a sweep.
func TestShortIdleTTLIsClamped(t *testing.T) {
	l, clock := newTestLimiter(10 * time.Second)
	if l.idleTTL < minIdleTTL {
		t.Fatalf("New kept idleTTL at %v, want it clamped to at least %v", l.idleTTL, minIdleTTL)
	}

	// One request per minute: after spending it, the tenant must wait a full
	// minute even if other tenants keep arriving and triggering sweeps.
	const perMinute = 1
	if !l.Allow("a", perMinute, 1) {
		t.Fatal("first request denied")
	}
	clock.Advance(11 * time.Second)
	l.Allow("b", perMinute, 1) // a new tenant triggers sweepLocked
	if l.Allow("a", perMinute, 1) {
		t.Error("tenant A got a fresh allowance 11s into a 60s refill because its bucket was evicted")
	}

	// Past the real refill period it is allowed again, as normal.
	clock.Advance(60 * time.Second)
	if !l.Allow("a", perMinute, 1) {
		t.Error("tenant A was still blocked after a full minute")
	}
}

func TestZeroIdleTTLDisablesEviction(t *testing.T) {
	l, clock := newTestLimiter(0)
	l.Allow("a", 60, 60)
	clock.Advance(24 * time.Hour)
	l.Allow("b", 60, 60)
	if l.Len() != 2 {
		t.Errorf("Len = %d with eviction disabled, want both buckets retained", l.Len())
	}
}
