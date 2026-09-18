// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// This is the CONTRACT leg of fleet execution, beside fleet.go (capacity) and
// chunks.go (corpus). The three types here are the minimal versioned shapes the
// scheduler exchanges across a process, host, or repo boundary:
//
//	HostFacts — what a worker host OBSERVABLY is (capability, never reach)
//	TaskSpec  — what a task DEMANDS (derived from task metadata, never the pool)
//	RunRecord — what ONE attempt DID (per-attempt, never per-task)
//
// Each carries an explicit SchemaVersion so a reader can refuse shapes it does
// not understand and a scheduler can detect stale facts, and each is committed
// or published as-is — which is why the standing privacy rule binds here
// hardest: no hostname, username, IP, or credential may appear in any of these
// types. Worker identity is a LOGICAL id (see LocalWorkerID — a venue name, not
// a hostname). Reach details (address, ssh user/port) live in pkg/fleet.Host
// and are consumed only at dial time by a transport; they never enter a
// RunRecord or a committed manifest.

// Schema versions for the three fleet contracts. Bump only on an incompatible
// shape change; readers reject anything newer than they were built for.
const (
	HostFactsSchemaVersion = 1
	TaskSpecSchemaVersion  = 1
	RunRecordSchemaVersion = 1
)

// reachKeys are label/match keys that would smuggle reach or identity details
// into a committed contract. Facts and specs carry CAPABILITY (os, arch, libc,
// gpu, …); where a machine is and who logs into it is pkg/fleet.Host's job, at
// dial time only.
var reachKeys = map[string]bool{
	"host": true, "hostname": true, "fqdn": true,
	"ip": true, "address": true, "addr": true,
	"user": true, "username": true,
	"ssh": true, "ssh_user": true, "ssh_port": true,
	"password": true, "token": true, "secret": true, "credential": true,
}

func reachKey(k string) bool { return reachKeys[strings.ToLower(strings.TrimSpace(k))] }

// checkSchemaVersion is the shared version gate: unversioned (0) shapes and
// shapes newer than this binary are both refused, with distinct messages so a
// reader can tell "old writer" from "upgrade me".
func checkSchemaVersion(what string, got, supported int) error {
	if got < 1 {
		return errf(weavecli.ExitInvalidArg, "%s has no schema_version (want %d)", what, supported)
	}
	if got > supported {
		return errf(weavecli.ExitInvalidArg,
			"%s schema_version %d is newer than this binary supports (%d)", what, got, supported)
	}
	return nil
}

// HostFacts is the OBSERVED capability of a worker host: what a probe of the
// machine reported, stamped with when it looked. It is capability only — the
// scheduler matches a TaskSpec against it — and deliberately has no field for
// where the machine is or who runs there.
type HostFacts struct {
	SchemaVersion int    `json:"schema_version"`
	Worker        string `json:"worker"` // logical worker id — never a hostname

	OS       string   `json:"os"`                  // GOOS vocabulary
	Arch     string   `json:"arch"`                // GOARCH vocabulary
	CPU      int      `json:"cpu"`                 // schedulable cores
	MemBytes uint64   `json:"mem_bytes,omitempty"` // 0 => unknown (not memory-gated)
	Venues   []string `json:"venues"`              // which venues the host can offer

	// Labels are extra capability facts (libc=musl, gpu=none, …). Reach-shaped
	// keys are rejected by Validate — see reachKeys.
	Labels map[string]string `json:"labels,omitempty"`

	// Accelerator and topology are observed scheduling facts. Domain is a
	// logical fabric identifier (for example "dragon-local"), never a hostname
	// or address. A coupled cohort may only span one domain.
	Accelerators []AcceleratorFacts `json:"accelerators,omitempty"`
	Topology     TopologyFacts      `json:"topology,omitempty"`
	Capacities   map[string]uint64  `json:"capacities,omitempty"`

	// ObservedAt is when the probe ran, in UTC. Facts are a snapshot, not a
	// registry: a scheduler must treat old facts as stale (see Stale), not true.
	ObservedAt time.Time `json:"observed_at"`
}

type AcceleratorFacts struct {
	Kind        string `json:"kind"`                   // cuda | mps | rocm | ...
	Family      string `json:"family,omitempty"`       // exact homogeneous family
	Count       int    `json:"count"`                  // schedulable devices
	MemoryBytes uint64 `json:"memory_bytes,omitempty"` // minimum bytes per device
}

