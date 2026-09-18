package weave

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func queueDirTestRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	root, err := weaveRepoRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestWeaveQueueDirFreshReadIsPure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := queueDirTestRepo(t)
	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("pure resolver created %s (stat err %v)", dir, err)
	}
	if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
		t.Fatalf("pure resolver created state root (stat err %v)", err)
	}
}

func TestWeaveQueueDirReadsBothLegacyNamesWithoutMigration(t *testing.T) {
	for _, nameKind := range []string{"tag", "path"} {
		t.Run(nameKind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := queueDirTestRepo(t)
			tag, pathName := weaveQueueNames(root)
			name := tag
			if nameKind == "path" {
				name = pathName
			}
			legacy := filepath.Join(weaveLegacyStateRoots(home)[0], name)
			if err := os.MkdirAll(legacy, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(legacy, "session.json"), []byte(`{"task_id":"legacy"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			subdir := filepath.Join(root, "subdir")
			if err := os.Mkdir(subdir, 0o755); err != nil {
				t.Fatal(err)
			}
			got, err := weaveQueueDir(subdir)
			if err != nil {
				t.Fatal(err)
			}
			if got != legacy {
				t.Fatalf("resolved %q, want legacy %q", got, legacy)
			}
			if _, err := os.Stat(legacy); err != nil {
				t.Fatalf("read migrated legacy state: %v", err)
			}
			if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
				t.Fatalf("read created canonical state: %v", err)
			}
		})
	}
}

func TestEnsureWeaveQueueDirKeepsLegacyAsSoleRootAcrossWriters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := queueDirTestRepo(t)
	tag, legacyName := weaveQueueNames(root)
	legacy := filepath.Join(weaveLegacyStateRoots(home)[0], legacyName)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(legacy, &weaveQueue{Root: root, NextID: 2, Items: []*weaveItem{{ID: 1, State: "todo"}}}); err != nil {
		t.Fatal(err)
	}
	// Simulate a writer that cached the resolved legacy path before another
	// writer asks for an ensured path.
	cached, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	ensured, err := ensureWeaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if cached != legacy || ensured != legacy {
		t.Fatalf("cached=%q ensured=%q, want sole legacy root %q", cached, ensured, legacy)
	}
	q, err := loadWeaveQueue(cached)
	if err != nil {
		t.Fatal(err)
	}
	q.Items = append(q.Items, &weaveItem{ID: 2, State: "todo"})
	if err := saveWeaveQueue(cached, q); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(weaveStateRoot(home), tag)
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("writer split legacy queue by creating canonical %s (stat err %v)", canonical, err)
	}
	got, err := loadWeaveQueue(legacy)
	if err != nil || len(got.Items) != 2 {
		t.Fatalf("legacy writer roundtrip = %+v, %v", got, err)
	}
}

func TestWeaveListAndStatusDoNotWriteQueueOrLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := queueDirTestRepo(t)
	t.Chdir(root)
	dir, err := ensureWeaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, NextID: 2, Items: []*weaveItem{{ID: 1, Title: "read me", State: "todo"}}}); err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(dir, "queue.json")
	before, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "list", "--json"); code != 0 {
		t.Fatalf("weave list = %d: %s", code, out)
	}
	if out, code := runWeave(t, "status", "1", "--json"); code != 0 {
		t.Fatalf("weave status = %d: %s", code, out)
	}
	after, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("list/status changed queue contents or replaced its inode")
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.lock")); !os.IsNotExist(err) {
		t.Fatalf("list/status created queue.lock (stat err %v)", err)
	}
}

func TestReadOnlyPollingCommandsDoNotCreateFreshState(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "fleet", args: []string{"fleet", "--json"}},
		{name: "wait", args: []string{"wait", "--all", "--json"}},
		{name: "list-watch", args: []string{"list", "--watch", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
			root := queueDirTestRepo(t)
			t.Chdir(root)
			cmd := NewWeaveCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if tc.name == "list-watch" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				cmd.SetContext(ctx)
			}
			if err := cmd.Execute(); err != nil {
				t.Fatalf("weave %s: %v", tc.name, err)
			}
			if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
				t.Fatalf("%s created weave state (stat err %v)", tc.name, err)
			}
		})
	}
}
