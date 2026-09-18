package weave

import (
	"encoding/json"
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
