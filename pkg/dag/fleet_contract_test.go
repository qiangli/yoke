// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

var contractNow = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

func validFacts() *HostFacts {
	return &HostFacts{
		SchemaVersion: HostFactsSchemaVersion,
		Worker:        LocalWorkerID,
		OS:            "linux",
		Arch:          "arm64",
		CPU:           8,
		MemBytes:      16 << 30,
		Venues:        []string{VenueUserland, VenueSandbox},
		Labels:        map[string]string{"libc": "musl"},
		ObservedAt:    contractNow,
	}
}

func validSpec() *TaskSpec {
	return &TaskSpec{
		SchemaVersion: TaskSpecSchemaVersion,
		Task:          "suite:shard=3",
		Venue:         VenueUserland,
		Distribution:  DistributionShardable,
		Match:         map[string]string{"os": "linux", "libc": "musl"},
		MemPerTask:    4 << 30,
		Timeout:       90 * time.Second,
		Retries:       2,
	}
}

func TestTaskSpecDistributionValidationModes(t *testing.T) {
	legacy := validSpec()
	legacy.Distribution = ""
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy validation rejected missing distribution: %v", err)
	}
	if err := legacy.ValidateForPipeline(); err == nil || !strings.Contains(err.Error(), "no distribution") {
		t.Fatalf("strict validation error = %v, want missing-distribution refusal", err)
	}

	for _, distribution := range []Distribution{
		DistributionSingle,
		DistributionShardable,
		DistributionReplicated,
		DistributionTopologyCoupled,
	} {
		spec := validSpec()
		spec.Distribution = distribution
		if err := spec.ValidateForPipeline(); err != nil {
			t.Errorf("strict validation rejected %q: %v", distribution, err)
		}
	}

	unknown := validSpec()
	unknown.Distribution = "scatter"
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "unknown distribution") {
		t.Fatalf("ordinary validation error = %v, want unknown-distribution refusal", err)
	}
}

func TestTaskSpecAcceptsClusterAndCloudVenues(t *testing.T) {
	for _, venue := range []string{VenueNative, VenueCluster, VenueCloud} {
		spec := validSpec()
		spec.Venue = venue
		if err := spec.Validate(); err != nil {
			t.Errorf("Validate rejected venue %q: %v", venue, err)
		}
	}
}

func TestParseTaskSpecForPipelineRequiresDistribution(t *testing.T) {
	spec := validSpec()
	spec.Distribution = ""
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTaskSpec(data); err != nil {
		t.Fatalf("legacy parser rejected missing distribution: %v", err)
	}
	if _, err := ParseTaskSpecForPipeline(data); err == nil {
		t.Fatal("strict pipeline parser accepted missing distribution")
	}
}

func validRecord() *RunRecord {
	return &RunRecord{
		SchemaVersion: RunRecordSchemaVersion,
		Task:          "suite:shard=3",
		Attempt:       2,
		Worker:        LocalWorkerID,
		Venue:         VenueUserland,
		Status:        RunFailed,
		ExitCode:      1,
		Duration:      3 * time.Second,
		Failure:       &FailureReason{Code: FailExitNonzero},
	}
}

// TestContractRoundTrip pins that each contract survives serialize → parse
// unchanged: what a scheduler commits is exactly what a reader gets back.
func TestContractRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		value any
		parse func([]byte) (any, error)
	}{
		{"host-facts", validFacts(), func(b []byte) (any, error) { return ParseHostFacts(b) }},
		{"task-spec", validSpec(), func(b []byte) (any, error) { return ParseTaskSpec(b) }},
		{"run-record", validRecord(), func(b []byte) (any, error) { return ParseRunRecord(b) }},
		{"run-record-passed", &RunRecord{
			SchemaVersion: RunRecordSchemaVersion, Task: "build", Attempt: 1,
			Worker: LocalWorkerID, Venue: VenueUserland,
			Status: RunPassed, Duration: time.Second,
		}, func(b []byte) (any, error) { return ParseRunRecord(b) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := tc.parse(data)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(got, tc.value) {
				t.Fatalf("round trip changed the value:\n got %+v\nwant %+v", got, tc.value)
			}
		})
	}
}

