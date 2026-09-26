package sched

import (
	"testing"
	"time"
)

func TestLLMRateLimiter_CheckUsageAndRollover(t *testing.T) {
	now := time.Date(2026, 1, 1, 23, 59, 0, 0, time.UTC)
	limiter := NewLLMRateLimiter(func() time.Time { return now })
	if allowed, usage := limiter.Check("principal", 1); !allowed || usage != 1 {
		t.Fatalf("first check = (%v, %d), want (true, 1)", allowed, usage)
	}
	if allowed, usage := limiter.Check("principal", 1); allowed || usage != 2 {
		t.Fatalf("second check = (%v, %d), want (false, 2)", allowed, usage)
	}
	now = now.Add(2 * time.Minute)
	if got := limiter.Usage("principal"); got != 0 {
		t.Fatalf("usage after UTC rollover = %d, want 0", got)
	}
}

func TestLLMRateLimiter_UnlimitedStillCounts(t *testing.T) {
	limiter := NewLLMRateLimiter(nil)
	for want := 1; want <= 3; want++ {
		allowed, usage := limiter.Check("principal", 0)
		if !allowed || usage != want {
			t.Fatalf("check %d = (%v, %d), want (true, %d)", want, allowed, usage, want)
		}
	}
}
