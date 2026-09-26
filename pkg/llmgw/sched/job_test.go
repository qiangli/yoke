package sched

import (
	"net/http/httptest"
	"testing"
	"time"
)

func freshJobTable() *JobTable {
	return NewJobTable()
}

func TestResolveJobID_Mint(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	id1, new1 := ResolveJobID(r)
	if !new1 {
		t.Errorf("missing header should mint a new ID")
	}
	if len(id1) < 16 {
		t.Errorf("minted ID looks too short: %q", id1)
	}
	// Distinct mints — IDs should be unique.
	id2, _ := ResolveJobID(r)
	if id1 == id2 {
		t.Errorf("two mints produced identical IDs (%q)", id1)
	}
}

func TestResolveJobID_HonorsClientHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set(JobIDHeader, "client-supplied-id")
	id, isNew := ResolveJobID(r)
	if id != "client-supplied-id" {
		t.Errorf("got %q, want client-supplied-id", id)
	}
	if isNew {
		t.Errorf("client-supplied ID should not be reported as freshly minted")
	}
}

func TestJobTable_LookupOrBind_NewVsFastPath(t *testing.T) {
	jt := freshJobTable()
	e1, fast := jt.LookupOrBind("job-1", "tk")
	if fast {
		t.Fatal("first lookup should not be fast-path")
	}
	if e1.SlotBackend != "" {
		t.Fatal("new job should have no slot yet")
	}
	if e1.ArrivalSeq == 0 {
		t.Fatal("arrival sequence should be stamped")
	}

	// Without binding a slot, the second lookup is still NOT fast-path —
	// fast-path requires SlotBackend != "".
	_, fast2 := jt.LookupOrBind("job-1", "tk")
	if fast2 {
		t.Fatal("unbound job should not fast-path on second lookup")
	}

	// Bind the slot, then look up again — fast-path.
	jt.BindSlot("job-1", "backend-A", "qwen:7b")
	e3, fast3 := jt.LookupOrBind("job-1", "tk")
	if !fast3 {
		t.Fatal("bound job should fast-path on subsequent lookup")
	}
	if e3.SlotBackend != "backend-A" || e3.SlotModel != "qwen:7b" {
		t.Errorf("fast-path entry has wrong slot: %+v", e3)
	}
}

func TestJobTable_PrincipalMismatchTreatedAsNewJob(t *testing.T) {
	jt := freshJobTable()
	_, _ = jt.LookupOrBind("job-1", "tk-A")
	jt.BindSlot("job-1", "backend-A", "m")

	// Same Job-ID, different principal. Treated as a new
	// job — the slot rebinds to the new owner.
	e, fast := jt.LookupOrBind("job-1", "tk-B")
	if fast {
		t.Fatal("principal mismatch must NOT fast-path")
	}
	if e.PrincipalID != "tk-B" {
		t.Errorf("entry's PrincipalID=%q, want tk-B (the new owner)", e.PrincipalID)
	}
}

func TestJobTable_SlotExpiresAfterIdleTTL(t *testing.T) {
	jt := freshJobTable()
	_, _ = jt.LookupOrBind("job-1", "tk")
	jt.BindSlot("job-1", "backend-A", "m")

	// Force the entry to look idle longer than JobIdleTTL.
	jt.mu.Lock()
	jt.jobs["job-1"].LastActiveAt = time.Now().Add(-JobIdleTTL - time.Second)
	jt.mu.Unlock()

	_, fast := jt.LookupOrBind("job-1", "tk")
	if fast {
		t.Fatal("idle-past-TTL job should NOT fast-path; should reset for re-admission")
	}
}

func TestJobTable_ExplicitRelease(t *testing.T) {
	jt := freshJobTable()
	_, _ = jt.LookupOrBind("job-1", "tk")
	jt.BindSlot("job-1", "backend", "m")
	jt.Release("job-1")
	_, ok := jt.Snapshot("job-1")
	if ok {
		t.Fatal("Release should remove the entry")
	}
}

func TestJobTable_VTCAccumulates(t *testing.T) {
	jt := freshJobTable()
	_, _ = jt.LookupOrBind("job-1", "tk")
	jt.AddTokensCharged("job-1", 100)
	jt.AddTokensCharged("job-1", 250)
	snap, _ := jt.Snapshot("job-1")
	if snap.VTCTokens != 350 {
		t.Errorf("VTCTokens=%d, want 350", snap.VTCTokens)
	}
}

func TestJobTable_SweepEvictsIdleJobs(t *testing.T) {
	jt := freshJobTable()
	_, _ = jt.LookupOrBind("idle", "tk")
	_, _ = jt.LookupOrBind("hot", "tk")
	jt.mu.Lock()
	jt.jobs["idle"].LastActiveAt = time.Now().Add(-JobIdleTTL - time.Second)
	jt.mu.Unlock()

	jt.sweepOnce_(time.Now())

	if _, ok := jt.Snapshot("idle"); ok {
		t.Errorf("idle job should be swept")
	}
	if _, ok := jt.Snapshot("hot"); !ok {
		t.Errorf("hot job should survive sweep")
	}
}
