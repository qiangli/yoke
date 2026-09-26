package sched

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Phase C — Admission, VTC fairness, retry-free nonces.
//
// Admitter owns admission state for requests:
//
//   1. Per-principal in-flight semaphore.
//      Over-cap requests get 429 with Retry-After. The plan later
//      promotes "over cap" to "queue" once Phase D's dispatcher loop
//      lands; for now over-cap is a hard reject so the v1 admission
//      surface is correct in isolation.
//
//   2. Virtual Token Counter (VTC) per principal. Charged once a
//      backend response drains. Idle entries (>10 min) drop
//      from the counter; on re-entry they're lifted to the current
//      minimum live VTC so a quiet principal returning after lunch
//      doesn't flood the pool until it "catches up" (OSDI'24 fairness
//      rule).
//
//   3. Retry-free nonce table. When the gateway sends 503 / 529 (our
//      overload, not the client's fault), it stamps a nonce. The
//      client retries with X-LLM-Retry-Of: <nonce>. The retried
//      attempt bypasses the in-flight cap and is not VTC-charged a
//      second time. 429 (principal over its own cap) stamps NO nonce —
//      that retry pays full price.

// VTCIdleTTL is how long a principal can be silent before we drop its
// VTC counter and treat the next request as a "returning" client
// (lifted to current min).
const VTCIdleTTL = 10 * time.Minute

// NonceTTL caps how long a 503/529-issued retry-free nonce remains
// honourable. Bounded so a stale client can't replay a hours-old
// nonce to skip the cap. Two-times the suggested Retry-After is the
// usual rule; we hard-cap at 30 s.
const NonceTTL = 30 * time.Second

// RetryFreeHeader is the client-facing nonce stamp on 503 / 529.
const RetryFreeHeader = "X-LLM-Retry-Free"

// RetryOfHeader is what the client sends on the retry attempt to
// claim free-retry treatment.
const RetryOfHeader = "X-LLM-Retry-Of"

// vtcEntry tracks one principal's cumulative VTC + last-touch time.
type vtcEntry struct {
	Tokens  int64
	LastUse time.Time
}

// nonceRec is one outstanding retry-free promise.
type nonceRec struct {
	PrincipalID string
	JobID       string
	ArrivalSeq  uint64
	Expires     time.Time
}

// Admitter is the admission state for a scheduler instance.
type Admitter struct {
	mu       sync.Mutex
	inFlight map[string]int      // principalID -> live count
	vtc      map[string]vtcEntry // principalID -> counter
	nonces   map[string]nonceRec // nonce string -> rec

	// arrival is a monotonic sequence number stamped on each admit
	// so the priority heap (Phase D) can break ties by FIFO within
	// a (tier, priority) bucket.
	arrival   uint64
	sweepOnce sync.Once
	stop      chan struct{}
}

// NewAdmitter returns an isolated admission scheduler.
func NewAdmitter() *Admitter {
	return &Admitter{
		inFlight: map[string]int{},
		vtc:      map[string]vtcEntry{},
		nonces:   map[string]nonceRec{},
		stop:     make(chan struct{}),
	}
}

// AdmitOutcome is what TryAdmit returns. AdmitOK means the caller
// can proceed; AdmitRateLimited means 429 with Retry-After (no
// nonce); AdmitOverload means 503 with Retry-After + nonce.
type AdmitOutcome struct {
	Result         AdmitResult
	ArrivalSeq     uint64
	RetryAfterSecs int
	Nonce          string // populated on Overload
}

type AdmitResult int

const (
	AdmitOK AdmitResult = iota
	AdmitRateLimited
	AdmitOverload
)

// MaxQueueDepth is the global hard cap on in-flight requests across
// all principals. Above this, we shed with 503 + Retry-After + nonce.
// The phase plan calls this LLMMaxQueueDepth; defaulting to 256 here
// matches the doc.
const MaxQueueDepth = 256

// TryAdmit gates one inbound request. retryOf, when non-empty,
// claims a previously-issued retry-free nonce: if the nonce is
// valid (matches principal + within TTL), admission bypasses the
// per-principal cap on this attempt.
func (a *Admitter) TryAdmit(principalID string, capLimit int, retryOf string) AdmitOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Honour retry-free nonces. A matched nonce is consumed
	// (one-time use) and admission proceeds with cap bypass.
	if retryOf != "" {
		if rec, ok := a.nonces[retryOf]; ok && time.Now().Before(rec.Expires) && rec.PrincipalID == principalID {
			delete(a.nonces, retryOf)
			a.arrival++
			a.inFlight[principalID]++
			return AdmitOutcome{Result: AdmitOK, ArrivalSeq: rec.ArrivalSeq}
		}
		// Stale or wrong-principal nonce — fall through to normal cap.
	}

	// Global queue hard cap → 503 + nonce.
	total := 0
	for _, n := range a.inFlight {
		total += n
	}
	if total >= MaxQueueDepth {
		nonce := mintNonceLocked(a, principalID, "", a.arrival+1)
		return AdmitOutcome{
			Result:         AdmitOverload,
			RetryAfterSecs: 1,
			Nonce:          nonce,
		}
	}

	// Per-principal cap → 429, no nonce.
	if a.inFlight[principalID] >= capLimit {
		return AdmitOutcome{
			Result:         AdmitRateLimited,
			RetryAfterSecs: 1,
		}
	}

	a.arrival++
	a.inFlight[principalID]++
	return AdmitOutcome{Result: AdmitOK, ArrivalSeq: a.arrival}
}

