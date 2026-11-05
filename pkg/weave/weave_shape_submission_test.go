package weave

import (
	"strings"
	"testing"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestLinkShapeSubmissionRecoversOrdinaryRun(t *testing.T) {
	root := setupIsolationFixture(t)
	store := todopkg.RepoStore(root)
	item, err := todopkg.Add(store, "Review changed greeting", "Keep existing body", "p1", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := LinkShapeSubmission(root, store, item.ID, "submission-one", "Review proposal revision one.")
	if err != nil {
		t.Fatal(err)
	}
	linked, err := store.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Weave != first.ID || linked.Sprint != 0 || linked.Status != todopkg.StatusAssigned || linked.Body != item.Body {
		t.Fatalf("unexpected item changes: %#v", linked)
	}
	// Simulate losing the reciprocal save after the durable queue append.
	linked.Weave = 0
	if _, err = store.Save(linked); err != nil {
		t.Fatal(err)
	}
	second, err := LinkShapeSubmission(root, store, item.ID, "submission-one", "Review proposal revision one.")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || len(q.Items) != 1 || q.Root != root || q.Items[0].State != "todo" || q.Items[0].Owner != "" || strings.Count(q.Items[0].Body, "Shape-Submission:") != 1 {
		t.Fatalf("duplicate or started queue item: %#v", q)
	}
}
