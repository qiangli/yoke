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
)

// A deterministic worker captures the actual prompt delivered through resume.
// No provider, subscription or metered API is involved.
func TestSteeringReceiptChild(t *testing.T) {
	if os.Getenv("WEAVE_STEERING_CHILD") != "1" {
		return
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