// Release decrements the per-principal in-flight counter. Idempotent.
func (a *Admitter) Release(principalID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n, ok := a.inFlight[principalID]; ok && n > 0 {
		a.inFlight[principalID] = n - 1
		if a.inFlight[principalID] == 0 {
			delete(a.inFlight, principalID)
		}
	}
}

// ChargeVTC adds model-token units to the principal's VTC counter.
func (a *Admitter) ChargeVTC(principalID string, tokens int64) {
	if tokens <= 0 || principalID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.vtc[principalID]
	e.Tokens += tokens
	e.LastUse = time.Now()
	a.vtc[principalID] = e
}

// minLiveVTC reports the smallest live VTC counter across all
// tracked principals. Used by the lift-to-min rule when a long-idle
// principal returns.
func (a *Admitter) minLiveVTC() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	var min int64
	first := true
	now := time.Now()
	for _, e := range a.vtc {
		if now.Sub(e.LastUse) > VTCIdleTTL {
			continue
		}
		if first || e.Tokens < min {
			min = e.Tokens
			first = false
		}
	}
	return min
}

// EnterFairBudget is called at admission time for a principal to ensure
// its VTC counter is sane. If the principal's entry is stale (>VTCIdleTTL)
// or missing, lift it to the current minimum live VTC so it can't
// flood-on-return. Returns the post-lift VTC value the priority
// heap should sort on.
func (a *Admitter) EnterFairBudget(principalID string) int64 {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.vtc[principalID]
	if !ok || now.Sub(e.LastUse) > VTCIdleTTL {
		// Lift to current min live VTC. Computed inline to avoid
		// re-entering the lock.
		var min int64
		first := true
		for id, x := range a.vtc {
			if id == principalID {
				continue
			}
			if now.Sub(x.LastUse) > VTCIdleTTL {
				continue
			}
			if first || x.Tokens < min {
				min = x.Tokens
				first = false
			}
		}
		e = vtcEntry{Tokens: min, LastUse: now}
		a.vtc[principalID] = e
	}
	return e.Tokens
}

// IssueNonce stamps a fresh retry-free nonce for the given principal /
// job / arrival triple. The caller embeds it in the response header
// X-LLM-Retry-Free. The nonce expires after NonceTTL.
func (a *Admitter) IssueNonce(principalID, jobID string, arrivalSeq uint64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return mintNonceLocked(a, principalID, jobID, arrivalSeq)
}

func mintNonceLocked(a *Admitter, principalID, jobID string, arrivalSeq uint64) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	nonce := hex.EncodeToString(b[:])
	a.nonces[nonce] = nonceRec{
		PrincipalID: principalID,
		JobID:       jobID,
		ArrivalSeq:  arrivalSeq,
		Expires:     time.Now().Add(NonceTTL),
	}
	return nonce
}

// sweepNoncesNow drops expired entries.
func (a *Admitter) sweepNoncesNow(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, rec := range a.nonces {
		if now.After(rec.Expires) {
			delete(a.nonces, k)
		}
	}
}

// StartSweeper starts this instance's background nonce sweeper.
// It is idempotent. Closing stop ends the sweeper; a nil stop uses
// the instance-owned stop channel.
func (a *Admitter) StartSweeper(stop <-chan struct{}) {
	if stop == nil {
		stop = a.stop
	}
	a.sweepOnce.Do(func() {
		go func() {
			t := time.NewTicker(NonceTTL / 2)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case now := <-t.C:
					a.sweepNoncesNow(now)
				}
			}
		}()
	})
}

// formatRetryAfter renders an integer-second value for the
// Retry-After header. Always at least 1.
func formatRetryAfter(secs int) string {
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// ApplyAdmissionHeaders stamps Retry-After and X-LLM-Retry-Free on
// the writer for an AdmitOverload / AdmitRateLimited outcome.
// Returns the HTTP status the caller should write.
func ApplyAdmissionHeaders(w http.ResponseWriter, o AdmitOutcome) int {
	switch o.Result {
	case AdmitOverload:
		w.Header().Set("Retry-After", formatRetryAfter(o.RetryAfterSecs))
		if o.Nonce != "" {
			w.Header().Set(RetryFreeHeader, o.Nonce)
		}
		return http.StatusServiceUnavailable
	case AdmitRateLimited:
		w.Header().Set("Retry-After", formatRetryAfter(o.RetryAfterSecs))
		return http.StatusTooManyRequests
	}
	return http.StatusOK
}

// InFlight returns the principal's current admitted request count.
func (a *Admitter) InFlight(principalID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight[principalID]
}

// vtcForTest returns the current VTC tokens for a principalID.
func (a *Admitter) vtcForTest(principalID string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vtc[principalID].Tokens
}

// snapshotNonceCount returns the live nonce-table size. Used by
// tests to assert eviction works.
func (a *Admitter) snapshotNonceCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.nonces)
}
