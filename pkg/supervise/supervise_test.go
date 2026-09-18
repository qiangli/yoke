package supervise

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

func TestWorkerLaunchFailureCannotPassAnAlreadyGreenGate(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Supervisor: "claude", Fleet: []string{"agy"}, MaxAttempts: 1, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: "true"}},
	}
	r := funcRunner(func(_ context.Context, _ string, _ []string, _ string) (string, int, error) {
		return "launch refused", 2, errors.New("launch refused")
	})
	v := runContract(context.Background(), p, p.Contracts[0], r, noProgress{})
	if v.Passed || v.GateExit == 0 {
		t.Fatalf("failed worker manufactured a passing gate verdict: %+v", v)
	}
}

func TestNoSupervisorDoesNotLaunchSummaryAndConverges(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Fleet: []string{"agy"}, MaxAttempts: 1, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: "true"}},
	}
	calls := 0
	r := funcRunner(func(_ context.Context, agent string, _ []string, _ string) (string, int, error) {
		calls++
		if agent != "agy" {
			t.Fatalf("unexpected final model invocation for %q", agent)
		}
		return "done", 0, nil
	})
	res, err := Run(context.Background(), p, r, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged || res.Judged || calls != 1 {
		t.Fatalf("gate-only run should converge after one worker call: calls=%d result=%+v", calls, res)
	}
}

func TestUnavailableOptionalSupervisorDoesNotBlockConvergence(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Supervisor: "ycode", Fleet: []string{"agy"}, MaxAttempts: 1, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: "true"}},
	}
	r := scriptRunner{reply: map[string]string{"agy": "done"}, code: map[string]int{"ycode": 1}}
	res, err := Run(context.Background(), p, r, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged || res.Judged {
		t.Fatalf("optional supervisor availability must not determine convergence: %+v", res)
	}
}

func TestExplicitSupervisorSummaryCannotOverrideConvergence(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Supervisor: "ycode", Fleet: []string{"agy"}, MaxAttempts: 1, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: "true"}},
	}
	r := scriptRunner{reply: map[string]string{"agy": "done", "ycode": "I would not ship this."}}
	res, err := Run(context.Background(), p, r, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged || !res.Judged || !strings.Contains(res.Judgment, "not ship") {
		t.Fatalf("explicit summary should be recorded but cannot override the gate: %+v", res)
	}
}

// scriptRunner returns a canned reply per agent so workers/supervisor can be
// driven deterministically without spawning real CLIs.
type scriptRunner struct {
	reply map[string]string
	code  map[string]int
}

func (s scriptRunner) Run(_ context.Context, agent string, _ []string, _ string) (string, int, error) {
	return s.reply[agent], s.code[agent], nil
}

type noProgress struct{}

func (noProgress) progress(string) {}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// funcRunner adapts a function to chat.Runner.
type funcRunner func(ctx context.Context, agent string, args []string, cwd string) (string, int, error)

func (f funcRunner) Run(ctx context.Context, agent string, args []string, cwd string) (string, int, error) {
	return f(ctx, agent, args, cwd)
}

func testEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_SUPERVISE_DIR", filepath.Join(dir, "store"))
	old := nowFn
	nowFn = func() time.Time { return time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFn = old })
	return dir
}

func TestParseTasks(t *testing.T) {
	cs, err := parseTasks([]string{
		"fix comsub :: make test | grep PASS",
		"@codex: fix errors :: true",
		"varenv| fix varenv :: false",
		"no gate task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 4 {
		t.Fatalf("want 4 contracts, got %d", len(cs))
	}
	if cs[0].Goal != "fix comsub" || cs[0].Gate != "make test | grep PASS" {
		t.Errorf("c0: %+v", cs[0])
	}
	if cs[1].Worker != "codex" || cs[1].Goal != "fix errors" || cs[1].Gate != "true" {
		t.Errorf("c1 (pinned worker): %+v", cs[1])
	}
	if cs[2].ID != "varenv" || cs[2].Goal != "fix varenv" || cs[2].Gate != "false" {
		t.Errorf("c2 (named id): %+v", cs[2])
	}
	if cs[3].gated() {
		t.Errorf("c3 should be ungated: %+v", cs[3])
	}
}

// The verdict is the GATE, not the worker's claim. A worker that "succeeds" but
// leaves the gate failing is recorded FAIL — the green-but-uncommitted defense.
func TestGateDecidesVerdictNotTheWorker(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Supervisor: "claude", Fleet: []string{"codex"}, MaxAttempts: 1, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: "false"}}, // gate always fails
	}
	// worker claims success (exit 0), but the gate `false` fails.
	r := scriptRunner{reply: map[string]string{"codex": "done!", "claude": "not met"}}
	v := runContract(context.Background(), p, p.Contracts[0], r, noProgress{})
	if v.Passed {
		t.Fatal("gate `false` must make the verdict FAIL even though the worker claimed success")
	}
	if v.GateExit == 0 {
		t.Errorf("gate exit should be non-zero, got %d", v.GateExit)
	}
}

// A passing gate converges.
func TestGatePassConverges(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Fleet: []string{"codex"}, MaxAttempts: 2, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: "true"}},
	}
	r := scriptRunner{reply: map[string]string{"codex": "ok"}}
	res, err := Run(context.Background(), p, r, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged || len(res.Verdicts) != 1 || !res.Verdicts[0].Passed {
		t.Fatalf("expected convergence, got %+v", res)
	}
	if res.Report == "" {
		t.Error("a report should be filed")
	}
}

