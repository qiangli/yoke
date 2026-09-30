package capability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
)

func ladderCertifyFixture(t *testing.T, verdict string) (ladderCertifyDeps, string, *int) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "host")
	t.Setenv("BASHY_HOME", home)
	bench := t.TempDir()
	if err := os.WriteFile(filepath.Join(bench, "DAG.md"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	deps := ladderCertifyDeps{
		now: func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) },
		resolve: func(name string) (fleet.Agent, fleet.Tool, fleet.Model, error) {
			return fleet.Agent{Name: name, Tool: "tool-a", Model: "model-a"}, fleet.Tool{Name: "tool-a", CLI: fleet.ToolCLI{Binary: "tool-bin"}}, fleet.Model{Name: "model-a"}, nil
		},
		run: func(ctx context.Context, dir, binary string, args ...string) (string, error) {
			*calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("runner has no timeout")
			}
			if binary == "tool-bin" {
				if strings.Join(args, " ") != "--version" {
					t.Errorf("version args: %v", args)
				}
				return "tool 2.4\n", nil
			}
			if dir != bench {
				t.Errorf("cwd = %q", dir)
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "K=1") || !strings.Contains(joined, "AGENT=agent-a") {
				t.Errorf("args: %v", args)
			}
			for _, arg := range args {
				if strings.HasPrefix(arg, "RUNS=") && !strings.HasPrefix(strings.TrimPrefix(arg, "RUNS="), home+string(os.PathSeparator)) {
					t.Errorf("results outside host: %s", arg)
				}
			}
			if args[1] == "verdict" {
				return fmt.Sprintf("agent-a [l1]: %s  20/20\n", verdict), nil
			}
			return "completed", nil
		},
	}
	return deps, bench, calls
}

func TestLadderCertifyHostOnly(t *testing.T) {
	for _, verdict := range []string{"PASS", "FAIL"} {
		t.Run(verdict, func(t *testing.T) {
			deps, bench, calls := ladderCertifyFixture(t, verdict)
			repo := t.TempDir()
			t.Chdir(repo)
			cmd := newLadderCertifyCmdWith(deps)
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs([]string{"--agent", "agent-a", "--packs", "l1", "--bench", bench})
			err := cmd.Execute()
			if (err != nil) != (verdict == "FAIL") {
				t.Fatalf("verdict=%s err=%v", verdict, err)
			}
			if *calls != 4 {
				t.Errorf("calls=%d, want version + smoke + bench + verdict", *calls)
			}
			// Certification is earned here: neither repository nor bench gets cert state.
			entries, _ := os.ReadDir(repo)
			if len(entries) != 0 {
				t.Fatalf("repository writes: %v", entries)
			}
			entries, _ = os.ReadDir(bench)
			if len(entries) != 1 {
				t.Fatalf("bench writes: %v", entries)
			}
			logs, _ := filepath.Glob(filepath.Join(os.Getenv("BASHY_HOME"), "ladder", "certify", "agent-a", "l1", "*", "certify.log"))
			if len(logs) != 1 {
				t.Fatalf("attempt logs: %v", logs)
			}
			data, _ := os.ReadFile(logs[0])
			if !strings.Contains(string(data), verdict) {
				t.Errorf("log lacks verdict: %s", data)
			}
			if verdict == "FAIL" {
				if _, err := os.Stat(ladder.DefaultStorePath()); !os.IsNotExist(err) {
					t.Fatal("FAIL created certificate store")
				}
				return
			}
			st, err := ladder.OpenStore(ladder.DefaultStorePath())
			if err != nil {
				t.Fatal(err)
			}
			events, err := st.Read()
			if err != nil || len(events) != 1 {
				t.Fatalf("events=%v err=%v", events, err)
			}
			e := events[0]
			if e.Kind != ladder.EventKindCert || e.Agent != "tool-a:model-a" || e.Cert.Kind != ladder.CertL1 || e.Cert.ModelVersion != "tool 2.4 / model-a" || e.Season != ladder.SeasonOf(deps.now()) || e.Cert.Season != e.Season || !strings.HasPrefix(e.Note, "host-certified: l1 PASS") {
				t.Fatalf("bad cert: %+v", e)
			}
		})
	}
}

func TestLadderCertifyDryRunAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		fail bool
	}{
		{"dry", []string{"--dry-run"}, false},
		{"absent", []string{"--bench", "/missing-certify-bench"}, true},
		{"pack", []string{"--packs", "floor"}, true},
		{"k", []string{"--k", "0"}, true},
		{"traversal", []string{"--agent", "../agent-a"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, bench, calls := ladderCertifyFixture(t, "PASS")
			cmd := newLadderCertifyCmdWith(deps)
			out := new(bytes.Buffer)
			cmd.SetOut(out)
			cmd.SetErr(out)
			cmd.SetArgs(append([]string{"--agent", "agent-a", "--packs", "l1", "--bench", bench}, tc.args...))
			err := cmd.Execute()
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v", err)
			}
			if *calls != 0 {
				t.Fatalf("executed %d commands", *calls)
			}
			if _, err := os.Stat(os.Getenv("BASHY_HOME")); !os.IsNotExist(err) {
				t.Fatal("validation/dry-run wrote host state")
			}
			if !tc.fail && (!strings.Contains(out.String(), "dag bench") || !strings.Contains(out.String(), "dag verdict")) {
				t.Fatal(out.String())
			}
		})
	}
}

func TestLadderCertifyPackKinds(t *testing.T) {
	for _, pack := range []string{"l1", "l2", "l3", "steer", "manager", "review", "judge", "l5"} {
		got, err := ladderCertifyKind(pack)
		if err != nil || string(got) != pack {
			t.Fatalf("%s: %s %v", pack, got, err)
		}
	}
	if _, err := ladderCertifyKind("unknown"); err == nil {
		t.Fatal("accepted unknown pack")
	}
}

