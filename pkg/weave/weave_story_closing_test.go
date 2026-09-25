package weave

import (
	"strings"
	"testing"
)

// A linked run whose checkout is gone must not send the operator off to
// commit and push a repo that is already clean: the closing report names the
// run and the exact unlink command, the same remedy `sprint prune` prints.
func TestClosingConditionsNameTheStaleRunAndItsFix(t *testing.T) {
	s := &weaveStory{ID: 42, Runs: []sprintRun{{Repo: "sh", ID: 13, Queue: "sh-7e2e7b65"}}}
	gone := func(sprintRun) (string, bool) { return "", false }

	repos := checkClosingConditions(s, nil, gone)
	if len(repos) != 1 {
		t.Fatalf("repos = %d, want 1", len(repos))
	}
	if repos[0].OK() {
		t.Fatal("a run with no checkout must not count as wrapped up")
	}
	got := repos[0].Describe()
	for _, want := range []string{"sh#13", "bashy sprint prune 42", "bashy sprint unlink 42 --repo sh --task 13"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
}
