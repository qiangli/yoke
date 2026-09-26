package sched

import (
	"testing"
	"time"
)

func resetAdmitter(t *testing.T) *Admitter {
	t.Helper()
	return NewAdmitter()
}

func TestAdmitter_PerPrincipalCapEnforcesRateLimit(t *testing.T) {
	a := resetAdmitter(t)
	cap := 2

	for i := 0; i < cap; i++ {
		out := a.TryAdmit("tk", cap, "")
		if out.Result != AdmitOK {
			t.Fatalf("admit %d should succeed; got %v", i, out.Result)
		}
	}
	out := a.TryAdmit("tk", cap, "")
	if out.Result != AdmitRateLimited {
		t.Errorf("over-cap should be rate-limited, got %v", out.Result)
	}
	if out.Nonce != "" {
		t.Errorf("rate-limited path must NOT issue a nonce: %q", out.Nonce)
	}

	// Release one slot — next admit should succeed.
	a.Release("tk")
	out = a.TryAdmit("tk", cap, "")
	if out.Result != AdmitOK {
		t.Errorf("post-release admit should succeed, got %v", out.Result)
	}
}

func TestAdmitter_RetryFreeNonceBypassesCap(t *testing.T) {
	a := resetAdmitter(t)
	cap := 1

	// Saturate the principal.
	if out := a.TryAdmit("tk", cap, ""); out.Result != AdmitOK {
		t.Fatalf("initial admit failed")
	}
	denied := a.TryAdmit("tk", cap, "")
	if denied.Result != AdmitRateLimited {
		t.Fatalf("expected rate-limited, got %v", denied.Result)
	}

	// Issue an overload nonce for this principal (simulating that an
	// earlier request had been 503'd).
	nonce := a.IssueNonce("tk", "job-1", 42)
	if nonce == "" {
		t.Fatalf("IssueNonce returned empty")
	}

	// Retry with nonce — should bypass the cap.
	out := a.TryAdmit("tk", cap, nonce)
	if out.Result != AdmitOK {
		t.Errorf("retry with valid nonce should bypass cap; got %v", out.Result)
	}
	// Nonce is one-shot: re-using it should not bypass.
	denied2 := a.TryAdmit("tk", cap, nonce)
	if denied2.Result == AdmitOK {
		t.Errorf("nonce should be consumed after first use")
	}
}

func TestAdmitter_NonceWrongPrincipalIsIgnored(t *testing.T) {
	a := resetAdmitter(t)
	nonce := a.IssueNonce("tk-A", "job", 1)

	// Saturate principal B.
	cap := 1
	_ = a.TryAdmit("tk-B", cap, "")
	out := a.TryAdmit("tk-B", cap, nonce) // wrong principal
	if out.Result != AdmitRateLimited {
		t.Errorf("nonce minted for tk-A must not bypass cap for tk-B; got %v", out.Result)
	}
}

func TestAdmitter_NonceExpires(t *testing.T) {
	a := resetAdmitter(t)
	nonce := a.IssueNonce("tk", "job", 1)
	// Force-expire by rewriting the entry.
	a.mu.Lock()
	rec := a.nonces[nonce]
	rec.Expires = time.Now().Add(-time.Second)
	a.nonces[nonce] = rec
	a.mu.Unlock()

	cap := 1
	_ = a.TryAdmit("tk", cap, "")
	out := a.TryAdmit("tk", cap, nonce)
	if out.Result == AdmitOK {
		t.Errorf("expired nonce should not bypass cap")
	}
}

func TestAdmitter_VTCLiftToMinOnIdleReturn(t *testing.T) {
	a := resetAdmitter(t)

	// Existing live principal with 1000 model tokens charged.
	a.ChargeVTC("busy-principal", 1000)

	// New principal arrives — should be lifted to min(live) = 1000, NOT
	// start at 0 (else it would dominate the fair pick until catching up).
	got := a.EnterFairBudget("returning")
	if got != 1000 {
		t.Errorf("returning principal VTC=%d, want 1000 (lift-to-min)", got)
	}
	// Verify the entry persists.
	if stored := a.vtcForTest("returning"); stored != 1000 {
		t.Errorf("stored VTC=%d, want 1000", stored)
	}
}

func TestAdmitter_VTCEmptyMin(t *testing.T) {
	a := resetAdmitter(t)
	got := a.EnterFairBudget("first")
	if got != 0 {
		t.Errorf("first principal with empty VTC table should start at 0, got %d", got)
	}
}

func TestAdmitter_GlobalQueueCapOverloads(t *testing.T) {
	a := resetAdmitter(t)
	// Pile MaxQueueDepth+1 admits across many principals.
	for i := 0; i < MaxQueueDepth; i++ {
		out := a.TryAdmit("tk"+itoa(i), 1000, "")
		if out.Result != AdmitOK {
			t.Fatalf("admit %d failed before queue cap: %v", i, out.Result)
		}
	}
	out := a.TryAdmit("tk-overflow", 1000, "")
	if out.Result != AdmitOverload {
		t.Errorf("at MaxQueueDepth global cap, expected overload, got %v", out.Result)
	}
	if out.Nonce == "" {
		t.Errorf("overload outcome must carry a retry-free nonce")
	}
}

// itoa is a tiny inline to avoid pulling strconv just for the loop.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
