package dag

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// CohortPlan is a deterministic, capability-only placement decision. Worker
// values are logical IDs; reach details never enter the plan.
type CohortPlan struct {
	Task           string   `json:"task"`
	Workers        []string `json:"workers"`
	TopologyClass  string   `json:"topology_class,omitempty"`
	TopologyDomain string   `json:"topology_domain,omitempty"`
}

// PlanCohort selects a complete compatible cohort from fresh observed facts.
// Coupled work never mixes topology domains or accelerator families. A
// single-node coupled request is the ordinary cohort-size-one case.
func PlanCohort(spec TaskSpec, facts []HostFacts, now time.Time, maxAge time.Duration) (CohortPlan, error) {
	if err := spec.ValidateForPlacement(); err != nil {
		return CohortPlan{}, err
	}
	size := spec.CohortSize
	if size == 0 {
		size = 1
	}
	type candidate struct {
		fact      HostFacts
		signature string
	}
	var candidates []candidate
	for _, fact := range facts {
		if err := fact.Validate(); err != nil || fact.Stale(now, maxAge) || !fact.Satisfies(spec) {
			continue
		}
		signature := fact.Topology.Domain
		if spec.Distribution == DistributionTopologyCoupled {
			signature += "|" + acceleratorSignature(fact.Accelerators, spec.Accelerator.Kind)
		}
		candidates = append(candidates, candidate{fact: fact, signature: signature})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].fact.Worker < candidates[j].fact.Worker })

	groups := map[string][]HostFacts{}
	var keys []string
	for _, candidate := range candidates {
		if _, ok := groups[candidate.signature]; !ok {
			keys = append(keys, candidate.signature)
		}
		groups[candidate.signature] = append(groups[candidate.signature], candidate.fact)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		if len(group) < size {
			continue
		}
		plan := CohortPlan{Task: spec.Task, TopologyClass: spec.TopologyClass}
		for _, fact := range group[:size] {
			plan.Workers = append(plan.Workers, fact.Worker)
		}
		if spec.Distribution == DistributionTopologyCoupled {
			plan.TopologyDomain = group[0].Topology.Domain
		}
		return plan, nil
	}
	return CohortPlan{}, errf(weavecli.ExitInvalidArg,
		"task %q needs a compatible cohort of %d, but observed capacity has none", spec.Task, size)
}

func acceleratorSignature(accelerators []AcceleratorFacts, kind string) string {
	var signatures []string
	for _, accelerator := range accelerators {
		if kind == "" || accelerator.Kind == kind {
			signatures = append(signatures, fmt.Sprintf("%s/%s", accelerator.Kind, accelerator.Family))
		}
	}
	sort.Strings(signatures)
	return strings.Join(signatures, ",")
}

type ChunkPlacement struct {
	Index   int      `json:"index"`
	Members []string `json:"members"`
	Worker  string   `json:"worker"`
}

// ChunkReducePlan binds stable corpus chunks to current capacity and names the
// one reducer that must consume the complete set. Membership comes exclusively
// from the immutable manifest; fleet size changes only Worker assignments.
type ChunkReducePlan struct {
	Task               string           `json:"task"`
	Reducer            string           `json:"reducer"`
	MembershipSHA256   string           `json:"membership_sha256"`
	Chunks             []ChunkPlacement `json:"chunks"`
	RequiredChunkIndex []int            `json:"required_chunk_indexes"`
}

func PlanChunkReduce(spec TaskSpec, manifest *ChunkManifest, facts []HostFacts, now time.Time, maxAge time.Duration) (ChunkReducePlan, error) {
	if err := spec.ValidateForPlacement(); err != nil {
		return ChunkReducePlan{}, err
	}
	if spec.Distribution != DistributionShardable {
		return ChunkReducePlan{}, errf(weavecli.ExitInvalidArg, "task %q is not shardable", spec.Task)
	}
	if manifest == nil {
		return ChunkReducePlan{}, errf(weavecli.ExitInvalidArg, "task %q has no chunk manifest", spec.Task)
	}
	if err := manifest.validate(); err != nil {
		return ChunkReducePlan{}, errf(weavecli.ExitInvalidArg, "task %q chunk manifest: %v", spec.Task, err)
	}

	eligible := make([]HostFacts, 0, len(facts))
	for _, fact := range facts {
		if err := fact.Validate(); err == nil && !fact.Stale(now, maxAge) && fact.Satisfies(spec) {
			eligible = append(eligible, fact)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].Worker < eligible[j].Worker })
	if len(eligible) == 0 {
		return ChunkReducePlan{}, errf(weavecli.ExitInvalidArg, "task %q has no eligible worker for its chunks", spec.Task)
	}

	chunks := append([]Chunk(nil), manifest.Chunks...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].ID < chunks[j].ID })
	plan := ChunkReducePlan{
		Task:             spec.Task,
		Reducer:          spec.Reducer,
		MembershipSHA256: strings.TrimPrefix(manifest.MembershipHash(), "m"),
	}
	for i, chunk := range chunks {
		plan.Chunks = append(plan.Chunks, ChunkPlacement{
			Index: chunk.ID, Members: chunk.Names(), Worker: eligible[i%len(eligible)].Worker,
		})
		plan.RequiredChunkIndex = append(plan.RequiredChunkIndex, chunk.ID)
	}
	return plan, nil
}
