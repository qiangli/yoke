package weave

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWeaveItemVisibleInListHidesTerminalGhosts(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	present := t.TempDir()

	tests := []struct {
		name    string
		item    *weaveItem
		history bool
		want    bool
	}{
		{"todo", &weaveItem{State: "todo"}, false, true},
		{"submitted", &weaveItem{State: "submitted"}, false, true},
		{"killed workspace present", &weaveItem{State: "killed", Workspace: present}, false, true},
		{"killed workspace absent", &weaveItem{State: "killed", Workspace: missing}, false, false},
		{"killed workspace cleared", &weaveItem{State: "killed"}, false, false},
		{"failed workspace cleared", &weaveItem{State: "failed"}, false, false},
		{"no-op", &weaveItem{State: "no-op"}, false, false},
		{"done", &weaveItem{State: "done"}, false, false},
		{"terminal history", &weaveItem{State: "killed"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := weaveItemVisibleInList(tt.item, tt.history); got != tt.want {
				t.Fatalf("visible=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestWeaveResetRemovesProjectStateAndEmptyParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := queueDirTestRepo(t)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	dir, err := ensureWeaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Root: repo, NextID: 2, Items: []*weaveItem{{ID: 1, State: "abandoned"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "issue-1.log"), []byte("old log\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runWeave(t, "reset", "--yes", "--plain")
	if code != 0 {
		t.Fatalf("reset exit=%d out=%s", code, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("project state survived reset: %v", err)
	}
	if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
		t.Fatalf("empty weave parent survived reset: %v", err)
	}
}
