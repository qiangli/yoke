package lens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
)

func localArtifact(script string) *CompiledScriptArtifact {
	b := []byte(script)
	return &CompiledScriptArtifact{Name: ScriptArtifactName(b), Digest: ScriptArtifactDigest(b), Script: b}
}

func localRunner() *LocalScriptRunner {
	return &LocalScriptRunner{AllowedCaps: []string{atlas.CapReadOnly}, AllowedEffects: []string{atlas.EffPure, atlas.EffRead}}
}

func TestLocalScriptBoundary(t *testing.T) {
	// No executable on PATH: pure-output execution must remain in process.
	t.Setenv("PATH", t.TempDir())
	r := localRunner()
	for _, tt := range []struct {
		script, out string
		code        int
	}{
		{"printf '%s\\n' 'hello local'\n", "hello local\n", 0},
		{"echo before\nfalse\n", "before\n", 1},
	} {
		res, err := r.RunScript(context.Background(), localArtifact(tt.script))
		if err != nil || res == nil || res.ExitCode != tt.code || string(res.Stdout) != tt.out || res.Recording != nil {
			t.Fatalf("script %q: result=%+v err=%v", tt.script, res, err)
		}
	}
	// Previously missed AST effects cannot reach execution, even with a valid digest.
	sentinel := filepath.Join(t.TempDir(), "must-not-exist")
	for _, script := range []string{
		"echo unsafe > " + sentinel, "echo $(touch " + sentinel + ")",
		"if touch " + sentinel + "; then echo unsafe; fi", "bashy llm 'hello'", "${COMMAND}",
	} {
		if res, err := r.RunScript(context.Background(), localArtifact(script)); err == nil || res != nil {
			t.Fatalf("unsafe script admitted: %q result=%+v err=%v", script, res, err)
		}
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("script escaped validator: %v", err)
	}
	art := localArtifact("echo original\n")
	art.Script = []byte("echo tampered\n")
	if _, err := r.RunScript(context.Background(), art); err == nil {
		t.Fatal("tampered artifact admitted")
	}
	for _, mutate := range []func(*LocalScriptRunner){
		func(r *LocalScriptRunner) { r.AllowedCaps = nil },
		func(r *LocalScriptRunner) { r.AllowedCaps = []string{} },
		func(r *LocalScriptRunner) { r.AllowedEffects = nil },
		func(r *LocalScriptRunner) { r.AllowedEffects = []string{} },
		func(r *LocalScriptRunner) { r.DeniedCaps = []string{atlas.CapReadOnly} },
		func(r *LocalScriptRunner) { r.AllowedEffects = []string{atlas.EffWrite} },
	} {
		runner := localRunner()
		mutate(runner)
		if res, err := runner.RunScript(context.Background(), localArtifact("echo denied\n")); err == nil || res != nil {
			t.Fatalf("invalid policy admitted: result=%+v err=%v", res, err)
		}
	}
}

func TestLocalWorkerDurableOutcome(t *testing.T) {
	for _, script := range []string{"echo complete\n", "echo before-failure\nfalse\n"} {
		t.Run(script, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			calls := 0
			worker := &LocalWeaveWorker{
				AllowedCaps: []string{atlas.CapReadOnly}, AllowedEffects: []string{atlas.EffPure, atlas.EffRead},
				RecordFunc: func(ctx context.Context, id int64, res *ExecutionResult) (*ScriptRecording, error) {
					calls++
					// Exercise a real durable test store, not a fake production queue API.
					data, err := json.Marshal(struct {
						Exit           int
						Stdout, Stderr []byte
					}{res.ExitCode, res.Stdout, res.Stderr})
					if err != nil {
						return nil, err
					}
					f, err := os.Create(path)
					if err != nil {
						return nil, err
					}
					_, err = f.Write(data)
					if err == nil {
						err = f.Sync()
					}
					closeErr := f.Close()
					if err == nil {
						err = closeErr
					}
					if err != nil {
						return nil, err
					}
					return &ScriptRecording{RunID: id, Ref: path}, nil
				},
			}
			art := localArtifact(script)
			res, err := worker.ExecuteTask(context.Background(), WeaveReceivingSeatTask{RunID: 42, Help: "script", NoInput: true, ArtifactName: art.Name, Digest: art.Digest, Script: art.Script})
			if err != nil || res == nil || res.Recording == nil || res.Recording.Ref != path || calls != 1 {
				t.Fatalf("result=%+v err=%v calls=%d", res, err, calls)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Exit           int
				Stdout, Stderr []byte
			}
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Exit != res.ExitCode || !bytes.Equal(saved.Stdout, res.Stdout) || !bytes.Equal(saved.Stderr, res.Stderr) {
				t.Fatalf("durable outcome differs: %+v vs %+v", saved, res)
			}
			if script == "echo before-failure\nfalse\n" && saved.Exit != 1 {
				t.Fatal("failed execution stored as success")
			}
		})
	}
}

