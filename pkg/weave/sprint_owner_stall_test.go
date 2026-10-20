package weave

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/room"
)

// stalledControlFixture is an ISOLATED stand-in for a hung managed manager:
// its control socket accepts connections and never acknowledges, exactly the
// Sprint 457 shape ("read unix …/ctl.sock: i/o timeout"). It lives in its own
// temp dir and never touches ~/.bashy/foreman, so no real session is disturbed.
func stalledControlFixture(t *testing.T) (sock string, stop func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed control sessions are unix-socket only")
	}
	dir, err := os.MkdirTemp("", "stall")
	if err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(dir, "ctl.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // held open, never answered
			mu.Unlock()
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = ln.Close()
			mu.Lock()
			for _, c := range conns {
				_ = c.Close()
			}
			mu.Unlock()
			_ = os.RemoveAll(dir)
		})
	}
	t.Cleanup(stop)
	return sock, stop
}

// stalledOwnerHost models bashy's seam over the fixture: while the fixture is
// live, delivery goes through the real control client and times out; once the
// session has been stopped, a relaunch delivers.
type stalledOwnerHost struct {
	sock     string
	stopFix  func()
	stopErr  error
	mu       sync.Mutex
	stalled  bool
	briefs   []string
	stops    int
	launches int
}

func (h *stalledOwnerHost) start(t *testing.T) func(context.Context, SprintOwnerRequest) (SprintOwnerSession, error) {
	return func(_ context.Context, req SprintOwnerRequest) (SprintOwnerSession, error) {
		h.mu.Lock()
		stalled := h.stalled
		h.briefs = append(h.briefs, req.Brief)
		h.mu.Unlock()
		if stalled {
			var ack struct{ OK bool }
			err := agentlaunch.SendJSONControl(h.sock, map[string]string{"verb": "tell", "message": req.Brief}, &ack, 150*time.Millisecond)
			if err == nil {
				t.Fatal("stalled fixture acknowledged")
			}
			return SprintOwnerSession{}, fmt.Errorf("deliver instruction to sprint-%d-manager: %w", req.Sprint, err)
		}
		h.mu.Lock()
		h.launches++
		h.mu.Unlock()
		publishManagedSprintOwner(t, req.Owner)
		return SprintOwnerSession{ID: fmt.Sprintf("sprint-%d-manager", req.Sprint), Transport: room.TransportManaged}, nil
	}
}

func (h *stalledOwnerHost) stop(context.Context, SprintOwnerRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stops++
	if h.stopErr != nil {
		return h.stopErr
	}
	h.stopFix()
	h.stalled = false
	return nil
}

type supervisionCapture struct {
	mu       sync.Mutex
	sent     []string
	announce []string
}

func captureSupervision(t *testing.T) *supervisionCapture {
	t.Helper()
	c := &supervisionCapture{}
	old := weaveSupervisionSend
	weaveSupervisionSend = func(to, body string) error {
		c.mu.Lock()
		c.sent = append(c.sent, to+": "+body)
		c.mu.Unlock()
		return nil
	}
	oldAnnounce := announceStageFn
	announceStageFn = func(_, body string) error {
		c.mu.Lock()
		c.announce = append(c.announce, body)
		c.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { weaveSupervisionSend = old; announceStageFn = oldAnnounce })
	return c
}

