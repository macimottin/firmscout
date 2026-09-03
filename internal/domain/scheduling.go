package domain

import (
	"math"
	"time"
)

// SchedulingPolicy holds the tunable bounds for adaptive source scheduling. Every
// value here is configuration with a documented default, not a constant, because the
// right cadence is an empirical question that measured publication history answers
// better than a guess made before launch.
type SchedulingPolicy struct {
	// MinInterval is the floor for any source, protecting vendors from being polled
	// more often than is polite.
	MinInterval time.Duration
	// MaxInterval is the ceiling a quiet source backs off to.
	MaxInterval time.Duration
	// BaseInterval applies when no history justifies anything else.
	BaseInterval time.Duration
	// RecentChangeInterval applies briefly after a change, because vendors publish
	// in clusters: a release is the best predictor of another release.
	RecentChangeInterval time.Duration
	// RecentChangeWindow is how long the accelerated cadence persists.
	RecentChangeWindow time.Duration
	// UnchangedBackoffFactor multiplies the interval for each consecutive unchanged
	// check, up to MaxInterval.
	UnchangedBackoffFactor float64
	// FailureBackoffFactor multiplies the interval for each consecutive failure.
	FailureBackoffFactor float64
	// EOLMultiplier stretches the interval for products that are no longer expected
	// to receive releases.
	EOLMultiplier float64
	// PopularFloor is the interval a highly-requested product is never checked less
	// often than.
	PopularFloor time.Duration
	// JitterFraction spreads checks so a fleet of sources on one vendor domain does
	// not stampede at the same instant.
	JitterFraction float64
}

// DefaultSchedulingPolicy returns the starting configuration. These values are
// deliberate starting points for tuning, not measurements.
func DefaultSchedulingPolicy() SchedulingPolicy {
	return SchedulingPolicy{
		MinInterval:            15 * time.Minute,
		MaxInterval:            30 * 24 * time.Hour,
		BaseInterval:           24 * time.Hour,
		RecentChangeInterval:   6 * time.Hour,
		RecentChangeWindow:     72 * time.Hour,
		UnchangedBackoffFactor: 1.35,
		FailureBackoffFactor:   2.0,
		EOLMultiplier:          7.0,
		PopularFloor:           6 * time.Hour,
		JitterFraction:         0.10,
	}
}

// ScheduleInput is everything the scheduling decision depends on. Passing it as one
// value keeps NextInterval a pure function, which is what makes the policy testable
// without a database or a clock.
type ScheduleInput struct {
	Policy               SchedulingPolicy
	Now                  time.Time
	Outcome              CheckOutcome
	Health               SourceHealth
	ConsecutiveUnchanged int
	ConsecutiveFailures  int
	LastChangedAt        time.Time
	ProductEndOfLife     bool
	SecurityCritical     bool
	Popular              bool
	// FreshnessFloor is a paid commitment: the interval may never exceed it.
	FreshnessFloor time.Duration
	// RetryAfter is the vendor's own instruction, which always wins.
	RetryAfter time.Duration
	// ConfiguredInterval is the source's own base frequency from the registry.
	ConfiguredInterval time.Duration
}

// ScheduleDecision is the computed next check together with the reason, so that a
// surprising cadence can be explained without re-deriving it.
type ScheduleDecision struct {
	Interval time.Duration
	NextAt   time.Time
	Reason   string
}

// NextInterval computes when a source should next be checked.
//
// The ordering of the rules matters and encodes the politeness policy: a vendor's
// explicit Retry-After beats everything, then failure backoff, then the accelerated
// post-change window, then unchanged backoff, then lifecycle. Paid freshness
// commitments and popularity apply as floors at the end, and can never override
// Retry-After.
func NextInterval(in ScheduleInput) ScheduleDecision {
	p := in.Policy
	if p.MinInterval <= 0 {
		p = DefaultSchedulingPolicy()
	}

	base := in.ConfiguredInterval
	if base <= 0 {
		base = p.BaseInterval
	}

	var interval time.Duration
	var reason string

	switch {
	case in.RetryAfter > 0:
		// The vendor told us when to come back. Nothing overrides this, including a
		// paid freshness commitment: FirmScout does not sell the right to ignore a
		// rate limit.
		interval = in.RetryAfter
		reason = "honouring the vendor's Retry-After instruction"
		return finalize(p, interval, in.Now, reason, true)

	case in.Outcome == OutcomeRateLimited:
		interval = scale(base, p.FailureBackoffFactor, maxInt(in.ConsecutiveFailures, 1))
		reason = "rate limited; backing off and lowering the base cadence"
		return finalize(p, interval, in.Now, reason, true)

	case in.Health == SourceBroken, in.Outcome == OutcomeUnavailable, in.Outcome == OutcomeParserFailed:
		interval = scale(base, p.FailureBackoffFactor, maxInt(in.ConsecutiveFailures, 1))
		reason = "recent failures; exponential backoff with jitter"

	case in.Health == SourceAuthenticationRequired:
		interval = p.MaxInterval
		reason = "source requires credentials FirmScout does not have; checking rarely pending review"
		return finalize(p, interval, in.Now, reason, true)

	case in.Outcome == OutcomeChanged:
		interval = p.RecentChangeInterval
		reason = "source just changed; vendors publish in clusters"

	case !in.LastChangedAt.IsZero() && in.Now.Sub(in.LastChangedAt) < p.RecentChangeWindow:
		interval = p.RecentChangeInterval
		reason = "inside the accelerated window after a recent change"

	case in.ConsecutiveUnchanged > 0:
		interval = scale(base, p.UnchangedBackoffFactor, in.ConsecutiveUnchanged)
		reason = "no change observed recently; gradually backing off"

	default:
		interval = base
		reason = "configured base cadence"
	}

	if in.ProductEndOfLife {
		interval = time.Duration(float64(interval) * p.EOLMultiplier)
		reason += "; product is end-of-life so checking far less often"
	}

	// Floors, applied last and never below MinInterval.
	if in.SecurityCritical && interval > base {
		interval = base
		reason += "; security-critical product holds the base cadence"
	}
	if in.Popular && interval > p.PopularFloor {
		interval = p.PopularFloor
		reason += "; frequently requested product holds a tighter floor"
	}
	if in.FreshnessFloor > 0 && interval > in.FreshnessFloor {
		interval = in.FreshnessFloor
		reason += "; paid freshness commitment applies"
	}

	return finalize(p, interval, in.Now, reason, true)
}

func finalize(p SchedulingPolicy, interval time.Duration, now time.Time, reason string, jitter bool) ScheduleDecision {
	if interval < p.MinInterval {
		interval = p.MinInterval
	}
	if interval > p.MaxInterval {
		interval = p.MaxInterval
	}
	next := now.Add(interval)
	if jitter && p.JitterFraction > 0 {
		// Deterministic spread derived from the interval itself. A real dispatcher
		// adds a random component; the domain stays pure so the policy is testable,
		// and the adapter applies randomised jitter on top.
		next = next.Add(time.Duration(float64(interval) * p.JitterFraction * 0.5))
	}
	return ScheduleDecision{Interval: interval, NextAt: next, Reason: reason}
}

func scale(base time.Duration, factor float64, steps int) time.Duration {
	if steps < 1 {
		steps = 1
	}
	if steps > 24 {
		steps = 24 // bound the exponent so the multiplication cannot overflow
	}
	return time.Duration(float64(base) * math.Pow(factor, float64(steps)))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
