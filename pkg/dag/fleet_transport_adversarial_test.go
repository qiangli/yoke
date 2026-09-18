// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// adversarialTransport is deliberately programmable. Tests control exactly
// when delivery starts and ends, so none of the scheduling assertions depend
// on a real host, a network, or a sleep.
type adversarialTransport struct {
	exec func(context.Context, *Worker, *Task, TaskIO) TaskResult
}

func (x *adversarialTransport) Exec(ctx context.Context, w *Worker, task *Task, tio TaskIO) TaskResult {
	return x.exec(ctx, w, task, tio)
}

func (*adversarialTransport) Close() error { return nil }

func waitResult(t *testing.T, results <-chan TaskResult) TaskResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not finish; pool is stuck")
		return TaskResult{}
	}
}

func assertInfraAttempt(t *testing.T, task *Task, worker *Worker, result TaskResult, code string) {
	t.Helper()
	if result.Status == StatusSkipped || result.Status == StatusConditionSkipped {
		t.Fatalf("infrastructure failure was reported as test skip %q", result.Status)
	}
	record := RecordAttempt(task, worker, 1, result)
	if record.Status != RunInfraFailed || record.Status.HasVerdict() {
		t.Fatalf("attempt = %q (verdict=%v), want infrastructure with no verdict", record.Status, record.Status.HasVerdict())
	}
	if record.Status == RunFailed {
		t.Fatal("infrastructure failure was reported as a conformance failure")
	}
	if record.Failure == nil || record.Failure.Code != code {
		t.Fatalf("failure = %+v, want structured code %q", record.Failure, code)
	}
}

func TestFakeTransportDisconnectMidTaskIsInfrastructure(t *testing.T) {
	started := make(chan struct{})
	disconnect := make(chan struct{})
	transport := &adversarialTransport{exec: func(_ context.Context, _ *Worker, task *Task, _ TaskIO) TaskResult {
		close(started)
		<-disconnect
		return TaskResult{
			Name: task.Name, Status: StatusFailed, ExitCode: 255,
			Err: fmt.Errorf("connection dropped after delivery began: %w", ErrWorkerUnreachable),
		}
	}}
	worker := &Worker{ID: "fake-remote", Venues: []string{VenueUserland}, CPU: 1, Transport: transport}
	e := engineFor(t, t.TempDir(), "## Tasks\n\n### disconnect-mid-task\n"+block("bash", "must-not-run-locally"))
	e.Capture, e.Fleet, e.Concurrency = true, true, 2
	e.Pool = NewPool(nil, worker)
	type runResult struct {
		report RunReport
		err    error
	}
	finished := make(chan runResult, 1)
	go func() {
		report, err := e.Run(context.Background(), "disconnect-mid-task")
		finished <- runResult{report: report, err: err}
	}()

	<-started
	close(disconnect)
	var run runResult
	select {
	case run = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not finish after fake transport disconnected")
	}
	if run.err != nil {
		t.Fatalf("Run returned graph error: %v", run.err)
	}
	if !run.report.Failed || len(run.report.Results) != 1 || len(run.report.Records) != 1 {
		t.Fatalf("report = %+v, want one failed result and one attempt", run.report)
	}
	result := run.report.Results[0]
	if result.Status == StatusSkipped || result.Status == StatusConditionSkipped {
		t.Fatalf("unreachable worker was reported as test skip %q", result.Status)
	}
	record := run.report.Records[0]
	if record.Status != RunInfraFailed || record.Status.HasVerdict() || record.Failure == nil || record.Failure.Code != FailUnreachable {
		t.Fatalf("record = %+v, want worker-unreachable infrastructure without verdict", record)
	}
	if record.Status == RunFailed {
		t.Fatal("unreachable worker was reported as a conformance failure")
	}
}

