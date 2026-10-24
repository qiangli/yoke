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
	p.runningExecutables = func(context.Context) ([]string, error) { return nil, nil }
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

func TestSelfPruneRetainsRunningOlderVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	cache := filepath.Join(home, "cache")
	t.Setenv("BASHY_BIN_CACHE", cache)
	base := filepath.Join(cache, "tool")
	for i, version := range []string{"v1", "v2", "v3", "v4"} {
		path := filepath.Join(base, version)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		date := time.Now().Add(time.Duration(i-4) * 24 * time.Hour)
		if err := os.Chtimes(path, date, date); err != nil {
			t.Fatal(err)
		}
	}
	active := []string{filepath.Join(base, "v1", "tool")}
	p := SelfPruner{Home: filepath.Join(home, ".bashy"), Cache: cache,
		ExecPath:           filepath.Join(base, "v4", "tool"),
		runningExecutables: func(context.Context) ([]string, error) { return active, nil },
	}
	items, err := p.DryRun(context.Background(), prune.Options{})
	if err != nil || len(items) != 1 || items[0].ID != filepath.Join(base, "v2") {
		t.Fatalf("dry-run = %+v, %v", items, err)
	}
	active = append(active, filepath.Join(base, "v2", "tool"))
	counts, err := p.Apply(context.Background(), items)
	if err != nil || counts.Removed != 0 {
		t.Fatalf("apply removed running version: %+v, %v", counts, err)
	}
	if _, err := os.Stat(filepath.Join(base, "v2")); err != nil {
		t.Fatalf("running version missing: %v", err)
	}
}
