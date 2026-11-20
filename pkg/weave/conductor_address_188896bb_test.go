package weave

import (
	"strings"
	"testing"
	"time"
)

// Story 188896bb: an active ("doing") sprint whose 30m heartbeat elapsed while
// waiting on a worker still keeps `conductor:<sprint>` addressable (with a
// stale-heartbeat warning), whereas non-doing expired leases stay excluded.
func TestConductorRoles_DoingSprintStaysAddressablePastTTLWithWarning(t *testing.T) {
	now := time.Now()
	q := &weaveQueue{Stories: []*weaveStory{
		{
			ID:     678,
			Column: "doing",
			Owner:  "claude-opus5.5",
			Lease: &weaveStoryLease{
				Holder:   "claude-opus5.5",
				Instance: "25a90a6d-9596-4eb3-a9a7-f76a018af67e",
				At:       now.Add(-45 * time.Minute),
			},
		},
		{
			ID:     679,
			Column: "done",
			Owner:  "claude-opus5.5",
			Lease: &weaveStoryLease{
				Holder: "claude-opus5.5",
				At:     now.Add(-45 * time.Minute),
			},
		},
		{
			ID:     680,
			Column: "doing",
			Owner:  "claude-opus5.5",
			Lease: &weaveStoryLease{
				Holder: "claude-opus5.5",
				At:     now.Add(time.Hour), // unproved future clock must still be refused
			},
		},
	}}

	got := rolesFromQueue(q, now)
	if len(got) != 1 {
		t.Fatalf("rolesFromQueue() = %+v, want exactly 1 doing seat", got)
	}
	if got[0].Label != "conductor:678" || got[0].Topic != "conductor.678" {
		t.Fatalf("seat = %+v, want conductor:678 / conductor.678", got[0])
	}
	if got[0].Holder != "claude-opus5.5" || got[0].HolderInstance != "25a90a6d-9596-4eb3-a9a7-f76a018af67e" {
		t.Fatalf("holder = %+v, want claude-opus5.5 + instance UUID", got[0])
	}
	if !strings.Contains(got[0].Warning, "stale") {
		t.Fatalf("warning = %q, want stale-heartbeat warning", got[0].Warning)
	}
}
