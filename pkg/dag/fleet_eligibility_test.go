// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestTaskPlacementMetadataFlowsThroughSpec(t *testing.T) {
	d := doc(t, "## Tasks\n\n### bench\nVenue: sandbox\nDistribution: shardable\nMatch: os=linux arch=arm64 libc=musl\nExclusive: true\nMemPerTask: 4GiB\n```bash\ntrue\n```\n")
	got := SpecFor(d.Tasks[0])
	want := TaskSpec{
		SchemaVersion: TaskSpecSchemaVersion, Task: "bench", Venue: VenueSandbox,
		Distribution: DistributionShardable,
		Match:        map[string]string{"os": "linux", "arch": "arm64", "libc": "musl"},
		Exclusive:    true, MemPerTask: 4 << 30,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SpecFor = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(got.Constraints(), constraintsFor(d.Tasks[0])) {
		t.Fatal("engine constraints do not use the TaskSpec projection")
	}
}

func TestPoolRefusesUnknownCapabilitiesAndStaleFacts(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name     string
		worker   *Worker
		wantReq  string
		wantMiss string
	}{
		{"missing-venue", &Worker{ID: "unknown", CPU: 1}, "venue=userland", "venue"},
		{"missing-os", &Worker{ID: "unknown", Venues: []string{VenueUserland}, CPU: 1}, "os=linux", "os"},
		{"missing-memory", &Worker{ID: "unknown", Venues: []string{VenueUserland}, CPU: 1}, "mem_bytes>=1024", "mem_bytes"},
		{"stale-inventory", &Worker{ID: "old", Venues: []string{VenueUserland}, CPU: 1, MaxFactsAge: time.Minute,
			Facts: &HostFacts{SchemaVersion: HostFactsSchemaVersion, Worker: "old", OS: "linux", Arch: "arm64", Venues: []string{VenueUserland}, ObservedAt: now.Add(-2 * time.Minute)}}, "fresh host facts", "observed_at"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Constraints{Venue: VenueUserland, Match: map[string]string{"os": "linux"}}
			if tc.wantMiss == "venue" {
				c.Match = nil
			}
			if tc.wantMiss == "mem_bytes" {
				c.Match = nil
				c.MemPerTask = 1024
			}
			p := NewPool(localTransport{}, tc.worker)
			if p.Eligible(c) {
				t.Fatal("unknown or stale capability was accepted")
			}
			refusals := p.Refusals(c)
			if len(refusals) != 1 || refusals[0].Requirement != tc.wantReq || refusals[0].Missing != tc.wantMiss {
				t.Fatalf("Refusals = %+v, want required=%q missing=%q", refusals, tc.wantReq, tc.wantMiss)
			}
			_, _, err := p.Acquire(t.Context(), c)
			var placement *PlacementError
			if !errors.As(err, &placement) || !reflect.DeepEqual(placement.Refusals, refusals) {
				t.Fatalf("Acquire error = %#v, want PlacementError with %+v", err, refusals)
			}
		})
	}
}

func TestPoolUsesObservedFactsBeforeStaticWorkerConfiguration(t *testing.T) {
	now := time.Now().UTC()
	w := &Worker{
		ID: "observed", Venues: []string{VenueUserland}, Labels: map[string]string{"os": "linux", "libc": "musl"}, CPU: 8, MemBytes: 16 << 30,
		MaxFactsAge: time.Hour,
		Facts: &HostFacts{SchemaVersion: HostFactsSchemaVersion, Worker: "observed", OS: "darwin", Arch: "arm64", MemBytes: 2 << 30,
			Venues: []string{VenueUserland}, Labels: map[string]string{"libc": "glibc"}, ObservedAt: now},
	}
	p := NewPool(localTransport{}, w)
	if p.Eligible(Constraints{Venue: VenueUserland, Match: map[string]string{"os": "linux"}}) {
		t.Fatal("static os overrode contradictory observed facts")
	}
	if p.Eligible(Constraints{Venue: VenueUserland, MemPerTask: 4 << 30}) {
		t.Fatal("static memory overrode observed memory")
	}
	if got := p.Slots(w, 1<<30); got != 2 {
		t.Fatalf("Slots = %d, want observed-memory limit 2", got)
	}
}

func TestPoolRejectsMalformedDirectConstraintsWithoutPanicking(t *testing.T) {
	now := time.Now().UTC()
	p := NewPool(localTransport{}, &Worker{
		ID: "observed", MaxFactsAge: time.Hour,
		Facts: &HostFacts{
			SchemaVersion: HostFactsSchemaVersion, Worker: "observed",
			OS: "linux", Arch: "amd64", CPU: 8, Venues: []string{VenueUserland},
			Accelerators: []AcceleratorFacts{{Kind: "cuda", Count: 1}},
			Capacities:   map[string]uint64{"example.com/device": 1},
			ObservedAt:   now,
		},
	})
	tests := []Constraints{
		{Accelerator: AcceleratorRequest{Kind: "cuda"}},
		{MinimumCapacity: map[string]uint64{"example.com/device": 0}},
		{CPUPerTask: -1},
	}
	for _, constraints := range tests {
		if p.Eligible(constraints) {
			t.Fatalf("Eligible(%+v) = true, want false", constraints)
		}
		if worker, release := p.TryAcquire(constraints); worker != nil || release != nil {
			t.Fatalf("TryAcquire(%+v) returned worker=%v release-present=%v, want neither",
				constraints, worker, release != nil)
		}
		refusals := p.Refusals(constraints)
		if len(refusals) != 1 || refusals[0].Code != "invalid-constraint" {
			t.Fatalf("Refusals(%+v) = %+v, want one invalid-constraint", constraints, refusals)
		}
	}
}

func TestPoolRefusalOrderIsDeterministic(t *testing.T) {
	p := NewPool(localTransport{},
		&Worker{ID: "b", Venues: []string{VenueUserland}, CPU: 1},
		&Worker{ID: "a", Venues: []string{VenueUserland}, CPU: 1},
	)
	c := Constraints{Venue: VenueSandbox}
	first := p.Refusals(c)
	for range 20 {
		if got := p.Refusals(c); !reflect.DeepEqual(got, first) {
			t.Fatalf("refusal ordering changed: got %+v, want %+v", got, first)
		}
	}
}
