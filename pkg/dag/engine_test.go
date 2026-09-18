// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	// Register the coreutils userland so a body's `cat` resolves in-process
	// through shell.Handler() — proving hermetic, pure-Go execution.
	_ "github.com/qiangli/coreutils/cmds/all"
)

func engineFor(t *testing.T, dir, md string) *Engine {
	t.Helper()
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return &Engine{
		Graph: g, Dir: dir, Env: os.Environ(),
		Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer),
	}
}

func TestEngineDependencyChain(t *testing.T) {
	dir := t.TempDir()
	// build writes via shell redirection; check reads via coreutils `cat`.
	md := "## Tasks\n\n" +
		"### clean\n" + block("bash", "rm -f out.txt") +
		"### build\nRequires: clean\n" + block("bash", "echo hello > out.txt") +
		"### check\nRequires: build\n" + block("bash", "cat out.txt")

	out := new(bytes.Buffer)
	eng := engineFor(t, dir, md)
	eng.Stdout = out

	report, err := eng.Run(context.Background(), "check")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Fatalf("report failed: %+v", report.Results)
	}
	if len(report.Results) != 3 {
		t.Fatalf("want 3 results, got %d", len(report.Results))
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("check did not read the built file; out=%q", out.String())
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "out.txt")); strings.TrimSpace(string(data)) != "hello" {
		t.Errorf("out.txt = %q", string(data))
	}
	for _, r := range report.Results {
		if r.Status != StatusDone {
			t.Errorf("task %s status = %s", r.Name, r.Status)
		}
	}
}

func TestEngineIncrementalSkip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	md := "## Tasks\n\n### gen\nSources: in.txt\nGenerates: out.txt\n" +
		block("bash", "cat in.txt > out.txt")
	cache := &Cache{Hashes: map[string]string{}}
	cache.path = filepath.Join(dir, "cache.json")
	newEng := func() *Engine {
		g, err := BuildGraph(doc(t, md))
		if err != nil {
			t.Fatal(err)
		}
		return &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1,
			FailFast: true, Cache: cache, Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	}
	status := func(r RunReport) Status { return r.Results[0].Status }

	r1, _ := newEng().Run(context.Background(), "gen")
	if status(r1) != StatusDone {
		t.Fatalf("first run: want done, got %s", status(r1))
	}
	r2, _ := newEng().Run(context.Background(), "gen")
	if status(r2) != StatusUpToDate {
		t.Errorf("unchanged rerun: want up-to-date, got %s", status(r2))
	}
	// Change the source → fingerprint changes → rebuild.
	if err := os.WriteFile(filepath.Join(dir, "in.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	r3, _ := newEng().Run(context.Background(), "gen")
	if status(r3) != StatusDone {
		t.Errorf("changed source: want done, got %s", status(r3))
	}
	// --force ignores the cache.
	eng := newEng()
	eng.Force = true
	r4, _ := eng.Run(context.Background(), "gen")
	if status(r4) != StatusDone {
		t.Errorf("force: want done, got %s", status(r4))
	}
}

func TestEngineParallelDiamond(t *testing.T) {
	dir := t.TempDir()
	// a -> {b, c} -> d. Each target asserts its deps ran (their .done files
	// exist) before touching its own, so a scheduling bug fails the target.
	md := "## Tasks\n\n" +
		"### a\n" + block("bash", "touch a.done") +
		"### b\nRequires: a\n" + block("bash", "test -f a.done && touch b.done") +
		"### c\nRequires: a\n" + block("bash", "test -f a.done && touch c.done") +
		"### d\nRequires: b, c\n" + block("bash", "test -f b.done && test -f c.done && touch d.done")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatal(err)
	}
	eng := &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 4,
		FailFast: true, Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	report, err := eng.Run(context.Background(), "d")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Fatalf("parallel diamond failed: %+v", report.Results)
	}
	if len(report.Results) != 4 {
		t.Fatalf("want 4 results, got %d", len(report.Results))
	}
	if _, err := os.Stat(filepath.Join(dir, "d.done")); err != nil {
		t.Errorf("d.done missing — dependency ordering not respected under -j")
	}
}

func TestEnginePerTargetEnv(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### e\nEnv: GREETING=hi\n" + block("bash", "echo \"${GREETING:-none}\"")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatal(err)
	}
	eng := &Engine{Graph: g, Dir: dir, Env: os.Environ(), Concurrency: 1, FailFast: true,
		Capture: true, Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
	report, err := eng.Run(context.Background(), "e")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(report.Results[0].Stdout, "hi") {
		t.Errorf("per-target Env not applied; stdout=%q", report.Results[0].Stdout)
	}
}

func TestEngineTimeout(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### slow\nTimeout: 1s\n" + block("bash", "sleep 5")
	eng := engineFor(t, dir, md)

	start := time.Now()
	report, err := eng.Run(context.Background(), "slow")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := report.Results[0]
	if r.Status != StatusFailed || r.ExitCode != 124 {
		t.Errorf("want failed/124, got %s/%d (err=%v)", r.Status, r.ExitCode, r.Err)
	}
	if elapsed > 4*time.Second {
		t.Errorf("timeout did not interrupt the body in ~1s; took %s", elapsed)
	}
}

func TestEngineRetries(t *testing.T) {
	dir := t.TempDir()
	// Flaky body: fails the first attempt (no marker yet), succeeds the second.
	// Retries: 2 => up to 3 attempts, so it must end StatusDone.
	body := "if [ -f attempt.marker ]; then exit 0; fi\ntouch attempt.marker\nexit 1"
	md := "## Tasks\n\n### flaky\nRetries: 2\n" + block("bash", body)
	eng := engineFor(t, dir, md)

	report, err := eng.Run(context.Background(), "flaky")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed || report.Results[0].Status != StatusDone {
		t.Errorf("flaky should succeed on retry, got %s (failed=%v)", report.Results[0].Status, report.Failed)
	}
}

