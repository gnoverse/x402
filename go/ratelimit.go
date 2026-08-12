package x402

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Default throttle for the chain-touching endpoints, per remote address.
// /verify runs a full chain simulation and /settle broadcasts, both for an
// anonymous caller, so the rate a single peer may sustain is the rate at which
// it may spend the node behind this service.
const (
	DefaultRatePerSecond = 10
	DefaultRateBurst     = 20
)

// sweepInterval is how often allow forgets the buckets it can. The map is keyed
// on addresses the callers choose, so without a sweep it is a memory exhaustion
// vector of its own; with one, what it holds is bounded by the distinct
// addresses seen within a single interval. The sweep runs no more often than
// this, so a flood cannot turn it into a full-map scan per request.
const sweepInterval = time.Minute

// defaultMaxBuckets caps how many peers the table tracks at once, so the size a
// flood reaches is bounded by configuration rather than by how many addresses the
// callers can mint inside one sweep interval. At ~136 B per entry this is ~9 MiB,
// orders of magnitude above any legitimate peer count for a service whose clients
// are sellers.
//
// Reaching the cap forces a sweep. A table still full afterwards holds only peers
// actively spending tokens, and an unseen key is then refused rather than
// admitted: under a flood the new keys ARE the flood, and a bounded refusal beats
// unbounded memory.
const defaultMaxBuckets = 1 << 16

// ipv6PrefixBits is how much of an IPv6 address keys a bucket: the /64 an
// operator assigns, rather than an address the holder of that /64 mints for free.
const ipv6PrefixBits = 64

// RateLimit configures the per-remote-address token bucket. A zero or negative
// PerSecond or Burst takes the default: there is no configuration that removes
// the throttle.
type RateLimit struct {
	PerSecond float64          // sustained refill rate; <=0 -> DefaultRatePerSecond
	Burst     int              // bucket capacity, the largest tolerated spike; <=0 -> DefaultRateBurst
	Now       func() time.Time // nil -> time.Now
}

// rateLimiter is a mutex-guarded token bucket per remote address, refilled at
// ratePerSecond up to burst and spent one token per request.
type rateLimiter struct {
	now           func() time.Time
	ratePerSecond float64
	burst         float64
	// maxBuckets is the table ceiling, defaultMaxBuckets outside tests — which
	// cannot afford to mint a production-sized flood.
	maxBuckets int

	mu        sync.Mutex
	buckets   map[string]bucket
	lastSweep time.Time
}

// bucket is one peer's remaining allowance and the instant it was measured.
type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(cfg RateLimit) *rateLimiter {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PerSecond <= 0 {
		cfg.PerSecond = DefaultRatePerSecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = DefaultRateBurst
	}
	return &rateLimiter{
		now:           cfg.Now,
		ratePerSecond: cfg.PerSecond,
		burst:         float64(cfg.Burst),
		maxBuckets:    defaultMaxBuckets,
		buckets:       make(map[string]bucket),
	}
}

// allow spends one token from key's bucket, reporting whether the request may
// proceed. An unseen key starts at full capacity, unless the table is full: see
// defaultMaxBuckets.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepFull(now)

	b, seen := l.buckets[key]
	switch {
	case !seen && len(l.buckets) >= l.maxBuckets:
		// The sweep just ran and could not free a slot, so every tracked peer is
		// still spending. Admitting an unseen key would grow the table without
		// bound; refusing it costs one request.
		return false
	case !seen:
		b = bucket{tokens: l.burst, last: now}
	case now.Before(b.last):
		// The clock stepped backwards. Re-anchor without granting a refill, or
		// the bucket sits measured in the future and refills for nobody until
		// real time catches up.
		b.last = now
	default:
		b.tokens = min(b.tokens+now.Sub(b.last).Seconds()*l.ratePerSecond, l.burst)
		b.last = now
	}

	granted := b.tokens >= 1
	if granted {
		b.tokens--
	}
	l.buckets[key] = b
	return granted
}

// sweepFull forgets every bucket that has refilled to capacity — such an entry
// decides exactly as an unseen key would, so forgetting it grants nothing.
//
// It rebuilds the table instead of deleting from it. Deleting does not shrink a
// Go map, so a delete-only sweep left the peak size allocated for the process
// lifetime and one flood established a permanent memory floor. A fresh map sized
// to what survives is what actually returns the memory.
//
// It runs on the interval, when the clock moves backwards — which would otherwise
// leave the trigger permanently in the future — and whenever the table has reached
// its ceiling, so the size a single interval can reach is bounded too.
//
// A bucket a backwards step left dated in the future is not forgettable until real
// time passes that date, since nothing can have refilled it. That set is bounded
// by the size of the step, and the ceiling bounds it regardless.
//
// Callers must hold l.mu.
func (l *rateLimiter) sweepFull(now time.Time) {
	due := now.Sub(l.lastSweep) >= sweepInterval || now.Before(l.lastSweep) || len(l.buckets) >= l.maxBuckets
	if !due {
		return
	}
	l.lastSweep = now
	kept := make(map[string]bucket)
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.ratePerSecond < l.burst {
			kept[key] = b
		}
	}
	l.buckets = kept
}

// peerAddress is the throttle key: the address the connection actually came from,
// narrowed to the unit a flooder cannot mint more of for free.
//
// For IPv6 that is the /64, not the address. A routed /64 is the standard
// residential and cloud allocation, so one host sources every request from a
// distinct /128 at no cost — and since an unseen key starts at full burst, keying
// the full address made the throttle decide nothing at all against rotation. IPv4
// keys the host, which is already the assigned unit.
//
// X-Forwarded-For and X-Real-IP are deliberately unread: this service
// authenticates nobody, so a caller sets those headers freely and honoring one
// would let a single flooder buy unlimited quota by rotating a value. A deployment
// behind a reverse proxy therefore sees only the proxy's address, collapsing every
// client into one bucket, and must throttle upstream.
func peerAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// An address of an unexpected shape still keys a bucket: not throttling
		// is never the answer.
		return r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil {
		return host
	}
	return ip.Mask(net.CIDRMask(ipv6PrefixBits, 8*net.IPv6len)).String() + "/64"
}