func TestFakeTransportNonzeroExitIsConformance(t *testing.T) {
	transport := &adversarialTransport{exec: func(_ context.Context, _ *Worker, task *Task, _ TaskIO) TaskResult {
		return TaskResult{Name: task.Name, Status: StatusFailed, ExitCode: 17, Err: errors.New("exit 17")}
	}}
	worker := &Worker{ID: "fake-remote", Venues: []string{VenueUserland}, CPU: 1, Transport: transport}
	task := &Task{Name: "real-test-failure"}
	result := NewPool(nil, worker).Exec(context.Background(), Constraints{}, task, TaskIO{})
	record := RecordAttempt(task, worker, 1, result)
	if record.Status != RunFailed || !record.Status.HasVerdict() {
		t.Fatalf("non-zero body exit = %q (verdict=%v), want conformance failure", record.Status, record.Status.HasVerdict())
	}
	if record.Failure == nil || record.Failure.Code != FailExitNonzero || record.ExitCode != 17 {
		t.Fatalf("record = %+v, want exit-nonzero/17", record)
	}
}

func TestFakeTransportCancellationMidFlightIsInfrastructure(t *testing.T) {
	started := make(chan struct{})
	transport := &adversarialTransport{exec: func(ctx context.Context, _ *Worker, task *Task, _ TaskIO) TaskResult {
		close(started)
		<-ctx.Done()
		return TaskResult{Name: task.Name, Status: StatusFailed, ExitCode: 1, Err: ctx.Err()}
	}}
	worker := &Worker{ID: "fake-remote", Venues: []string{VenueUserland}, CPU: 1, Transport: transport}
	task := &Task{Name: "cancel-mid-flight"}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan TaskResult, 1)
	go func() { results <- NewPool(nil, worker).Exec(ctx, Constraints{}, task, TaskIO{}) }()

	<-started
	cancel()
	assertInfraAttempt(t, task, worker, waitResult(t, results), FailCanceled)
}

