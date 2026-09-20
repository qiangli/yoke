package weave

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestSprintOrientationRequiresPrimaryGoalAndSpec(t *testing.T) {
	tests := []struct {
		name string
		s    *weaveStory
		want string
	}{
		{"both missing", &weaveStory{ID: 1}, "primary goal and master execution plan"},
		{"goal missing", &weaveStory{ID: 1, SpecRef: "docs/plan.md"}, "primary goal"},
		{"spec missing", &weaveStory{ID: 1, PrimaryGoal: "ship one outcome"}, "master execution plan"},
		{"complete", &weaveStory{ID: 1, PrimaryGoal: "ship one outcome", SpecRef: "docs/plan.md"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sprintOrientationError(tc.s)
			if tc.want == "" && err != nil {
				t.Fatalf("complete orientation refused: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("orientation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSprintReportMarksMissingEvidenceAndTwoUnchangedCheckpointsStagnant(t *testing.T) {
	repo := t.TempDir()
	it, err := todopkg.Add(todopkg.RepoStore(repo), "open outcome", "", "p0", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	it.Sprint = 7
	if _, err := todopkg.RepoStore(repo).Save(it); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s := &weaveStory{ID: 7, StoryRoots: []string{repo}, Thread: []weaveComment{
		{At: now.Add(-2 * time.Minute), Kind: "progress", Body: "checkpoint"},
		{At: now.Add(-time.Minute), Kind: "progress", Body: "checkpoint"},
	}}
	r := sprintOutcomeCost(s, now)
	if !r.Stagnant || r.Metric != "0/1 stories closed" || r.MetricChange != 0 {
		t.Fatalf("report = %+v", r)
	}
	if r.TokensAvailable || r.DiffAvailable {
		t.Fatalf("unavailable evidence was inferred: %+v", r)
	}
	_ = os.Remove(filepath.Join(repo, "unused"))
}

func TestSprintPrimaryGoalPersistsAcrossRestart(t *testing.T) {
	want := &weaveStory{ID: 7, PrimaryGoal: "deliver the repository-neutral guardrail", SpecRef: "docs/plan.md"}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got weaveStory
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.PrimaryGoal != want.PrimaryGoal {
		t.Fatalf("primary goal after reload = %q, want %q", got.PrimaryGoal, want.PrimaryGoal)
	}
}

// Sprint 220, todo:4c6a5982: the report carries the stories' kind vocabulary
// in use (most used first), and `sprint show` prints it as one line.
func TestSprintReportBreaksStoriesDownByKind(t *testing.T) {
	repo := t.TempDir()
	st := todopkg.RepoStore(repo)
	for i, kind := range []string{"bug", "feature", "bug", "doc"} {
		it, err := todopkg.Add(st, fmt.Sprintf("story %d", i), "", "p1", nil, "", "")
		if err != nil {
			t.Fatal(err)
		}
		it.Sprint, it.Kind = 9, kind
		if _, err := st.Save(it); err != nil {
			t.Fatal(err)
		}
	}
	r := sprintOutcomeCost(&weaveStory{ID: 9, StoryRoots: []string{repo}}, time.Now())
	if len(r.Kinds) != 3 || r.Kinds[0].Word != "bug" || r.Kinds[0].Count != 2 || r.Kinds[1].Word != "doc" || r.Kinds[2].Word != "feature" {
		t.Fatalf("kinds = %+v (most used first, then alphabetical)", r.Kinds)
	}
	var buf bytes.Buffer
	renderSprintOutcomeCost(&buf, r)
	if !strings.Contains(buf.String(), "kinds:       2 bug · 1 doc · 1 feature\n") {
		t.Fatalf("rendered:\n%s", buf.String())
	}
}
