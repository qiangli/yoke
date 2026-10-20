package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wrapupScopeFixture builds the shape Sprint 454 could not close in: an
// umbrella workspace holding the checkout this sprint links (yoke, carrying one
// delivery commit of its own and one another sprint delivered) and a sibling it
// only PINS (sh, stale, nobody linking it). The nested checkouts are ignored by
// the umbrella, so the only state under test is what each case writes.
func wrapupScopeFixture(t *testing.T) (string, string) {
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
		".gitignore":            "yoke/\nsh/\n",
		"go.work":               "go 1.24\n\nuse (\n\t./yoke\n\t./sh\n)\n",
		"yoke/go.mod":           "module example.com/yoke\n\ngo 1.24\n\nrequire example.com/sh v0.0.0-20200101000000-0123456789ab\n",
		"yoke/pkg/own/own.go":   "package own\n",
		"yoke/other/foreign.go": "package other\n",
		"sh/go.mod":             "module example.com/sh\n\ngo 1.24\n",
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
	for _, repo := range []string{"yoke", "sh"} {
		nested := filepath.Join(root, repo)
		git(nested, "init", "-q")
		git(nested, "config", "user.name", "test")
		git(nested, "config", "user.email", "test@example.invalid")
		git(nested, "add", ".")
		git(nested, "commit", "-qm", "base")
	}
	// THE EVIDENCE THE SPRINT ALREADY RECORDS: delivery commits carry the
	// sprint number, so each sprint's footprint in the checkout is readable.
	yoke := filepath.Join(root, "yoke")
	for _, c := range []struct{ file, message string }{
		{"pkg/own/own.go", "yoke: our work\n\nSprint: #1\nStory: #10\nStory-ID: 1111aaaa1111"},
		{"other/foreign.go", "yoke: the other sprint's work\n\nSprint: #2\nStory: #20\nStory-ID: 2222bbbb2222"},
	} {
		p := filepath.Join(yoke, filepath.FromSlash(c.file))
		if err := os.WriteFile(p, []byte("package x // delivered\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(yoke, "commit", "-aqm", c.message)
	}
	queue, err := weaveQueueDir(yoke)
	if err != nil {
		t.Fatal(err)
	}
	born := q.Stories[0].Boxes[0].StartedAt
	if err := saveWeaveQueue(queue, &weaveQueue{Root: yoke, Items: []*weaveItem{
		{ID: 1, Created: born, State: "done", Disposition: weaveDispositionMerged},
	}}); err != nil {
		t.Fatal(err)
	}
	q.Stories[0].Runs = []sprintRun{{Repo: "yoke", Queue: filepath.Base(queue), ID: 1, Born: born}}
	git(root, "add", ".")
	git(root, "commit", "-qm", "umbrella")
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

// Sprint 454 (and 476) could not end over uncommitted files another live
// session owned, in a checkout the sprint links. Wrap-up scopes its checks to
// what the sprint touched; foreign dirt is a warning naming the owner.
func TestSprintEndWarnsOnForeignDirtItNeverTouched(t *testing.T) {
	root, dir := wrapupScopeFixture(t)
	foreign := filepath.Join(root, "yoke", "other", "foreign.go")
	if err := os.WriteFile(foreign, []byte("package other // #2 live edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("another session's dirt blocked end: %s", out)
	}
	if !strings.Contains(out, "uncommitted other/foreign.go (sprint #2)") {
		t.Errorf("end did not warn naming the owner: %s", out)
	}
	after, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stories[0].Column != "done" {
		t.Fatalf("sprint #1 not ended: %+v", after.Stories[0])
	}
	if data, err := os.ReadFile(foreign); err != nil || !strings.Contains(string(data), "#2 live edit") {
		t.Fatalf("end touched the other session's file: %q %v", data, err)
	}
}

// The narrowing is scoped, not a waiver: dirt in a path this sprint's own
// commits touched — or in a directory they touched, which is where a new file
// of the sprint's own work lands — still refuses.
func TestSprintEndStillRefusesDirtItTouched(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"own file", "pkg/own/own.go"},
		{"new file beside its own work", "pkg/own/new.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := wrapupScopeFixture(t)
			p := filepath.Join(root, "yoke", filepath.FromSlash(tc.file))
			if err := os.WriteFile(p, []byte("package own // unfinished\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, code := runSprint(t, "end", "1")
			if code == 0 || !strings.Contains(out, "uncommitted file") {
				t.Fatalf("end accepted its own uncommitted work: %s", out)
			}
		})
	}
}

// A sibling pin trails whoever moved the sibling's HEAD. Sprint 454 was refused
// over pins in deps it never changed, in two repos, with no way to answer them.
func TestSprintEndWarnsOnPinDriftItNeverCaused(t *testing.T) {
	_, dir := wrapupScopeFixture(t)
	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("drift in a dep the sprint never changed blocked end: %s", out)
	}
	if !strings.Contains(out, "stale pin sh") {
		t.Errorf("end did not warn about the pin: %s", out)
	}
	after, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stories[0].Column != "done" {
		t.Fatalf("sprint #1 not ended: %+v", after.Stories[0])
	}
}

// A pin trailing a commit THIS sprint made in the sibling is its own chore: the
// pin is the only sibling source CI reads, so that one still refuses.
func TestSprintEndStillRefusesPinDriftItCaused(t *testing.T) {
	root, _ := wrapupScopeFixture(t)
	sh := filepath.Join(root, "sh")
	if out, err := gitOutputForTest(sh, "commit", "--allow-empty", "-qm",
		"sh: our work\n\nSprint: #1\nStory: #10\nStory-ID: 1111aaaa1111"); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	out, code := runSprint(t, "end", "1")
	if code == 0 || !strings.Contains(out, "stale pin(s): sh") {
		t.Fatalf("end accepted a pin trailing its own commit: %s", out)
	}
}