func TestFakeTransportCapacitySaturationNeverExceedsSlots(t *testing.T) {
	const (
		slots = 2
		tasks = 11
	)
	entered := make(chan struct{}, tasks)
	release := make(chan struct{})
	var mu sync.Mutex
	active, peak := 0, 0
	transport := &adversarialTransport{exec: func(_ context.Context, _ *Worker, task *Task, _ TaskIO) TaskResult {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return TaskResult{Name: task.Name, Status: StatusDone}
	}}
	pool := NewPool(nil, &Worker{ID: "fake-remote", Venues: []string{VenueUserland}, CPU: slots, Transport: transport})

	results := make(chan TaskResult, tasks)
	for i := range tasks {
		go func() {
			results <- pool.Exec(context.Background(), Constraints{}, &Task{Name: fmt.Sprintf("task-%02d", i)}, TaskIO{})
		}()
	}
	// Hold the first wave in the transport. If accounting is correct, it fills
	// both slots and every excess task waits in Acquire.
	for range slots {
		<-entered
	}
	close(release)
	for range tasks {
		if result := waitResult(t, results); result.Status != StatusDone {
			t.Fatalf("saturated task result = %+v", result)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak != slots {
		t.Fatalf("peak fake-remote executions = %d, want exactly %d", peak, slots)
	}
	if active != 0 {
		t.Fatalf("active executions after completion = %d, want 0", active)
	}
}

func TestMemoryDerivedSlotsAreEnforcedDuringSaturation(t *testing.T) {
	const memPerTask = 4 << 30
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	transport := &adversarialTransport{exec: func(_ context.Context, _ *Worker, task *Task, _ TaskIO) TaskResult {
		started <- struct{}{}
		<-release
		return TaskResult{Name: task.Name, Status: StatusDone}
	}}
	worker := &Worker{
		ID: "memory-bound", Venues: []string{VenueUserland},
		CPU: 4, MemBytes: 8 << 30, Transport: transport,
	}
	pool := NewPool(nil, worker)
	constraints := Constraints{MemPerTask: memPerTask}
	if slots := pool.Slots(worker, memPerTask); slots != 2 {
		t.Fatalf("derived slots = %d, want 2", slots)
	}

	results := make(chan TaskResult, 2)
	for i := range 2 {
		go func() {
			results <- pool.Exec(context.Background(), constraints, &Task{Name: fmt.Sprintf("memory-task-%d", i)}, TaskIO{})
		}()
	}
	<-started
	<-started

	third, releaseThird := pool.TryAcquire(constraints)
	if third != nil {
		// Clean up every reservation and blocked transport before failing so the
		// race detector observes a quiescent test, even on the defective path.
		releaseThird()
		close(release)
		waitResult(t, results)
		waitResult(t, results)
		t.Fatal("pool admitted a third 4GiB task into 8GiB; derived two-slot limit is not enforced")
	}
	close(release)
	waitResult(t, results)
	waitResult(t, results)
}

func TestFakeTransportExclusiveTaskActuallyRunsAlone(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	transport := &adversarialTransport{exec: func(_ context.Context, _ *Worker, task *Task, _ TaskIO) TaskResult {
		started <- task.Name
		<-release
		return TaskResult{Name: task.Name, Status: StatusDone}
	}}
	pool := NewPool(nil, &Worker{ID: "wide-remote", Venues: []string{VenueUserland}, CPU: 4, Transport: transport})

	exclusiveDone := make(chan TaskResult, 1)
	go func() {
		exclusiveDone <- pool.Exec(context.Background(), Constraints{Exclusive: true}, &Task{Name: "exclusive"}, TaskIO{})
	}()
	if name := <-started; name != "exclusive" {
		t.Fatalf("first task = %q, want exclusive", name)
	}
	if worker, _ := pool.TryAcquire(Constraints{}); worker != nil {
		t.Fatal("ordinary work co-scheduled while exclusive transport call was in flight")
	}
	release <- struct{}{}
	if result := waitResult(t, exclusiveDone); result.Status != StatusDone {
		t.Fatalf("exclusive result = %+v", result)
	}

	sharedDone := make(chan TaskResult, 1)
	go func() {
		sharedDone <- pool.Exec(context.Background(), Constraints{}, &Task{Name: "shared"}, TaskIO{})
	}()
	if name := <-started; name != "shared" {
		t.Fatalf("second task = %q, want shared", name)
	}
	if worker, _ := pool.TryAcquire(Constraints{Exclusive: true}); worker != nil {
		t.Fatal("exclusive work co-scheduled while ordinary transport call was in flight")
	}
	release <- struct{}{}
	if result := waitResult(t, sharedDone); result.Status != StatusDone {
		t.Fatalf("shared result = %+v", result)
	}
}

func TestIneligibleWorkerIsInfraNeverSkipOrConformance(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		worker *Worker
		match  string
		code   string
		miss   string
	}{
		{
			name: "stale-facts",
			worker: &Worker{ID: "stale", Venues: []string{VenueUserland}, CPU: 1, MaxFactsAge: time.Minute,
				Facts: &HostFacts{SchemaVersion: HostFactsSchemaVersion, Worker: "stale", OS: "linux", Arch: "arm64", Venues: []string{VenueUserland}, ObservedAt: now.Add(-time.Hour)}},
			match: "os=linux", code: "unknown-capability", miss: "observed_at",
		},
		{
			name: "unknown-capability",
			worker: &Worker{ID: "unknown", Venues: []string{VenueUserland}, CPU: 1,
				Facts: &HostFacts{SchemaVersion: HostFactsSchemaVersion, Worker: "unknown", Venues: []string{VenueUserland}, ObservedAt: now}},
			match: "libc=musl", code: "unknown-capability", miss: "libc",
		},
		{
			name: "unsatisfied-requirement",
			worker: &Worker{ID: "wrong-os", Venues: []string{VenueUserland}, CPU: 1,
				Facts: &HostFacts{SchemaVersion: HostFactsSchemaVersion, Worker: "wrong-os", OS: "darwin", Arch: "arm64", Venues: []string{VenueUserland}, ObservedAt: now}},
			match: "os=linux", code: "capability-mismatch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			md := "## Tasks\n\n### target\nMatch: " + tc.match + "\n" + block("bash", "echo must-not-run")
			e := engineFor(t, t.TempDir(), md)
			e.Capture, e.Fleet, e.Concurrency = true, true, 2
			e.Pool = NewPool(nil, tc.worker)
			report, err := e.Run(context.Background(), "target")
			if err != nil {
				t.Fatalf("Run returned graph error: %v", err)
			}
			if !report.Failed || len(report.Results) != 1 || len(report.Records) != 1 {
				t.Fatalf("report = %+v, want one failed result and one attempt record", report)
			}
			result := report.Results[0]
			if result.Status == StatusSkipped || result.Status == StatusConditionSkipped {
				t.Fatalf("ineligible target reported as skip %q", result.Status)
			}
			var placement *PlacementError
			if !errors.As(result.Err, &placement) || len(placement.Refusals) != 1 {
				t.Fatalf("result error = %#v, want one structured PlacementError refusal", result.Err)
			}
			refusal := placement.Refusals[0]
			if refusal.Code != tc.code || refusal.Missing != tc.miss {
				t.Fatalf("refusal = %+v, want code=%q missing=%q", refusal, tc.code, tc.miss)
			}
			record := report.Records[0]
			if record.Status != RunInfraFailed || record.Status.HasVerdict() || record.Failure == nil || record.Failure.Code != FailNoWorker {
				t.Fatalf("record = %+v, want no-eligible-worker infrastructure without verdict", record)
			}
			if record.Status == RunFailed {
				t.Fatal("ineligible target was counted as a conformance failure")
			}
		})
	}
}

