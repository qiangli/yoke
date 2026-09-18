package weave

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testWeaveAutopilotRun struct {
	output string
	exit   int
	err    error
}

type testWeaveAutopilotRunner struct {
	mu      sync.Mutex
	runs    []string
	actions map[string][]testWeaveAutopilotRun
	healthy map[string]bool
}

func (r *testWeaveAutopilotRunner) Healthy(ctx context.Context, m weaveMember) bool {
	_ = ctx
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthy == nil {
		return true
	}
	return r.healthy[m.Label()]
}

func (r *testWeaveAutopilotRunner) Run(ctx context.Context, m weaveMember, prompt, queueDir string, onOutput func(string)) (int, error) {
	_ = ctx
	_ = prompt
	_ = queueDir
	tool := m.Label()
	r.mu.Lock()
	r.runs = append(r.runs, tool)
	var action testWeaveAutopilotRun
	if list := r.actions[tool]; len(list) > 0 {
		action = list[0]
		r.actions[tool] = list[1:]
	}
	r.mu.Unlock()
	if action.output != "" {
		onOutput(action.output)
	}
	return action.exit, action.err
}

func (r *testWeaveAutopilotRunner) runList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.runs...)
}

func testWeaveAutopilotQueue(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	q := &weaveQueue{
		NextID: 2,
		Root:   "/repo",
		Items: []*weaveItem{{
			ID:       1,
			Title:    "ship the thing",
			Priority: "p1",
			State:    "todo",
			Created:  time.Now().UTC(),
		}},
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatalf("save queue: %v", err)
	}
	return dir
}

func TestWeaveAutopilotRotatesOnOverloadSignature(t *testing.T) {
	dir := testWeaveAutopilotQueue(t)
	runner := &testWeaveAutopilotRunner{
		actions: map[string][]testWeaveAutopilotRun{
			"claude": {{output: "HTTP 529 Overloaded\n", exit: 1}},
			"codex":  {{exit: 0}},
		},
	}

	res, err := runWeaveAutopilotLoop(context.Background(), weaveAutopilotLoopOptions{
		queueDir:  dir,
		repoRoot:  "/repo",
		fleet:     testMembers("claude", "codex"),
		leaseTTL:  time.Second,
		heartbeat: 10 * time.Millisecond,
		backoff:   time.Millisecond,
		runner:    runner,
		holder:    "test-holder",
		maxRuns:   2,
	})
	if err != nil {
		t.Fatalf("autopilot: %v", err)
	}
	if got := strings.Join(runner.runList(), ","); got != "claude,codex" {
		t.Fatalf("runs = %s, want claude,codex", got)
	}
	if res.Tool != "codex" {
		t.Fatalf("result tool = %q, want codex", res.Tool)
	}
	if _, ok, err := loadWeaveAutopilotLease(dir); err != nil || ok {
		t.Fatalf("lease after clean exit: ok=%v err=%v, want released", ok, err)
	}
	log := readWeaveAutopilotTestLog(dir)
	if !strings.Contains(log, "failover from=claude reason=api-overload signature") {
		t.Fatalf("autopilot log missing overload failover:\n%s", log)
	}
}

