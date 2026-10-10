package weave

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sharedEndFixture puts sprint #1 and active sprint #2 on one shared checkout
// (the umbrella's yoke) plus a sibling sh that yoke pins stale. states maps a
// sprint to its yoke run state; shLinks lists the sprints linking sh.
func sharedEndFixture(t *testing.T, states map[int64]string, shLinks ...int64) (string, string) {
	t.Helper()
	t.Setenv("GOWORK", "")
	root, dir, q := concurrentCloseFixture(t)
	git := func(where string, args ...string) {
		t.Helper()
		if out, err := gitOutputForTest(where, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	files := map[string]string{
		"go.work":      "go 1.24\n\nuse (\n\t./yoke\n\t./sh\n)\n",
		"yoke/go.mod":  "module example.com/yoke\n\ngo 1.24\n\nrequire example.com/sh v0.0.0-20200101000000-0123456789ab\n",
		"yoke/file.go": "package yoke\n",
		"sh/go.mod":    "module example.com/sh\n\ngo 1.24\n",
	}
	for rel, data := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	queues := map[string]string{}
	for _, repo := range []string{"yoke", "sh"} {
		nested := filepath.Join(root, repo)
		git(nested, "init", "-q")
		git(nested, "config", "user.name", "test")
		git(nested, "config", "user.email", "test@example.invalid")
		git(nested, "add", ".")
		git(nested, "commit", "-qm", "base")
		queue, err := weaveQueueDir(nested)
		if err != nil {
			t.Fatal(err)
		}
		queues[repo] = queue
	}
	born := q.Stories[0].Boxes[0].StartedAt
	yoke := &weaveQueue{Root: filepath.Join(root, "yoke")}
	for i, s := range q.Stories {
		id := int64(i + 1)
		item := &weaveItem{ID: id, Created: born, State: states[s.ID]}
		if isPrunableState(item.State) {
			item.Disposition = weaveDispositionMerged
		}
		yoke.Items = append(yoke.Items, item)
		s.Runs = []sprintRun{{Repo: "yoke", Queue: filepath.Base(queues["yoke"]), ID: id, Born: born}}
	}
	sh := &weaveQueue{Root: filepath.Join(root, "sh")}
	for _, owner := range shLinks {
		sh.Items = append(sh.Items, &weaveItem{ID: owner, Created: born, State: "done", Disposition: weaveDispositionMerged})
		s := q.Stories[owner-1]
		s.Runs = append(s.Runs, sprintRun{Repo: "sh", Queue: filepath.Base(queues["sh"]), ID: owner, Born: born})
	}
	for path, wq := range map[string]*weaveQueue{queues["yoke"]: yoke, queues["sh"]: sh} {
		if err := saveWeaveQueue(path, wq); err != nil {
			t.Fatal(err)
		}
	}
	git(root, "add", ".")
	git(root, "commit", "-qm", "umbrella")
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

// Sprint 329 could not end while Sprint 413 was editing the shared yoke
// checkout and had moved sh past yoke's pin. Neither state was #329's.
func TestSprintEndSharedCheckoutAttributesOtherActiveSprint(t *testing.T) {
	root, dir := sharedEndFixture(t, map[int64]string{1: "done", 2: "working"}, 2)
	dirty := filepath.Join(root, "yoke", "file.go")
	if err := os.WriteFile(dirty, []byte("package yoke // #2 live edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("other sprint's dirt and pin blocked end: %s", out)
	}
	for _, want := range []string{"uncommitted file.go (sprint #2)", "stale pin sh (sprint #2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("end did not warn %q: %s", want, out)
		}
	}
	after, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stories[0].Column != "done" {
		t.Fatalf("sprint #1 not ended: %+v", after.Stories[0])
	}
	other := after.Stories[1]
	if other.currentBox() == nil || other.Column != before.Stories[1].Column || !reflect.DeepEqual(other.Runs, before.Stories[1].Runs) {
		t.Fatalf("end touched sprint #2: %+v", other)
	}
	if data, err := os.ReadFile(dirty); err != nil || !strings.Contains(string(data), "#2 live edit") {
		t.Fatalf("end touched sprint #2's file: %q %v", data, err)
	}
	queue, err := weaveQueueDir(filepath.Join(root, "yoke"))
	if err != nil {
		t.Fatal(err)
	}
	yoke, err := loadWeaveQueue(queue)
	if err != nil {
		t.Fatal(err)
	}
	if it := findWeaveItem(yoke, 2); it == nil || it.State != "working" {
		t.Fatalf("end touched sprint #2's run: %+v", it)
	}
}

// Shared ownership stays ours unless the run records settle it, and a pin on
// a sibling we produced in (or nobody did) is never waved through.
func TestSprintEndSharedCheckoutRefusesOwnedState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verb    string
		states  map[int64]string
		shLinks []int64
		dirty   bool
		want    string
	}{
		{"ours still working", "stop", map[int64]string{1: "working", 2: "working"}, []int64{2}, true, "1 uncommitted file"},
		{"other not producing", "end", map[int64]string{1: "done", 2: "done"}, []int64{2}, true, "1 uncommitted file"},
		{"own pin", "end", map[int64]string{1: "done", 2: "working"}, []int64{1, 2}, false, "stale pin(s): sh"},
		{"unattributed pin", "end", map[int64]string{1: "done", 2: "working"}, nil, false, "stale pin(s): sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := sharedEndFixture(t, tc.states, tc.shLinks...)
			if tc.dirty {
				if err := os.WriteFile(filepath.Join(root, "yoke", "file.go"), []byte("package yoke // edit\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, code := runSprint(t, tc.verb, "1")
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("%s accepted owned state (want %q): %s", tc.verb, tc.want, out)
			}
		})
	}
}
