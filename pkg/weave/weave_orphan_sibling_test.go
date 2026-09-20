package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sibling clone (<queue>/workspaces/<dep>) is unclaimed by any row and clean,
// which is exactly what an orphan looks like — but every live run in the queue
// builds against it. It holds while any run is live and is reclaimable after.
func TestWeaveOrphanSiblingCloneHeldWhileARunIsLive(t *testing.T) {
	root := setupIsolationFixture(t)
	dir, _ := weaveQueueDir(root)
	sib := filepath.Join(dir, "workspaces", "sh")
	if err := os.MkdirAll(filepath.Dir(sib), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "clone", "-q", root, sib)
	q := &weaveQueue{Root: root, Items: []*weaveItem{{ID: 1, State: "working", Workspace: filepath.Join(dir, "workspaces", "issue-1")}}}
	targets, err := weaveOrphanWorkspaceTargets(dir, q)
	if err != nil || len(targets) != 1 || !strings.Contains(targets[0].Hold, "live run #1") {
		t.Fatalf("sibling clone not held while run #1 is live: %+v err=%v", targets, err)
	}
	q.Items[0].State = "done"
	targets, _ = weaveOrphanWorkspaceTargets(dir, q)
	if len(targets) != 1 || targets[0].Hold != "" {
		t.Fatalf("sibling clone still held with no live run: %+v", targets)
	}
}