func setupStalledSprint(t *testing.T) *stalledOwnerHost {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	seedAgent(t, "manager")
	sock, stopFix := stalledControlFixture(t)
	h := &stalledOwnerHost{sock: sock, stopFix: stopFix}
	withSprintOwnerLauncher(t, h.start(t))
	withSprintOwnerStopper(t, h.stop)
	if out, code := runSprint(t, "add", "stalled manager"); code != 0 {
		t.Fatal(out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "manager", "--instruction", "first"); code != 0 {
		t.Fatal(out)
	}
	// From here on the manager is hung: its socket accepts and never answers.
	h.mu.Lock()
	h.stalled = true
	h.mu.Unlock()
	for _, k := range []string{"BASHY_PRINCIPAL", "WEAVE_CONDUCTOR", "BASHY_AGENT_ID", "WEAVE_AGENT"} {
		t.Setenv(k, "")
	}
	// The instructing caller is NOT the supervisor: the stall notice must reach
	// the seat the steward registry names for this sprint.
	t.Setenv("BASHY_AGENT", "instructing-caller")
	withSprintSupervisor(t, "deputy:1", true)
	return h
}

func withSprintSupervisor(t *testing.T, address string, ok bool) {
	t.Helper()
	old := sprintSupervisorAddress
	sprintSupervisorAddress = func(int64, string, string) (string, bool) { return address, ok }
	t.Cleanup(func() { sprintSupervisorAddress = old })
}

// stallReports returns the sprint-thread stage entries that report a stall.
func stallReports(t *testing.T) []string {
	t.Helper()
	dir, _ := sprintStoreDir()
	q, err := readWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range findWeaveStory(q, 1).Thread {
		if c.Kind == kindStage && strings.Contains(c.Body, "stalled") {
			out = append(out, c.Body)
		}
	}
	return out
}

func announcedStall(c *supervisionCapture) bool {
	for _, b := range c.announce {
		if strings.Contains(b, "stalled") {
			return true
		}
	}
	return false
}

func TestSprintOwnerStallReportsStoredLeaseHeartbeat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	const holder = "manager"
	if err := room.Join(room.Card{ID: room.AgentClaimID(holder), Nick: holder,
		PID: os.Getpid(), Caps: []string{room.CapInboxDelivery}}); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{Lease: &weaveStoryLease{Holder: holder, At: time.Now().Add(-40 * time.Minute)}}
	if got := sprintOwnerHeartbeat(s); !strings.Contains(got, "last lease heartbeat 40m") {
		t.Fatalf("stall report heartbeat = %q, want stored beat 40m ago", got)
	}
}

func TestSprintOwnerStallUnknownLeaseHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		story *weaveStory
	}{
		{name: "nil story"},
		{name: "nil lease", story: &weaveStory{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sprintOwnerHeartbeat(tc.story); got != "lease heartbeat unknown" {
				t.Fatalf("heartbeat = %q, want lease heartbeat unknown", got)
			}
		})
	}
}

// The Sprint 457 reproducer: a hung manager is detected, reported to the
// thread, the board and the instructing supervisor, and recovered once through
// the existing stop/start lifecycle with the instruction delivered.
func TestSprintInstructStalledManagerIsReportedAndRecovered(t *testing.T) {
	h := setupStalledSprint(t)
	sup := captureSupervision(t)

	out, code := runSprint(t, "instruct", "1", "--instruction", "status?")
	if code != 0 {
		t.Fatalf("stalled manager was not recovered: exit=%d: %s", code, out)
	}
	if !strings.Contains(out, "recovered") {
		t.Errorf("instruct output does not say it recovered: %s", out)
	}
	if h.stops != 1 || h.launches != 2 {
		t.Fatalf("stops=%d launches=%d; want one lifecycle stop and one relaunch", h.stops, h.launches)
	}
	if got := h.briefs[len(h.briefs)-1]; got != "status?" {
		t.Fatalf("relaunched brief = %q", got)
	}
	reports := stallReports(t)
	if len(reports) != 1 || !strings.Contains(reports[0], "timeout") || !strings.Contains(reports[0], "heartbeat") || !strings.Contains(reports[0], "recovered") {
		t.Fatalf("sprint thread stall report = %#v", reports)
	}
	if len(sup.sent) != 1 || !strings.HasPrefix(sup.sent[0], "deputy:1: sprint #1") || !strings.Contains(sup.sent[0], "stalled") {
		t.Fatalf("supervisor notifications = %#v", sup.sent)
	}
	if !announcedStall(sup) {
		t.Fatalf("stall was not announced to seated conductors: %#v", sup.announce)
	}
}

