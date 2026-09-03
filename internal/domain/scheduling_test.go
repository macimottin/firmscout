package domain_test

import (
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

func TestRetryAfterOverridesEverything(t *testing.T) {
	t.Parallel()
	now := time.Now()
	in := domain.ScheduleInput{
		Policy:           domain.DefaultSchedulingPolicy(),
		Now:              now,
		Outcome:          domain.OutcomeRateLimited,
		RetryAfter:       4 * time.Hour,
		FreshnessFloor:   15 * time.Minute, // a paid commitment must not override politeness
		Popular:          true,
		SecurityCritical: true,
	}
	got := domain.NextInterval(in)
	if got.Interval < 4*time.Hour {
		t.Fatalf("interval %v is shorter than the vendor's Retry-After; FirmScout does not sell the right to ignore rate limits", got.Interval)
	}
}

func TestRecentChangeAcceleratesChecking(t *testing.T) {
	t.Parallel()
	p := domain.DefaultSchedulingPolicy()
	now := time.Now()
	changed := domain.NextInterval(domain.ScheduleInput{
		Policy: p, Now: now, Outcome: domain.OutcomeChanged, Health: domain.SourceActive,
	})
	quiet := domain.NextInterval(domain.ScheduleInput{
		Policy: p, Now: now, Outcome: domain.OutcomeUnchanged, Health: domain.SourceActive,
		ConsecutiveUnchanged: 5,
	})
	if changed.Interval >= quiet.Interval {
		t.Errorf("a source that just changed (%v) is not checked more often than a quiet one (%v)",
			changed.Interval, quiet.Interval)
	}
}

func TestUnchangedSourcesBackOff(t *testing.T) {
	t.Parallel()
	p := domain.DefaultSchedulingPolicy()
	now := time.Now()
	var prev time.Duration
	for _, streak := range []int{1, 3, 6, 10} {
		got := domain.NextInterval(domain.ScheduleInput{
			Policy: p, Now: now, Outcome: domain.OutcomeUnchanged,
			Health: domain.SourceActive, ConsecutiveUnchanged: streak,
		})
		if got.Interval < prev {
			t.Errorf("interval decreased as the unchanged streak grew: %v then %v", prev, got.Interval)
		}
		if got.Interval > p.MaxInterval {
			t.Errorf("interval %v exceeded the ceiling %v", got.Interval, p.MaxInterval)
		}
		prev = got.Interval
	}
}

func TestFailuresBackOffExponentially(t *testing.T) {
	t.Parallel()
	p := domain.DefaultSchedulingPolicy()
	now := time.Now()
	one := domain.NextInterval(domain.ScheduleInput{
		Policy: p, Now: now, Outcome: domain.OutcomeUnavailable, ConsecutiveFailures: 1,
	})
	three := domain.NextInterval(domain.ScheduleInput{
		Policy: p, Now: now, Outcome: domain.OutcomeUnavailable, ConsecutiveFailures: 3,
	})
	if three.Interval <= one.Interval {
		t.Errorf("failure backoff is not increasing: %v then %v", one.Interval, three.Interval)
	}
}

func TestEndOfLifeProductsCheckedFarLessOften(t *testing.T) {
	t.Parallel()
	p := domain.DefaultSchedulingPolicy()
	now := time.Now()
	active := domain.NextInterval(domain.ScheduleInput{Policy: p, Now: now, Health: domain.SourceActive})
	eol := domain.NextInterval(domain.ScheduleInput{
		Policy: p, Now: now, Health: domain.SourceActive, ProductEndOfLife: true,
	})
	if eol.Interval <= active.Interval {
		t.Errorf("EOL product interval %v is not longer than active %v", eol.Interval, active.Interval)
	}
}

func TestIntervalsAlwaysWithinBounds(t *testing.T) {
	t.Parallel()
	p := domain.DefaultSchedulingPolicy()
	now := time.Now()
	cases := []domain.ScheduleInput{
		{Policy: p, Now: now, Outcome: domain.OutcomeUnchanged, ConsecutiveUnchanged: 1000},
		{Policy: p, Now: now, Outcome: domain.OutcomeUnavailable, ConsecutiveFailures: 1000},
		{Policy: p, Now: now, Outcome: domain.OutcomeChanged, FreshnessFloor: time.Second},
		{Policy: p, Now: now, ProductEndOfLife: true, ConsecutiveUnchanged: 100},
	}
	for i, in := range cases {
		got := domain.NextInterval(in)
		if got.Interval < p.MinInterval {
			t.Errorf("case %d: interval %v below the floor %v", i, got.Interval, p.MinInterval)
		}
		if got.Interval > p.MaxInterval {
			t.Errorf("case %d: interval %v above the ceiling %v", i, got.Interval, p.MaxInterval)
		}
		if got.Reason == "" {
			t.Errorf("case %d: no reason recorded", i)
		}
		if !got.NextAt.After(now) {
			t.Errorf("case %d: next check is not in the future", i)
		}
	}
}

func TestPaidFreshnessFloorTightensCadence(t *testing.T) {
	t.Parallel()
	p := domain.DefaultSchedulingPolicy()
	now := time.Now()
	without := domain.NextInterval(domain.ScheduleInput{
		Policy: p, Now: now, Outcome: domain.OutcomeUnchanged, ConsecutiveUnchanged: 8,
	})
	with := domain.NextInterval(domain.ScheduleInput{
		Policy: p, Now: now, Outcome: domain.OutcomeUnchanged, ConsecutiveUnchanged: 8,
		FreshnessFloor: time.Hour,
	})
	if with.Interval >= without.Interval {
		t.Errorf("freshness commitment did not tighten the cadence: %v vs %v", with.Interval, without.Interval)
	}
}