func TestRecordResultFailsClosed(t *testing.T) {
	ctx := context.Background()
	storeErr := errors.New("store full")
	for _, tt := range []struct {
		name   string
		worker *LocalWeaveWorker
		id     int64
	}{
		{"nil worker", nil, 1}, {"nil recorder", &LocalWeaveWorker{}, 1},
		{"store error", &LocalWeaveWorker{RecordFunc: func(context.Context, int64, *ExecutionResult) (*ScriptRecording, error) { return nil, storeErr }}, 1},
		{"nil receipt", &LocalWeaveWorker{RecordFunc: func(context.Context, int64, *ExecutionResult) (*ScriptRecording, error) { return nil, nil }}, 1},
		{"wrong run", &LocalWeaveWorker{RecordFunc: func(context.Context, int64, *ExecutionResult) (*ScriptRecording, error) {
			return &ScriptRecording{RunID: 9, Ref: "record"}, nil
		}}, 1},
		{"empty ref", &LocalWeaveWorker{RecordFunc: func(context.Context, int64, *ExecutionResult) (*ScriptRecording, error) {
			return &ScriptRecording{RunID: 1}, nil
		}}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := &ExecutionResult{ExitCode: 7, Stdout: []byte("actual stdout"), Stderr: []byte("actual stderr"), Recording: &ScriptRecording{RunID: 100, Ref: "stale"}}
			err := tt.worker.RecordResult(ctx, tt.id, res)
			if err == nil || res.Recording != nil || res.ExitCode != 7 || string(res.Stdout) != "actual stdout" || string(res.Stderr) != "actual stderr" {
				t.Fatalf("result=%+v err=%v", res, err)
			}
			if tt.name == "store error" && !errors.Is(err, storeErr) {
				t.Fatal("lost recorder error")
			}
		})
	}
	if res, err := (&LocalWeaveWorker{}).ExecuteTask(ctx, WeaveReceivingSeatTask{RunID: 1}); err == nil || res != nil {
		t.Fatal("missing recorder must fail before execution")
	}
}

func TestDelegationRequiresOutcomeAndPolicy(t *testing.T) {
	plan, err := PlanPipeline([]byte("```sh\necho hello\n```\n"), PipelineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	seat := WorkerSeat{Help: "script", NoInput: true, Runner: WorkerRunnerFunc(func(context.Context, *CompiledScriptArtifact) (*ExecutionResult, error) { calls++; return nil, nil })}
	if _, err := plan.Delegate(context.Background(), seat); err == nil || calls != 0 {
		t.Fatal("missing policies did not fail before worker")
	}
	seat.AllowedCaps = []string{atlas.CapReadOnly}
	seat.AllowedEffects = []string{atlas.EffPure, atlas.EffRead}
	res, err := plan.Delegate(context.Background(), seat)
	if err == nil || res.Success || res.CanRunNext() || calls != 1 {
		t.Fatalf("nil outcome accepted: %+v %v", res, err)
	}
	seat.Runner = nil
	res, err = plan.Delegate(context.Background(), seat)
	if err != nil || res.CanRunNext() {
		t.Fatalf("planning counted as execution: %+v %v", res, err)
	}
	if _, err := res.Promote("dag", "pending"); err == nil {
		t.Fatal("unexecuted plan promoted")
	}
}

func TestDelegationPreservesOutcomeOnRecordingFailure(t *testing.T) {
	plan, err := PlanPipeline([]byte("```sh\necho hello\n```\n"), PipelineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recordErr := errors.New("recording failed")
	res, err := plan.Delegate(context.Background(), WorkerSeat{
		Help: "script", NoInput: true, AllowedCaps: []string{atlas.CapReadOnly}, AllowedEffects: []string{atlas.EffPure, atlas.EffRead},
		Runner: WorkerRunnerFunc(func(context.Context, *CompiledScriptArtifact) (*ExecutionResult, error) {
			return &ExecutionResult{ExitCode: 7, Stdout: []byte("partial output"), Stderr: []byte("worker error")}, recordErr
		}),
	})
	if !errors.Is(err, recordErr) || res.ExitCode != 7 || string(res.Stdout) != "partial output" || string(res.Stderr) != "worker error" || res.Success || res.Recording != nil || res.CanRunNext() {
		t.Fatalf("lost actual outcome: %+v err=%v", res, err)
	}
}

func TestRecordingRejectsInvalidInputBeforeStore(t *testing.T) {
	calls := 0
	worker := &LocalWeaveWorker{RecordFunc: func(context.Context, int64, *ExecutionResult) (*ScriptRecording, error) { calls++; return nil, nil }}
	if err := worker.RecordResult(context.Background(), 1, nil); err == nil {
		t.Fatal("nil result accepted")
	}
	if err := worker.RecordResult(context.Background(), 0, &ExecutionResult{}); err == nil {
		t.Fatal("invalid run accepted")
	}
	if calls != 0 {
		t.Fatal("invalid input reached recorder")
	}
}
