package sched

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Phase C — Job table.
//
// A "job" is one user-level work unit, realized as one or more HTTP
// request/response pairs against /v1/chat/completions or
// /v1/embeddings with arbitrary tool-call rounds between them. The
// streaming response of any single HTTP call is part of one job
// (never split). A job has a single X-LLM-Job-ID carried by every
// constituent request; admission, fairness, priority, and slot
// ownership are evaluated per job, not per HTTP request.
//
// First request: full admission via llm_admit.go.
// Subsequent same-Job-ID: fast-path dispatch to the bound (backend,
// model) — skip the queue entirely as long as the slot is held.
// Slot release: (a) client signals X-LLM-Job-Done: true on a response
// read by the proxy on the way back, OR (b) JobIdleTTL elapses
// without an in-flight request for this Job-ID.
//
// Bind jobs[jobID].PrincipalID on first admission. Later requests with
// a mismatching principal are treated as a new job.

const (
	JobIDHeader   = "X-LLM-Job-ID"
	JobDoneHeader = "X-LLM-Job-Done"
)

// JobIdleTTL is the wall-clock budget between consecutive requests
// of the same job. Tuned to cover most agentic tool-call round
// trips while bounding worst-case slot hold under abandoned jobs.
const JobIdleTTL = 120 * time.Second

// JobEntry is the per-job state the scheduler tracks.
type JobEntry struct {
	JobID        string
	PrincipalID  string    // owner; a later request from a different principal makes a new job
	SlotBackend  string    // bound on first successful dispatch
	SlotModel    string    // bound on first successful dispatch
	LastActiveAt time.Time // last time any request for this job was in flight
	VTCTokens    int64     // cumulative tokens served against this job
	ArrivalSeq   uint64    // monotonic sequence at admission; used by priority heap
}

// JobTable is a scheduler instance's job registry.
type JobTable struct {
	mu        sync.Mutex
	jobs      map[string]*JobEntry
	arrival   uint64
	sweepOnce sync.Once
	sweepDone chan struct{}
}

// NewJobTable returns an isolated job registry.
func NewJobTable() *JobTable {
	return &JobTable{
		jobs:      map[string]*JobEntry{},
		sweepDone: make(chan struct{}),
	}
}

// ResolveJobID returns the (jobID, isNew) pair for one inbound
// request. Mints a fresh UUID-ish identifier when the client didn't
// send one. The minted form is hex-only so it never confuses
// downstream JSON encoders.
func ResolveJobID(r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.Header.Get(JobIDHeader))
	if id != "" {
		return id, false
	}
	return mintJobID(), true
}

func mintJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// LookupOrBind atomically inspects the table:
//
//   - If a job exists with matching principal AND its slot has not gone
//     stale (within JobIdleTTL), returns (entry, true) — caller may
//     fast-path dispatch to entry.SlotBackend / entry.SlotModel.
//   - Otherwise, creates a new entry (or rebinds an existing one when
//     the principal doesn't match) and returns
//     (entry, false). Caller must run full admission.
//
// LastActiveAt is touched on both paths so the sweeper doesn't
// expire an actively-used job.
func (jt *JobTable) LookupOrBind(jobID, principalID string) (*JobEntry, bool) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if e, ok := jt.jobs[jobID]; ok && e.PrincipalID == principalID {
		if time.Since(e.LastActiveAt) <= JobIdleTTL && e.SlotBackend != "" {
			e.LastActiveAt = time.Now()
			return e, true
		}
		// Same principal, but slot expired: reset for re-admission.
		e.SlotBackend = ""
		e.SlotModel = ""
		e.LastActiveAt = time.Now()
		jt.arrival++
		e.ArrivalSeq = jt.arrival
		return e, false
	}
	jt.arrival++
	e := &JobEntry{
		JobID:        jobID,
		PrincipalID:  principalID,
		LastActiveAt: time.Now(),
		ArrivalSeq:   jt.arrival,
	}
	jt.jobs[jobID] = e
	return e, false
}

// BindSlot stamps the (backend, model) selected by the dispatcher onto
// the job so the next request from the same Job-ID can fast-path
// directly to it.
func (jt *JobTable) BindSlot(jobID, backend, model string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if e, ok := jt.jobs[jobID]; ok {
		e.SlotBackend = backend
		e.SlotModel = model
		e.LastActiveAt = time.Now()
	}
}

// MarkActive refreshes LastActiveAt for one job. Called when an
// in-flight request completes so the idle-TTL window resets from
// "now" rather than from request entry.
func (jt *JobTable) MarkActive(jobID string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if e, ok := jt.jobs[jobID]; ok {
		e.LastActiveAt = time.Now()
	}
}

// Release explicitly removes a job entry — used when the client
// signals X-LLM-Job-Done: true. Idempotent.
func (jt *JobTable) Release(jobID string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	delete(jt.jobs, jobID)
}

// AddTokensCharged accumulates VTC token cost against the job.
// Called from the audit-tail reader once the upstream response has
// been fully drained and we know the prompt_tokens + completion_tokens
// total.
func (jt *JobTable) AddTokensCharged(jobID string, tokens int64) {
	if tokens <= 0 {
		return
	}
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if e, ok := jt.jobs[jobID]; ok {
		e.VTCTokens += tokens
		e.LastActiveAt = time.Now()
	}
}

// Snapshot returns a copy of the entry for the given job.
func (jt *JobTable) Snapshot(jobID string) (JobEntry, bool) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	e, ok := jt.jobs[jobID]
	if !ok {
		return JobEntry{}, false
	}
	return *e, true
}

// sweepLoop drops entries whose LastActiveAt is older than
// JobIdleTTL. Runs every JobIdleTTL/4 so abandoned jobs free their
// slots within a quarter-window of actual idleness — fast enough to
// recover capacity, slow enough not to thrash.
func (jt *JobTable) sweepLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(JobIdleTTL / 4)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			jt.sweepOnce_(time.Now())
		}
	}
}

func (jt *JobTable) sweepOnce_(now time.Time) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	for id, e := range jt.jobs {
		if now.Sub(e.LastActiveAt) > JobIdleTTL {
			delete(jt.jobs, id)
		}
	}
}

// StartSweeper starts this table's sweep loop exactly once. A nil stop
// channel uses the table-owned channel.
func (jt *JobTable) StartSweeper(stop <-chan struct{}) {
	if stop == nil {
		stop = jt.sweepDone
	}
	jt.sweepOnce.Do(func() {
		go jt.sweepLoop(stop)
	})
}

// stopSweeperForTest stops the sweeper so a test can drive
// sweepOnce_ deterministically. Not exported — tests in the same
// package call it directly.
func (jt *JobTable) stopSweeperForTest() {
	close(jt.sweepDone)
}