func TestWeaveAutopilotStandbyTakesOverExpiredLease(t *testing.T) {
	dir := testWeaveAutopilotQueue(t)
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	stale := weaveOrchestratorLease{
		Holder:      "dead-primary",
		Tool:        "claude",
		PID:         12345,
		AcquiredAt:  now.Add(-2 * time.Minute),
		HeartbeatAt: now.Add(-2 * time.Minute),
		ExpiresAt:   now.Add(-time.Minute),
		Generation:  7,
	}
	if err := saveWeaveAutopilotLease(dir, stale); err != nil {
		t.Fatalf("save stale lease: %v", err)
	}
	runner := &testWeaveAutopilotRunner{
		actions: map[string][]testWeaveAutopilotRun{
			"codex": {{exit: 0}},
		},
	}

	res, err := runWeaveAutopilotLoop(context.Background(), weaveAutopilotLoopOptions{
		queueDir:  dir,
		repoRoot:  "/repo",
		fleet:     testMembers("codex"),
		standby:   true,
		leaseTTL:  time.Minute,
		heartbeat: 10 * time.Millisecond,
		backoff:   time.Millisecond,
		runner:    runner,
		holder:    "standby",
		maxRuns:   1,
		now:       func() time.Time { return now },
		sleep:     func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("autopilot standby: %v", err)
	}
	if res.Tool != "codex" {
		t.Fatalf("result tool = %q, want codex", res.Tool)
	}
	if got := strings.Join(runner.runList(), ","); got != "codex" {
		t.Fatalf("runs = %s, want codex", got)
	}
	log := readWeaveAutopilotTestLog(dir)
	if !strings.Contains(log, "takeover tool=codex holder=standby generation=8") {
		t.Fatalf("autopilot log missing standby takeover:\n%s", log)
	}
}

func TestWeaveAutopilotLeaseBusyWithoutStandby(t *testing.T) {
	dir := testWeaveAutopilotQueue(t)
	now := time.Now().UTC()
	if err := saveWeaveAutopilotLease(dir, weaveOrchestratorLease{
		Holder:      "live",
		Tool:        "claude",
		HeartbeatAt: now,
		ExpiresAt:   now.Add(time.Minute),
		Generation:  1,
	}); err != nil {
		t.Fatalf("save lease: %v", err)
	}
	_, err := runWeaveAutopilotLoop(context.Background(), weaveAutopilotLoopOptions{
		queueDir:  dir,
		repoRoot:  "/repo",
		fleet:     testMembers("codex"),
		leaseTTL:  time.Minute,
		heartbeat: time.Second,
		runner:    &testWeaveAutopilotRunner{},
		holder:    "contender",
		now:       func() time.Time { return now },
	})
	if !errors.Is(err, errWeaveAutopilotLeaseBusy) {
		t.Fatalf("err = %v, want lease busy", err)
	}
}

func TestWeaveAutopilotReviewAgentWithoutVerdictIsHarnessError(t *testing.T) {
	dir := testWeaveAutopilotQueue(t)
	pinPassthroughJudge(t)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	q.Items[0].State = "submitted"
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	runner := &testWeaveAutopilotRunner{actions: map[string][]testWeaveAutopilotRun{
		"codex": {{exit: 1}},
	}}
	var stdout bytes.Buffer
	res, err := runWeaveAutopilotLoop(context.Background(), weaveAutopilotLoopOptions{
		queueDir: dir, repoRoot: "/repo", fleet: testMembers("codex"),
		leaseTTL: time.Second, heartbeat: 10 * time.Millisecond, backoff: time.Millisecond,
		runner: runner, holder: "review-test", maxRuns: 1, reviewAgent: "reviewer", stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("autopilot: %v", err)
	}
	if res.PairVerdict != string(weavePairHarnessError) || res.PairExit != weavePairHarnessErrorExit {
		t.Fatalf("pair result = %#v", res)
	}
	if !strings.Contains(stdout.String(), "PAIR HARNESS-ERROR") || !strings.Contains(stdout.String(), "without a pair verdict") {
		t.Fatalf("stdout missing verdict and reason: %q", stdout.String())
	}
	q, err = loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if q.Items[0].State != "submitted" || !q.Items[0].NeedsSteward || !strings.Contains(q.Items[0].StewardReason, "PAIR HARNESS-ERROR") {
		t.Fatalf("submitted run did not retain named harness reason: %#v", q.Items[0])
	}
}

func readWeaveAutopilotTestLog(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "autopilot.log"))
	return strings.TrimSpace(string(b))
}

// testMembers builds a bare-tool roster, the shape autopilot took before
// agents existed.
func testMembers(names ...string) []weaveMember {
	out := make([]weaveMember, 0, len(names))
	for _, n := range names {
		out = append(out, weaveMember{Name: n, Tool: n, Bin: n})
	}
	return out
}
