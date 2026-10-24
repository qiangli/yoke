package src_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/prune"
	"github.com/qiangli/yoke/pkg/src"
)

func TestSrcPruneOnlyAncestorBuilds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", home)
	oldSHA, newSHA := seedFixture(t, home)
	t.Setenv("BASHY_SRC_COMMIT", newSHA)
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatal(err)
	}
	if err := src.EnsureLocal(home, "yoke"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, "src", ".builds", "yoke")
	for _, name := range []string{oldSHA, newSHA, strings.Repeat("f", 40)} {
		path := filepath.Join(base, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "binary"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := src.Pruner{Home: home}
	items, err := p.DryRun(context.Background(), prune.Options{})
	if err != nil || len(items) != 1 || !strings.HasSuffix(items[0].ID, oldSHA) {
		t.Fatalf("dry-run = %+v, %v", items, err)
	}
	if _, err := os.Stat(filepath.Join(base, oldSHA, "binary")); err != nil {
		t.Fatalf("dry-run changed build: %v", err)
	}
	counts, err := p.Apply(context.Background(), items)
	if err != nil || counts.Removed != 1 {
		t.Fatalf("apply = %+v, %v", counts, err)
	}
	for _, name := range []string{newSHA, strings.Repeat("f", 40)} {
		if _, err := os.Stat(filepath.Join(base, name, "binary")); err != nil {
			t.Fatalf("build %s lost: %v", name, err)
		}
	}
}
