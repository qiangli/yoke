package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/lens"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

func TestAssignScript(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	home := t.TempDir()
	for k, v := range map[string]string{"BASHY_HOME": home, "BASHY_SPRINT_DIR": filepath.Join(home, "sprint"), "BASHY_ROOM_DIR": filepath.Join(home, "room")} {
		t.Setenv(k, v)
	}
	if out, code := runSprint(t, "add", "script fixture", "--announce=false"); code != 0 {
		t.Fatal(out)
	}
	story, err := todopkg.Add(todopkg.RepoStore(root), "script", "prose must not reach worker", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	story.Assignee = "existing-holder"
	if _, err = todopkg.RepoStore(root).Save(story); err != nil {
		t.Fatal(err)
	}
	script := []byte("echo assignment-proof\n")
	expectedState, expectedExit := "no-op", 0
	if os.Getenv("S446_SCRIPT_FAIL") == "1" {
		script = append(script, []byte("false\n")...)
		expectedState, expectedExit = "failed", 1
	}
	task := lens.ScriptTask{Schema: "lens-script-task-v1", Artifact: lens.CompiledScriptArtifact{Name: lens.ScriptArtifactName(script), Digest: lens.ScriptArtifactDigest(script), Script: script}, Caps: []string{"read-only"}, Effects: []string{"pure"}, Help: "script", NoInput: true}
	body, _ := json.MarshalIndent(task, "", "  ")
	body = append(body, '\n')
	file := filepath.Join(home, "task.json")
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	live := os.Getenv("S446_SCRIPT_CHILD") == "1"
	if live {
		runner = os.Getenv("LENS_TEST_YOKE")
	}
	launches := 0
	original := sprintAssignLaunchWorker
	t.Cleanup(func() { sprintAssignLaunchWorker = original })
	sprintAssignLaunchWorker = func(cmd *cobra.Command, r sprintAssignLaunch) error {
		launches++
		dir, _ := weaveQueueDir(root)
		q, err := loadWeaveQueue(dir)
		if err != nil {
			t.Fatal(err)
		}
		item := findWeaveItem(q, r.Run)
		if item == nil || item.Body != string(body) || item.Register != story.ID {
			t.Fatalf("body/link mismatch: %+v", item)
		}
		if r.Agent != "" || len(r.Env) != 0 || r.PTY != "always" || r.MaxRuntime != time.Minute || strings.Join(r.Argv, " ") != strings.Join(append([]string{runner}, task.WorkerArgs()...), " ") {
			t.Fatalf("launch: %+v", r)
		}
		if live {
			return original(cmd, r)
		}
		return nil
	}
	invoke := func(dry bool) error {
		cmd := newSprintAssignCmd()
		args := []string{"1", story.ID, "--repo", root, "--script-task", file, "--script-runner", runner}
		if dry {
			args = append(args, "--dry-run")
		}
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		return cmd.ExecuteContext(t.Context())
	}
	// Invalid bodies must fail before allocation, even in dry-run.
	invalid := []string{`{}`, string(body) + `{}`, strings.Replace(string(body), `"no_input": true`, `"no_input": false`, 1), strings.Replace(string(body), task.Artifact.Digest, "sha256:bad", 1)}
	for _, source := range []string{"go build ./...", "go test ./...", "printf release > artifact-v1.txt", "sh -c true", "printf escape > ../outside", "printf escape > /tmp/outside", "curl https://example.invalid", "bashy model list"} {
		bad := task
		b := []byte(source + "\n")
		bad.Artifact = lens.CompiledScriptArtifact{Name: lens.ScriptArtifactName(b), Digest: lens.ScriptArtifactDigest(b), Script: b}
		encoded, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		invalid = append(invalid, string(encoded))
	}
	for _, missingCaps := range []bool{false, true} {
		bad := task
		if missingCaps {
			bad.Caps = nil
		} else {
			bad.Effects = nil
		}
		encoded, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		invalid = append(invalid, string(encoded))
	}
	for _, bad := range invalid {
		if err := os.WriteFile(file, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		for _, dry := range []bool{true, false} {
			if err := invoke(dry); err == nil {
				t.Fatal("accepted invalid task")
			}
		}
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Items) != 0 || launches != 0 {
		t.Fatal("invalid task allocated or dispatched a run")
	}
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := invoke(true); err != nil {
		t.Fatal(err)
	}
	loaded, err := todopkg.RepoStore(root).Resolve(story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if launches != 0 || loaded.Weave != 0 {
		t.Fatal("validation/dry-run mutated assignment")
	}
	if err := invoke(false); err != nil && expectedExit == 0 {
		t.Fatal(err)
	}
	if launches != 1 {
		t.Fatal(launches)
	}
	loaded, err = todopkg.RepoStore(root).Resolve(story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Weave == 0 || loaded.Assignee != story.Assignee {
		t.Fatal("link or holder changed")
	}
	if err := invoke(false); err == nil {
		t.Fatal("duplicate assignment accepted")
	}
	if live {
		dir, _ := weaveQueueDir(root)
		q, err := loadWeaveQueue(dir)
		if err != nil {
			t.Fatal(err)
		}
		item := findWeaveItem(q, loaded.Weave)
		if item.State != expectedState || item.ExitCode == nil || *item.ExitCode != expectedExit || item.FinishedAt.IsZero() {
			t.Fatalf("durable outcome: %+v", item)
		}
		data, err := os.ReadFile(item.LogPath)
		if err != nil || !bytes.Contains(data, []byte("assignment-proof")) {
			t.Fatalf("log: %q %v", data, err)
		}
		cursor := filepath.Join(home, "cursor")
		command := exec.CommandContext(t.Context(), runner, "lens", "--since", "start", "--cursor-out", cursor, "-t", "jsonl", "run:"+filepath.Base(root)+"-1")
		data, err = command.CombinedOutput()
		if err != nil {
			t.Fatalf("delta: %s %v", data, err)
		}
		if !bytes.Contains(data, []byte(`"type":"lifecycle"`)) || !bytes.Contains(data, []byte(`"state":"`+expectedState+`"`)) || !bytes.Contains(data, []byte("assignment-proof")) {
			t.Fatalf("missing supervision: %s", data)
		}
		command = exec.CommandContext(t.Context(), runner, "lens", "--since", "@"+cursor, "--cursor-out", cursor, "-t", "jsonl", "run:"+filepath.Base(root)+"-1")
		again, e := command.CombinedOutput()
		if e != nil || len(again) != 0 {
			t.Fatalf("cursor replay: %q %v", again, e)
		}
		if len(item.WorkerStories) != 0 {
			t.Fatal("script claimed story")
		}
		t.Logf("durable body verified; delta: %s", data)
	}
}

// Run the current command implementation and actual worker in a separate process
// with no live credentials, agent identity, bus configuration or production queues.
func TestAssignScriptIntegration(t *testing.T) {
	yoke := os.Getenv("LENS_TEST_YOKE")
	if yoke == "" {
		t.Skip("set LENS_TEST_YOKE to built Yoke")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"0", "1"} {
		home := t.TempDir()
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestAssignScript$", "-test.v")
		cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=/usr/bin:/bin:/opt/homebrew/bin", "GIT_CONFIG_NOSYSTEM=1", "TERM=dumb", "S446_SCRIPT_CHILD=1", "S446_SCRIPT_FAIL=" + failure, "LENS_TEST_YOKE=" + yoke}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("current assignment integration: %v\n%s", err, out)
		}
		if !bytes.Contains(out, []byte("durable body verified")) {
			t.Fatalf("missing verdict: %s", out)
		}
		t.Log(string(out))
	}
}

func TestScriptAssignmentContract(t *testing.T) {
	b := []byte("echo contract\n")
	task := lens.ScriptTask{Schema: "lens-script-task-v1", Artifact: lens.CompiledScriptArtifact{Name: lens.ScriptArtifactName(b), Digest: lens.ScriptArtifactDigest(b), Script: b}, Caps: []string{"read-only"}, Effects: []string{"pure"}, Help: "script", NoInput: true}
	body, _ := json.Marshal(task)
	for _, change := range []func(*weaveItem){
		func(i *weaveItem) { i.Body += " " },
		func(i *weaveItem) { i.ScriptAssignment.Digest = "wrong" },
		func(i *weaveItem) { i.LaunchSpec.Tool = "other" },
		func(i *weaveItem) { i.LaunchSpec.Argv = []string{"lens-script"} },
		func(i *weaveItem) { i.LaunchSpec.Agent = "model-agent" },
		func(i *weaveItem) { i.LaunchSpec = nil },
	} {
		item := &weaveItem{Body: string(body), ScriptAssignment: &weaveScriptAssignment{Body: string(body), Digest: task.Artifact.Digest, Runner: "/trusted/yoke"}, LaunchSpec: &weaveLaunchSpec{Tool: "/trusted/yoke", Argv: task.WorkerArgs()}}
		if err := weaveClaimWorkerStories(&cobra.Command{}, "unused", item); err != nil {
			t.Fatal(err)
		}
		change(item)
		if err := weaveClaimWorkerStories(&cobra.Command{}, "unused", item); err == nil {
			t.Fatal("mutated launch accepted")
		}
	}
}
