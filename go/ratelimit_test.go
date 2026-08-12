package x402

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handClock is a clock the test advances itself, so a throttle decision is a
// function of the requests made and never of how long the test took to run.
// Guarded because the concurrency test reads it from many goroutines.
type handClock struct {
	mu sync.Mutex
	t  time.Time
}

func newHandClock() *handClock {
	return &handClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *handClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *handClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// throttledFacilitator builds a facilitator whose throttle is driven by clock.
func throttledFacilitator(clock *handClock, burst int) http.Handler {
	return NewFacilitator(&fakeNode{}, "dev", WithRateLimit(RateLimit{
		PerSecond: DefaultRatePerSecond,
		Burst:     burst,
		Now:       clock.now,
	})).Handler()
}

// verifyFrom posts a valid verify request as if it came from remoteAddr, with
// the given headers set.
func verifyFrom(t *testing.T, h http.Handler, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/verify",
		bytes.NewReader(facilitatorRequestBody(t, reqFixture(), txFixture(t, nil))))
	req.RemoteAddr = remoteAddr
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const testPeer = "203.0.113.7:44321"

// TestFacilitator_RateLimitSpendsBurstThenRefuses pins the throttle in front of
// the chain-touching endpoints: /verify runs a full chain simulation per request
// and needs no credential, so an unthrottled endpoint turns cheap garbage into
// unbounded node load.
func TestFacilitator_RateLimitSpendsBurstThenRefuses(t *testing.T) {
	for _, endpoint := range []string{"/verify", "/settle"} {
		t.Run(endpoint, func(t *testing.T) {
			const burst = 3
			clock := newHandClock()
			h := throttledFacilitator(clock, burst)

			for i := range burst {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, endpoint,
					bytes.NewReader(facilitatorRequestBody(t, reqFixture(), txFixture(t, nil))))
				req.RemoteAddr = testPeer
				h.ServeHTTP(rec, req)
				require.Equal(t, http.StatusOK, rec.Code, "request %d is inside the burst", i+1)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, endpoint,
				bytes.NewReader(facilitatorRequestBody(t, reqFixture(), txFixture(t, nil))))
			req.RemoteAddr = testPeer
			h.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusTooManyRequests, rec.Code, "the burst is spent")
		})
	}
}

func TestFacilitator_RateLimitRefills(t *testing.T) {
	clock := newHandClock()
	h := throttledFacilitator(clock, 1)

	require.Equal(t, http.StatusOK, verifyFrom(t, h, testPeer, nil).Code)
	require.Equal(t, http.StatusTooManyRequests, verifyFrom(t, h, testPeer, nil).Code)

	// Less than one token's worth of time buys nothing at 10/s.
	clock.advance(50 * time.Millisecond)
	assert.Equal(t, http.StatusTooManyRequests, verifyFrom(t, h, testPeer, nil).Code)

	clock.advance(time.Second)
	assert.Equal(t, http.StatusOK, verifyFrom(t, h, testPeer, nil).Code)
}

// TestFacilitator_RateLimitIsPerPeer proves one flooding caller cannot deny the
// service to everyone else.
func TestFacilitator_RateLimitIsPerPeer(t *testing.T) {
	clock := newHandClock()
	h := throttledFacilitator(clock, 1)

	require.Equal(t, http.StatusOK, verifyFrom(t, h, "203.0.113.7:1111", nil).Code)
	require.Equal(t, http.StatusTooManyRequests, verifyFrom(t, h, "203.0.113.7:2222", nil).Code,
		"a second port is the same peer")

	assert.Equal(t, http.StatusOK, verifyFrom(t, h, "198.51.100.2:1111", nil).Code,
		"another peer has its own bucket")
}