func TestEngineExitCodesSkip(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### maybe\nExitCodes: 75=skip\n" + block("bash", "exit 75")
	eng := engineFor(t, dir, md)

	report, err := eng.Run(context.Background(), "maybe")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Fatalf("mapped skip should keep run green: %+v", report.Results)
	}
	r := report.Results[0]
	if r.Status != StatusConditionSkipped || r.ExitCode != 75 {
		t.Errorf("want condition-skipped/75, got %s/%d", r.Status, r.ExitCode)
	}
}

func TestEngineExitCodesRetry(t *testing.T) {
	dir := t.TempDir()
	body := "if [ -f attempt.marker ]; then exit 0; fi\ntouch attempt.marker\nexit 2"
	md := "## Tasks\n\n### flaky\nExitCodes: 2=retry\nRetries: 1\n" + block("bash", body)
	eng := engineFor(t, dir, md)

	report, err := eng.Run(context.Background(), "flaky")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed || report.Results[0].Status != StatusDone {
		t.Errorf("retry-classified exit should succeed on retry, got %s (failed=%v)",
			report.Results[0].Status, report.Failed)
	}
}

func TestSandboxArgsFromEffects(t *testing.T) {
	if got := sandboxArgs(nil); !reflect.DeepEqual(got, []string{"--network=none", "--read-only"}) {
		t.Fatalf("sandboxArgs(nil) = %v", got)
	}
	if got := sandboxArgs([]string{"net"}); !reflect.DeepEqual(got, []string{"--read-only"}) {
		t.Fatalf("sandboxArgs(net) = %v", got)
	}
	if got := sandboxArgs([]string{"write"}); !reflect.DeepEqual(got, []string{"--network=none"}) {
		t.Fatalf("sandboxArgs(write) = %v", got)
	}
	if got := sandboxArgs([]string{"net", "write"}); len(got) != 0 {
		t.Fatalf("sandboxArgs(net,write) = %v", got)
	}
}

func TestSandboxCommandArgs(t *testing.T) {
	name, args := sandboxCommandArgs("bashy podman run", []string{"read"}, "echo hi")
	if name != "bashy" {
		t.Fatalf("name = %q", name)
	}
	want := []string{"podman", "run", "--network=none", "--read-only", "bash", "-c", "echo hi"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	// DAG_SANDBOX_IMAGE pins/overrides the image.
	t.Setenv("DAG_SANDBOX_IMAGE", "docker.io/library/bash:5.2")
	_, args = sandboxCommandArgs("bashy podman run", nil, "echo hi")
	if args[len(args)-3] != "docker.io/library/bash:5.2" {
		t.Fatalf("image override not applied: %v", args)
	}
}

type recordingExecutor struct {
	hosts []string
}

func (x *recordingExecutor) Execute(ctx context.Context, t *Task, tio TaskIO) TaskResult {
	x.hosts = append(x.hosts, t.Host)
	return TaskResult{Name: t.Name, Host: t.Host, Status: StatusDone}
}

func TestEngineExecutorSeamReceivesHost(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### remote\nHost: host-a\n" + block("bash", "echo remote")
	eng := engineFor(t, dir, md)
	rec := &recordingExecutor{}
	eng.Executor = rec

	report, err := eng.Run(context.Background(), "remote")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed || report.Results[0].Host != "host-a" {
		t.Fatalf("host not carried through executor/report: %+v", report.Results)
	}
	if !reflect.DeepEqual(rec.hosts, []string{"host-a"}) {
		t.Fatalf("executor hosts = %v", rec.hosts)
	}
}

func TestEngineToolsPreflight(t *testing.T) {
	dir := t.TempDir()
	// A present tool (sh is on PATH) lets the body run.
	okEng := engineFor(t, dir, "## Tasks\n\n### needs\nTools: sh\n"+block("bash", "echo body-ran"))
	okEng.Capture = true
	r, _ := okEng.Run(context.Background(), "needs")
	if r.Results[0].Status != StatusDone || !strings.Contains(r.Results[0].Stdout, "body-ran") {
		t.Fatalf("present tool should run body: %+v", r.Results[0])
	}
	// A missing tool fails the target (exit 3) before the body runs.
	missEng := engineFor(t, dir, "## Tasks\n\n### needs\nTools: definitely-not-a-real-tool-xyz\n"+
		block("bash", "echo SHOULD-NOT-RUN"))
	missEng.Capture = true
	r2, _ := missEng.Run(context.Background(), "needs")
	if r2.Results[0].Status != StatusFailed || r2.Results[0].ExitCode != 3 {
		t.Errorf("missing tool should fail exit 3: %+v", r2.Results[0])
	}
	if strings.Contains(r2.Results[0].Stdout, "SHOULD-NOT-RUN") {
		t.Errorf("body ran despite missing tool")
	}
}

func TestEngineFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n" +
		"### boom\n" + block("bash", "exit 3") +
		"### after\nRequires: boom\n" + block("bash", "echo should-not-run")
	eng := engineFor(t, dir, md)
	eng.FailFast = false // so we observe the skipped dependent

	report, err := eng.Run(context.Background(), "after")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Failed {
		t.Fatal("want report.Failed")
	}
	byName := map[string]TaskResult{}
	for _, r := range report.Results {
		byName[r.Name] = r
	}
	if byName["boom"].Status != StatusFailed || byName["boom"].ExitCode != 3 {
		t.Errorf("boom = %+v", byName["boom"])
	}
	if byName["after"].Status != StatusSkipped {
		t.Errorf("after should be skipped, got %s", byName["after"].Status)
	}
}