// Retry rotates the fleet: a gate that fails then passes should land the second
// attempt on a DIFFERENT worker.
func TestRetryRotatesFleet(t *testing.T) {
	testEnv(t)
	// gate fails on the first run, passes on the second (a file toggled by the worker turns)
	gatefile := filepath.Join(t.TempDir(), "flag")
	gate := "test -f " + shellQuote(gatefile)
	attempts := 0
	// A runner that creates the flag file on its SECOND invocation.
	r := funcRunner(func(_ context.Context, agent string, _ []string, _ string) (string, int, error) {
		attempts++
		if attempts == 2 {
			_ = os.WriteFile(gatefile, []byte("x"), 0o644)
		}
		return "turn by " + agent, 0, nil
	})
	p := &Plan{
		Goal: "g", Supervisor: "claude", Fleet: []string{"codex", "opencode"}, MaxAttempts: 3, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: gate}},
	}
	v := runContract(context.Background(), p, p.Contracts[0], r, noProgress{})
	if !v.Passed {
		t.Fatal("should pass on the 2nd attempt")
	}
	if v.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", v.Attempts)
	}
	if v.Worker != "opencode" {
		t.Errorf("2nd attempt should rotate to opencode, got %q", v.Worker)
	}
}

// A pinned @worker is never rotated.
func TestPinnedWorkerNotRotated(t *testing.T) {
	testEnv(t)
	p := &Plan{Fleet: []string{"codex", "opencode"}}
	c := &Contract{ID: "t1", Goal: "x", Worker: "codex"}
	for a := 0; a < 3; a++ {
		if p.pick(c, a) != "codex" {
			t.Fatalf("pinned worker must not rotate (attempt %d)", a)
		}
	}
}

// An ungated task can converge from a successful worker exit, while UNVERIFIED
// records that no independent gate supplied stronger evidence.
func TestUngatedSuccessfulTaskConvergesAsUnverified(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Fleet: []string{"codex"}, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do"}}, // no gate
	}
	r := scriptRunner{reply: map[string]string{"codex": "ok"}}
	res, err := Run(context.Background(), p, r, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged {
		t.Error("a successful ungated task should converge")
	}
	if !res.Verdicts[0].Unverified {
		t.Error("ungated task should be marked Unverified")
	}
}

func TestUngatedWorkerFailureIsUnverifiedAndDoesNotConverge(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "g", Fleet: []string{"codex"}, MaxAttempts: 1, Cwd: os.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do"}},
	}
	r := scriptRunner{reply: map[string]string{"codex": "failed"}, code: map[string]int{"codex": 1}}
	res, err := Run(context.Background(), p, r, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Converged || res.Verdicts[0].Passed || !res.Verdicts[0].Unverified {
		t.Fatalf("failed ungated task must retain evidence metadata without converging: %+v", res)
	}
}

// Validation enforces the roster invariants.
func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		p    Plan
		want string
	}{
		{"no goal", Plan{Supervisor: "c", Fleet: []string{"x"}, Contracts: []*Contract{{Goal: "g"}}}, "--goal"},
		{"no worker", Plan{Goal: "g", Contracts: []*Contract{{Goal: "g"}}}, "--worker"},
		{"no tasks", Plan{Goal: "g", Supervisor: "c", Fleet: []string{"x"}}, "--task"},
		{"pinned worker off-fleet", Plan{Goal: "g", Supervisor: "c", Fleet: []string{"x"},
			Contracts: []*Contract{{ID: "t1", Goal: "g", Worker: "codex"}}}, "not in --worker fleet"},
	}
	for _, tc := range cases {
		err := tc.p.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	ok := Plan{Goal: "g", Fleet: []string{"codex"}, Contracts: []*Contract{{ID: "t1", Goal: "g", Gate: "true"}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

// The report captures the verdict table and an explicitly requested supervisor
// summary; home is redacted.
func TestReportContents(t *testing.T) {
	testEnv(t)
	p := &Plan{
		Goal: "fix the thing", Supervisor: "claude", Fleet: []string{"codex"}, Cwd: t.TempDir(),
		Contracts: []*Contract{{ID: "t1", Goal: "do", Gate: "true"}},
	}
	r := scriptRunner{reply: map[string]string{"codex": "ok", "claude": "The goal is met; nothing left."}}
	res, err := Run(context.Background(), p, r, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Report)
	md := string(b)
	for _, must := range []string{"# Supervision — fix the thing", "CONVERGED", "Supervisor summary (optional)", "The goal is met", "| Task | Verdict |"} {
		if !strings.Contains(md, must) {
			t.Fatalf("report missing %q\n%s", must, md)
		}
	}
}

func TestHelpMakesGateAndSupervisorOptional(t *testing.T) {
	cmd := NewSuperviseCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	help := out.String()
	for _, want := range []string{"goal [:: gate]", "gate optional", "optional final summary", "never determines convergence"} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q:\n%s", want, help)
		}
	}
}

// TestMain permits launching agents with their own approval gate disabled.
//
// These tests drive the real launch path (with fake runners) against baseline
// tools whose templates carry a `--dangerously-*` flag, which chat's
// guardUnsafeArgs refuses on an uncontained host. The gate is the point of that
// guard and is tested in pkg/chat; here it is a precondition, not the subject.
func TestMain(m *testing.M) {
	os.Setenv(chat.UnsafeLaunchEnv, "1")
	os.Exit(m.Run())
}