type TopologyFacts struct {
	Class  string `json:"class,omitempty"`  // grouping key, e.g. kubernetes.io/hostname
	Domain string `json:"domain,omitempty"` // logical cohort domain, not reach
}

// ObserveLocalHost probes the machine this process runs on and returns its
// facts under the local worker's LOGICAL identity. MemBytes is left 0
// (unknown): the stdlib has no portable total-memory probe, and 0 already
// means "not memory-gated" everywhere in the pool.
func ObserveLocalHost(now time.Time) HostFacts {
	return HostFacts{
		SchemaVersion: HostFactsSchemaVersion,
		Worker:        LocalWorkerID,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPU:           runtime.NumCPU(),
		Venues:        []string{VenueUserland},
		ObservedAt:    now.UTC(),
	}
}

// Validate rejects facts a scheduler must not act on: an unknown schema
// version, a missing identity or capability field, or a label that would carry
// reach details into a committed manifest.
func (f *HostFacts) Validate() error {
	if err := checkSchemaVersion("host facts", f.SchemaVersion, HostFactsSchemaVersion); err != nil {
		return err
	}
	if strings.TrimSpace(f.Worker) == "" {
		return errf(weavecli.ExitInvalidArg, "host facts have no worker id")
	}
	if f.OS == "" || f.Arch == "" {
		return errf(weavecli.ExitInvalidArg, "host facts for %q are missing os/arch", f.Worker)
	}
	if len(f.Venues) == 0 {
		return errf(weavecli.ExitInvalidArg, "host facts for %q offer no venue", f.Worker)
	}
	if f.ObservedAt.IsZero() {
		return errf(weavecli.ExitInvalidArg, "host facts for %q have no observed_at", f.Worker)
	}
	for k := range f.Labels {
		if reachKey(k) {
			return errf(weavecli.ExitInvalidArg,
				"host facts for %q carry reach label %q — reach details belong in pkg/fleet.Host, not in facts", f.Worker, k)
		}
	}
	for k := range f.Capacities {
		if reachKey(k) {
			return errf(weavecli.ExitInvalidArg, "host facts for %q carry reach capacity %q", f.Worker, k)
		}
	}
	for i, accelerator := range f.Accelerators {
		if accelerator.Kind == "" || accelerator.Count < 1 {
			return errf(weavecli.ExitInvalidArg, "host facts for %q have invalid accelerator %d", f.Worker, i)
		}
	}
	if (f.Topology.Class == "") != (f.Topology.Domain == "") {
		return errf(weavecli.ExitInvalidArg, "host facts for %q must declare topology class and domain together", f.Worker)
	}
	return nil
}

// Stale reports whether these facts must be re-observed before scheduling on
// them: written under a different schema, never stamped, or older than maxAge.
func (f *HostFacts) Stale(now time.Time, maxAge time.Duration) bool {
	if f.SchemaVersion != HostFactsSchemaVersion || f.ObservedAt.IsZero() {
		return true
	}
	return now.Sub(f.ObservedAt) > maxAge
}

// Satisfies reports whether a host with these facts could run the task the
// spec describes. The os and arch match keys resolve against the typed fields;
// everything else against Labels. Staleness is the caller's check — facts that
// satisfy a spec are still unusable if Stale.
func (f *HostFacts) Satisfies(s TaskSpec) bool {
	venue := s.Venue
	if venue == "" {
		venue = VenueUserland
	}
	if !slices.Contains(f.Venues, venue) {
		return false
	}
	for k, v := range s.Match {
		switch strings.ToLower(k) {
		case "os":
			if f.OS != v {
				return false
			}
		case "arch":
			if f.Arch != v {
				return false
			}
		default:
			if f.Labels[k] != v {
				return false
			}
		}
	}
	if s.MemPerTask > 0 {
		// An absent observation is not evidence that the task fits. Treat it as
		// unknown so a memory-gated task cannot be placed speculatively.
		if f.MemBytes == 0 || f.MemBytes < s.MemPerTask {
			return false
		}
	}
	if s.CPUPerTask > 0 && f.CPU < s.CPUPerTask {
		return false
	}
	if !acceleratorSatisfies(f.Accelerators, s.Accelerator) {
		return false
	}
	if s.TopologyClass != "" && f.Topology.Class != s.TopologyClass {
		return false
	}
	for resource, minimum := range s.MinimumCapacity {
		if f.Capacities[resource] < minimum {
			return false
		}
	}
	return true
}

