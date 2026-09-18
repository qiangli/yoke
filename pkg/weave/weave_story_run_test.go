package weave

import "testing"

func TestSameSprintRunRejectsDifferentRepositoriesBeforeLegacyResolution(t *testing.T) {
	// Neither legacy queue needs to exist: issue IDs are scoped by repository,
	// so unrelated repos can never name the same run. Reaching queue resolution
	// here used to make a missing historical queue block a valid new link.
	same, err := sameSprintRun(
		sprintRun{Repo: "old-repo", ID: 3},
		sprintRun{Repo: "new-repo", ID: 3, Queue: "new-repo-deadbeef"},
	)
	if err != nil {
		t.Fatalf("different repositories required queue resolution: %v", err)
	}
	if same {
		t.Fatal("same issue number in different repositories reported as one run")
	}
}
