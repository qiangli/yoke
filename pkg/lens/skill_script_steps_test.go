package lens

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/dag"
)

func TestScriptStepsRecoveryAndPromotion(t *testing.T) {
	skill := func(s string) []byte { return []byte("```sh\n" + s + "\n```\n") }
	local := &LocalScriptRunner{AllowedCaps: []string{"read-only"}, AllowedEffects: []string{"pure"}, WorkDir: t.TempDir()}
	var calls []string
	seat := WorkerSeat{Name: "local", Help: "script", NoInput: true, AllowedCaps: local.AllowedCaps, AllowedEffects: local.AllowedEffects, Runner: WorkerRunnerFunc(func(ctx context.Context, a *CompiledScriptArtifact) (*ExecutionResult, error) {
		calls = append(calls, string(a.Script))
		return local.RunScript(ctx, a)
	})}
	steps := []ScriptStep{{Name: "before", Skill: skill("echo before")}, {Name: "repair", Skill: skill("false"), Retries: 1, ReplacementSkill: skill("echo repaired")}, {Name: "after", Skill: skill("echo after")}}
	attempts, err := ExecuteScriptSteps(t.Context(), steps, PipelineOptions{StoreDir: t.TempDir()}, seat)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 5 || strings.Join(calls, "") != "echo before\nfalse\nfalse\necho repaired\necho after\n" || !attempts[3].Recompiled {
		t.Fatalf("attempts %#v calls %q", attempts, calls)
	}
	if attempts[1].Result.Artifact.Digest != attempts[2].Result.Artifact.Digest || attempts[2].Result.Artifact.Digest == attempts[3].Result.Artifact.Digest {
		t.Fatal("retry/recompile identity")
	}
	path := filepath.Join(t.TempDir(), "dag.md")
	if _, err := attempts[1].Result.PublishDAG(path, "repaired"); err == nil {
		t.Fatal("failed promotion")
	}
	result := attempts[3].Result
	if _, err := result.PublishDAG(path, "repaired"); err != nil {
		t.Fatal(err)
	}
	// Execute the published file with the real DAG parser/runner.
	cmd := dag.NewDagCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{path, "repaired"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("DAG: %v %s", err, stderr.String())
	}
	if !strings.Contains(out.String(), "repaired") {
		t.Fatalf("DAG output %q", out.String())
	}
	original, _ := os.ReadFile(path)
	if _, err := result.PublishDAG(path, "repaired"); err == nil {
		t.Fatal("overwrote DAG")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) {
		t.Fatal("changed existing DAG")
	}
	result.Artifact.Script = []byte("echo mutated\n")
	if _, err := result.PublishDAG(filepath.Join(t.TempDir(), "dag.md"), "repaired"); err == nil {
		t.Fatal("mutated promotion")
	}
	calls = nil
	steps[1].ReplacementSkill = nil
	attempts, err = ExecuteScriptSteps(t.Context(), steps, PipelineOptions{}, seat)
	if err == nil || len(attempts) != 3 || strings.Contains(strings.Join(calls, ""), "after") {
		t.Fatalf("failure did not stop: %v %q", err, calls)
	}
	steps[1].ReplacementSkill = skill("echo $(pwd)")
	calls = nil
	if _, err = ExecuteScriptSteps(t.Context(), steps, PipelineOptions{}, seat); err == nil || len(calls) != 3 {
		t.Fatalf("unsafe replacement reached worker: %v %q", err, calls)
	}
}

func TestScriptStepsFailClosed(t *testing.T) {
	calls := 0
	seat := WorkerSeat{Name: "test", Help: "script", NoInput: true, AllowedCaps: []string{"read-only"}, AllowedEffects: []string{"pure"}, Runner: WorkerRunnerFunc(func(context.Context, *CompiledScriptArtifact) (*ExecutionResult, error) { calls++; return nil, nil })}
	step := ScriptStep{Name: "one", Skill: []byte("```sh\ntrue\n```\n"), Retries: 3}
	attempts, err := ExecuteScriptSteps(t.Context(), []ScriptStep{step}, PipelineOptions{}, seat)
	if err == nil || calls != 1 || len(attempts) != 1 {
		t.Fatalf("missing outcome retried: %d %v", calls, err)
	}
	calls = 0
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = ExecuteScriptSteps(ctx, []ScriptStep{step}, PipelineOptions{}, seat); err == nil || calls != 0 {
		t.Fatal("cancelled work executed")
	}
	step.Retries = 4
	if _, err = ExecuteScriptSteps(t.Context(), []ScriptStep{step}, PipelineOptions{}, seat); err == nil || calls != 0 {
		t.Fatal("unbounded retry accepted")
	}
	forged := &DelegationResult{Executed: true, Success: true, Artifact: &CompiledScriptArtifact{Script: []byte("true\n")}}
	if _, err = forged.PublishDAG(filepath.Join(t.TempDir(), "dag.md"), "verified"); err == nil {
		t.Fatal("unverified result promoted")
	}
}