// TestContractSchemaVersion pins version handling: unversioned and
// newer-than-supported shapes are refused with a clear error; current parses.
func TestContractSchemaVersion(t *testing.T) {
	set := func(v any, version int) []byte {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		m["schema_version"] = version
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	parsers := []struct {
		name      string
		value     any
		supported int
		parse     func([]byte) error
	}{
		{"host-facts", validFacts(), HostFactsSchemaVersion,
			func(b []byte) error { _, err := ParseHostFacts(b); return err }},
		{"task-spec", validSpec(), TaskSpecSchemaVersion,
			func(b []byte) error { _, err := ParseTaskSpec(b); return err }},
		{"run-record", validRecord(), RunRecordSchemaVersion,
			func(b []byte) error { _, err := ParseRunRecord(b); return err }},
	}
	for _, p := range parsers {
		t.Run(p.name, func(t *testing.T) {
			if err := p.parse(set(p.value, p.supported)); err != nil {
				t.Fatalf("current version rejected: %v", err)
			}
			for _, bad := range []int{0, p.supported + 1} {
				err := p.parse(set(p.value, bad))
				if err == nil {
					t.Fatalf("schema_version %d accepted, want refusal", bad)
				}
				if ExitCodeOf(err) != weavecli.ExitInvalidArg {
					t.Fatalf("schema_version %d: exit code %d, want %d", bad, ExitCodeOf(err), weavecli.ExitInvalidArg)
				}
			}
		})
	}
}

// TestHostFactsStale pins the stale-facts check: fresh facts pass, old or
// wrong-schema or unstamped facts must be re-observed.
func TestHostFactsStale(t *testing.T) {
	maxAge := time.Hour
	tests := []struct {
		name  string
		mut   func(*HostFacts)
		stale bool
	}{
		{"fresh", func(f *HostFacts) {}, false},
		{"at-max-age", func(f *HostFacts) { f.ObservedAt = contractNow.Add(-maxAge) }, false},
		{"too-old", func(f *HostFacts) { f.ObservedAt = contractNow.Add(-maxAge - time.Second) }, true},
		{"never-stamped", func(f *HostFacts) { f.ObservedAt = time.Time{} }, true},
		{"old-schema", func(f *HostFacts) { f.SchemaVersion = HostFactsSchemaVersion + 1 }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := validFacts()
			tc.mut(f)
			if got := f.Stale(contractNow, maxAge); got != tc.stale {
				t.Fatalf("Stale = %v, want %v", got, tc.stale)
			}
		})
	}
}

// TestObserveLocalHost pins that local observation produces valid, non-stale
// facts under the LOGICAL worker id.
func TestObserveLocalHost(t *testing.T) {
	f := ObserveLocalHost(contractNow)
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if f.Worker != LocalWorkerID {
		t.Fatalf("Worker = %q, want %q", f.Worker, LocalWorkerID)
	}
	if f.Stale(contractNow, time.Hour) {
		t.Fatal("freshly observed facts report stale")
	}
	if !f.Satisfies(SpecFor(&Task{Name: "build"})) {
		t.Fatal("local facts do not satisfy the default userland spec")
	}
}