func TestLadderCertifyRejectsBadVerdicts(t *testing.T) {
	for _, output := range []string{"PASS", "other [l1]: PASS", "agent-a [review]: PASS", "agent-a [l1]: PASSIVE", "agent-a [l1]: PASS\nagent-a [l1]: FAIL"} {
		if _, err := ladderCertifyVerdict(output, "agent-a", "l1"); err == nil {
			t.Errorf("accepted %q", output)
		}
	}
	deps, bench, _ := ladderCertifyFixture(t, "PASS")
	deps.run = func(context.Context, string, string, ...string) (string, error) {
		return "", errors.New("probe failed")
	}
	cmd := newLadderCertifyCmdWith(deps)
	cmd.SetArgs([]string{"--agent", "agent-a", "--packs", "l1", "--bench", bench})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err == nil {
		t.Fatal("ignored runner error")
	}
	if _, err := os.Stat(ladder.DefaultStorePath()); !os.IsNotExist(err) {
		t.Fatal("error created cert")
	}
}

func TestLadderCertifyMatrixAndFreshAttempts(t *testing.T) {
	deps, bench, _ := ladderCertifyFixture(t, "PASS")
	var runs []string
	deps.run = func(ctx context.Context, dir, binary string, args ...string) (string, error) {
		if binary == "tool-bin" {
			return "tool 2.4", nil
		}
		values := map[string]string{}
		for _, arg := range args[2:] {
			key, value, _ := strings.Cut(arg, "=")
			values[key] = value
		}
		if values["K"] != "2" {
			t.Errorf("K=%q", values["K"])
		}
		if args[1] == "bench" {
			runs = append(runs, values["RUNS"])
		}
		if args[1] == "verdict" {
			return fmt.Sprintf("%s [%s]: PASS  complete", values["AGENT"], values["PACK"]), nil
		}
		return "complete", nil
	}
	for i := 0; i < 2; i++ {
		cmd := newLadderCertifyCmdWith(deps)
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs([]string{"--agent", "agent-a,agent-b", "--packs", "l1,review", "--bench", bench, "--k", "2"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if len(runs) != 8 {
		t.Fatalf("bench runs=%v", runs)
	}
	seen := map[string]bool{}
	for _, path := range runs {
		if seen[path] {
			t.Errorf("attempt reused %s", path)
		}
		seen[path] = true
	}
	st, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.Read()
	if err != nil || len(events) != 8 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}

func TestLadderCertifyStageFailures(t *testing.T) {
	for _, stage := range []string{"smoke", "bench", "verdict"} {
		t.Run(stage, func(t *testing.T) {
			deps, bench, _ := ladderCertifyFixture(t, "PASS")
			run := deps.run
			deps.run = func(ctx context.Context, dir, binary string, args ...string) (string, error) {
				if len(args) > 1 && args[1] == stage {
					return "partial output", context.DeadlineExceeded
				}
				return run(ctx, dir, binary, args...)
			}
			cmd := newLadderCertifyCmdWith(deps)
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs([]string{"--agent", "agent-a", "--packs", "l1", "--bench", bench})
			if err := cmd.Execute(); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(ladder.DefaultStorePath()); !os.IsNotExist(err) {
				t.Fatal("failed stage wrote cert")
			}
			logs, _ := filepath.Glob(filepath.Join(os.Getenv("BASHY_HOME"), "ladder", "certify", "agent-a", "l1", "*", "certify.log"))
			if len(logs) != 1 {
				t.Fatalf("logs=%v", logs)
			}
			data, err := os.ReadFile(logs[0])
			if err != nil || !strings.Contains(string(data), "deadline exceeded") {
				t.Fatalf("log=%s err=%v", data, err)
			}
		})
	}
}

func TestLadderCertifyBenchDiscovery(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "yoke")
	bench := filepath.Join(root, "agent-bench")
	home := filepath.Join(root, "host")
	for _, dir := range []string{repo, bench, filepath.Join(home, "agent-bench")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module github.com/qiangli/yoke\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{bench, filepath.Join(home, "agent-bench")} {
		if err := os.WriteFile(filepath.Join(dir, "DAG.md"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	originalRoots := ladderCertifyDiscoveryRoots
	ladderCertifyDiscoveryRoots = func() []string { return []string{repo} }
	t.Cleanup(func() { ladderCertifyDiscoveryRoots = originalRoots })
	got, err := ladderCertifyBench("", home)
	if err != nil || got != bench {
		t.Fatalf("bench=%s err=%v", got, err)
	}
	if err := os.Remove(filepath.Join(bench, "DAG.md")); err != nil {
		t.Fatal(err)
	}
	got, err = ladderCertifyBench("", home)
	if err != nil || got != filepath.Join(home, "agent-bench") {
		t.Fatalf("fallback=%s err=%v", got, err)
	}
	if _, err := ladderCertifyBench(bench, home); err == nil {
		t.Fatal("explicit invalid path fell back")
	}
	if err := os.Remove(filepath.Join(home, "agent-bench", "DAG.md")); err != nil {
		t.Fatal(err)
	}
	got, err = ladderCertifyBench("", home)
	if got != "" || err == nil || !strings.Contains(err.Error(), "agent-bench checkout absent (need DAG.md)") || !strings.Contains(err.Error(), "--bench PATH") {
		t.Fatalf("missing checkout: bench=%q err=%v", got, err)
	}
}

func TestLadderCertifyRegistered(t *testing.T) {
	cmd, _, err := NewLeaderboardCmd().Find([]string{"certify"})
	if err != nil || cmd.Name() != "certify" {
		t.Fatalf("command=%v err=%v", cmd, err)
	}
}
