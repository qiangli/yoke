package weave

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSprintResourceInventorySeparatesRecycledRunsAndKeepsCompetitors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	store := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", store)
	write := func(path string, q weaveQueue) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(q)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	born := time.Now().UTC()
	write(filepath.Join(store, "queue.json"), weaveQueue{Stories: []*weaveStory{{ID: 138, Owner: "manager", Runs: []sprintRun{{Repo: "repo", Queue: "repo-hash", ID: 1, Born: born}, {Repo: "repo", Queue: "repo-hash", ID: 2, Born: born.Add(-time.Hour)}}}}})
	write(filepath.Join(weaveStateRoot(home), "repo-hash", "queue.json"), weaveQueue{Root: "/repo", Items: []*weaveItem{{ID: 1, Created: born, State: "working", Owner: "worker", Register: "todo-123", WrapperPid: 123, WrapperStartID: "fixture:old"}, {ID: 2, Created: born, State: "working", Owner: "competitor", WrapperPid: 456}}})
	cache := filepath.Join(home, "cache")
	got, err := ReadSprintInventory(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || len(got.Workloads) != 2 {
		t.Fatalf("inventory=%+v", got)
	}
	if got.Workloads[0].Sprint != 138 || got.Workloads[0].Todo != "todo-123" || got.Workloads[0].StartID != "fixture:old" || got.Workloads[1].Sprint != 0 || got.Workloads[0].ID == got.Workloads[1].ID {
		t.Fatalf("run generations/competitors lost: %+v", got.Workloads)
	}
	before, err := os.Stat(filepath.Join(cache, "sprint-inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReadSprintInventory(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(cache, "sprint-inventory.json"))
	if !second.At.Equal(got.At) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged observation rewrote shared cache")
	}
	if _, err := os.Stat(filepath.Join(store, "queue.lock")); !os.IsNotExist(err) {
		t.Fatalf("observation touched queue mutation lock: %v", err)
	}
}

func TestSprintResourceOwnerFenceRefusesFormerOwner(t *testing.T) {
	lease := seedSprintLease(t, "current")
	before := lease()
	sent := false
	err := WithSprintObservationOwner(context.Background(), 98, "former", func() error { sent = true; return nil })
	if err == nil || sent {
		t.Fatal("former owner received a resource notice")
	}
	if after := lease(); after != before {
		t.Fatal("observation changed lease")
	}
}

func TestSprintResourceInventoryRejectsFutureCacheAndPrioritizesActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	store := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", store)
	cache := filepath.Join(home, "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	b, _ := json.Marshal(SprintInventory{At: future, ExpiresAt: future.Add(time.Hour), Complete: true})
	os.WriteFile(filepath.Join(cache, "sprint-inventory.json"), b, 0600)
	board := weaveQueue{Stories: []*weaveStory{nil}}
	for i := int64(1); i <= 300; i++ {
		board.Stories = append(board.Stories, &weaveStory{ID: i})
	}
	board.Stories = append(board.Stories, &weaveStory{ID: 999, Owner: "active", Boxes: []weaveStoryBox{{StartedAt: time.Now()}}})
	os.MkdirAll(store, 0700)
	b, _ = json.Marshal(board)
	os.WriteFile(filepath.Join(store, "queue.json"), b, 0600)
	got, err := ReadSprintInventory(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	if got.At.Equal(future) || len(got.Sprints) != 256 || got.Sprints[0].ID != 999 || got.Complete {
		t.Fatalf("future cache or inactive rows hid active work: %+v", got)
	}
}

func TestSprintResourceInventoryPrioritizesKnownActiveQueueBeforeDiscoveryCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	store := filepath.Join(home, "sprint")
	t.Setenv("BASHY_SPRINT_DIR", store)
	write := func(path string, q weaveQueue) {
		t.Helper()
		os.MkdirAll(filepath.Dir(path), 0700)
		b, _ := json.Marshal(q)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	root := weaveStateRoot(home)
	for i := 0; i < 70; i++ {
		write(filepath.Join(root, fmt.Sprintf("old-%03d", i), "queue.json"), weaveQueue{Root: "/old"})
	}
	born := time.Now().UTC()
	write(filepath.Join(root, "active-last", "queue.json"), weaveQueue{Root: "/active", Items: []*weaveItem{{ID: 9, Created: born, State: "working", WrapperPid: 123}}})
	write(filepath.Join(store, "queue.json"), weaveQueue{Stories: []*weaveStory{{ID: 138, Boxes: []weaveStoryBox{{StartedAt: born}}, Runs: []sprintRun{{Queue: "active-last", ID: 9, Born: born}}}}})
	got, err := ReadSprintInventory(context.Background(), filepath.Join(home, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete || len(got.Workloads) != 1 || got.Workloads[0].Sprint != 138 {
		t.Fatalf("directory cap hid linked active queue: %+v", got)
	}
}
