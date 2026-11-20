package weave

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestSprintSubmitRefusesUnclaimedStoryWithoutMutation(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", home)
	t.Setenv("WEAVE_CONDUCTOR", "worker")
	st := todopkg.RepoStore(repo)
	it, err := todopkg.Add(st, "deliver", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 1
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	q := &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{{ID: 1, Title: "neutral", PrimaryGoal: "deliver", SpecRef: "docs/plan.md", Column: "doing", StoryRoots: []string{repo}, Created: time.Now().UTC()}}}
	if err := saveWeaveQueue(home, q); err != nil {
		t.Fatal(err)
	}

	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"submit", "1", it.ID, "--repo", repo, "--as", "worker", "-m", "commit abc; tests pass"})
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "sprint claim 1") {
		t.Fatalf("unclaimed submit error=%v output=%q", err, out.String())
	}
	got, _ := todopkg.ResolveRef(st, it.ID)
	if got.Assignee != "" || got.Status != todopkg.StatusTodo {
		t.Fatalf("refusal mutated story: %+v", got)
	}
}

func TestSprintStoryClosureAuditNeedsClosureNotAcceptanceEvidence(t *testing.T) {
	repo := t.TempDir()
	st := todopkg.RepoStore(repo)
	now := time.Now().UTC()
	it := &issue.Issue{ID: issue.NewID(), Kind: issue.KindTask, Title: "edited closure", Status: todopkg.StatusDone, Sprint: 9, Assignee: "worker", Closed: &now, Created: now}
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 9, StoryRoots: []string{repo}}
	if err := sprintStoryClosureAudit(s); err != nil {
		t.Fatalf("closed story without acceptance artifact must pass: %v", err)
	}
	it.Status, it.Closed = todopkg.StatusTodo, nil
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	if err := sprintStoryClosureAudit(s); err == nil || !strings.Contains(err.Error(), "still todo") {
		t.Fatalf("open story must block end: %v", err)
	}
}

func TestSprintManagerAcceptIsTheOnlyClosePath(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", home)
	t.Setenv("WEAVE_CONDUCTOR", "manager")
	st := todopkg.RepoStore(repo)
	it, err := todopkg.Add(st, "delivered", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 1
	it.Assignee = "worker"
	it.Status = todopkg.StatusAssigned
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 1, Title: "neutral", PrimaryGoal: "deliver", SpecRef: "docs/plan.md", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: time.Now().UTC()}, StoryRoots: []string{repo}, Created: time.Now().UTC()}
	if err := saveWeaveQueue(home, &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}

	submit := NewSprintCmd()
	var submitOut bytes.Buffer
	submit.SetOut(&submitOut)
	submit.SetErr(&submitOut)
	submit.SetArgs([]string{"submit", "1", it.ID, "--repo", repo, "--as", "worker"})
	if err := submit.Execute(); err != nil {
		t.Fatalf("submit without message: %v\n%s", err, submitOut.String())
	}
	q, err := loadWeaveQueue(home)
	if err != nil {
		t.Fatal(err)
	}
	if thread := q.Stories[0].Thread; len(thread) != 1 || thread[0].Kind != "submit" || thread[0].StoryID != it.ID || thread[0].EvidenceRef != "" {
		t.Fatalf("submission without evidence = %+v", thread)
	}

	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"accept", "1", it.ID, "--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accept: %v\n%s", err, out.String())
	}
	got, _ := todopkg.ResolveRef(st, it.ID)
	if got.Status != todopkg.StatusDone || got.Closed == nil || got.ClosedBy != "manager" {
		t.Fatalf("accepted story = %+v", got)
	}
	q, err = loadWeaveQueue(home)
	if err != nil {
		t.Fatal(err)
	}
	if !sprintAcceptanceEvidence(q.Stories[0], it.ID) {
		t.Fatal("manager acceptance evidence was not persisted")
	}
}