// TestFacilitator_RateLimitIgnoresForwardedHeaders pins the key on the real peer
// address. A caller sets X-Forwarded-For and X-Real-IP freely, so honoring
// either would let one flooder buy unlimited quota by rotating a header value.
func TestFacilitator_RateLimitIgnoresForwardedHeaders(t *testing.T) {
	clock := newHandClock()
	h := throttledFacilitator(clock, 1)

	require.Equal(t, http.StatusOK, verifyFrom(t, h, testPeer, nil).Code)
	for _, claimed := range []string{"198.51.100.1", "198.51.100.2", "10.0.0.1, 198.51.100.3"} {
		assert.Equal(t, http.StatusTooManyRequests,
			verifyFrom(t, h, testPeer, map[string]string{"X-Forwarded-For": claimed}).Code,
			"a claimed X-Forwarded-For must buy no quota")
		assert.Equal(t, http.StatusTooManyRequests,
			verifyFrom(t, h, testPeer, map[string]string{"X-Real-IP": claimed}).Code,
			"a claimed X-Real-IP must buy no quota")
	}
}

// TestFacilitator_SupportedIsNotRateLimited leaves the static endpoint open: it
// answers from a constant and touches no chain.
func TestFacilitator_SupportedIsNotRateLimited(t *testing.T) {
	clock := newHandClock()
	h := throttledFacilitator(clock, 1)

	require.Equal(t, http.StatusOK, verifyFrom(t, h, testPeer, nil).Code)
	require.Equal(t, http.StatusTooManyRequests, verifyFrom(t, h, testPeer, nil).Code)

	for range 5 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/supported", nil)
		req.RemoteAddr = testPeer
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	}
}

// TestRateLimiter_SweepsFullBuckets pins the bound on the map: it is keyed on
// caller-chosen source addresses, so a flood from rotating addresses would grow
// it without bound.
func TestRateLimiter_SweepsFullBuckets(t *testing.T) {
	t.Run("a bucket back at capacity is dropped", func(t *testing.T) {
		clock := newHandClock()
		l := newRateLimiter(RateLimit{PerSecond: DefaultRatePerSecond, Burst: 2, Now: clock.now})

		require.True(t, l.allow("198.51.100.1"))
		require.True(t, l.allow("198.51.100.2"))
		require.Len(t, l.buckets, 2)

		// Past the sweep trigger, with both buckets long since refilled.
		clock.advance(sweepInterval + time.Second)
		require.True(t, l.allow("198.51.100.3"))
		assert.Len(t, l.buckets, 1,
			"a bucket refilled to capacity is indistinguishable from an unseen one")
	})

	t.Run("a bucket still short of capacity is kept", func(t *testing.T) {
		clock := newHandClock()
		// One token per 100s: a spent bucket is still short of capacity when the
		// sweep runs, so forgetting it would hand back quota.
		l := newRateLimiter(RateLimit{PerSecond: 0.01, Burst: 2, Now: clock.now})

		require.True(t, l.allow("198.51.100.1"))
		require.True(t, l.allow("198.51.100.1"))
		require.False(t, l.allow("198.51.100.1"))

		clock.advance(sweepInterval + time.Second)
		require.True(t, l.allow("198.51.100.2"))
		assert.Len(t, l.buckets, 2, "the sweep drops only what it can forget")
		assert.Contains(t, l.buckets, "198.51.100.1")
	})
}

// TestPeerAddress_CollapsesIPv6ToItsPrefix pins the throttle key at the unit an
// operator actually assigns.
//
// A host with a routed IPv6 /64 — the standard residential and cloud allocation —
// sources every request from a distinct /128 at no cost. Keying the full address
// therefore made the throttle decide nothing at all against rotation: every
// request looks like an unseen peer, starts at full burst, and is granted. The /64
// is the smallest unit a flooder cannot mint more of for free.
func TestPeerAddress_CollapsesIPv6ToItsPrefix(t *testing.T) {
	cases := map[string]struct {
		a, b string
		same bool
	}{
		"two addresses in one /64": {
			a: "[2001:db8:1:2::1]:1111", b: "[2001:db8:1:2:ffff:ffff:ffff:ffff]:2222", same: true,
		},
		"different /64s": {
			a: "[2001:db8:1:2::1]:1111", b: "[2001:db8:1:3::1]:1111", same: false,
		},
		"one IPv4 host, two ports": {
			a: "203.0.113.7:1111", b: "203.0.113.7:2222", same: true,
		},
		"two IPv4 hosts": {
			a: "203.0.113.7:1111", b: "203.0.113.8:1111", same: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			keyA := peerAddress(&http.Request{RemoteAddr: tc.a})
			keyB := peerAddress(&http.Request{RemoteAddr: tc.b})
			require.NotEmpty(t, keyA)
			if tc.same {
				assert.Equal(t, keyA, keyB)
				return
			}
			assert.NotEqual(t, keyA, keyB)
		})
	}
}

