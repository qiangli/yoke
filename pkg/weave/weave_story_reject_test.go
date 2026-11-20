package weave

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestSprintRejectRecordsDecisionAndAllowsResubmit(t *testing.T) {
	f := newSprintLinkStoryFixture(t, false, sprintLinkStoryRun(7, time.Now().UTC().Add(-time.Hour)))
	if _, stderr, err := f.link(t, 7, "--story", f.story.ID); err != nil {
		t.Fatalf("link: %v %s", err, stderr)
	}
	reason := "cover the timeout path"
	out, stderr, err := f.run(t, "reject", "1", f.story.ID, "--repo", f.storyRoot, "-m", reason)
	if err != nil || !strings.Contains(out, "returned to doing") {
		t.Fatalf("reject: %v out=%q stderr=%q", err, out, stderr)
	}
	it, err := todopkg.ResolveRef(todopkg.RepoStore(f.storyRoot), f.story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != todopkg.StatusDoing || it.Assignee != "worker" || !strings.Contains(it.Body, reason) {
		t.Fatalf("story after reject: status=%q assignee=%q body=%q", it.Status, it.Assignee, it.Body)
	}
	if thread := f.sprint(t).Thread; !strings.Contains(thread[len(thread)-1].Body, reason) {
		t.Fatalf("sprint thread missing reason: %+v", thread)
	}
	comments := f.queue(t).Items[0].Comments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, reason) {
		t.Fatalf("linked run missing reason: %+v", comments)
	}
	_, stderr, err = f.run(t, "accept", "1", f.story.ID, "--repo", f.storyRoot, "--no-rating")
	if err == nil || !strings.Contains(stderr, "no submitted delivery evidence") {
		t.Fatalf("accept of rejected delivery: %v stderr=%q", err, stderr)
	}
	_, stderr, err = f.run(t, "submit", "1", f.story.ID, "--repo", f.storyRoot, "--as", "worker", "-m", "fixed")
	if err != nil {
		t.Fatalf("resubmit: %v %s", err, stderr)
	}
	_, stderr, err = f.run(t, "reject", "1", f.story.ID, "--repo", f.storyRoot, "-m", "one more check")
	if err != nil {
		t.Fatalf("second reject: %v %s", err, stderr)
	}
	_, stderr, err = f.run(t, "submit", "1", f.story.ID, "--repo", f.storyRoot, "--as", "worker", "-m", "fixed")
	if err != nil {
		t.Fatalf("identical evidence resubmit: %v %s", err, stderr)
	}
	_, stderr, err = f.run(t, "accept", "1", f.story.ID, "--repo", f.storyRoot, "--no-rating")
	if err != nil {
		t.Fatalf("accept resubmission: %v %s", err, stderr)
	}
}

func TestSprintRejectRequiresReasonAndPendingDelivery(t *testing.T) {
	f := newSprintLinkStoryFixture(t, false)
	for _, args := range [][]string{
		{"reject", "1", f.story.ID, "--repo", f.storyRoot},
		{"reject", "1", f.story.ID, "--repo", f.storyRoot, "-m", "   "},
	} {
		if _, _, err := f.run(t, args...); err == nil {
			t.Fatalf("accepted invalid rejection: %v", args)
		}
	}
	if len(f.sprint(t).Thread) != 1 {
		t.Fatal("invalid rejection changed sprint thread")
	}
	_, stderr, err := f.run(t, "reject", "1", f.story.ID, "--repo", f.storyRoot, "-m", "rework")
	if err != nil {
		t.Fatalf("first reject: %v %s", err, stderr)
	}
	_, stderr, err = f.run(t, "reject", "1", f.story.ID, "--repo", f.storyRoot, "-m", "again")
	if err == nil || !strings.Contains(stderr, "no submitted delivery") {
		t.Fatalf("duplicate reject: %v %s", err, stderr)
	}
}

func TestSprintRejectRecordsLegacyAssignedRun(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour)
	f := newSprintLinkStoryFixture(t, false, sprintLinkStoryRun(7, created))
	f.story.Weave = 7
	if _, err := todopkg.RepoStore(f.storyRoot).Save(f.story); err != nil {
		t.Fatal(err)
	}
	if err := withWeaveQueueLock(f.board, func(q *weaveQueue) error {
		s := findWeaveStory(q, 1)
		s.Runs = append(s.Runs, sprintRun{Repo: filepath.Base(f.runRepo), Queue: f.tag, ID: 7, Born: created})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := f.run(t, "reject", "1", f.story.ID, "--repo", f.storyRoot, "-m", "check output")
	if err != nil {
		t.Fatalf("reject: %v %s", err, stderr)
	}
	comments := f.queue(t).Items[0].Comments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "check output") {
		t.Fatalf("legacy run missing decision: %+v", comments)
	}
}
