package sched

import (
	"net/http"
	"testing"
	"time"
)

func withFrozenBreakerClock(at time.Time) (*Breaker, *time.Time) {
	now := at
	return newBreaker(func() time.Time { return now }), &now
}

func TestBackendBreaker_TripAndExpire(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, now := withFrozenBreakerClock(base)
	b.Trip("backendA", 5*time.Second)
	if !b.InCooldown("backendA") {
		t.Fatal("backendA should be cooling right after Trip")
	}
	if b.InCooldown("backendB") {
		t.Error("backendB never tripped — must not be cooling")
	}
	// Advance well past the cooldown (5s + up to 10s jitter).
	*now = base.Add(30 * time.Second)
	if b.InCooldown("backendA") {
		t.Error("backendA cooldown should have expired")
	}
}

func TestBackendBreaker_FilterAvailable(t *testing.T) {
	b, _ := withFrozenBreakerClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b.Trip("backendB", 30*time.Second)
	got := b.FilterAvailable([]string{"backendA", "backendB", "backendC"})
	if len(got) != 2 || got[0] != "backendA" || got[1] != "backendC" {
		t.Errorf("FilterAvailable: want [backendA backendC], got %v", got)
	}
	// When EVERY candidate is cooling, return empty so the caller falls
	// back to the full set (never fail-closed).
	b.Trip("backendA", 30*time.Second)
	b.Trip("backendC", 30*time.Second)
	if got := b.FilterAvailable([]string{"backendA", "backendB", "backendC"}); len(got) != 0 {
		t.Errorf("all cooling: want empty, got %v", got)
	}
}

func TestBackendBreaker_TripPreservesLonger(t *testing.T) {
	b, _ := withFrozenBreakerClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	b.Trip("h", 2*time.Minute)
	long := b.until["h"]
	b.Trip("h", 1*time.Second) // a shorter Trip must not shorten the window
	if b.until["h"].Before(long) {
		t.Error("a shorter Trip shortened an existing longer cooldown")
	}
}

func TestBackendBreaker_MaxCooldownClamp(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, _ := withFrozenBreakerClock(base)
	b.Trip("h", time.Hour) // hostile Retry-After
	if got := b.until["h"].Sub(base); got > breakerMaxCooldown {
		t.Errorf("cooldown not clamped: %v > %v", got, breakerMaxCooldown)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := ParseRetryAfter("30"); d != 30*time.Second {
		t.Errorf("seconds: want 30s, got %v", d)
	}
	if d := ParseRetryAfter(""); d != 0 {
		t.Errorf("empty: want 0, got %v", d)
	}
	if d := ParseRetryAfter("garbage"); d != 0 {
		t.Errorf("garbage: want 0, got %v", d)
	}
	future := time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	if d := ParseRetryAfter(future); d <= 0 || d > 2*time.Minute {
		t.Errorf("http-date: want ~1m, got %v", d)
	}
}
