package binmgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/prune"
)

func TestSelfPruneKeepsCurrentAndPrevious(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	cache := filepath.Join(home, "cache")
	t.Setenv("BASHY_BIN_CACHE", cache)
	base := filepath.Join(cache, "tool")
	now := time.Now()
	for i, v := range []string{"v1", "v2", "v3", "v4"} {
		path := filepath.Join(base, v)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "tool"), []byte(v), 0700); err != nil {
			t.Fatal(err)
		}
		date := now.Add(time.Duration(i-4) * 24 * time.Hour)
		if err := os.Chtimes(path, date, date); err != nil {
			t.Fatal(err)
		}
	}
	p := SelfPruner{Home: filepath.Join(home, ".bashy"), Cache: cache, ExecPath: filepath.Join(base, "v4", "tool")}
	before, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	items, err := p.DryRun(context.Background(), prune.Options{})
	if err != nil || len(items) != 2 {
		t.Fatalf("dry-run = %+v, %v", items, err)
	}
	after, _ := os.ReadDir(base)
	if len(before) != len(after) {
		t.Fatal("dry-run changed cache")
	}
	counts, err := p.Apply(context.Background(), items)
	if err != nil || counts.Removed != 2 {
		t.Fatalf("apply = %+v, %v", counts, err)
	}
	for _, v := range []string{"v3", "v4"} {
		if _, err := os.Stat(filepath.Join(base, v, "tool")); err != nil {
			t.Fatalf("kept version %s missing: %v", v, err)
		}
	}
}
