package httpapi

import (
	"math"
	"sync"
	"time"
)

// EndpointClass groups routes that should share a rate-limit policy.
//
// There is deliberately no single global limit. A search is a trigram scan across the
// catalogue; a product fetch is one indexed read of a precomputed summary row. Giving
// both the same budget means either search is generous enough to be a denial-of-service
// vector or product fetches are stingy enough to break the public website, which is a
// client of this same API.
type EndpointClass string

const (
	// ClassSearch is the expensive full-text path.
	ClassSearch EndpointClass = "search"
	// ClassList is a cursor-paginated collection read.
	ClassList EndpointClass = "list"
	// ClassDetail is a single indexed read by slug or id.
	ClassDetail EndpointClass = "detail"
	// ClassSystem is /healthz, /readyz and /metrics, which are never rate limited.
	ClassSystem EndpointClass = "system"
)

// Policy is one token bucket's shape: how many requests may arrive at once, and how
// fast the allowance is replenished.
type Policy struct {
	// Burst is the bucket capacity, i.e. how many requests a caller may make back
	// to back from a full bucket.
	Burst int
	// PerMinute is the sustained refill rate.
	PerMinute float64
}

func (p Policy) valid() bool { return p.Burst > 0 && p.PerMinute > 0 }

// RateLimitConfig holds the per-class policies for anonymous and keyed callers.
//
// # An honest statement of what this does and does not bound
//
// The bucket lives in this process's memory. With N API instances behind a load
// balancer, a caller's effective limit is up to N times the configured one, because
// each instance counts only the requests it happens to receive; a caller who spreads
// requests across connections will, on average, see roughly N times the burst. That
// is a deliberate trade, not an oversight: a shared counter in Redis or PostgreSQL
// would put a network round trip and a new hard dependency on the hot path of every
// request, including the anonymous ones the public website makes, to buy precision
// that this gate does not actually need.
//
// What this gate is for is smoothing: stopping one badly written script from
// saturating an instance. The real ceiling on aggregate abuse is the WAF/edge rate
// limit in front of the fleet, which does see every request and is where a global
// bound belongs. Billing-relevant volume is not this gate's job either -- that is the
// durable monthly quota in PostgreSQL, which is exact by design because it has money
// attached.
type RateLimitConfig struct {
	// Anonymous applies to callers with no API key, keyed by client IP.
	Anonymous map[EndpointClass]Policy
	// Keyed applies to authenticated consumers, keyed by consumer id.
	Keyed map[EndpointClass]Policy
	// MaxTrackedKeys bounds the bucket map so a spray of forged source addresses
	// cannot grow it without limit. Zero means DefaultMaxTrackedKeys.
	MaxTrackedKeys int
	// Disabled turns the gate off entirely. Useful behind an edge limiter that
	// already does this job, and in tests that are not about rate limiting.
	Disabled bool
}

// DefaultMaxTrackedKeys bounds the in-memory bucket map.
const DefaultMaxTrackedKeys = 50_000

// DefaultRateLimits are the starting policies. Search is the tightest class because it
// is the most expensive; a keyed caller gets roughly five times an anonymous one.
func DefaultRateLimits() RateLimitConfig {
	return RateLimitConfig{
		Anonymous: map[EndpointClass]Policy{
			ClassSearch: {Burst: 10, PerMinute: 20},
			ClassList:   {Burst: 30, PerMinute: 60},
			ClassDetail: {Burst: 60, PerMinute: 120},
		},
		Keyed: map[EndpointClass]Policy{
			ClassSearch: {Burst: 60, PerMinute: 120},
			ClassList:   {Burst: 120, PerMinute: 300},
			ClassDetail: {Burst: 300, PerMinute: 600},
		},
	}
}

// policyFor resolves the policy for a caller and endpoint class. An unknown class or
// a missing entry falls back to the detail policy, and finally to a permissive
// default: a routing mistake must not turn into an outage.
func (c RateLimitConfig) policyFor(keyed bool, class EndpointClass) (Policy, bool) {
	table := c.Anonymous
	if keyed {
		table = c.Keyed
	}
	if p, ok := table[class]; ok && p.valid() {
		return p, true
	}
	if p, ok := table[ClassDetail]; ok && p.valid() {
		return p, true
	}
	return Policy{}, false
}

// rateLimitDecision is what the middleware needs to answer a request and to set the
// X-RateLimit-* headers the contract puts on every response, not only on 429s.
type rateLimitDecision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	Reset      time.Time
	RetryAfter time.Duration
}

// tokenBucketLimiter is a lazily-refilled token bucket per (scope, key, class).
//
// Refill is computed on access rather than by a ticker: with tens of thousands of
// live buckets a ticker would do work proportional to the map on every tick, almost
// all of it for buckets nobody is asking about.
type tokenBucketLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	lastSweep  time.Time
	maxEntries int
	now        func() time.Time
}

type tokenBucket struct {
	tokens   float64
	lastSeen time.Time
}

const (
	bucketSweepInterval = time.Minute
	bucketIdleTTL       = 10 * time.Minute
)

func newTokenBucketLimiter(maxEntries int, now func() time.Time) *tokenBucketLimiter {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxTrackedKeys
	}
	if now == nil {
		now = time.Now
	}
	return &tokenBucketLimiter{
		buckets:    make(map[string]*tokenBucket),
		maxEntries: maxEntries,
		now:        now,
	}
}

// allow takes one token from the bucket identified by key, refilling it first.
func (l *tokenBucketLimiter) allow(key string, p Policy) rateLimitDecision {
	if !p.valid() {
		return rateLimitDecision{Allowed: true}
	}
	perSecond := p.PerMinute / 60
	capacity := float64(p.Burst)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: capacity, lastSeen: now}
		l.buckets[key] = b
	} else if elapsed := now.Sub(b.lastSeen); elapsed > 0 {
		b.tokens = math.Min(capacity, b.tokens+elapsed.Seconds()*perSecond)
	}
	b.lastSeen = now

	d := rateLimitDecision{Limit: p.Burst}
	if b.tokens >= 1 {
		b.tokens -= 1
		d.Allowed = true
	} else {
		// Time until one whole token exists again.
		need := 1 - b.tokens
		d.RetryAfter = time.Duration(math.Ceil(need/perSecond)) * time.Second
		if d.RetryAfter < time.Second {
			d.RetryAfter = time.Second
		}
	}
	d.Remaining = int(b.tokens)
	if d.Remaining < 0 {
		d.Remaining = 0
	}
	// Reset is when the bucket would be full again, which is the most useful reading
	// of "when does my allowance come back" for a bucket that has no fixed window.
	refillSeconds := (capacity - b.tokens) / perSecond
	d.Reset = now.Add(time.Duration(refillSeconds * float64(time.Second)))
	return d
}

// sweepLocked evicts idle buckets. It runs at most once a minute, and unconditionally
// when the map has grown past its ceiling.
//
// If eviction cannot get the map back under the ceiling -- which means tens of
// thousands of *currently active* distinct callers, i.e. a distributed flood -- the
// map is reset. That deliberately fails open for one interval rather than letting an
// attacker turn the limiter into an out-of-memory kill: the edge limiter is the layer
// that is supposed to survive that scenario.
func (l *tokenBucketLimiter) sweepLocked(now time.Time) {
	over := len(l.buckets) > l.maxEntries
	if !over && now.Sub(l.lastSweep) < bucketSweepInterval {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > bucketIdleTTL {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) > l.maxEntries {
		l.buckets = make(map[string]*tokenBucket, l.maxEntries)
	}
}