// ParseHostFacts reads and validates serialized host facts.
func ParseHostFacts(data []byte) (*HostFacts, error) {
	f := &HostFacts{}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "parse host facts: %v", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// TaskSpec is what one task demands of a worker, derived from the task's own
// metadata and NEVER from the pool: the same task file must produce the same
// spec whether zero or twenty workers are online. It is the serializable,
// versioned form of Constraints plus the per-attempt policy a remote venue
// must enforce on its side of the wire (timeout, retries).
//
// Task.Host (placement intent, a fleet alias) is deliberately NOT here: an
// alias resolves to reach details at dial time via pkg/fleet, and the resolved
// form must never travel with the spec.
type TaskSpec struct {
	SchemaVersion int    `json:"schema_version"`
	Task          string `json:"task"`

	Venue           string             `json:"venue"`
	Distribution    Distribution       `json:"distribution,omitempty"`
	Match           map[string]string  `json:"match,omitempty"` // host capability labels
	Exclusive       bool               `json:"exclusive,omitempty"`
	CPUPerTask      int                `json:"cpu_per_task,omitempty"`
	MemPerTask      uint64             `json:"mem_per_task,omitempty"`
	Accelerator     AcceleratorRequest `json:"accelerator,omitempty"`
	MinimumCapacity map[string]uint64  `json:"minimum_capacity,omitempty"`
	TopologyClass   string             `json:"topology_class,omitempty"`
	CohortSize      int                `json:"cohort_size,omitempty"`
	Reducer         string             `json:"reducer,omitempty"`

	Timeout time.Duration `json:"timeout_ns,omitempty"`
	Retries int           `json:"retries,omitempty"`
}

// SpecFor derives a task's demand from its metadata. A task with no Venue:
// requests the ordinary userland lane; all declared requirements travel through
// this function before they reach the scheduler.
func SpecFor(t *Task) TaskSpec {
	venue := t.Venue
	if venue == "" {
		venue = VenueUserland
	}
	return TaskSpec{
		SchemaVersion:   TaskSpecSchemaVersion,
		Task:            t.Name,
		Venue:           venue,
		Distribution:    t.Distribution,
		Match:           t.Match,
		Exclusive:       t.Exclusive,
		CPUPerTask:      t.CPUPerTask,
		MemPerTask:      t.MemPerTask,
		Accelerator:     t.Accelerator,
		MinimumCapacity: t.MinimumCapacity,
		TopologyClass:   t.TopologyClass,
		CohortSize:      t.CohortSize,
		Reducer:         t.Reducer,
		Timeout:         t.Timeout,
		Retries:         t.Retries,
	}
}

// Constraints projects the spec onto the pool's in-memory constraint shape.
// This is the one bridge between the committed contract and the scheduler.
func (s TaskSpec) Constraints() Constraints {
	return Constraints{
		Venue:           s.Venue,
		Match:           s.Match,
		Exclusive:       s.Exclusive,
		CPUPerTask:      s.CPUPerTask,
		MemPerTask:      s.MemPerTask,
		Accelerator:     s.Accelerator,
		MinimumCapacity: s.MinimumCapacity,
		TopologyClass:   s.TopologyClass,
	}
}

// Validate rejects a spec no scheduler should place: unknown schema version,
// unnamed task, unknown venue, or a match key that would demand reach details
// rather than capability.
func (s *TaskSpec) Validate() error {
	return s.validate(false)
}

// ValidateForPipeline applies the strict dhnt.pipeline/v1 boundary: unlike
// ordinary DAG execution, a pipeline lowering must state how every task may be
// distributed. This opt-in preserves compatibility with legacy DAG files while
// giving DKS and other pipeline consumers a fail-closed contract.
func (s *TaskSpec) ValidateForPipeline() error {
	return s.validate(true)
}

func (s *TaskSpec) validate(requireDistribution bool) error {
	if err := checkSchemaVersion("task spec", s.SchemaVersion, TaskSpecSchemaVersion); err != nil {
		return err
	}
	if strings.TrimSpace(s.Task) == "" {
		return errf(weavecli.ExitInvalidArg, "task spec has no task name")
	}
	switch s.Venue {
	case VenueUserland, VenueWorkspace, VenueSandbox, VenueNative, VenueCluster, VenueCloud:
	default:
		return errf(weavecli.ExitInvalidArg, "task spec for %q names unknown venue %q", s.Task, s.Venue)
	}
	switch s.Distribution {
	case DistributionSingle, DistributionShardable, DistributionReplicated, DistributionTopologyCoupled:
	case "":
		if requireDistribution {
			return errf(weavecli.ExitInvalidArg, "task spec for %q has no distribution", s.Task)
		}
	default:
		return errf(weavecli.ExitInvalidArg, "task spec for %q names unknown distribution %q", s.Task, s.Distribution)
	}
	for k := range s.Match {
		if reachKey(k) {
			return errf(weavecli.ExitInvalidArg,
				"task spec for %q matches on reach key %q — specs demand capability, never reach", s.Task, k)
		}
	}
	for k, minimum := range s.MinimumCapacity {
		if reachKey(k) {
			return errf(weavecli.ExitInvalidArg, "task spec for %q requests reach capacity %q", s.Task, k)
		}
		if strings.TrimSpace(k) == "" || minimum == 0 {
			return errf(weavecli.ExitInvalidArg, "task spec for %q has invalid minimum capacity %q", s.Task, k)
		}
	}
	if s.CPUPerTask < 0 {
		return errf(weavecli.ExitInvalidArg, "task spec for %q has negative cpu_per_task", s.Task)
	}
	if err := s.Accelerator.validate(); err != nil {
		return errf(weavecli.ExitInvalidArg, "task spec for %q: %v", s.Task, err)
	}
	if s.CohortSize < 0 {
		return errf(weavecli.ExitInvalidArg, "task spec for %q has negative cohort_size", s.Task)
	}
	return nil
}

// ValidateForPlacement is the fail-closed scheduler boundary. Legacy DAG and
// pipeline readers remain additive-compatible, while an executor that claims
// topology/chunk safety opts into these stronger requirements.
func (s *TaskSpec) ValidateForPlacement() error {
	if err := s.ValidateForPipeline(); err != nil {
		return err
	}
	switch s.Distribution {
	case DistributionTopologyCoupled:
		if s.TopologyClass == "" {
			return errf(weavecli.ExitInvalidArg, "task spec for %q topology-coupled work requires topology_class", s.Task)
		}
		if s.CohortSize < 1 {
			return errf(weavecli.ExitInvalidArg, "task spec for %q topology-coupled work requires positive cohort_size", s.Task)
		}
	case DistributionShardable:
		if strings.TrimSpace(s.Reducer) == "" {
			return errf(weavecli.ExitInvalidArg, "task spec for %q shardable work requires a reducer", s.Task)
		}
	default:
		if s.CohortSize > 1 {
			return errf(weavecli.ExitInvalidArg, "task spec for %q non-coupled work cannot request cohort_size %d", s.Task, s.CohortSize)
		}
	}
	return nil
}

// ParseTaskSpec reads and validates a serialized task spec.
func ParseTaskSpec(data []byte) (*TaskSpec, error) {
	s := &TaskSpec{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "parse task spec: %v", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// ParseTaskSpecForPipeline reads a serialized task spec at the strict
// dhnt.pipeline/v1 boundary, where Distribution is required.
func ParseTaskSpecForPipeline(data []byte) (*TaskSpec, error) {
	s := &TaskSpec{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "parse task spec: %v", err)
	}
	if err := s.ValidateForPipeline(); err != nil {
		return nil, err
	}
	return s, nil
}

// RunStatus classifies ONE attempt. The vocabulary separates the two things a
// failure can mean, because they demand opposite reactions:
//
//	RunFailed      — CONFORMANCE: the worker executed the body and the verdict
//	                 is failure. Retrying elsewhere will not change it.
//	RunInfraFailed — INFRASTRUCTURE: the fleet could not deliver a verdict
//	                 (worker unreachable, no eligible worker, orchestrator
//	                 cancelled). The corpus was never judged.
//
// An infra-failed attempt is NEVER folded into skip or fail counts: an
// unreachable worker says nothing about the code under test. Schedulers
// re-place the work or surface the infrastructure problem by name.
type RunStatus string

const (
	RunPassed      RunStatus = "passed"
	RunFailed      RunStatus = "failed"
	RunInfraFailed RunStatus = "infra-failed"
)

// HasVerdict reports whether an attempt with this status judged the corpus at
// all. Infra failures did not — they are void attempts, not verdicts.
func (s RunStatus) HasVerdict() bool { return s == RunPassed || s == RunFailed }

// Stable machine-readable failure codes for FailureReason.Code.
const (
	FailExitNonzero   = "exit-nonzero"         // conformance: body exited non-zero
	FailTimeout       = "timeout"              // conformance: body exceeded its declared Timeout
	FailPrecondition  = "precondition-failed"  // conformance: a Require check failed, body never ran
	FailPostcondition = "postcondition-failed" // conformance: body exited 0 but Ensure failed
	FailNoWorker      = "no-eligible-worker"   // infra: no worker could ever satisfy the spec
	FailUnreachable   = "worker-unreachable"   // infra: transport could not reach the worker
	FailCanceled      = "canceled"             // infra: orchestrator cancelled before a verdict
	FailUnclassified  = "unclassified"         // infra: failed for a reason we cannot place on the body
)

// FailureReason is the structured half of a failed attempt: a stable code a
// scheduler can branch on. It deliberately has no free-text field. Hostnames
// and addresses cannot be recognized reliably in arbitrary prose, so prose is
// excluded by construction from records, which travel into committed
// artifacts. Full human-readable errors still reach the operator through the
// task's stdout/stderr and local log.
type FailureReason struct {
	Code string `json:"code"`
}

// ErrWorkerUnreachable is the sentinel a transport wraps when it could not
// reach its worker at all. It is the difference between "the body ran and
// failed" and "we never got a verdict", which no exit code can express: a
// transport that cannot dial has no body exit code to report. Transport.Exec
// implementations wrap it so RecordAttempt cannot mistake non-delivery for a
// conformance verdict. See Transport.Exec and RecordAttempt for the recorder
// side of this contract.
var ErrWorkerUnreachable = errors.New("worker unreachable")

// FleetFailure is implemented by errors that carry their OWN classification.
// It is how a transport hands the recorder a curated reason instead of a raw
// error string. Transport.Exec implementations may return one for failures
// that need a classification other than ErrWorkerUnreachable; RecordAttempt
// accepts only known stable status+codes and never its Error text.
type FleetFailure interface {
	FleetFailure() (status RunStatus, reason FailureReason)
}

// RunRecord is the outcome of ONE attempt of one task — attempt three of a
// retried task is its own record, not a mutation of attempt one's. Worker is
// the LOGICAL worker id (LocalWorkerID-style): a record stamped with the
// machine that produced it could not be compared against one produced
// anywhere else, and records travel into committed artifacts.
type RunRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Task          string `json:"task"`
	Attempt       int    `json:"attempt"` // 1-based
	Worker        string `json:"worker"`  // logical id — never a hostname
	Venue         string `json:"venue"`

	Status   RunStatus      `json:"status"`
	ExitCode int            `json:"exit_code"`
	Duration time.Duration  `json:"duration_ns"`
	Failure  *FailureReason `json:"failure,omitempty"` // nil iff Status == RunPassed
}

// RecordAttempt seals one attempt's TaskResult into a RunRecord. w may be nil
// (no fleet configured), which records the local logical worker. Only the
// worker's logical ID enters the record — never Worker.Host, which is reach.
//
// Classification obeys one rule: A VERDICT MUST BE POSITIVELY EARNED. A
// StatusFailed result records a conformance verdict because executors use that
// status only after the body ran and lost. A transport that could not deliver
// the body MUST instead wrap ErrWorkerUnreachable or return an error implementing
// FleetFailure; otherwise its StatusFailed result is indistinguishable here
// from a real body failure and records as RunFailed against code that never
// ran. See Transport.Exec for the producer side.
//
// No text is ever taken from res.Err. A FleetFailure may publish only a known,
// stable FailureReason.Code; operator-facing prose stays in stdout/stderr and
// the local log.
func RecordAttempt(t *Task, w *Worker, attempt int, res TaskResult) RunRecord {
	worker := LocalWorkerID
	if w != nil && w.ID != "" {
		worker = w.ID
	}
	r := RunRecord{
		SchemaVersion: RunRecordSchemaVersion,
		Task:          t.Name,
		Attempt:       max(1, attempt),
		Worker:        worker,
		Venue:         SpecFor(t).Venue,
		ExitCode:      res.ExitCode,
		Duration:      res.Duration,
	}
	var carrier FleetFailure
	switch {
	case res.Err != nil && errors.As(res.Err, &carrier):
		// The producer classified its own failure and chose its own detail.
		r.Status, r.Failure = classifiedBy(carrier)
	case res.Err != nil && ExitCodeOf(res.Err) == weavecli.ExitDepUnhealthy:
		r.Status, r.Failure = RunInfraFailed, &FailureReason{Code: FailNoWorker}
	case res.Err != nil && errors.Is(res.Err, ErrWorkerUnreachable):
		r.Status, r.Failure = RunInfraFailed, &FailureReason{Code: FailUnreachable}
	case res.Err != nil && errors.Is(res.Err, context.Canceled):
		r.Status, r.Failure = RunInfraFailed, &FailureReason{Code: FailCanceled}
	case res.Err != nil && errors.Is(res.Err, context.DeadlineExceeded):
		r.Status, r.Failure = RunInfraFailed, &FailureReason{Code: FailCanceled}
	case res.Status == StatusFailed:
		// The body ran and lost. Reaching StatusFailed means an executor got far
		// enough to run the body and observe its exit, so the failure is the
		// code's own — which is precisely why the obligation above is on the
		// TRANSPORT: a transport that cannot deliver a task must say so with
		// ErrWorkerUnreachable or a FleetFailure, because once an undeliverable
		// attempt arrives here indistinguishable from a real exit, no amount of
		// care at this layer can tell them apart. The verdict is earned by the
		// executor having run the body; the sentinels are what keep attempts
		// that never ran from reaching this branch at all.
		r.Status = RunFailed
		code := FailExitNonzero
		switch {
		case res.ExitCode == 124:
			code = FailTimeout
		case res.Attestation != nil && !res.Attestation.Valid && res.Attestation.Clause == "require":
			code = FailPrecondition
		case res.Attestation != nil && !res.Attestation.Valid:
			code = FailPostcondition
		}
		r.Failure = &FailureReason{Code: code}
	default:
		r.Status = RunPassed
	}
	return r
}

// classifiedBy takes a producer's own classification, but does not take it on
// trust: a carrier that returns success, an unknown status, or an unknown code
// is downgraded to an unclassified infra failure rather than allowed to write a
// verdict. Failure reasons contain no prose, so reach details cannot enter a
// record through this carrier surface.
func classifiedBy(c FleetFailure) (RunStatus, *FailureReason) {
	status, reason := c.FleetFailure()
	switch status {
	case RunPassed:
		return RunInfraFailed, &FailureReason{Code: FailUnclassified}
	case RunFailed, RunInfraFailed:
	default:
		return RunInfraFailed, &FailureReason{Code: FailUnclassified}
	}
	if !knownFailureCode(reason.Code) {
		return RunInfraFailed, &FailureReason{Code: FailUnclassified}
	}
	return status, &reason
}

func knownFailureCode(code string) bool {
	switch code {
	case FailExitNonzero, FailTimeout, FailPrecondition, FailPostcondition, FailNoWorker,
		FailUnreachable, FailCanceled, FailUnclassified:
		return true
	default:
		return false
	}
}

// Validate rejects a record no aggregator should count: unknown schema
// version, missing identity, an unknown status, or a failure shape that
// contradicts the status.
func (r *RunRecord) Validate() error {
	if err := checkSchemaVersion("run record", r.SchemaVersion, RunRecordSchemaVersion); err != nil {
		return err
	}
	if strings.TrimSpace(r.Task) == "" {
		return errf(weavecli.ExitInvalidArg, "run record has no task name")
	}
	if r.Attempt < 1 {
		return errf(weavecli.ExitInvalidArg, "run record for %q has attempt %d (attempts are 1-based)", r.Task, r.Attempt)
	}
	if strings.TrimSpace(r.Worker) == "" {
		return errf(weavecli.ExitInvalidArg, "run record for %q has no worker id", r.Task)
	}
	switch r.Status {
	case RunPassed:
		if r.Failure != nil {
			return errf(weavecli.ExitInvalidArg, "run record for %q passed but carries a failure reason", r.Task)
		}
	case RunFailed, RunInfraFailed:
		if r.Failure == nil || r.Failure.Code == "" {
			return errf(weavecli.ExitInvalidArg, "run record for %q is %s but has no failure code", r.Task, r.Status)
		}
		if !knownFailureCode(r.Failure.Code) {
			return errf(weavecli.ExitInvalidArg, "run record for %q has unknown failure code %q", r.Task, r.Failure.Code)
		}
	default:
		return errf(weavecli.ExitInvalidArg, "run record for %q has unknown status %q", r.Task, r.Status)
	}
	return nil
}

// ParseRunRecord reads and validates a serialized run record.
func ParseRunRecord(data []byte) (*RunRecord, error) {
	r := &RunRecord{}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, errf(weavecli.ExitInvalidArg, "parse run record: %v", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}