func TestFreshObservedFactsOverrideStaticVenueAndCPU(t *testing.T) {
	now := time.Now().UTC()
	worker := &Worker{
		ID: "observed", Venues: []string{VenueUserland}, CPU: 8,
		Facts: &HostFacts{
			SchemaVersion: HostFactsSchemaVersion, Worker: "observed",
			OS: "linux", Arch: "arm64", CPU: 2, Venues: []string{VenueSandbox}, ObservedAt: now,
		},
	}
	pool := NewPool(nil, worker)

	if pool.Eligible(Constraints{Venue: VenueUserland}) {
		t.Error("static userland venue overrode fresh facts that offer only sandbox")
	} else {
		refusals := pool.Refusals(Constraints{Venue: VenueUserland})
		if len(refusals) != 1 || refusals[0].Code != "missing-capability" || refusals[0].Missing != "venue" {
			t.Errorf("venue refusals = %+v, want structured missing venue", refusals)
		}
	}
	if !pool.Eligible(Constraints{Venue: VenueSandbox}) {
		t.Error("fresh observed sandbox venue was ignored")
	}
	if slots := pool.Slots(worker, 0); slots != 2 {
		t.Errorf("slots = %d, want fresh observed CPU limit 2", slots)
	}
}

// synchronizedExecutor forces the first two independent targets to overlap.
// The fake remote deliberately finishes before local, exercising deterministic
// aggregation rather than accidentally observing topological completion order.
type synchronizedExecutor struct {
	mu             sync.Mutex
	arrived        int
	bothArrived    chan struct{}
	remoteFinished chan struct{}
	localTasks     []string
}

func newSynchronizedExecutor(remoteFinished chan struct{}) *synchronizedExecutor {
	return &synchronizedExecutor{bothArrived: make(chan struct{}), remoteFinished: remoteFinished}
}

func (x *synchronizedExecutor) Execute(_ context.Context, task *Task, tio TaskIO) TaskResult {
	x.mu.Lock()
	x.arrived++
	arrival := x.arrived
	if arrival == 2 {
		close(x.bothArrived)
	}
	x.mu.Unlock()
	if arrival <= 2 {
		<-x.bothArrived
	}
	isLocal := false
	for _, env := range tio.Env {
		if env == "DAG_FLEET_WORKER="+LocalWorkerID {
			isLocal = true
			break
		}
	}
	if isLocal && x.remoteFinished != nil {
		<-x.remoteFinished
	}
	if isLocal {
		x.mu.Lock()
		x.localTasks = append(x.localTasks, task.Name)
		x.mu.Unlock()
	}
	fmt.Fprintf(tio.Stdout, "verdict:%s", task.Name)
	return TaskResult{Name: task.Name, Status: StatusDone, ExitCode: 0, Duration: 37 * time.Nanosecond}
}

type fakeRemoteExecutorTransport struct {
	exec     Executor
	finished chan struct{}
	once     sync.Once
	mu       sync.Mutex
	tasks    []string
}

func (x *fakeRemoteExecutorTransport) Exec(ctx context.Context, _ *Worker, task *Task, tio TaskIO) TaskResult {
	x.mu.Lock()
	x.tasks = append(x.tasks, task.Name)
	x.mu.Unlock()
	result := x.exec.Execute(ctx, task, tio)
	x.once.Do(func() { close(x.finished) })
	return result
}

func (*fakeRemoteExecutorTransport) Close() error { return nil }

type targetVerdict struct {
	Name        string
	Host        string
	Status      Status
	ExitCode    int
	Duration    time.Duration
	Error       string
	Stdout      string
	Stderr      string
	UpToDate    bool
	Artifacts   []string
	Attestation *Attestation
}

