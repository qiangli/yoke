package prune

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testNoun struct {
	name string
	path string
}

func (n testNoun) Name() string { return n.name }
func (n testNoun) DryRun(_ context.Context, _ Options) ([]Item, error) {
	b, err := os.ReadFile(n.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []Item{{Noun: n.name, ID: n.path, Reason: "obsolete fixture", Size: int64(len(b))}}, nil
}
func (n testNoun) Apply(_ context.Context, items []Item) (Counts, error) {
	if len(items) == 0 {
		return Counts{}, nil
	}
	return Counts{Removed: 1, Bytes: items[0].Size}, os.Remove(n.path)
}

func TestRegistryOverviewAndNounCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	path := filepath.Join(home, "stale")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	for _, n := range []Noun{testNoun{"zeta", path}, testNoun{"alpha", path}} {
		if err := r.Register(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(testNoun{"alpha", path}); err == nil {
		t.Fatal("duplicate noun registered")
	}
	before, _ := os.ReadFile(path)
	rows := r.Overview(context.Background())
	if len(rows) != 2 || rows[0].Noun != "alpha" || rows[1].Noun != "zeta" || rows[0].Count != 1 || rows[0].Command != "bashy alpha prune --apply" {
		t.Fatalf("overview = %+v", rows)
	}
	cmd := NewOverviewCommand(r)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "bashy zeta prune --apply") {
		t.Fatalf("overview: %v %q", err, out.String())
	}
	cmd = NewNounCommand(testNoun{"alpha", path})
	out.Reset()
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), "obsolete fixture") {
		t.Fatalf("dry-run: %v %q", err, out.String())
	}
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("dry-run changed the file")
	}
	cmd = NewNounCommand(testNoun{"alpha", path})
	out.Reset()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--apply", "--json"})
	if err := cmd.Execute(); err != nil || !strings.Contains(out.String(), `"removed":1`) {
		t.Fatalf("apply: %v %q", err, out.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("apply did not remove fixture")
	}
}

func TestParseOlderDays(t *testing.T) {
	got, err := ParseOlder("30d")
	if err != nil || got != 30*24*time.Hour {
		t.Fatalf("30d = %s, %v", got, err)
	}
	for _, value := range []string{"0d", "-1d", "bogus"} {
		if _, err := ParseOlder(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