// TestHostFactsSatisfies pins spec-vs-facts matching: venue, typed os/arch
// keys, capability labels, and the memory gate.
func TestHostFactsSatisfies(t *testing.T) {
	tests := []struct {
		name string
		spec TaskSpec
		want bool
	}{
		{"default", *validSpec(), true},
		{"venue-not-offered", TaskSpec{Venue: VenueWorkspace}, false},
		{"empty-venue-is-userland", TaskSpec{}, true},
		{"os-mismatch", TaskSpec{Venue: VenueUserland, Match: map[string]string{"os": "windows"}}, false},
		{"arch-match", TaskSpec{Venue: VenueUserland, Match: map[string]string{"arch": "arm64"}}, true},
		{"label-mismatch", TaskSpec{Venue: VenueUserland, Match: map[string]string{"libc": "glibc"}}, false},
		{"mem-gated-out", TaskSpec{Venue: VenueUserland, MemPerTask: 32 << 30}, false},
		{"mem-fits", TaskSpec{Venue: VenueUserland, MemPerTask: 8 << 30}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validFacts().Satisfies(tc.spec); got != tc.want {
				t.Fatalf("Satisfies = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpecForDerivesFromTaskOnly pins that a spec is a pure function of task
// metadata and projects onto exactly the constraints the scheduler uses.
func TestSpecForDerivesFromTaskOnly(t *testing.T) {
	task := &Task{Name: "bench", Timeout: 30 * time.Second, Retries: 3, Host: "some-alias"}
	s := SpecFor(task)
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.Task != "bench" || s.Venue != VenueUserland || s.Timeout != 30*time.Second || s.Retries != 3 {
		t.Fatalf("unexpected spec: %+v", s)
	}
	if !reflect.DeepEqual(s.Constraints(), constraintsFor(task)) {
		t.Fatalf("spec constraints %+v disagree with scheduler constraints %+v", s.Constraints(), constraintsFor(task))
	}
	// Placement intent (Task.Host) resolves to reach at dial time; it must not
	// appear anywhere in the committed spec.
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "some-alias") {
		t.Fatalf("Task.Host leaked into the spec: %s", data)
	}
}

// TestRecordAttemptClassification pins the infra/conformance split: an
// executed body that fails is a conformance verdict; an undeliverable attempt
// is infrastructure and yields NO verdict — never a skip, never a failure of
// the code under test.
func TestRecordAttemptClassification(t *testing.T) {
	task := &Task{Name: "suite:shard=1"}
	tests := []struct {
		name       string
		res        TaskResult
		status     RunStatus
		code       string
		hasVerdict bool
	}{
		{"done", TaskResult{Status: StatusDone, Duration: time.Second}, RunPassed, "", true},
		{"up-to-date", TaskResult{Status: StatusUpToDate}, RunPassed, "", true},
		{"exit-nonzero", TaskResult{Status: StatusFailed, ExitCode: 2, Err: errf(1, "exit 2")},
			RunFailed, FailExitNonzero, true},
		{"timeout", TaskResult{Status: StatusFailed, ExitCode: 124, Err: errf(1, "timeout after 90s")},
			RunFailed, FailTimeout, true},
		{"postcondition", TaskResult{Status: StatusFailed, ExitCode: weavecli.ExitPrecondFail,
			Err:         errf(weavecli.ExitPrecondFail, "postcondition failed"),
			Attestation: &Attestation{Target: "suite:shard=1", Valid: false}},
			RunFailed, FailPostcondition, true},
		{"no-eligible-worker", TaskResult{Status: StatusFailed, ExitCode: 1,
			Err: errf(weavecli.ExitDepUnhealthy, "no worker offers venue=sandbox")},
			RunInfraFailed, FailNoWorker, false},
		{"canceled", TaskResult{Status: StatusFailed, ExitCode: 1, Err: context.Canceled},
			RunInfraFailed, FailCanceled, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := RecordAttempt(task, nil, 1, tc.res)
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if r.Status != tc.status {
				t.Fatalf("Status = %q, want %q", r.Status, tc.status)
			}
			if r.Status.HasVerdict() != tc.hasVerdict {
				t.Fatalf("HasVerdict = %v, want %v", r.Status.HasVerdict(), tc.hasVerdict)
			}
			if tc.code == "" && r.Failure != nil {
				t.Fatalf("unexpected failure reason %+v", r.Failure)
			}
			if tc.code != "" && (r.Failure == nil || r.Failure.Code != tc.code) {
				t.Fatalf("failure = %+v, want code %q", r.Failure, tc.code)
			}
			if r.Worker != LocalWorkerID {
				t.Fatalf("nil worker recorded as %q, want %q", r.Worker, LocalWorkerID)
			}
			// The hard invariant: infrastructure is never a conformance verdict.
			if r.Status == RunInfraFailed && (r.Status == RunFailed || r.Status.HasVerdict()) {
				t.Fatal("infra failure counted as a verdict")
			}
		})
	}
}

// TestRunRecordValidate pins the shapes an aggregator must refuse.
func TestRunRecordValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*RunRecord)
	}{
		{"no-task", func(r *RunRecord) { r.Task = "" }},
		{"attempt-zero", func(r *RunRecord) { r.Attempt = 0 }},
		{"no-worker", func(r *RunRecord) { r.Worker = "" }},
		{"unknown-status", func(r *RunRecord) { r.Status = "flaky" }},
		{"passed-with-failure", func(r *RunRecord) { r.Status = RunPassed }},
		{"failed-without-reason", func(r *RunRecord) { r.Failure = nil }},
		{"infra-without-code", func(r *RunRecord) { r.Status = RunInfraFailed; r.Failure = &FailureReason{} }},
		{"unknown-failure-code", func(r *RunRecord) { r.Failure.Code = "new-and-unknown" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := validRecord()
			tc.mut(r)
			if err := r.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", r)
			}
		})
	}
}

