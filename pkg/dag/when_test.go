// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/qiangli/coreutils/cmds/all"
)

func whenEngine(t *testing.T, dir, md string, env []string) *Engine {
	t.Helper()
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return &Engine{Graph: g, Dir: dir, Env: env, Concurrency: 1, FailFast: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)}
}

func TestWhenConditionFalseSkips(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### deploy\nWhen: test -n \"$DEPLOY\"\n" +
		block("bash", "touch deployed")

	// DEPLOY unset → skipped, NOT failed, body did not run.
	report, err := whenEngine(t, dir, md, []string{}).Run(context.Background(), "deploy")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Errorf("condition-false skip must not fail the run: %+v", report.Results)
	}
	if report.Results[0].Status != StatusConditionSkipped {
		t.Errorf("status = %s, want condition-skipped", report.Results[0].Status)
	}
	if _, err := os.Stat(filepath.Join(dir, "deployed")); err == nil {
		t.Errorf("skipped target ran its body (deployed created)")
	}
}

func TestWhenConditionTrueRuns(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### deploy\nWhen: test -n \"$DEPLOY\"\n" +
		block("bash", "touch deployed")

	report, err := whenEngine(t, dir, md, []string{"DEPLOY=1"}).Run(context.Background(), "deploy")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed || report.Results[0].Status != StatusDone {
		t.Errorf("DEPLOY set → should run; got %s (failed=%v)", report.Results[0].Status, report.Failed)
	}
	if _, err := os.Stat(filepath.Join(dir, "deployed")); err != nil {
		t.Errorf("condition true but body did not run: %v", err)
	}
}

func TestWhenSkipDoesNotBlockDependents(t *testing.T) {
	dir := t.TempDir()
	// 'after' Requires the When-skipped 'gated'. A condition-false skip is NOT a
	// dependency failure, so 'after' still runs and the run stays green.
	md := "## Tasks\n\n" +
		"### gated\nWhen: test -n \"$DEPLOY\"\n" + block("bash", "touch gated.done") +
		"### after\nRequires: gated\n" + block("bash", "touch after.done")
	report, err := whenEngine(t, dir, md, []string{}).Run(context.Background(), "after")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Errorf("a When-skipped dependency must not fail dependents: %+v", report.Results)
	}
	byName := map[string]TaskResult{}
	for _, r := range report.Results {
		byName[r.Name] = r
	}
	if byName["gated"].Status != StatusConditionSkipped {
		t.Errorf("gated = %s, want condition-skipped", byName["gated"].Status)
	}
	if byName["after"].Status != StatusDone {
		t.Errorf("after = %s, want done (dependent of a skipped target still runs)", byName["after"].Status)
	}
}