func TestSprintInstructStalledManagerThatCannotStopIsNotRelaunched(t *testing.T) {
	h := setupStalledSprint(t)
	sup := captureSupervision(t)
	h.stopErr = errors.New("managed session sprint-1-manager accepted stop but did not terminate")

	out, code := runSprint(t, "instruct", "1", "--instruction", "status?")
	if code == 0 {
		t.Fatalf("recovery over a session that would not stop reported success: %s", out)
	}
	if !strings.Contains(out, "stalled") {
		t.Errorf("instruct error does not name the stall: %s", out)
	}
	if h.stops != 1 || h.launches != 1 {
		t.Fatalf("stops=%d launches=%d; a second manager must not launch beside the hung one", h.stops, h.launches)
	}
	if r := stallReports(t); len(r) != 1 || !strings.Contains(r[0], "recovery failed") {
		t.Fatalf("thread stall report = %#v", r)
	}
	if len(sup.sent) != 1 || !strings.Contains(sup.sent[0], "recovery failed") {
		t.Fatalf("supervisor notifications = %#v", sup.sent)
	}
}

func TestSprintInstructOrdinaryFailureIsNotTreatedAsStall(t *testing.T) {
	h := setupStalledSprint(t)
	sup := captureSupervision(t)
	withSprintOwnerLauncher(t, func(context.Context, SprintOwnerRequest) (SprintOwnerSession, error) {
		return SprintOwnerSession{}, errors.New("managed session sprint-1-manager already belongs to someone-else")
	})
	if out, code := runSprint(t, "instruct", "1", "--instruction", "status?"); code == 0 {
		t.Fatalf("ordinary failure reported success: %s", out)
	}
	if h.stops != 0 || len(sup.sent) != 0 || len(stallReports(t)) != 0 {
		t.Fatalf("non-stall failure triggered stall handling: stops=%d notices=%v", h.stops, sup.sent)
	}
}

func TestSprintInstructHasNoRecoverFlag(t *testing.T) {
	setupStalledSprint(t)
	captureSupervision(t)
	if out, code := runSprint(t, "instruct", "1", "--recover", "--instruction", "status?"); code == 0 {
		t.Fatalf("--recover was accepted; recovery must use the existing lifecycle: %s", out)
	}
}

// With no steward or deputy seat over the sprint there is nobody to notify:
// the gap is stated on the thread, and the instructing caller is not
// silently substituted for the supervisor.
func TestSprintInstructStalledManagerWithNoSupervisorReportsTheGap(t *testing.T) {
	setupStalledSprint(t)
	withSprintSupervisor(t, "", false)
	sup := captureSupervision(t)

	if out, code := runSprint(t, "instruct", "1", "--instruction", "status?"); code != 0 {
		t.Fatalf("stalled manager was not recovered: exit=%d: %s", code, out)
	}
	if r := stallReports(t); len(r) != 1 || !strings.Contains(r[0], "no supervisor") {
		t.Fatalf("thread stall report does not state the supervisor gap: %#v", r)
	}
	if len(sup.sent) != 0 {
		t.Fatalf("stall notice sent with no registered supervisor: %#v", sup.sent)
	}
}

