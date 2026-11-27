package weave

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A state root without queue.json is removed only when it holds weave's own
// bookkeeping and sat unchanged for a day; anything else keeps it.
func TestWeaveGCStatelessRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := weaveStateRoot(home)
	old := time.Now().Add(-48 * time.Hour)

	mk := func(name string, files map[string]string, dirs ...string) string {
		dir := filepath.Join(root, name)
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f, body := range files {
			p := filepath.Join(dir, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	age := func(dir string, when time.Time) {
		_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
			return os.Chtimes(p, when, when)
		})
	}

	leftover := mk("work-1", map[string]string{"memory.jsonl": "{}\n", "queue.lock": "", "session.json": "{}", "feed-cursor-x-y": "1"}, "workspaces", "logs")
	age(leftover, old)
	fresh := mk("gate-2", map[string]string{"memory.jsonl": "{}\n", "queue.lock": ""})
	logs := mk("clone-3", map[string]string{"memory.jsonl": "{}\n", "logs/run.log": "evidence"})
	age(logs, old)
	foreign := mk("tools", map[string]string{"agy.json": "{}", "queue.lock": ""})
	age(foreign, old)
	hidden := mk(".go-build-cache", map[string]string{"memory.jsonl": ""})
	age(hidden, old)

	now := time.Now()
	_, stateless := weaveGCStateDirs(home)
	got := map[string]weaveGCRoot{}
	for _, d := range stateless {
		if r, ok := weaveGCStateless(d, false, now); ok {
			got[filepath.Base(d)] = r
		}
	}
	if _, ok := got[".go-build-cache"]; ok {
		t.Fatal("hidden directory treated as a state root")
	}
	if _, ok := got["tools"]; ok {
		t.Fatal("root with an unknown file was reported")
	}
	if r := got["work-1"]; len(r.Removed) != 1 || r.Removed[0].Kind != "state-root" {
		t.Fatalf("leftover root not reclaimable: %+v", r)
	}
	if r := got["gate-2"]; len(r.Removed) != 0 || len(r.Retained) != 1 {
		t.Fatalf("recent root not retained: %+v", r)
	}
	if r := got["clone-3"]; len(r.Removed) != 0 || len(r.Retained) != 1 {
		t.Fatalf("root with logs not retained: %+v", r)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Fatal("report mode removed something")
	}

	if r, _ := weaveGCStateless(leftover, true, now); len(r.Removed) != 1 || !r.Removed[0].Done {
		t.Fatalf("apply did not remove: %+v", r)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("leftover root still present after apply")
	}
	for _, d := range []string{fresh, logs, foreign, hidden} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s touched: %v", d, err)
		}
	}
}
