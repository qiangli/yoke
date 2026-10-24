package weave

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSprintPruneDryRunDoesNotChangeBoard(t *testing.T) {
	_, _, board := lifecycleHome(t)
	if err := saveWeaveQueue(board, &weaveQueue{NextID: 2, Stories: []*weaveStory{{ID: 1, Title: "fixture", Column: "doing", Created: time.Now().Add(-time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(board, "queue.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, code := runSprint(t, "prune", "1", "--json")
	if code != 0 || !strings.Contains(out, `"sprint": 1`) {
		t.Fatalf("dry-run exit=%d: %s", code, out)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("dry-run changed board: %v", err)
	}
}
