package weave

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/quotaobs"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

func TestTakeoverCause(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ log, want string }{
		{"usage limit reached", "quota-exhausted"}, {"insufficient_quota", "quota-exhausted"}, {"HTTP 502 bad gateway", "vendor-outage"}, {"API error: 500", "vendor-outage"}, {"overloaded_error", "vendor-outage"}, {"service_unavailable", "vendor-outage"}, {"connection refused api.vendor.example", "vendor-outage"}, {"connection refused localhost:8000", "other"}, {"rate limit exceeded", "other"}, {"authentication failed", "other"}, {"compiler error", "other"},
	} {
		t.Run(tc.log, func(t *testing.T) {
			got, _ := weaveTakeoverCause(tc.log, nil, "a", now, "")
			if got != tc.want {
				t.Fatalf("%s != %s", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		age     time.Duration
		account string
		want    string
	}{{9 * time.Minute, "a", "vendor-outage"}, {11 * time.Minute, "a", "other"}, {time.Minute, "b", "other"}, {-time.Minute, "a", "other"}} {
		got, _ := weaveTakeoverCause("", []quotaobs.Observation{{At: now.Add(-tc.age), Account: tc.account, Status: 503, Excerpt: "vendor failed"}}, "a", now, "")
		if got != tc.want {
			t.Fatalf("%+v: %s", tc, got)
		}
	}
	_, e := weaveTakeoverCause("quota exhausted token=secret-123456789", nil, "a", now, "")
	if strings.Contains(e, "secret-123") {
		t.Fatal(e)
	}
}

func TestTakeoverSelection(t *testing.T) {
	old := weaveTakeoverBinding
	t.Cleanup(func() { weaveTakeoverBinding = old })
	identities := map[string]llmbudget.Binding{"dead": {Account: "a", Provider: "v1"}, "alias": {Account: "a", Provider: "v1"}, "same-vendor": {Account: "b", Provider: "v1"}, "other-vendor": {Account: "c", Provider: "v2"}}
	weaveTakeoverBinding = func(a string) (llmbudget.Binding, error) { return identities[a], nil }
	pool := []ladder.Entrant{{Agent: "dead", Free: true, Band: 5}, {Agent: "alias", Free: true, Band: 5}, {Agent: "same-vendor", Free: true, Band: 4}, {Agent: "other-vendor", Free: true, Band: 3}}
	for _, tc := range []struct{ cause, want string }{{"quota-exhausted", "same-vendor"}, {"vendor-outage", "other-vendor"}} {
		if got := weaveTakeoverPick(pool, "dead", identities["dead"], tc.cause, nil); got != tc.want {
			t.Fatalf("%s: %s", tc.cause, got)
		}
	}
	pool[3].Free = false
	if got := weaveTakeoverPick(pool, "dead", identities["dead"], "vendor-outage", nil); got != "" {
		t.Fatal(got)
	}
	if got := weaveTakeoverPick(pool, "dead", identities["dead"], "quota-exhausted", map[string]bool{"same-vendor": true}); got != "" {
		t.Fatal(got)
	}
}

func TestTakeoverWorkspaceSingleton(t *testing.T) {
	it := &weaveItem{ID: 1, State: "failed", Workspace: t.TempDir(), Branch: "preserved"}
	gitInitCommit(t, it.Workspace, "file", "content")
	weaveTestGit(t, it.Workspace, "branch", "-M", "preserved")
	q := &weaveQueue{Items: []*weaveItem{it}}
	if err := weaveTakeoverTerminal(q, it); err != nil {
		t.Fatal(err)
	}
	it.ChildPID = os.Getpid()
	if weaveTakeoverTerminal(q, it) == nil {
		t.Fatal("live child allowed")
	}
	it.ChildPID = 0
	it.ResourceReservationID = "unsettled"
	if weaveTakeoverTerminal(q, it) == nil {
		t.Fatal("unsettled allowed")
	}
	it.ResourceTerminated = true
	q.Items = append(q.Items, &weaveItem{ID: 2, State: "working", Workspace: it.Workspace})
	if weaveTakeoverTerminal(q, it) == nil {
		t.Fatal("shared workspace allowed")
	}
}

func TestTakeoverDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, log      string
		dry, available bool
	}{{"quota", "usage limit reached", false, true}, {"heartbeat", "usage limit reached", false, true}, {"dry", "usage limit reached", true, true}, {"unavailable", "usage limit reached", false, false}, {"other", "compile error", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("BASHY_HOME", home)
			board := filepath.Join(home, "sprint")
			t.Setenv("BASHY_SPRINT_DIR", board)
			t.Setenv("BASHY_QUOTA_OBSERVATIONS", filepath.Join(home, "observations.jsonl"))
			restore := llmbudget.SetDefault(llmbudget.New(llmbudget.Config{Policy: &llmbudget.Policy{Version: 1}}))
			defer restore()
			oldBinding, oldPool, oldLaunch, oldTransfer, oldSend := weaveTakeoverBinding, sprintReassignSeatPool, weaveTakeoverLaunch, weaveTakeoverTransfer, weaveSupervisionSend
			t.Cleanup(func() {
				weaveTakeoverBinding, sprintReassignSeatPool, weaveTakeoverLaunch, weaveTakeoverTransfer, weaveSupervisionSend = oldBinding, oldPool, oldLaunch, oldTransfer, oldSend
			})
			weaveTakeoverBinding = func(a string) (llmbudget.Binding, error) { return llmbudget.Binding{Account: a, Provider: a}, nil }
			sprintReassignSeatPool = func(_ string, _ []ladder.Event, _ time.Time, _ ...sprintAssignExclusions) ([]ladder.Entrant, ladder.Lines, error) {
				return []ladder.Entrant{{Agent: "replacement", Free: tc.available, Band: 3}}, ladder.Lines{}, nil
			}
			launches, alarms := 0, 0
			weaveTakeoverLaunch = func(root, dir string, id int64, a string) error {
				launches++
				if id != 1 || a != "replacement" {
					return fmt.Errorf("wrong launch")
				}
				return nil
			}
			weaveTakeoverTransfer = func(string, *weaveItem, string) error { return nil }
			weaveSupervisionSend = func(to, msg string) error {
				alarms++
				if to != "manager" || !strings.Contains(msg, "mode=handoff-only") {
					t.Fatal(to, msg)
				}
				return nil
			}
			dir := filepath.Join(home, "queue")
			workspace := t.TempDir()
			gitInitCommit(t, workspace, "file", "content")
			weaveTestGit(t, workspace, "branch", "-M", "preserved")
			log := filepath.Join(home, "run.log")
			if err := os.WriteFile(log, []byte(tc.log), 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			it := &weaveItem{ID: 1, Register: "story-a", State: "failed", Owner: "dead", Workspace: workspace, Branch: "preserved", Created: now, LogPath: log, Body: "Original task"}
			if err := saveWeaveQueue(dir, &weaveQueue{Root: workspace, NextID: 2, Items: []*weaveItem{it}}); err != nil {
				t.Fatal(err)
			}
			s := &weaveStory{ID: 1, Lease: &weaveStoryLease{Holder: "manager", At: now}, Runs: []sprintRun{{ID: 1, Queue: filepath.Base(dir), Born: now}}}
			if err := saveWeaveQueue(board, &weaveQueue{Stories: []*weaveStory{s}}); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			before, _ := os.ReadFile(filepath.Join(dir, "queue.json"))
			beforeBoard, _ := os.ReadFile(filepath.Join(board, "queue.json"))
			if tc.name == "heartbeat" {
				oldStop := weaveTakeoverStop
				t.Cleanup(func() { weaveTakeoverStop = oldStop })
				stops := 0
				weaveTakeoverStop = func(_ *cobra.Command, id int64) error {
					stops++
					return withWeaveQueueLock(dir, func(q *weaveQueue) error { q.Items[0].State = "killed"; return nil })
				}
				if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
					q.Items[0].State = "working"
					q.Items[0].StartedAt = now.Add(-time.Hour)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(log, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				weaveTakeoverHeartbeat(cmd, dir, 10*time.Minute, now)
				if stops != 1 {
					t.Fatalf("stops=%d", stops)
				}
			} else if err := runWeaveTakeover(cmd, dir, 1, "", tc.dry, false); err != nil {
				t.Fatal(err)
			}
			q, _ := loadWeaveQueue(dir)
			if tc.dry || tc.name == "other" {
				after, _ := os.ReadFile(filepath.Join(dir, "queue.json"))
				afterBoard, _ := os.ReadFile(filepath.Join(board, "queue.json"))
				if !bytes.Equal(before, after) || !bytes.Equal(beforeBoard, afterBoard) || launches != 0 || alarms != 0 {
					t.Fatal("read-only path mutated state")
				}
				if tc.dry && !strings.Contains(out.String(), `"to":"replacement"`) {
					t.Fatal(out.String())
				}
				return
			}
			if alarms != 1 {
				t.Fatalf("alarms=%d", alarms)
			}
			if tc.available {
				if launches != 1 || q.Items[0].Workspace != workspace || q.Items[0].Branch != "preserved" || !strings.Contains(q.Items[0].Body, "do not continue the implementation") || len(q.Items[0].Handoffs) != 1 {
					t.Fatalf("launches=%d run=%+v", launches, q.Items[0])
				}
				if err := runWeaveTakeover(cmd, dir, 1, "", false, true); err == nil || !strings.Contains(err.Error(), "limit") {
					t.Fatalf("limit not enforced: %v", err)
				}
			} else {
				if launches != 0 || !q.Items[0].NeedsTakeover {
					t.Fatalf("launches=%d flag=%v", launches, q.Items[0].NeedsTakeover)
				}
			}
		})
	}
}

func TestTakeoverFlagStory(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("BASHY_HOME", home)
	board := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", board)
	store := todopkg.RepoStore(repo)
	story, err := todopkg.Add(store, "recover", "body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	story.Sprint = 1
	if _, err = store.Save(story); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "queue")
	now := time.Now().UTC()
	it := &weaveItem{ID: 1, Register: story.ID, Created: now}
	s := &weaveStory{ID: 1, StoryRoots: []string{repo}, Runs: []sprintRun{{ID: 1, Queue: filepath.Base(dir), Born: now}}}
	if err := saveWeaveQueue(board, &weaveQueue{Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}
	if err := weaveTakeoverFlag(dir, it); err != nil {
		t.Fatal(err)
	}
	got, err := store.Resolve(story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsReassignLabel(got.Labels, "needs-takeover") {
		t.Fatal(got.Labels)
	}
}

func TestTakeoverBriefAndNativeGit(t *testing.T) {
	repo := t.TempDir()
	gitInitCommit(t, repo, "tracked.txt", "before\n")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status, stat := weaveTakeoverGit(repo)
	if !strings.Contains(status, "tracked.txt") || !strings.Contains(stat, "1 insertions(+), 1 deletions(-)") {
		t.Fatal(status, stat)
	}
	it := &weaveItem{Register: "abc", Workspace: repo, Body: "Implement task"}
	brief := weaveTakeoverBrief(it, "handoff-only", "Story: #999\ntoken=secret-value")
	if !strings.Contains(brief, "| Story: #999") || !strings.Contains(brief, "do not continue the implementation") {
		t.Fatal(brief)
	}
	if _, err := weaveBriefTrace(brief); err != nil {
		t.Fatal(err)
	}
	brief = weaveTakeoverBrief(it, "continue", "")
	if !strings.Contains(brief, "Continue the story and submit.") {
		t.Fatal(brief)
	}
}

func TestTakeoverRedactsSecrets(t *testing.T) {
	for _, s := range []string{"Bearer secret-value", `"api_key":"secret-value"`, "token=secret-value", "password=secret-value", "sk-abcdefghijklmnopqr"} {
		got := takeoverRedact(s)
		if strings.Contains(got, "secret-value") || strings.Contains(got, "abcdefghijklmnopqr") {
			t.Fatal(got)
		}
	}
}