// TestFacilitator_RateLimitCollapsesAnIPv6Prefix drives the same claim through the
// endpoint: rotating the low half of an address must buy no quota.
func TestFacilitator_RateLimitCollapsesAnIPv6Prefix(t *testing.T) {
	clock := newHandClock()
	h := throttledFacilitator(clock, 1)

	require.Equal(t, http.StatusOK, verifyFrom(t, h, "[2001:db8:1:2::1]:1111", nil).Code)
	for _, addr := range []string{"[2001:db8:1:2::2]:1111", "[2001:db8:1:2::dead:beef]:2222"} {
		assert.Equal(t, http.StatusTooManyRequests, verifyFrom(t, h, addr, nil).Code,
			"a fresh address inside the same /64 must buy no quota")
	}
	assert.Equal(t, http.StatusOK, verifyFrom(t, h, "[2001:db8:1:3::1]:1111", nil).Code,
		"a different /64 is a different peer")
}

// TestRateLimiter_TableIsBoundedAndReturnsMemory pins the two halves of the
// memory bound.
//
// Deleting keys does not shrink a Go map, so a sweep alone left the peak size
// retained for the process lifetime: measured ~112 MiB kept after every entry was
// deleted. The sweep therefore rebuilds into a fresh map, which is what actually
// returns the table — and the rebuild has to be reachable by size as well as by
// the clock, or one burst inside a single interval sets the peak.
func TestRateLimiter_TableIsBoundedAndReturnsMemory(t *testing.T) {
	t.Run("a full table forces a rebuild before the interval", func(t *testing.T) {
		clock := newHandClock()
		l := newRateLimiter(RateLimit{PerSecond: DefaultRatePerSecond, Burst: 1, Now: clock.now})
		l.maxBuckets = 8

		// Every bucket refills within the interval, so all of them are
		// forgettable — but the clock never reaches the sweep trigger.
		for i := range l.maxBuckets * 4 {
			clock.advance(time.Second)
			l.allow(fmt.Sprintf("198.51.100.%d", i))
			require.LessOrEqual(t, len(l.buckets), l.maxBuckets, "the table must never pass its ceiling")
		}
	})

	t.Run("the sweep replaces the table rather than emptying it", func(t *testing.T) {
		clock := newHandClock()
		l := newRateLimiter(RateLimit{PerSecond: DefaultRatePerSecond, Burst: 1, Now: clock.now})

		for i := range 64 {
			l.allow(fmt.Sprintf("198.51.100.%d", i))
		}
		before := reflect.ValueOf(l.buckets).Pointer()

		clock.advance(sweepInterval + time.Second)
		l.allow("203.0.113.1")

		assert.NotEqual(t, before, reflect.ValueOf(l.buckets).Pointer(),
			"deleting keys leaves the table at its peak; only a fresh map returns it")
		assert.Len(t, l.buckets, 1)
	})
}

