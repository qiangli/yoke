package dag

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

type orderedFakeTransport struct {
	remoteFinished chan struct{}
	remote         bool
}

func (x orderedFakeTransport) Exec(ctx context.Context, _ *Worker, task *Task, _ TaskIO) TaskResult {
	if x.remote {
		close(x.remoteFinished)
	} else {
		select {
		case <-x.remoteFinished:
		case <-ctx.Done():
			return TaskResult{Name: task.Name, Status: StatusFailed, ExitCode: 1, Err: ctx.Err()}
		}
	}
	return TaskResult{Name: task.Name, Status: StatusDone, ExitCode: 0}
}

func (orderedFakeTransport) Close() error { return nil }

func TestTwoWorkerResultsStayTopologicalWhenRemoteFinishesFirst(t *testing.T) {
	finished := make(chan struct{})
	remoteRecorded := make(chan struct{})
	local := &Worker{ID: LocalWorkerID, Transport: orderedFakeTransport{remoteFinished: finished}}
	remote := &Worker{ID: "fake-remote", Transport: orderedFakeTransport{remoteFinished: finished, remote: true}}
	localTask := &Task{Name: "local-target"}
	remoteTask := &Task{Name: "remote-target"}
	log := newAttemptLog([]*Node{{Task: localTask}, {Task: remoteTask}})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res := remote.Transport.Exec(context.Background(), remote, remoteTask, TaskIO{})
		log.add(RecordAttempt(remoteTask, remote, 1, res))
		close(remoteRecorded)
	}()
	go func() {
		defer wg.Done()
		res := local.Transport.Exec(context.Background(), local, localTask, TaskIO{})
		<-remoteRecorded
		log.add(RecordAttempt(localTask, local, 1, res))
	}()
	wg.Wait()
	report := RunReport{Records: log.all()}

	gotOrder := make([]string, 0, len(report.Records))
	gotResults := make([]RunStatus, 0, len(report.Records))
	for _, record := range report.Records {
		gotOrder = append(gotOrder, record.Task)
		gotResults = append(gotResults, record.Status)
	}
	if want := []string{"local-target", "remote-target"}; !reflect.DeepEqual(gotOrder, want) {
		t.Fatalf("record order = %v, want topological order %v", gotOrder, want)
	}
	if want := []RunStatus{RunPassed, RunPassed}; !reflect.DeepEqual(gotResults, want) {
		t.Fatalf("per-target results = %v, want identical local/remote results %v", gotResults, want)
	}
	if report.Records[0].Worker != LocalWorkerID || report.Records[1].Worker != "fake-remote" {
		t.Fatalf("workers = %q, %q; want local then fake-remote", report.Records[0].Worker, report.Records[1].Worker)
	}
}
