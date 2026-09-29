package toolcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// fakeJobs wires the job lifecycle to run in-process: spawnJob starts
// RunJob in a goroutine (the "detached runner"), signalJob cancels it, and
// runCommand is a fake that never touches a real tool or bashy's binary.
type fakeJobs struct {
	mu      sync.Mutex
	cancels map[int]context.CancelFunc
	done    map[int]chan struct{}
	nextPID int
	release chan struct{} // closed to let a blocking fake run finish
}

func setupFakeJobs(t *testing.T, run func(ctx context.Context, release <-chan struct{}) (Result, error)) *fakeJobs {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SELF", filepath.Join(t.TempDir(), "no-such-bashy"))
	f := &fakeJobs{cancels: map[int]context.CancelFunc{}, done: map[int]chan struct{}{}, nextPID: 900000, release: make(chan struct{})}
	prev := struct {
		spawn  func(string, string) (int, error)
		lookup func(string) (fleet.Tool, fleet.ToolCommand, error)
		run    func(context.Context, fleet.Tool, fleet.ToolCommand, string, Options) (Result, error)
		sig    func(int) error
		alive  func(int) bool
		self   func() ([]string, error)
	}{spawnJob, lookupCommand, runCommand, signalJob, pidAlive, jobSelfArgv}
	t.Cleanup(func() {
		spawnJob, lookupCommand, runCommand, signalJob, pidAlive, jobSelfArgv = prev.spawn, prev.lookup, prev.run, prev.sig, prev.alive, prev.self
		f.mu.Lock()
		for _, c := range f.cancels {
			c()
		}
		chans := f.done
		f.mu.Unlock()
		for _, d := range chans {
			<-d
		}
	})
	jobSelfArgv = func() ([]string, error) { return nil, errors.New("tests never exec the self binary") }
	lookupCommand = func(ref string) (fleet.Tool, fleet.ToolCommand, error) {
		tool, name, _ := strings.Cut(ref, ":")
		if tool != "fake" {
			return fleet.Tool{}, fleet.ToolCommand{}, errors.New("unknown tool")
		}
		return fleet.Tool{Name: "fake"}, fleet.ToolCommand{Name: name, Slash: "/" + name, Mode: "print"}, nil
	}
	runCommand = func(ctx context.Context, tool fleet.Tool, cmd fleet.ToolCommand, args string, opts Options) (Result, error) {
		return run(ctx, f.release)
	}
	spawnJob = func(id, dir string) (int, error) {
		f.mu.Lock()
		f.nextPID++
		pid := f.nextPID
		ctx, cancel := context.WithCancel(context.Background())
		d := make(chan struct{})
		f.cancels[pid], f.done[pid] = cancel, d
		f.mu.Unlock()
		go func() {
			defer close(d)
			_ = RunJob(ctx, id)
		}()
		return pid, nil
	}
	signalJob = func(pid int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		// RunJob records os.Getpid() (this test process) as the runner pid;
		// cancel every fake runner, which is what SIGTERM to it would do.
		for _, c := range f.cancels {
			c()
		}
		return nil
	}
	pidAlive = func(pid int) bool { return true }
	return f
}

