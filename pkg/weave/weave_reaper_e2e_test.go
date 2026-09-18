package weave

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// THE GATE, end to end through the real verbs.
//
// Kill a wrapper OUT OF BAND — the way a crash, an OOM, or a `kill -9` from
// outside weave actually happens — and the very next `weave list` must report
// the run as stale without mutating it. `weave doctor` then records
// failed(wrapper-died). Before the reaper this run stayed "working"
// forever: observed live at 2h56m and 5h26m, with `weave wait --all` blocked on
// it and the scheduler still counting it as live capacity.
//
// The unit tests pin the rules; this pins that the rules are actually WIRED
// into the command a conductor types.
func TestGateListObservesAndDoctorReapsOutOfBandKilledWrapper(t *testing.T) {
	dir, root := newQueueInTempRepo(t)

	// A real wrapper process, which we then kill from outside weave.
	wrapper := newProbeWrapper(t)
	pid := wrapper.pid
	if err := saveWeaveQueue(dir, &weaveQueue{NextID: 2, Root: root, Items: []*weaveItem{{
		ID: 1, Title: "live run", State: "working", WrapperPid: pid,
		StartedAt: time.Now().Add(-time.Hour), Created: time.Now(),
	}}}); err != nil {
		t.Fatalf("save queue: %v", err)
	}

	// While the wrapper lives, the run is left strictly alone. A reaper that
	// raced live agents would be worse than the limbo it replaces.
	if _, err := weaveReapQueue(dir, root, weaveBaseBranch(root)); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if q, _ := loadWeaveQueue(dir); q.Items[0].State != "working" {
		t.Fatalf("live wrapper was reaped: state = %q, want working", q.Items[0].State)
	}

	wrapper.kill()
	for i := 0; i < 100 && pidAlive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("probe wrapper %d survived the kill; the test cannot observe a dead wrapper", pid)
	}

	var out bytes.Buffer
	list := newWeaveListCmd()
	list.SetOut(&out)
	list.SetErr(&out)
	list.SetArgs(nil)
	if err := list.Execute(); err != nil {
		t.Fatalf("weave list: %v\n%s", err, out.String())
	}

	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatalf("load queue: %v", err)
	}
	if q.Items[0].State != "working" {
		t.Fatalf("read-only `weave list` changed state = %q, want working", q.Items[0].State)
	}
	if !strings.Contains(out.String(), "wrapper process is dead") {
		t.Errorf("`weave list` did not surface stale state:\n%s", out.String())
	}
	doctor := newWeaveDoctorCmd()
	doctor.SetOut(&out)
	doctor.SetErr(&out)
	if err := doctor.Execute(); err != nil {
		t.Fatalf("weave doctor: %v\n%s", err, out.String())
	}
	q, err = loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if q.Items[0].State != "failed" || !strings.Contains(q.Items[0].Completion, "wrapper-died") {
		t.Fatalf("doctor did not persist wrapper death: %#v", q.Items[0])
	}
}

// `weave doctor` answers the invariant's question for every open item: what
// closes this? An item whose answer is "nothing" is a limbo, and doctor is
// where that would show up.
func TestGateDoctorGivesEveryOpenItemAClosurePath(t *testing.T) {
	dir, root := newQueueInTempRepo(t)
	now := time.Now()
	if err := saveWeaveQueue(dir, &weaveQueue{NextID: 5, Root: root, Items: []*weaveItem{
		{ID: 1, Title: "orphan alloc", State: "allocated", WrapperPid: deadPID(t),
			LaunchPhase: "provisioning workspace", StartedAt: now.Add(-time.Hour), Created: now},
		{ID: 2, Title: "unpulled", State: "submitted", CommitsAhead: 3,
			FinishedAt: now.Add(-4 * time.Hour), Created: now},
		{ID: 3, Title: "queued", State: "todo", Created: now},
	}}); err != nil {
		t.Fatalf("save queue: %v", err)
	}

	var out bytes.Buffer
	doc := newWeaveDoctorCmd()
	doc.SetOut(&out)
	doc.SetErr(&out)
	doc.SetArgs(nil)
	if err := doc.Execute(); err != nil {
		t.Fatalf("weave doctor: %v\n%s", err, out.String())
	}
	got := out.String()

	// The orphan allocation was swept.
	q, _ := loadWeaveQueue(dir)
	if st := findWeaveItem(q, 1).State; st != "failed" {
		t.Errorf("orphan allocation state = %q, want failed", st)
	}
	// The stale submission is a NAMED pending decision, not a silent hang.
	sub := findWeaveItem(q, 2)
	if !sub.NeedsSteward {
		t.Errorf("a submission unmerged for 4h must be flagged needs-steward")
	}
	if !strings.Contains(got, "needs-steward") {
		t.Errorf("doctor output must surface the steward decision:\n%s", got)
	}
	// And every open item got a next step.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "NO DECLARED TRANSITION") {
			t.Errorf("doctor found a limbo: %s", line)
		}
	}
	if !strings.Contains(got, "open (") {
		t.Errorf("doctor must audit what is still open:\n%s", got)
	}
}

// weaveProbeWrapperEnv makes this test binary re-exec itself as a long-lived
// process. `sleep` is not a thing on Windows, and the windows leg is the
// product — the self-exec helper is the portable way to get a real, killable
// PID.
const weaveProbeWrapperEnv = "WEAVE_TEST_PROBE_WRAPPER"

// The helper's lifetime is deliberately short. A leaked probe outlives the test
// run and adds load to this package's timing-sensitive PTY tests — observed
// once, as two unrelated flakes — so it self-terminates even if every kill path
// fails.
const weaveProbeWrapperLifetime = 30 * time.Second

func TestWeaveProbeWrapperHelper(t *testing.T) {
	if os.Getenv(weaveProbeWrapperEnv) != "1" {
		t.Skip("helper process for TestGateListReapsOutOfBandKilledWrapper; not a test")
	}
	time.Sleep(weaveProbeWrapperLifetime)
}

type probeWrapper struct {
	cmd    *exec.Cmd
	pid    int
	killed bool
}

// kill terminates the probe and reaps it. Idempotent, so the test body and the
// cleanup can both call it.
func (p *probeWrapper) kill() {
	if p.killed || p.cmd.Process == nil {
		return
	}
	p.killed = true
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
}

func newProbeWrapper(t *testing.T) *probeWrapper {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestWeaveProbeWrapperHelper$",
		"-test.timeout="+(2*weaveProbeWrapperLifetime).String())
	c.Env = append(os.Environ(), weaveProbeWrapperEnv+"=1")
	if err := c.Start(); err != nil {
		t.Skipf("cannot spawn a probe wrapper: %v", err)
	}
	p := &probeWrapper{cmd: c, pid: c.Process.Pid}
	t.Cleanup(p.kill)
	return p
}

// newQueueInTempRepo puts the test in a throwaway git repo with a private HOME,
// so the queue it reaps is its own and never the developer's.
func newQueueInTempRepo(t *testing.T) (queueDir, root string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-qm", "base", "--allow-empty"}} {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	root, err = weaveRepoRoot(repo)
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	queueDir, err = weaveQueueDir(root)
	if err != nil {
		t.Fatalf("queue dir: %v", err)
	}
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		t.Fatalf("mkdir queue: %v", err)
	}
	return queueDir, root
}
