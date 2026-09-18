package dag

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func placementFact(worker, domain, family string, memory uint64, now time.Time) HostFacts {
	return HostFacts{
		SchemaVersion: HostFactsSchemaVersion,
		Worker:        worker, OS: "linux", Arch: "amd64", CPU: 8, MemBytes: 32 << 30,
		Venues: []string{VenueNative},
		Accelerators: []AcceleratorFacts{{
			Kind: "cuda", Family: family, Count: 1, MemoryBytes: memory,
		}},
		Capacities: map[string]uint64{"dhnt.io/vram": memory, "nvidia.com/gpu": 1},
		Topology:   TopologyFacts{Class: "kubernetes.io/hostname", Domain: domain},
		ObservedAt: now,
	}
}

func TestPlacementMetadataFlowsFromDAGToConstraints(t *testing.T) {
	md := "## Tasks\n\n### pretrain\n" +
		"Venue: native\nDistribution: topology-coupled\nCPUPerTask: 4\nMemPerTask: 16GiB\n" +
		"Accelerator: cuda\nAcceleratorFamily: rtx-3070\nAcceleratorCount: 1\nAcceleratorMemory: 8GiB\n" +
		"Capacity: dhnt.io/vram=8Gi nvidia.com/gpu=1\n" +
		"Topology: kubernetes.io/hostname\nCohort: 1\n```bash\ntrue\n```\n"
	document, err := Parse(strings.NewReader(md), "dag.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := SpecFor(document.Tasks[0])
	if err := spec.ValidateForPlacement(); err != nil {
		t.Fatal(err)
	}
	if spec.CPUPerTask != 4 || spec.Accelerator.Kind != "cuda" ||
		spec.Accelerator.MemoryBytes != 8<<30 || spec.TopologyClass != "kubernetes.io/hostname" ||
		spec.MinimumCapacity["dhnt.io/vram"] != 8<<30 || spec.MinimumCapacity["nvidia.com/gpu"] != 1 ||
		spec.CohortSize != 1 {
		t.Fatalf("incomplete placement spec: %+v", spec)
	}
	if !reflect.DeepEqual(spec.Constraints(), constraintsFor(document.Tasks[0])) {
		t.Fatal("serialized and in-memory placement constraints diverged")
	}
}

func TestPlanCohortRejectsAggregateMemoryAndMixedTopology(t *testing.T) {
	now := time.Now().UTC()
	spec := TaskSpec{
		SchemaVersion: TaskSpecSchemaVersion, Task: "pretrain", Venue: VenueNative,
		Distribution: DistributionTopologyCoupled, TopologyClass: "kubernetes.io/hostname", CohortSize: 2,
		Accelerator: AcceleratorRequest{Kind: "cuda", Count: 1, MemoryBytes: 8 << 30},
	}
	facts := []HostFacts{
		placementFact("a", "fabric-a", "rtx-3070", 8<<30, now),
		placementFact("b", "fabric-b", "rtx-3070", 8<<30, now),
		placementFact("c", "fabric-a", "rtx-4090", 24<<30, now),
	}
	if _, err := PlanCohort(spec, facts, now, time.Minute); err == nil {
		t.Fatal("mixed topology domains/families formed a coupled cohort")
	}
	facts[2] = placementFact("c", "fabric-a", "rtx-3070", 8<<30, now)
	plan, err := PlanCohort(spec, facts, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Workers, []string{"a", "c"}) || plan.TopologyDomain != "fabric-a" {
		t.Fatalf("plan = %+v", plan)
	}

	single := spec
	single.CohortSize = 1
	single.Accelerator.Count = 2
	if _, err := PlanCohort(single, facts, now, time.Minute); err == nil {
		t.Fatal("two one-GPU hosts were aggregated to satisfy one-host count=2")
	}
}

func TestPlanChunkReducePreservesMembershipAcrossFleetSize(t *testing.T) {
	now := time.Now().UTC()
	manifest := &ChunkManifest{
		SchemaVersion: 1, Suite: "demo", ChunkCount: 3,
		Chunks: []Chunk{
			{ID: 3, Fixtures: []Fixture{{Name: "e"}}},
			{ID: 1, Fixtures: []Fixture{{Name: "a"}, {Name: "b"}}},
			{ID: 2, Fixtures: []Fixture{{Name: "c"}, {Name: "d"}}},
		},
	}
	spec := TaskSpec{
		SchemaVersion: TaskSpecSchemaVersion, Task: "dataset", Venue: VenueNative,
		Distribution: DistributionShardable, Reducer: "dataset-reduce",
	}
	facts := []HostFacts{
		placementFact("worker-b", "fabric-b", "rtx-3070", 8<<30, now),
		placementFact("worker-a", "fabric-a", "rtx-3070", 8<<30, now),
	}
	two, err := PlanChunkReduce(spec, manifest, facts, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	one, err := PlanChunkReduce(spec, manifest, facts[:1], now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if two.MembershipSHA256 != one.MembershipSHA256 ||
		!reflect.DeepEqual(two.RequiredChunkIndex, []int{1, 2, 3}) ||
		!reflect.DeepEqual(two.Chunks[0].Members, one.Chunks[0].Members) ||
		two.Reducer != "dataset-reduce" {
		t.Fatalf("fleet size changed corpus/reduce contract:\ntwo=%+v\none=%+v", two, one)
	}
	if two.Chunks[0].Worker != "worker-a" || two.Chunks[1].Worker != "worker-b" {
		t.Fatalf("chunk placement is not deterministic: %+v", two.Chunks)
	}
}

func TestPlacementValidationFailsClosed(t *testing.T) {
	tests := []TaskSpec{
		{SchemaVersion: 1, Task: "coupled", Venue: VenueNative, Distribution: DistributionTopologyCoupled, CohortSize: 1},
		{SchemaVersion: 1, Task: "shards", Venue: VenueNative, Distribution: DistributionShardable},
		{SchemaVersion: 1, Task: "single", Venue: VenueNative, Distribution: DistributionSingle, CohortSize: 2},
	}
	for _, spec := range tests {
		if err := spec.ValidateForPlacement(); err == nil {
			t.Fatalf("unsafe placement spec accepted: %+v", spec)
		}
	}

	document, err := Parse(strings.NewReader(
		"## Tasks\n\n### bad\nVenue: native\nDistribution: topology-coupled\n"+
			"CPUPerTask: many\nAccelerator: cuda\nAcceleratorCount: one\n"+
			"AcceleratorMemory: huge\nCapacity: dhnt.io/vram=lots\n"+
			"Topology: kubernetes.io/hostname\nCohort: 1\n```bash\ntrue\n```\n",
	), "dag.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := SpecFor(document.Tasks[0])
	if err := spec.ValidateForPlacement(); err == nil {
		t.Fatal("malformed capacity quantities were silently weakened")
	}
}