func TestJobLifecycleDone(t *testing.T) {
	f := setupFakeJobs(t, func(ctx context.Context, release <-chan struct{}) (Result, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return Result{Outcome: OutcomeCancelled}, ctx.Err()
		}
		return Result{Tool: "fake", Command: "review", Outcome: OutcomeSuccess, Text: "LGTM"}, nil
	})
	j, err := StartJob("fake:review", "look", Options{Argv: []string{"look"}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := jobDir(j.ID)
	if root, _ := JobsDir(); !strings.HasPrefix(dir, filepath.Join(os.Getenv("BASHY_HOME"), "toolcmd", "jobs")) || !strings.HasPrefix(dir, root) {
		t.Fatalf("job dir %s not under $BASHY_HOME/toolcmd/jobs", dir)
	}
	// running
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		cur, err := ReadJob(j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status == JobRunning {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("job never ran: %+v", cur)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := ReadResult(j.ID); err == nil {
		t.Fatal("result available before the job finished")
	}
	close(f.release)
	done, err := WaitJob(ctx, j.ID, 10*time.Millisecond)
	if err != nil || done.Status != JobDone || done.Outcome != OutcomeSuccess {
		t.Fatalf("wait: %+v %v", done, err)
	}
	r, err := ReadResult(j.ID)
	if err != nil || r.Text != "LGTM" {
		t.Fatalf("result %+v %v", r, err)
	}
	// cancel after done is a no-op on a terminal job
	if c, err := CancelJob(context.Background(), j.ID, time.Second); err != nil || c.Status != JobDone {
		t.Fatalf("cancel terminal: %+v %v", c, err)
	}
	js, _ := ListJobs()
	if len(js) != 1 || js[0].ID != j.ID {
		t.Fatalf("jobs: %+v", js)
	}
}

func TestJobLifecycleCancelled(t *testing.T) {
	setupFakeJobs(t, func(ctx context.Context, release <-chan struct{}) (Result, error) {
		<-ctx.Done()
		return Result{Outcome: OutcomeCancelled}, ctx.Err()
	})
	j, err := StartJob("fake:review", "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		cur, _ := ReadJob(j.ID)
		if cur.Status == JobRunning {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("job never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, err := CancelJob(ctx, j.ID, 3*time.Second)
	if err != nil || c.Status != JobCancelled {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	r, err := ReadResult(j.ID)
	if err != nil || r.Outcome != OutcomeCancelled {
		t.Fatalf("result %+v %v", r, err)
	}
}

func TestJobFailedAndLost(t *testing.T) {
	setupFakeJobs(t, func(ctx context.Context, release <-chan struct{}) (Result, error) {
		return Result{Outcome: OutcomeUnavailable, Error: "unavailable"}, ErrUnavailable
	})
	j, err := StartJob("fake:plan", "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done, err := WaitJob(ctx, j.ID, 10*time.Millisecond)
	if err != nil || done.Status != JobFailed || done.Outcome != OutcomeUnavailable {
		t.Fatalf("wait: %+v %v", done, err)
	}
	// unknown tool refused before anything is spawned
	if _, err := StartJob("nope:plan", "", Options{}); err == nil {
		t.Fatal("unknown tool accepted")
	}
	// a running record whose process is gone reads as lost
	lost := Job{ID: "20260101T000000-deadbeef", Tool: "fake", Command: "x", Status: JobRunning, PID: 424242, Created: time.Now()}
	d, _ := jobDir(lost.ID)
	_ = os.MkdirAll(d, 0o700)
	if err := saveJob(lost); err != nil {
		t.Fatal(err)
	}
	pidAlive = func(int) bool { return false }
	if got, _ := ReadJob(lost.ID); got.Status != JobLost {
		t.Fatalf("status %q, want lost", got.Status)
	}
	if _, err := jobDir("../escape"); err == nil {
		t.Fatal("path-escaping job id accepted")
	}
}

func TestCmdAsyncWaitResult(t *testing.T) {
	f := setupFakeJobs(t, func(ctx context.Context, release <-chan struct{}) (Result, error) {
		<-release
		return Result{Outcome: OutcomeSuccess, Text: "report ready"}, nil
	})
	close(f.release)
	exec := func(args ...string) (string, error) {
		c := NewCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs(args)
		err := c.Execute()
		return out.String(), err
	}
	id, err := exec("run", "fake:review", "--async", "--", "look")
	if err != nil {
		t.Fatal(err)
	}
	id = strings.TrimSpace(id)
	out, err := exec("wait", id, "--timeout", "5s", "--json")
	if err != nil {
		t.Fatalf("wait: %v %s", err, out)
	}
	var j Job
	if json.Unmarshal([]byte(out), &j) != nil || j.Status != JobDone {
		t.Fatalf("wait json: %s", out)
	}
	if out, err := exec("result", id); err != nil || strings.TrimSpace(out) != "report ready" {
		t.Fatalf("result: %q %v", out, err)
	}
	// NewCmd set the production self argv; restore the refusing stub.
	jobSelfArgv = func() ([]string, error) { return nil, errors.New("tests never exec the self binary") }
}

func TestCmdDryRunPrintsArgv(t *testing.T) {
	stubInvoke(t, "", 0, nil)
	prev := lookupCommand
	t.Cleanup(func() { lookupCommand = prev })
	lookupCommand = func(string) (fleet.Tool, fleet.ToolCommand, error) {
		return codexTool(), fleet.ToolCommand{Name: "review", Slash: "/review {args}", Mode: "print", Exec: "codex exec review --json {args}"}, nil
	}
	c := NewCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&bytes.Buffer{})
	c.SetArgs([]string{"run", "codex:review", "--dry-run", "--", "--uncommitted", "focus on auth"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(out.String()), "fakebin exec review --json --uncommitted 'focus on auth'"; got != want {
		t.Fatalf("dry-run %q want %q", got, want)
	}
}

func TestCommandRowsAvailability(t *testing.T) {
	prev := lookPath
	t.Cleanup(func() { lookPath = prev })
	lookPath = func(name string) (string, error) {
		if name == "claude" {
			return "/bin/claude", nil
		}
		return "", errors.New("not found")
	}
	cl := claudeTool()
	cl.Commands = []fleet.ToolCommand{{Name: "deep-research", Slash: "/deep-research {args}", Mode: "print"}, {Name: "plan", Slash: "/plan", Mode: "tui"}}
	cx := codexTool()
	cx.Commands = []fleet.ToolCommand{{Name: "review", Slash: "/review", Mode: "print", Exec: "codex exec review {args}"}}
	rows := commandRows([]fleet.Tool{cl, cx})
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	if !rows[0].Available || rows[1].Available || rows[1].Invalid == "" || rows[2].Available || rows[2].Why != "codex not on PATH" {
		t.Fatalf("availability %+v", rows)
	}
}