func targetVerdicts(report RunReport) []targetVerdict {
	verdicts := make([]targetVerdict, 0, len(report.Results))
	for _, result := range report.Results {
		errorText := ""
		if result.Err != nil {
			errorText = result.Err.Error()
		}
		verdicts = append(verdicts, targetVerdict{
			Name: result.Name, Host: result.Host, Status: result.Status, ExitCode: result.ExitCode,
			Duration: result.Duration, Error: errorText,
			Stdout: result.Stdout, Stderr: result.Stderr, UpToDate: result.UpToDate,
			Artifacts: result.Artifacts, Attestation: result.Attestation,
		})
	}
	return verdicts
}

func runTwoLeafFleet(t *testing.T, pool func(Executor) *Pool, exec Executor) RunReport {
	t.Helper()
	md := "## Tasks\n\n### aggregate\nRequires: local-candidate, remote-candidate\n" +
		"### local-candidate\n" + block("bash", "ignored") +
		"### remote-candidate\n" + block("bash", "ignored")
	e := engineFor(t, t.TempDir(), md)
	e.Capture, e.Fleet, e.Concurrency, e.Executor = true, true, 8, exec
	e.Pool = pool(exec)
	report, err := e.Run(context.Background(), "aggregate")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Fatalf("run failed: %+v", report.Results)
	}
	return report
}

func TestTwoWorkerLocalAndFakeRemoteHaveIdenticalDeterministicTargetResults(t *testing.T) {
	baselineExec := newSynchronizedExecutor(nil)
	baseline := runTwoLeafFleet(t, func(Executor) *Pool { return LocalPool(2) }, baselineExec)
	want := targetVerdicts(baseline)
	wantOrder := []string{"local-candidate", "remote-candidate", "aggregate"}

	remoteFinished := make(chan struct{})
	heterogeneousExec := newSynchronizedExecutor(remoteFinished)
	remote := &fakeRemoteExecutorTransport{exec: heterogeneousExec, finished: remoteFinished}
	heterogeneous := runTwoLeafFleet(t, func(Executor) *Pool {
		return NewPool(nil,
			&Worker{ID: LocalWorkerID, Venues: []string{VenueUserland}, CPU: 1},
			&Worker{ID: "fake-remote", Venues: []string{VenueUserland}, CPU: 1, Transport: remote},
		)
	}, heterogeneousExec)

	got := targetVerdicts(heterogeneous)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("heterogeneous per-target result set differs from all-local:\n got %#v\nwant %#v", got, want)
	}
	resultOrder := make([]string, 0, len(heterogeneous.Results))
	for _, result := range heterogeneous.Results {
		resultOrder = append(resultOrder, result.Name)
	}
	recordOrder := make([]string, 0, len(heterogeneous.Records))
	for _, record := range heterogeneous.Records {
		recordOrder = append(recordOrder, record.Task)
		if !record.Status.HasVerdict() || record.Status != RunPassed {
			t.Fatalf("record for %s = %+v, want passing verdict", record.Task, record)
		}
	}
	if !reflect.DeepEqual(resultOrder, wantOrder) || !reflect.DeepEqual(recordOrder, wantOrder) {
		t.Fatalf("aggregation is not topological: results=%v records=%v want=%v", resultOrder, recordOrder, wantOrder)
	}
	remote.mu.Lock()
	remoteTasks := append([]string(nil), remote.tasks...)
	remote.mu.Unlock()
	// At least one result must come through the true localTransport path too;
	// synchronizedExecutor identifies that path by its injected logical env.
	heterogeneousExec.mu.Lock()
	localTasks := append([]string(nil), heterogeneousExec.localTasks...)
	heterogeneousExec.mu.Unlock()
	isLeaf := func(tasks []string) bool {
		for _, task := range tasks {
			if task == "local-candidate" || task == "remote-candidate" {
				return true
			}
		}
		return false
	}
	if !isLeaf(remoteTasks) {
		t.Fatalf("headline gate sent no leaf target to fake remote; remote tasks = %v", remoteTasks)
	}
	if !isLeaf(localTasks) {
		t.Fatalf("headline gate sent no leaf target through local transport; local tasks = %v", localTasks)
	}
}
