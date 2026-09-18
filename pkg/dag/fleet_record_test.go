package dag

import (
	"bytes"
	"context"
	"testing"
)

func TestUnplaceableTaskRecordsInfrastructureFailure(t *testing.T) {
	pool := NewPool(localTransport{}, &Worker{
		ID:     "container-only",
		Venues: []string{VenueSandbox},
		CPU:    4,
	})
	task := &Task{Name: "chunk", Venue: VenueUserland}
	res := pool.Exec(context.Background(), Constraints{Venue: VenueUserland}, task,
		TaskIO{Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})

	record := RecordAttempt(task, nil, 1, res)
	if record.Status != RunInfraFailed {
		t.Fatalf("status = %q, want %q", record.Status, RunInfraFailed)
	}
	if record.Failure == nil || record.Failure.Code != FailNoWorker {
		t.Fatalf("failure = %+v, want code %q", record.Failure, FailNoWorker)
	}
	if record.Status.HasVerdict() {
		t.Fatal("an unplaceable task must not produce a conformance verdict")
	}
}

func TestEngineRecordsUnplaceableTask(t *testing.T) {
	e := engineFor(t, t.TempDir(), "## Tasks\n\n### chunk\nVenue: userland\n"+block("bash", "true"))
	e.Fleet = true
	e.Pool = NewPool(localTransport{}, &Worker{
		ID: "container-only", Venues: []string{VenueSandbox}, CPU: 1,
	})

	report, err := e.Run(context.Background(), "chunk")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("records = %+v, want exactly one", report.Records)
	}
	assertRecord(t, report.Records, 0, "chunk", 1, RunInfraFailed, FailNoWorker)
}

func TestEngineRecordsUnreachableSSHWorker(t *testing.T) {
	e := engineFor(t, t.TempDir(), "## Tasks\n\n### remote\n"+block("bash", "true"))
	e.Fleet = true
	e.Pool = NewPool(localTransport{}, &Worker{
		ID: "ssh-1", Venues: []string{VenueUserland}, CPU: 1, Transport: &SSHTransport{},
	})

	report, err := e.Run(context.Background(), "remote")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("records = %+v, want exactly one", report.Records)
	}
	assertRecord(t, report.Records, 0, "remote", 1, RunInfraFailed, FailUnreachable)
	if report.Records[0].Worker != "ssh-1" {
		t.Fatalf("worker = %q, want logical worker id ssh-1", report.Records[0].Worker)
	}
}

func TestEngineRecordsNonzeroBodyAsConformanceFailure(t *testing.T) {
	e := engineFor(t, t.TempDir(), "## Tasks\n\n### broken\n"+block("bash", "exit 7"))

	report, err := e.Run(context.Background(), "broken")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("records = %+v, want exactly one", report.Records)
	}
	assertRecord(t, report.Records, 0, "broken", 1, RunFailed, FailExitNonzero)
	if !report.Records[0].Status.HasVerdict() {
		t.Fatal("a body that ran and exited non-zero must retain its conformance verdict")
	}
}

func TestEngineRecordsEveryRetryAttempt(t *testing.T) {
	dir := t.TempDir()
	body := "if [ -f attempt.marker ]; then exit 0; fi\ntouch attempt.marker\nexit 1"
	e := engineFor(t, dir, "## Tasks\n\n### flaky\nRetries: 2\n"+block("bash", body))

	report, err := e.Run(context.Background(), "flaky")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Records) != 2 {
		t.Fatalf("records = %+v, want exactly two attempts", report.Records)
	}
	assertRecord(t, report.Records, 0, "flaky", 1, RunFailed, FailExitNonzero)
	assertRecord(t, report.Records, 1, "flaky", 2, RunPassed, "")
}

func TestEngineRecordsPostconditionFailureAfterAttestation(t *testing.T) {
	md := "## Tasks\n\n### lie\nEnsure: file-exists missing\n" + block("bash", "true")
	e := engineFor(t, t.TempDir(), md)

	report, err := e.Run(context.Background(), "lie")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Records) != 1 {
		t.Fatalf("records = %+v, want exactly one", report.Records)
	}
	assertRecord(t, report.Records, 0, "lie", 1, RunFailed, FailPostcondition)
}

func TestEngineDoesNotRecordSkipClassifiedAttempt(t *testing.T) {
	md := "## Tasks\n\n### maybe\nExitCodes: 75=skip\n" + block("bash", "exit 75")
	e := engineFor(t, t.TempDir(), md)

	report, err := e.Run(context.Background(), "maybe")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Records) != 0 {
		t.Fatalf("skip-classified attempt produced records: %+v", report.Records)
	}
}

func TestEngineRecordsFollowTopologyNotCompletionOrder(t *testing.T) {
	md := "## Tasks\n\n### first\n" + block("bash", "true") +
		"\n### second\n" + block("bash", "true")
	e := engineFor(t, t.TempDir(), md)
	e.Concurrency = 2
	secondDone := make(chan struct{})
	e.Executor = executorFunc(func(_ context.Context, task *Task, _ TaskIO) TaskResult {
		if task.Name == "first" {
			<-secondDone
		} else {
			close(secondDone)
		}
		return TaskResult{Name: task.Name, Status: StatusDone}
	})

	report, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Records) != 2 {
		t.Fatalf("records = %+v, want exactly two", report.Records)
	}
	assertRecord(t, report.Records, 0, "first", 1, RunPassed, "")
	assertRecord(t, report.Records, 1, "second", 1, RunPassed, "")
}

func assertRecord(t *testing.T, records []RunRecord, index int, task string, attempt int, status RunStatus, code string) {
	t.Helper()
	if len(records) <= index {
		t.Fatalf("records = %+v, want index %d", records, index)
	}
	record := records[index]
	if record.Task != task || record.Attempt != attempt || record.Status != status {
		t.Fatalf("record[%d] = %+v, want task=%q attempt=%d status=%q", index, record, task, attempt, status)
	}
	if code == "" {
		if record.Failure != nil {
			t.Fatalf("record[%d] failure = %+v, want nil", index, record.Failure)
		}
	} else if record.Failure == nil || record.Failure.Code != code {
		t.Fatalf("record[%d] failure = %+v, want code %q", index, record.Failure, code)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record[%d] Validate: %v", index, err)
	}
}
