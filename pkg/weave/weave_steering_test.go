package weave

import (
	"fmt"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// A deterministic worker captures the actual prompt delivered through resume.
// No provider, subscription or metered API is involved.
func TestSteeringReceiptChild(t *testing.T) {
	if os.Getenv("WEAVE_STEERING_CHILD") != "1" {
		return
	}
	if os.Getenv("WEAVE_STEERING_WAIT") == "1" {
		if err := os.WriteFile(".git/steering-ready", nil, 0600); err != nil {
			panic(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(".git/steering-release"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				panic("steering fixture release timeout")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.WriteFile("steering-receipt.txt", []byte(os.Args[len(os.Args)-1]), 0600); err != nil {
		panic(err)
	}
	if strings.Contains(os.Args[len(os.Args)-1], "STEERING-2370:") {
		if err := os.WriteFile("correction.txt", []byte("incorporated steering 2370\n"), 0600); err != nil {
			panic(err)
		}
		repo, err := gogit.PlainOpen(".")
		if err != nil {
			panic(err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			panic(err)
		}
		if _, err = wt.Add("steering-receipt.txt"); err != nil {
			panic(err)
		}
		if _, err = wt.Add("correction.txt"); err != nil {
			panic(err)
		}
		if _, err = wt.Commit("Incorporate received steering", &gogit.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.invalid", When: time.Now()}}); err != nil {
			panic(err)
		}
	}
	os.Exit(0)
}

func TestResumeSteeringReceipt(t *testing.T) {
	for _, channel := range []string{"comment", "say", "mb"} {
		t.Run(channel, func(t *testing.T) {
			root, dir, it := workerStoryFixture(t)
			t.Chdir(root)
			t.Setenv("BASHY_ROOM_DIR", t.TempDir())
			t.Setenv("BASHY_MB_DIR", t.TempDir())
			t.Setenv(fleet.InstanceDirEnv, t.TempDir())
			t.Setenv("BASHY_INSTANCE", "")
			t.Setenv("BASHY_INSTANCE_UUID", "")
			cat := pinAgentFleet(t)
			t.Setenv("BASHY_FLEET_DIR", cat.Root())
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := cat.SaveTool(fleet.Tool{Name: "steering-fixture", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: exe, Launch: fleet.ToolLaunch{Exec: fmt.Sprintf("%q -test.run=^TestSteeringReceiptChild$ -- {model} {prompt}", exe)}}}); err != nil {
				t.Fatal(err)
			}
			if err := cat.SaveAgent(fleet.Agent{Name: "steering-worker", Tool: "steering-fixture", Model: "fable"}); err != nil {
				t.Fatal(err)
			}
			family, _, err := cat.FamilyOf("steering-worker")
			if err != nil {
				t.Fatal(err)
			}
			inst, err := fleet.NewInstanceStore("").Open(family, fleet.OpenOptions{Label: "Lucia"})
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(it.Workspace, "unique.txt"), "prior unique work\n")
			gitT(t, it.Workspace, "add", "unique.txt")
			gitT(t, it.Workspace, "commit", "-qm", "preserved prior work")
			priorHead := strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
			it.Instance = inst.UUID
			it.State = "submitted"
			it.Body = "Original task: preserve unique work."
			it.LaunchSpec = nil
			if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
				t.Fatal(err)
			}
			marker := "STEERING-2370: incorporate the requested correction before resubmitting"
			var out string
			var code int
			if channel == "comment" {
				out, code = runWeave(t, "comment", "1", "--kind", "review", marker)
				if code != 0 {
					t.Fatalf("comment: %d %s", code, out)
				}
			} else if channel == "say" {
				out, code = runWeave(t, "say", "1", marker)
				if code != 0 {
					t.Fatalf("say: %d %s", code, out)
				}
			} else {
				res, err := bus.Send(bus.SendRequest{From: "manager", To: "steering-worker", Topic: "mb", Body: marker})
				if err != nil {
					t.Fatal(err)
				}
				posts, err := bus.Posts()
				if err != nil {
					t.Fatal(err)
				}
				if len(posts) != 1 || posts[0].To != inst.MailAddress() {
					t.Fatalf("wrong mailbox: %+v", posts)
				}
				t.Logf("clone resolved to recorded UUID %s (label %s), receipt %+v", inst.UUID, inst.Label, res)
			}
			if _, err := bus.PostMessageSeq(bus.Post{From: "other", To: "foreign-worker", Body: "FOREIGN-DO-NOT-INJECT"}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WEAVE_STEERING_CHILD", "1")
			out, code = runWeave(t, "start", "--run", "1", "--resume", "--pty", "never", "--mem-limit", "0", "--json", "--", "steering-worker")
			receipt, err := os.ReadFile(filepath.Join(it.Workspace, "steering-receipt.txt"))
			if err != nil {
				t.Fatalf("child never received prompt: %v; exit=%d %s", err, code, out)
			}
			if !strings.Contains(string(receipt), marker) {
				t.Fatalf("resumed child received original prompt without steering:\n%s", receipt)
			}
			if !strings.Contains(string(receipt), it.Body) {
				t.Fatal("original task lost")
			}
			if strings.Contains(string(receipt), "FOREIGN-DO-NOT-INJECT") {
				t.Fatal("foreign mail leaked into prompt")
			}
			if _, ok := bus.CursorSeq(inst.UUID); ok {
				t.Fatal("argv assembly falsely acknowledged mailbox receipt")
			}
			if code != 0 {
				t.Fatalf("resume failed: %d %s", code, out)
			}
			currentHead := strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
			if currentHead == priorHead {
				t.Fatal("resumed worker resubmitted unchanged HEAD")
			}
			correction, err := os.ReadFile(filepath.Join(it.Workspace, "correction.txt"))
			if err != nil || string(correction) != "incorporated steering 2370\n" {
				t.Fatalf("steering not incorporated: %q %v", correction, err)
			}
			unique, err := os.ReadFile(filepath.Join(it.Workspace, "unique.txt"))
			if err != nil || string(unique) != "prior unique work\n" {
				t.Fatal("unique work lost")
			}
			t.Logf("resumed worker incorporated steering: HEAD %s -> %s", priorHead, currentHead)
			t.Logf("actual resumed child receipt: %s", receipt)
		})
	}
}

func TestSaySteeringSurvivesStoppedRun(t *testing.T) {
	root, dir, it := workerStoryFixture(t)
	t.Chdir(root)
	it.State, it.WrapperPid = "submitted", 0
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "say", "1", "late steering for resume")
	if code != 0 {
		t.Fatalf("late steering must queue durably: %d %s", code, out)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range q.Items[0].Comments {
		found = found || c.Body == "late steering for resume"
	}
	if !found {
		t.Fatal("say lost steering from the durable run thread")
	}
	if strings.Contains(out, "sent to") {
		t.Fatalf("stopped worker falsely reported delivery: %s", out)
	}
}

func TestLiveSteeringCommentAndSayReceipt(t *testing.T) {
	for _, channel := range []string{"comment", "say"} {
		t.Run(channel, func(t *testing.T) {
			root, dir, it := workerStoryFixture(t)
			t.Chdir(root)
			it.WrapperPid = os.Getpid()
			it.CtlSock = filepath.Join(t.TempDir(), "control")
			mustWrite(t, it.CtlSock, "")
			if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
				t.Fatal(err)
			}
			marker := "live-steering-receipt-2370"
			out, code := runWeave(t, channel, "1", marker)
			if code != 0 {
				t.Fatalf("%s: %d %s", channel, code, out)
			}
			got, err := os.ReadFile(it.CtlSock)
			if err != nil || !strings.Contains(string(got), marker) {
				t.Fatalf("actual control receiver: %q %v", got, err)
			}
			q, err := loadWeaveQueue(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(q.Items[0].Comments) == 0 || q.Items[0].Comments[len(q.Items[0].Comments)-1].Body != marker {
				t.Fatal("live steering was not durable")
			}
		})
	}
}

// A one-shot worker deliberately never reads stdin, as headless Exec tools do.
// The test observes the real launch while it is alive, sends steering, then
// resumes the same workspace and checks the correction produced from argv.
func TestCodexExecSteeringRequiresResume(t *testing.T) {
	root, dir, it := workerStoryFixture(t)
	t.Chdir(root)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv(fleet.InstanceDirEnv, t.TempDir())
	t.Setenv("BASHY_INSTANCE", "")
	t.Setenv("BASHY_INSTANCE_UUID", "")
	t.Setenv("BASHY_NO_COACH", "1")
	cat := pinAgentFleet(t)
	t.Setenv("BASHY_FLEET_DIR", cat.Root())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(fleet.Tool{Name: "codex", Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: exe, Launch: fleet.ToolLaunch{Exec: fmt.Sprintf("%q -test.run=^TestSteeringReceiptChild$ exec {model} {prompt}", exe), SteerExec: "codex"}}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "one-shot-worker", Tool: "codex", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	it.State = "submitted"
	it.LaunchSpec = nil
	it.Body = "Original task: preserve unique work."
	mustWrite(t, filepath.Join(it.Workspace, "unique.txt"), "preserved")
	gitT(t, it.Workspace, "add", "unique.txt")
	gitT(t, it.Workspace, "commit", "-qm", "prior work")
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_STEERING_CHILD", "1")
	t.Setenv("WEAVE_STEERING_WAIT", "1")
	type observation struct{ socket, say, comment, err string }
	done := make(chan observation, 1)
	go func() {
		var got observation
		defer func() {
			_ = os.WriteFile(filepath.Join(it.Workspace, ".git/steering-release"), nil, 0600)
			done <- got
		}()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(it.Workspace, ".git/steering-ready")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				got.err = "child did not become ready"
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			got.err = err.Error()
			return
		}
		got.socket = q.Items[0].CtlSock
		card, ok, err := room.Find(fmt.Sprintf("weave-%d-%d", it.ID, os.Getpid()))
		if err != nil || !ok {
			got.err = fmt.Sprintf("live presence missing: %v", err)
			return
		}
		if card.CtlSock != "" {
			got.err = "headless presence advertises live socket: " + card.CtlSock
		}
		var sayCode, commentCode int
		got.say, sayCode = runWeave(t, "say", "1", "STEERING-2370: say correction", "--json")
		got.comment, commentCode = runWeave(t, "comment", "1", "STEERING-2370: comment correction")
		if sayCode != 0 || commentCode != 0 {
			got.err = fmt.Sprintf("steering failed: say=%d %s comment=%d %s", sayCode, got.say, commentCode, got.comment)
		}
	}()
	out, code := runWeave(t, "start", "--run", "1", "--resume", "--pty", "always", "--mem-limit", "0", "--json", "--", "one-shot-worker")
	got := <-done
	if got.err != "" {
		t.Fatalf("%s: exit=%d %s", got.err, code, out)
	}
	receipt, err := os.ReadFile(filepath.Join(it.Workspace, "steering-receipt.txt"))
	if err != nil || strings.Contains(string(receipt), "STEERING-2370:") {
		t.Fatalf("one-shot receipt %q %v", receipt, err)
	}
	if got.socket != "" {
		t.Errorf("headless Exec advertised a live steering socket: %s; say=%s", got.socket, got.say)
	}
	if !strings.Contains(got.say, `"delivered": false`) {
		t.Errorf("headless say must report queued only: %s", got.say)
	}
	prior := strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
	t.Setenv("WEAVE_STEERING_WAIT", "0")
	out, code = runWeave(t, "start", "--run", "1", "--resume", "--pty", "never", "--mem-limit", "0", "--json", "--", "one-shot-worker")
	if code != 0 {
		t.Fatalf("resume: %d %s", code, out)
	}
	receipt, err = os.ReadFile(filepath.Join(it.Workspace, "steering-receipt.txt"))
	for _, marker := range []string{"say correction", "comment correction"} {
		if err != nil || !strings.Contains(string(receipt), marker) {
			t.Errorf("missing actual resumed receipt %s: %q %v", marker, receipt, err)
		}
	}
	current := strings.TrimSpace(gitT(t, it.Workspace, "rev-parse", "HEAD"))
	if current == prior {
		t.Fatal("resumed worker did not incorporate steering")
	}
	if unique, err := os.ReadFile(filepath.Join(it.Workspace, "unique.txt")); err != nil || string(unique) != "preserved" {
		t.Fatal("lost unique work")
	}
	t.Logf("headless input absent; resumed child incorporated say/comment, HEAD %s -> %s", prior, current)
}

func TestCodexExecLegacySocketDoesNotAcceptSteering(t *testing.T) {
	root, dir, it := workerStoryFixture(t)
	t.Chdir(root)
	it.WrapperPid = os.Getpid()
	it.CtlSock = filepath.Join(t.TempDir(), "legacy-control")
	it.LaunchSpec.Tool = "codex"
	it.LaunchSpec.Argv = []string{"exec", "original prompt"}
	mustWrite(t, it.CtlSock, "")
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"say", "comment"} {
		out, code := runWeave(t, channel, "1", "STEERING-2370: queued for resume", "--json")
		if code != 0 || !strings.Contains(out, `"delivered": false`) {
			t.Errorf("%s claims live delivery: %d %s", channel, code, out)
		}
	}
	if b, err := os.ReadFile(it.CtlSock); err != nil || len(b) != 0 {
		t.Errorf("headless control got prose: %q %v", b, err)
	}
}

func TestCodexExecControlCapability(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		args       []string
		want       bool
	}{
		{"exec", "codex", []string{"-c", "x=true", "exec", "--model", "test", "prompt"}, true},
		{"windows exec", "codex.exe", []string{"exec", "prompt"}, true},
		{"interactive", "codex", []string{"--model", "test", "prompt"}, false},
		{"prompt word", "codex", []string{"--", "exec"}, false},
		{"config value", "codex", []string{"-c", "exec", "prompt"}, false},
		{"other streaming tool", "claude", []string{"--input-format", "stream-json", "exec"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := weaveCodexExec(&weaveLaunchSpec{Tool: tc.tool, Argv: tc.args}); got != tc.want {
				t.Fatalf("capability=%v want %v", got, tc.want)
			}
		})
	}
}