// The heartbeat half (Sprint 457: silent 20:47→22:52 with nobody instructing):
// the immune tick finds a doing sprint whose managed manager is still running
// while its lease heartbeat has lapsed, and reports it ONCE to the thread and
// the registered supervisor. No real session: the card proof is stubbed.
func TestImmuneTickReportsASilentManagedManagerOnce(t *testing.T) {
	withQuotaImmuneFakes(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	withSprintSupervisor(t, "deputy:457", true)
	oldCard := sprintOwnerManagedCardLive
	t.Cleanup(func() { sprintOwnerManagedCardLive = oldCard })
	sprintOwnerManagedCardLive = func(owner string, sprint int64) bool { return owner == "manager" && sprint == 457 }

	now := time.Date(2026, 10, 17, 22, 52, 0, 0, time.UTC)
	store, _ := sprintStoreDir()
	s := &weaveStory{ID: 457, Column: "doing", Lease: &weaveStoryLease{Holder: "manager", At: now.Add(-125 * time.Minute)}}
	if err := withWeaveQueueLock(store, func(q *weaveQueue) error { q.Stories = []*weaveStory{s}; return nil }); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(root, 0o755)
	dir, _ := weaveQueueDir(root)
	quotaImmuneNow = func() time.Time { return now }
	quotaImmunePolicy = func() (llmbudget.ImmuneSettings, error) { return llmbudget.ImmuneSettings{}, nil }
	quotaImmuneResolveSeat = func(string, string, string) (quotaImmuneSeat, bool) { return quotaImmuneSeat{}, false }
	var sent []string
	weaveSupervisionSend = func(to, body string) error { sent = append(sent, to+": "+body); return nil }
	oldAnnounce := announceStageFn
	announceStageFn = func(string, string) error { return nil }
	t.Cleanup(func() { announceStageFn = oldAnnounce })
	cmd, _ := immuneTestCommand()

	actions, err := runQuotaImmuneTick(cmd, root, dir, false)
	if err != nil || len(actions) != 1 || actions[0].Kind != "manager-silent" || actions[0].Sprint != 457 {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "deputy:457: sprint #457 manager manager silent:") || !strings.Contains(sent[0], "lapsed") || !strings.Contains(sent[0], "heartbeat") {
		t.Fatalf("supervisor notifications = %#v", sent)
	}
	if actions, err := runQuotaImmuneTick(cmd, root, dir, false); err != nil || len(actions) != 0 || len(sent) != 1 {
		t.Fatalf("a lapse was reported twice: actions=%+v sent=%v err=%v", actions, sent, err)
	}
	board, _ := loadWeaveQueue(store)
	if th := board.Stories[0].Thread; len(th) != 1 || th[0].Kind != kindStage || !strings.Contains(th[0].Body, "silent:") {
		t.Fatalf("thread=%+v", th)
	}
}

// The sweep judges a snapshot and reports under the queue lock later. A
// manager that checkpoints in between is healthy again: the report must
// recheck the CURRENT lease (same heartbeat, still not live), not just the
// holder, or a recovered manager is alarmed as hung.
func TestImmuneTickDoesNotReportAManagerThatRefreshedAfterTheSnapshot(t *testing.T) {
	withQuotaImmuneFakes(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	withSprintSupervisor(t, "deputy:457", true)
	now := time.Date(2026, 10, 17, 22, 52, 0, 0, time.UTC)
	store, _ := sprintStoreDir()
	s := &weaveStory{ID: 457, Column: "doing", Lease: &weaveStoryLease{Holder: "manager", At: now.Add(-125 * time.Minute)}}
	if err := withWeaveQueueLock(store, func(q *weaveQueue) error { q.Stories = []*weaveStory{s}; return nil }); err != nil {
		t.Fatal(err)
	}
	oldCard := sprintOwnerManagedCardLive
	t.Cleanup(func() { sprintOwnerManagedCardLive = oldCard })
	// Runs after the sweep took its snapshot and before it reports: the
	// manager checkpoints right here.
	sprintOwnerManagedCardLive = func(string, int64) bool {
		_ = withWeaveQueueLock(store, func(q *weaveQueue) error {
			findWeaveStory(q, 457).Lease.At = now
			return nil
		})
		return true
	}
	var sent []string
	weaveSupervisionSend = func(to, body string) error { sent = append(sent, to+": "+body); return nil }
	oldAnnounce := announceStageFn
	announceStageFn = func(string, string) error { return nil }
	t.Cleanup(func() { announceStageFn = oldAnnounce })

	actions, err := checkSilentSprintManagers(now, false)
	if err != nil || len(actions) != 0 || len(sent) != 0 {
		t.Fatalf("refreshed manager alarmed: actions=%+v sent=%v err=%v", actions, sent, err)
	}
	board, _ := loadWeaveQueue(store)
	if th := board.Stories[0].Thread; len(th) != 0 {
		t.Fatalf("thread=%+v", th)
	}
}