func TestSubmitDefaultsToTheStoryHolder(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", home)
	t.Setenv("WEAVE_CONDUCTOR", "manager")
	st := todopkg.RepoStore(repo)
	it, err := todopkg.Add(st, "delivered by worker", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 1
	it.Assignee = "worker"
	it.Status = todopkg.StatusAssigned
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 1, Title: "neutral", PrimaryGoal: "deliver", SpecRef: "docs/plan.md", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: time.Now().UTC()}, StoryRoots: []string{repo}, Created: time.Now().UTC()}
	if err := saveWeaveQueue(home, &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}

	blockedRoom := filepath.Join(t.TempDir(), "room")
	if err := os.WriteFile(blockedRoom, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_ROOM_DIR", blockedRoom)
	submit := func() string {
		t.Helper()
		cmd := NewSprintCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"submit", "1", shortSprintStoryID(it.ID), "--repo", repo, "-m", "  commit abc; tests pass  "})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("submit defaulting to holder failed: %v\n%s", err, out.String())
		}
		return out.String()
	}
	if out := submit(); !strings.Contains(out, "manager NOT notified") {
		t.Fatalf("submit should report the notification failure: %s", out)
	}
	if out := submit(); !strings.Contains(out, "delivery already submitted") {
		t.Fatalf("submit replay should reuse the persisted entry: %s", out)
	}
	q, err := loadWeaveQueue(home)
	if err != nil {
		t.Fatal(err)
	}
	if !sprintSubmissionEvidence(q.Stories[0], it.ID) {
		t.Fatal("submission evidence was not recorded")
	}
	thread := q.Stories[0].Thread
	if len(thread) != 1 {
		t.Fatalf("submission thread = %+v, want a single entry", thread)
	}
	entry := thread[0]
	if entry.Kind != "submit" || entry.StoryID != it.ID || entry.Author != "worker" || entry.EvidenceRef != "commit abc; tests pass" || entry.At.IsZero() {
		t.Fatalf("submission entry = %+v", entry)
	}
	if want := "worker submitted story " + shortSprintStoryID(it.ID) + " for merge/closure: commit abc; tests pass"; entry.Body != want {
		t.Fatalf("submission text = %q, want %q", entry.Body, want)
	}
}

func TestSprintSubmissionEvidenceReadsTypedAndLegacyEntries(t *testing.T) {
	const storyID = "48a92d652802"
	s := &weaveStory{Thread: []weaveComment{{
		Kind: "submit", StoryID: storyID, Body: "worker delivered",
	}}}
	if !sprintSubmissionEvidence(s, storyID) {
		t.Fatal("typed submission should match by full story ID")
	}
	s.Thread[0].Body = "worker submitted story " + shortSprintStoryID(storyID) + " for merge/closure"
	if sprintSubmissionEvidence(s, "48a92d653333") {
		t.Fatal("typed submission should not match a different story with the same prefix")
	}
	s.Thread[0] = weaveComment{Kind: "decision", Body: s.Thread[0].Body}
	if !sprintSubmissionEvidence(s, storyID) {
		t.Fatal("legacy submission should still be recognized")
	}
}

func TestConductorAcceptsItsOwnClaimWithoutTheDance(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("BASHY_SPRINT_DIR", home)
	t.Setenv("WEAVE_CONDUCTOR", "manager")
	st := todopkg.RepoStore(repo)
	it, err := todopkg.Add(st, "manager delivered", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 1
	it.Assignee = "manager"
	it.Status = todopkg.StatusAssigned
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 1, Title: "neutral", PrimaryGoal: "deliver", SpecRef: "docs/plan.md", Column: "doing", Owner: "manager", Lease: &weaveStoryLease{Holder: "manager", At: time.Now().UTC()}, StoryRoots: []string{repo}, Created: time.Now().UTC()}
	if err := saveWeaveQueue(home, &weaveQueue{NextStoryID: 2, Stories: []*weaveStory{s}}); err != nil {
		t.Fatal(err)
	}

	cmd := NewSprintCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"accept", "1", it.ID, "--repo", repo, "--no-rating"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("accept own claim: %v\n%s", err, out.String())
	}
	got, _ := todopkg.ResolveRef(st, it.ID)
	if got.Status != todopkg.StatusDone || got.Closed == nil || got.ClosedBy != "manager" {
		t.Fatalf("accepted story = %+v", got)
	}
}