// TestRateLimiter_ClockStepsBackwards keeps a clock that moves the wrong way from
// disabling either half of the limiter. Wall-clock time does step backwards — NTP
// corrections, VM migrations — and both the sweep trigger and the bucket refill
// compare against a stored instant, so each would otherwise sit in the future and
// never fire again.
func TestRateLimiter_ClockStepsBackwards(t *testing.T) {
	t.Run("the trigger does not stay in the future", func(t *testing.T) {
		clock := newHandClock()
		l := newRateLimiter(RateLimit{PerSecond: DefaultRatePerSecond, Burst: 1, Now: clock.now})
		require.True(t, l.allow("198.51.100.1"))

		clock.advance(-time.Hour)
		l.allow("198.51.100.2")
		require.Equal(t, clock.now(), l.lastSweep,
			"a step backwards must re-anchor the trigger, or it is never due again")

		// And the sweep is really due from the stepped instant, not from the one
		// an hour ahead: a bucket spent after the step, and refilled since, is
		// forgotten on the next interval.
		clock.advance(sweepInterval + time.Second)
		l.allow("203.0.113.1")
		assert.NotContains(t, l.buckets, "198.51.100.2",
			"the sweep runs on the post-step timeline")
	})

	t.Run("a spent bucket still refills", func(t *testing.T) {
		clock := newHandClock()
		l := newRateLimiter(RateLimit{PerSecond: DefaultRatePerSecond, Burst: 1, Now: clock.now})
		require.True(t, l.allow("198.51.100.1"))
		require.False(t, l.allow("198.51.100.1"))

		// A backwards step leaves the bucket measured in the future. Without a
		// correction the peer waits out the step before it refills at all.
		clock.advance(-time.Hour)
		require.False(t, l.allow("198.51.100.1"), "no step buys a token")
		clock.advance(time.Second)
		assert.True(t, l.allow("198.51.100.1"), "a second of real refill still grants")
	})
}

// TestFacilitator_RateLimitRefusalEchoesNothing keeps the refusal mute: this
// endpoint answers anyone, and the key it throttles on is caller-supplied.
func TestFacilitator_RateLimitRefusalEchoesNothing(t *testing.T) {
	logs := captureLogs(t)
	clock := newHandClock()
	h := throttledFacilitator(clock, 1)

	require.Equal(t, http.StatusOK, verifyFrom(t, h, testPeer, nil).Code)
	rec := verifyFrom(t, h, testPeer, map[string]string{"X-Forwarded-For": decodeMarker})

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "too many requests", strings.TrimSpace(rec.Body.String()),
		"a refused caller is told it asked too often, and nothing more")
	assert.NotContains(t, rec.Body.String(), decodeMarker, "a refusal echoes nothing caller-supplied")
	assert.NotContains(t, rec.Body.String(), testPeer)

	record := logRecord(t, logs, "x402: reject request")
	assert.Equal(t, "rate_limited", record["detail"], "the operator must be able to tell why the request was refused")
	assert.NotContains(t, logs.String(), decodeMarker, "a caller-supplied header must never reach the logs")
	assert.NotContains(t, logs.String(), "203.0.113.7", "this package records no peer addresses")
}

// TestRateLimiter_ConcurrentAllow fires the throttle from many goroutines at one
// frozen instant: no refill can occur, so exactly the burst may pass however the
// calls interleave.
func TestRateLimiter_ConcurrentAllow(t *testing.T) {
	const (
		burst = 5
		calls = 50
	)
	clock := newHandClock()
	l := newRateLimiter(RateLimit{PerSecond: DefaultRatePerSecond, Burst: burst, Now: clock.now})

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("198.51.100.1") {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(burst), allowed.Load())
}

// TestNewFacilitator_ThrottlesByDefault pins that protection cannot be reached
// by omission: Handler hands out a public endpoint, and a caller that passes no
// option still gets the documented defaults.
func TestNewFacilitator_ThrottlesByDefault(t *testing.T) {
	f := NewFacilitator(&fakeNode{}, "dev")
	require.NotNil(t, f.limiter)
	assert.Equal(t, float64(DefaultRatePerSecond), f.limiter.ratePerSecond)
	assert.Equal(t, float64(DefaultRateBurst), f.limiter.burst)
}

// TestRateLimit_ZeroAndNegativeTakeDefaults keeps an unset or nonsensical
// configuration from disabling the throttle.
func TestRateLimit_ZeroAndNegativeTakeDefaults(t *testing.T) {
	for name, cfg := range map[string]RateLimit{
		"zero":     {},
		"negative": {PerSecond: -1, Burst: -1},
	} {
		t.Run(name, func(t *testing.T) {
			l := newRateLimiter(cfg)
			assert.Equal(t, float64(DefaultRatePerSecond), l.ratePerSecond)
			assert.Equal(t, float64(DefaultRateBurst), l.burst)
		})
	}
}
