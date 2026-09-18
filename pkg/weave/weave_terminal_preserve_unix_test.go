//go:build !windows

package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A worker can finish useful edits and then fail on its way out. Preservation
// is the default terminal behavior: the isolated branch gets an honest WIP
// commit, while the non-zero exit still leaves the run failed and unmergeable
// without the salvage gate.
func TestWeaveStartPreservesDirtyFailedRunWithoutAutoCommitFlag(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if out, code := runWeave(t, "add", "preserve failed WIP", "--json"); code != 0 {
		t.Fatalf("weave add exit=%d: %s", code, out)
	}

	script := "printf 'partial work\\n' > partial.txt; exit 7"
	out, code := runWeave(t, "start", "--issue", "1", "--pty", "never", "--json", "--", "sh", "-c", script)
	if code == 0 || !strings.Contains(out, "tool exited with 7") {
		t.Fatalf("failed worker result = exit %d, %s", code, out)
	}

	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil || it.State != "failed" || !it.AutoCommitted || it.CommitsAhead != 1 || it.Dirty {
		t.Fatalf("terminal record did not preserve WIP without asserting success: %#v", it)
	}
	if _, err := os.Stat(filepath.Join(it.Workspace, "partial.txt")); err != nil {
		t.Fatalf("preserved file missing from workspace: %v", err)
	}
	subject := gitT(t, it.Workspace, "log", "-1", "--format=%s")
	if !strings.Contains(subject, "work preserved from a run that exited 7") {
		t.Fatalf("preservation commit is not honest about failure: %q", subject)
	}
}
