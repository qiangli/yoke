package lenscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens"
)

func workerTask(script string) scriptTask {
	b := []byte(script)
	return scriptTask{Schema: "lens-script-task-v1", Artifact: lens.CompiledScriptArtifact{Name: lens.ScriptArtifactName(b), Digest: lens.ScriptArtifactDigest(b), Script: b}, Caps: []string{"read-only"}, Effects: []string{"pure"}, Help: "script", NoInput: true}
}

func TestScriptWorkerContract(t *testing.T) {
	for _, name := range []string{"success", "nonzero", "tamper", "policy", "missing", "metadata", "unsafe", "trailing"} {
		t.Run(name, func(t *testing.T) {
			task := workerTask("echo local\n")
			want := 0
			switch name {
			case "nonzero":
				task = workerTask("echo local\nfalse\n")
				want = 1
			case "tamper":
				task.Artifact.Script = []byte("echo changed\n")
				want = lens.ExitInvalid
			case "policy":
				task.Effects = []string{"write"}
				want = lens.ExitInvalid
			case "missing":
				task.Caps = nil
				want = lens.ExitInvalid
			case "metadata":
				task.NoInput = false
				want = lens.ExitInvalid
			case "unsafe":
				task = workerTask("echo $(curl example.com)\n")
				want = lens.ExitInvalid
			case "trailing":
				want = lens.ExitInvalid
			}
			body, _ := json.Marshal(task)
			if name == "trailing" {
				body = append(body, []byte("{}")...)
			}
			var out, stderr bytes.Buffer
			rc := &tool.RunContext{Ctx: t.Context(), Dir: t.TempDir(), Env: []string{"WEAVE_ISSUE_BODY=" + string(body)}, Stdio: tool.Stdio{Out: &out, Err: &stderr}}
			code := runScriptWorker(rc, []string{"--digest", task.Artifact.Digest, "--caps", "read-only", "--effects", "pure"})
			if code != want {
				t.Fatalf("code %d want %d: %s", code, want, stderr.String())
			}
			if name == "success" || name == "nonzero" {
				if out.String() != "local\n" {
					t.Fatalf("output %q", out.String())
				}
			} else if out.Len() != 0 {
				t.Fatalf("rejected task executed: %q", out.String())
			}
		})
	}
}

