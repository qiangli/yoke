package weave

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/ladder"
)

func sprintGradeFixture(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	dir := t.TempDir()
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.Config()
	cfg.User.Name = "Manager"
	cfg.User.Email = "manager@example.invalid"
	if err = r.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	base := sprintGradeCommit(t, dir, "pkg/code_test.go", "original", "base")
	head := sprintGradeCommit(t, dir, "code.go", "implementation", "Implement story\n\nSprint: #331\nStory: #1217\nStory-ID: 4474b08d8299\n")
	return dir, base, head
}

func sprintGradeCommit(t *testing.T, dir, name, body, message string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	if _, err = w.Add(name); err != nil {
		t.Fatal(err)
	}
	h, err := w.Commit(message, &gogit.CommitOptions{Author: &object.Signature{Name: "Attempt Author", Email: "author@example.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}

func TestGradeFreshGateAndTamper(t *testing.T) {
	for _, tc := range []struct {
		name, gate, verdict string
		tamper              bool
	}{
		{"pass", "test ! -e residue; pwd; touch residue", "pass", false},
		{"fail", "exit 7", "fail", false},
		{"tamper", "exit 0", "tamper", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, _ := sprintGradeFixture(t)
			if tc.tamper {
				sprintGradeCommit(t, dir, "pkg/code_test.go", "weakened", "change tests")
			}
			fork := filepath.Join(t.TempDir(), "fork.git")
			if _, err := gogit.PlainClone(fork, true, &gogit.CloneOptions{URL: dir}); err != nil {
				t.Fatal(err)
			}
			item := &weaveItem{ID: 1, Workspace: dir, Branch: "master", BoothForkURL: fork, BaseSHA: base}
			for i := 0; i < 2; i++ {
				ev, err := sprintGradeAttempt(context.Background(), 331, "repo#1", item, "", base, tc.gate, nil, time.Second*5)
				if err != nil {
					t.Fatal(err)
				}
				if ev.Verdict != tc.verdict || ev.Tamper != tc.tamper {
					t.Fatalf("grade = %+v", ev)
				}
				rel, _ := filepath.Rel(dir, ev.Checkout)
				if !strings.HasPrefix(rel, "..") {
					t.Fatalf("checkout inside workspace: %s", ev.Checkout)
				}
				if tc.name == "fail" && ev.GateExit != 7 {
					t.Fatalf("exit = %d", ev.GateExit)
				}
			}
		})
	}
}

func TestMergeRequiresGradeAndDominance(t *testing.T) {
	dir, base, head := sprintGradeFixture(t)
	s := &weaveStory{}
	if _, err := sprintGradeLatest(s, "repo#1", "generation"); err == nil {
		t.Fatal("accepted missing grade")
	}
	if err := sprintGradeDominance("agent-a", "agent-b", nil); err == nil || !strings.Contains(err.Error(), "escalate") {
		t.Fatalf("missing evidence: %v", err)
	}
	r, _ := gogit.PlainOpen(dir)
	w, _ := r.Worktree()
	if err := w.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: plumbing.NewHash(base)}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.git")
	sr, err := gogit.PlainClone(source, true, &gogit.CloneOptions{URL: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err = sr.Storer.SetReference(plumbing.NewHashReference("refs/heads/attempt", plumbing.NewHash(head))); err != nil {
		t.Fatal(err)
	}
	ev := sprintGradeEvent{Run: "repo#1", Commit: head, Verdict: "pass", GateExit: 0}
	if _, err = sprintGradeMerge(context.Background(), dir, source, "attempt", nil, ev, "agent-a"); err != nil {
		t.Fatal(err)
	}
	ref, _ := r.Head()
	c, _ := r.CommitObject(ref.Hash())
	if len(c.ParentHashes) != 2 || c.ParentHashes[1].String() != head {
		t.Fatalf("parents: %v", c.ParentHashes)
	}
	for _, trailer := range []string{"Sprint: #331", "Story: #1217", "Story-ID: 4474b08d8299", "Agent: agent-a"} {
		if !strings.Contains(c.Message, trailer) {
			t.Fatalf("missing %s in %s", trailer, c.Message)
		}
	}
	original, _ := r.CommitObject(plumbing.NewHash(head))
	if original.Author.Name != "Attempt Author" {
		t.Fatal("author changed")
	}
}

func TestGradeMergeCommands(t *testing.T) {
	dir, base, _ := sprintGradeFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "agent-manager")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	t.Setenv(sprintLeaseTokenEnv, "secret")
	fork := filepath.Join(t.TempDir(), "fork.git")
	if _, err := gogit.PlainClone(fork, true, &gogit.CloneOptions{URL: dir}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real")
	r, err := gogit.PlainClone(target, false, &gogit.CloneOptions{URL: dir})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := r.Config()
	cfg.User.Name = "Manager"
	cfg.User.Email = "manager@example.invalid"
	if err = r.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	if err = w.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: plumbing.NewHash(base)}); err != nil {
		t.Fatal(err)
	}
	queue, err := weaveQueueDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	item := &weaveItem{ID: 1, Owner: "agent-a", Workspace: dir, Branch: "master", BoothForkURL: fork, BaseSHA: base, Created: time.Now().UTC()}
	if err = saveWeaveQueue(queue, &weaveQueue{Root: dir, NextID: 2, Items: []*weaveItem{item}}); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Base(dir)
	run := repo + "#1"
	s := &weaveStory{ID: 331, Lease: &weaveStoryLease{Holder: "agent-manager", TokenHash: sprintLeaseTokenHash("secret"), At: time.Now()}, Runs: []sprintRun{{Repo: repo, Queue: filepath.Base(queue), ID: 1, Born: item.Created}}, Arena: &sprintArena{Repos: []arenaRepo{{Repo: repo, Base: base}}}}
	board, _ := sprintStoreDir()
	if err = withWeaveQueueLock(board, func(q *weaveQueue) error { q.Stories = append(q.Stories, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "merge", "331", "--run", run, "--into", target); code == 0 || !strings.Contains(out, "passing grade") {
		t.Fatalf("missing grade: %d %s", code, out)
	}
	if out, code := runSprint(t, "grade", "331", "--run", run, "--gate", "exit 0", "--json"); code != 0 || !strings.Contains(out, `"verdict":"pass"`) {
		t.Fatalf("grade: %d %s", code, out)
	}
	if out, code := runSprint(t, "merge", "331", "--run", run, "--into", target, "--reviewer", "agent-b"); code == 0 || !strings.Contains(out, "escalate") {
		t.Fatalf("dominance: %d %s", code, out)
	}
	t.Setenv(sprintLeaseTokenEnv, "wrong")
	if out, code := runSprint(t, "merge", "331", "--run", run, "--into", target); code == 0 {
		t.Fatalf("invalid token allowed: %s", out)
	}
	t.Setenv(sprintLeaseTokenEnv, "secret")
	if out, code := runSprint(t, "grade", "331", "--run", run, "--gate", "exit 3", "--json"); code == 0 || !strings.Contains(out, `"gate_exit":3`) {
		t.Fatalf("failed grade: %d %s", code, out)
	}
	if out, code := runSprint(t, "merge", "331", "--run", run, "--into", target); code == 0 {
		t.Fatalf("latest failure ignored: %s", out)
	}
	if out, code := runSprint(t, "grade", "331", "--run", run, "--gate", "exit 0"); code != 0 {
		t.Fatalf("regrade: %d %s", code, out)
	}
	if out, code := runSprint(t, "merge", "331", "--run", run, "--into", target, "--json"); code != 0 || !strings.Contains(out, `"merge_commit"`) {
		t.Fatalf("merge: %d %s", code, out)
	}
	q, err := loadWeaveQueue(board)
	if err != nil {
		t.Fatal(err)
	}
	card := findWeaveStory(q, 331)
	kinds := []string{}
	for _, c := range card.Thread {
		if c.Kind == "grade" || c.Kind == "merge" {
			kinds = append(kinds, c.Kind)
		}
	}
	if strings.Join(kinds, ",") != "grade,grade,grade,merge" {
		t.Fatalf("events = %v", kinds)
	}
	if _, err = os.Stat(dir); err != nil {
		t.Fatal("workspace was deleted")
	}
}

func TestGradeDeletedTestsAndCustomGlobs(t *testing.T) {
	dir, base, _ := sprintGradeFixture(t)
	r, _ := gogit.PlainOpen(dir)
	w, _ := r.Worktree()
	if _, err := w.Remove("pkg/code_test.go"); err != nil {
		t.Fatal(err)
	}
	hash, err := w.Commit("delete test", &gogit.CommitOptions{Author: &object.Signature{Name: "Author", Email: "author@example.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	bc, _ := r.CommitObject(plumbing.NewHash(base))
	ac, _ := r.CommitObject(hash)
	tamper, err := sprintGradeTamper(bc, ac, nil)
	if err != nil || !tamper {
		t.Fatalf("deleted test: %t %v", tamper, err)
	}
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{{"tests/**", "tests/a/b.txt", true}, {"**/checks/*.txt", "checks/key.txt", true}, {"**/checks/*.txt", "pkg/checks/key.txt", true}, {"*_test.go", "pkg/a_test.go", true}, {"tests/**", "pkg/a.go", false}} {
		got, err := sprintGradeMatch(tc.pattern, tc.name)
		if err != nil || got != tc.want {
			t.Fatalf("%s %s = %t %v", tc.pattern, tc.name, got, err)
		}
	}
	if _, err = sprintGradeTamper(bc, ac, []string{"["}); err == nil {
		t.Fatal("invalid pattern allowed")
	}
}

func TestMergeDominanceReplay(t *testing.T) {
	now := time.Now()
	season := ladder.SeasonOf(now)
	events := []ladder.Event{{ID: "a", Agent: "agent-a", Kind: ladder.EventKindSeed, Duty: ladder.DutyCode, SeedR: 1600, SeedRD: 150, Season: season, At: now}, {ID: "b", Agent: "agent-b", Kind: ladder.EventKindSeed, Duty: ladder.DutyCode, SeedR: 1800, SeedRD: 150, Season: season, At: now}}
	if err := sprintGradeDominance("agent-b", "agent-a", events); err == nil {
		t.Fatal("point rating incorrectly used instead of conservative rating")
	}
	events[1].SeedR = 2000
	if err := sprintGradeDominance("agent-b", "agent-a", events); err != nil {
		t.Fatal(err)
	}
}

func TestGradeTimeoutAndWorkspaceFallback(t *testing.T) {
	dir, base, _ := sprintGradeFixture(t)
	it := &weaveItem{ID: 1, Workspace: dir, Branch: "master", BaseSHA: base}
	ev, err := sprintGradeAttempt(context.Background(), 331, "repo#1", it, "", base, "while :; do :; done", nil, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Verdict != "fail" || ev.GateExit == 0 {
		t.Fatalf("timeout = %+v", ev)
	}
	t.Setenv("BASHY_HOME", filepath.Join(dir, "state"))
	if _, err = sprintGradeAttempt(context.Background(), 331, "repo#1", it, "", base, "exit 0", nil, time.Second); err == nil {
		t.Fatal("grade allowed inside workspace")
	}
}

func TestMergeRejectsChangedAttempt(t *testing.T) {
	dir, base, head := sprintGradeFixture(t)
	target := filepath.Join(t.TempDir(), "real")
	r, err := gogit.PlainClone(target, false, &gogit.CloneOptions{URL: dir})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := r.Worktree()
	if err = w.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: plumbing.NewHash(base)}); err != nil {
		t.Fatal(err)
	}
	sprintGradeCommit(t, dir, "new.go", "new", "new attempt")
	_, err = sprintGradeMerge(context.Background(), target, dir, "master", nil, sprintGradeEvent{Commit: head, Verdict: "pass"}, "agent-a")
	if err == nil || !strings.Contains(err.Error(), "changed since grade") {
		t.Fatalf("stale grade: %v", err)
	}
	ref, _ := r.Head()
	if ref.Hash().String() != base {
		t.Fatal("real branch moved on rejection")
	}
}

func TestGradeBoothCredentials(t *testing.T) {
	dir := t.TempDir()
	if _, err := boothCredentialFile(dir, 1, "http://localhost:3000/booth/fork.git", "booth-user", "secret"); err != nil {
		t.Fatal(err)
	}
	item := &weaveItem{ID: 1, Branch: "attempt", BoothForkURL: "http://localhost:3000/booth/fork.git", BoothUser: "booth-user"}
	source, auth, err := sprintGradeSource(item, dir)
	if err != nil || auth == nil || strings.Contains(source, "secret") || strings.Contains(source, "booth-user@") {
		t.Fatalf("credentials: %q %v", source, err)
	}
	item.BoothForkURL = "http://localhost:3000/other/fork.git"
	if _, _, err = sprintGradeSource(item, dir); err == nil {
		t.Fatal("credentials accepted for a different fork")
	}
}

func TestMergeManagerRequiredInAdvisoryMode(t *testing.T) {
	t.Setenv("BASHY_SPRINT_ENFORCE", "should")
	t.Setenv(sprintLeaseTokenEnv, "wrong")
	s := &weaveStory{}
	if err := sprintGradeManager(s); err == nil {
		t.Fatal("no lease allowed")
	}
	s.Lease = &weaveStoryLease{TokenHash: sprintLeaseTokenHash("secret")}
	if err := sprintGradeManager(s); err == nil {
		t.Fatal("wrong token allowed")
	}
	t.Setenv(sprintLeaseTokenEnv, "secret")
	if err := sprintGradeManager(s); err != nil {
		t.Fatal(err)
	}
}
