package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSessionPointerRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := t.TempDir()

	want := &SessionPointer{
		TaskID:       "task-1",
		CloudboxBase: "https://cloudbox.example",
		TokenRef:     "keychain:cloudbox",
	}
	if err := WriteSessionPointer(repoRoot, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSessionPointer(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != *want {
		t.Fatalf("pointer = %+v, want %+v", got, want)
	}
}

func TestReadSessionPointerMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := t.TempDir()

	got, err := ReadSessionPointer(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("pointer = %+v, want nil", got)
	}
}

func TestReadSessionPointerFromRepoSubdirFindsLegacyWithoutMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	root, err := weaveRepoRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	_, legacyName := weaveQueueNames(root)
	legacy := filepath.Join(weaveLegacyStateRoots(home)[0], legacyName)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "session.json"), []byte(`{"task_id":"legacy-task"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSessionPointer(filepath.Join(repo, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.TaskID != "legacy-task" {
		t.Fatalf("legacy pointer = %+v", got)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("read migrated legacy dir: %v", err)
	}
	if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
		t.Fatalf("read created canonical root: %v", err)
	}
}