// This opt-in gate drives the installed Bashy wrapper and built Yoke entry.
// Only supported CLI verbs write the isolated queue; readback proves outcomes.
func TestScriptWorkerWeaveIntegration(t *testing.T) {
	bashy := os.Getenv("LENS_TEST_BASHY")
	if bashy == "" {
		t.Skip("set LENS_TEST_BASHY to an absolute installed Bashy executable")
	}
	if !filepath.IsAbs(bashy) {
		t.Fatal("LENS_TEST_BASHY must be absolute")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	yoke := os.Getenv("LENS_TEST_YOKE")
	if yoke == "" {
		t.Skip("set LENS_TEST_YOKE to a built Yoke executable")
	}
	if !filepath.IsAbs(yoke) {
		t.Fatal("LENS_TEST_YOKE must be absolute")
	}
	workerArgv := []string{yoke, "lens-script"}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + root, "USERPROFILE=" + root, "BASHY_HOME=" + filepath.Join(root, "state"), "PATH=" + filepath.Dir(git) + string(os.PathListSeparator) + "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "TERM=dumb"}
	invoke := func(binary string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = repo
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", binary, args, err, out)
		}
		return out
	}
	invoke(git, "init", "-b", "main")
	invoke(git, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	i := -1
	var digests []string
	runner := lens.WorkerRunnerFunc(func(ctx context.Context, artifact *lens.CompiledScriptArtifact) (*lens.ExecutionResult, error) {
		i++
		script := string(artifact.Script)
		digests = append(digests, artifact.Digest)
		task := workerTask(script)
		body, _ := json.Marshal(task)
		invoke(bashy, "weave", "add", "bounded script", "--body", string(body))
		id := strconv.Itoa(i + 1)
		expectedState, expectedExit := "no-op", 0
		if i == 1 || i == 2 {
			expectedState, expectedExit = "failed", 1
		}
		cursor := filepath.Join(root, "cursor-"+id)
		// weave start may itself report a nonzero code for the failed child;
		// the durable readback below, not its shell status, is the verdict.
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		argv := append([]string{"weave", "start", "--run", id, "--pty", "always", "--"}, workerArgv...)
		argv = append(argv, "--digest", task.Artifact.Digest, "--caps", "read-only", "--effects", "pure")
		cmd := exec.CommandContext(ctx, bashy, argv...)
		cmd.Dir = repo
		cmd.Env = env
		out, _ := cmd.CombinedOutput()
		cancel()
		t.Logf("start %s: %s", id, out)
		status := invoke(bashy, "weave", "status", id, "--json")
		t.Logf("status %s: %s", id, status)
		delta := invoke(yoke, "lens", "--since", "start", "--cursor-out", cursor, "-t", "jsonl", "run:repo-"+id)
		marker := strings.TrimPrefix(strings.Split(script, "\n")[0], "echo ")
		if !bytes.Contains(delta, []byte(`"evidence":"persisted-log"`)) || !bytes.Contains(delta, []byte(marker)) {
			t.Fatalf("captured output missing from run delta: %s", delta)
		}
		var lifecycleCount int
		for _, line := range bytes.Split(delta, []byte("\n")) {
			var row struct {
				Type  string `json:"type"`
				Attrs struct {
					State      string    `json:"state"`
					ExitCode   *int      `json:"exit_code"`
					FinishedAt time.Time `json:"finished_at"`
				} `json:"attrs"`
			}
			if len(line) == 0 {
				continue
			}
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			if row.Type == "lifecycle" {
				lifecycleCount++
				if row.Attrs.State != expectedState || row.Attrs.ExitCode == nil || *row.Attrs.ExitCode != expectedExit || row.Attrs.FinishedAt.IsZero() {
					t.Fatalf("bad lifecycle: %s", line)
				}
			}
		}
		if lifecycleCount != 1 {
			t.Fatalf("want one lifecycle row: %s", delta)
		}
		if again := invoke(yoke, "lens", "--since", "@"+cursor, "--cursor-out", cursor, "-t", "jsonl", "run:repo-"+id); len(again) != 0 {
			t.Fatalf("unchanged run replayed evidence: %s", again)
		}

		// Assert exact durable fields after the wrapper has finished.
		var found bool
		filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || entry.Name() != "queue.json" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var q struct {
				Items []struct {
					ID         int    `json:"id"`
					State      string `json:"state"`
					ExitCode   *int   `json:"exit_code"`
					FinishedAt string `json:"finished_at"`
					Body       string `json:"body"`
					LogPath    string `json:"log_path"`
				} `json:"items"`
			}
			if err := json.Unmarshal(data, &q); err != nil {
				t.Fatal(err)
			}
			for _, item := range q.Items {
				if item.ID == i+1 {
					found = true
					if item.State != expectedState || item.ExitCode == nil || *item.ExitCode != expectedExit || item.FinishedAt == "" || item.Body != string(body) {
						t.Fatalf("incorrect durable outcome: %+v", item)
					}
					log, err := os.ReadFile(item.LogPath)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(log), strings.TrimPrefix(strings.Split(script, "\n")[0], "echo ")) {
						t.Fatalf("missing captured output: %q", log)
					}
				}
			}
			return nil
		})
		if !found {
			t.Fatal("no durable run found")
		}
		return &lens.ExecutionResult{ExitCode: expectedExit}, nil
	})
	skill := func(s string) []byte { return []byte("```sh\n" + s + "\n```\n") }
	attempts, err := lens.ExecuteScriptSteps(t.Context(), []lens.ScriptStep{
		{Name: "success", Skill: skill("echo weave-local-success")},
		{Name: "repair", Skill: skill("echo weave-local-failure\nfalse"), Retries: 1, ReplacementSkill: skill("echo weave-local-recompiled")},
	}, lens.PipelineOptions{StoreDir: filepath.Join(root, "artifacts")}, lens.WorkerSeat{
		Name: "native-weave", Help: "script", NoInput: true, AllowedCaps: []string{"read-only"}, AllowedEffects: []string{"pure"}, Runner: runner,
	})
	if err != nil || len(attempts) != 4 {
		t.Fatalf("native recovery: %d attempts, %v", len(attempts), err)
	}
	if digests[1] != digests[2] || digests[2] == digests[3] || !attempts[3].Recompiled {
		t.Fatal("native retry/recompile identity")
	}
	promoted := filepath.Join(root, "verified-dag.md")
	if _, err := attempts[1].Result.PublishDAG(promoted, "verified"); err == nil {
		t.Fatal("native failure promoted")
	}
	if _, err := attempts[3].Result.PublishDAG(promoted, "verified"); err != nil {
		t.Fatal(err)
	}
	if out := invoke(bashy, "dag", promoted, "verified"); !bytes.Contains(out, []byte("weave-local-recompiled")) {
		t.Fatalf("promoted DAG did not execute: %s", out)
	}
}