// forbiddenJSONKeys are field names that would carry reach or identity into a
// committed contract. No contract type may serialize any of them.
var forbiddenJSONKeys = map[string]bool{
	"host": true, "hostname": true, "fqdn": true,
	"ip": true, "address": true, "addr": true,
	"user": true, "username": true,
	"ssh": true, "ssh_user": true, "ssh_port": true,
	"password": true, "token": true, "secret": true, "credential": true,
}

// jsonKeys collects every json field name a struct type (recursively) can emit.
func jsonKeys(t *testing.T, typ reflect.Type, into map[string]bool) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || typ == reflect.TypeFor[time.Time]() {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		if name != "-" {
			into[name] = true
		}
		jsonKeys(t, f.Type, into)
	}
}

// TestContractsCarryNoReachDetails is the privacy gate, twice over:
//
//  1. Statically — no contract type declares a field whose serialized name is
//     reach-shaped (hostname, user, ip, ssh, credential, …).
//  2. Dynamically — a record produced from a Worker that DOES carry reach
//     details (Worker.Host) serializes without a trace of them, and facts or
//     specs that try to smuggle reach through labels are rejected.
func TestContractsCarryNoReachDetails(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[HostFacts](),
		reflect.TypeFor[TaskSpec](),
		reflect.TypeFor[RunRecord](),
	} {
		keys := map[string]bool{}
		jsonKeys(t, typ, keys)
		for k := range keys {
			if forbiddenJSONKeys[k] {
				t.Errorf("%s serializes forbidden field %q", typ.Name(), k)
			}
		}
	}

	// A remote-shaped worker: reach details present in memory at dial time.
	w := &Worker{
		ID:     "arm-builder", // logical
		Host:   "203.0.113.7", // reach — must never serialize
		Venues: []string{VenueUserland},
	}
	rec := RecordAttempt(&Task{Name: "suite:shard=2"}, w, 1,
		TaskResult{Status: StatusDone, Duration: time.Second})
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "203.0.113.7") {
		t.Fatalf("worker reach address leaked into run record: %s", data)
	}
	if rec.Worker != "arm-builder" {
		t.Fatalf("record worker = %q, want the logical id", rec.Worker)
	}

	f := validFacts()
	f.Labels["ssh_user"] = "root"
	if err := f.Validate(); err == nil {
		t.Fatal("host facts accepted an ssh_user label")
	}
	s := validSpec()
	s.Match["hostname"] = "builder-3.corp"
	if err := s.Validate(); err == nil {
		t.Fatal("task spec accepted a hostname match key")
	}
}
