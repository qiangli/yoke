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
	todopkg "github.com/qiangli/yoke/pkg/todo"
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
			if err := sprintGradePushAttempt(context.Background(), item, ""); err != nil {
				t.Fatal(err)
			}
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
	if _, err = sprintGradeMerge(context.Background(), dir, source, "attempt", nil, ev, "agent-a", map[string]string{"Sprint": "#331", "Story": "#1217", "Story-ID": "4474b08d8299"}); err != nil {
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
	for _, tc := range []struct{ name, message, link, want string }{
		{"blind-register", "Blind implementation", "register", ""},
		{"blind-story", "Blind implementation", "story", ""},
		{"matching", "Implement\n\nSprint: #331\nStory: #1217\nStory-ID: 4474b08d8299\n", "register", ""},
		{"sprint-mismatch", "Implement\n\nSprint: #332", "register", "Sprint trailer disagrees"},
		{"story-mismatch", "Implement\n\nStory: #1218", "register", "Story trailer disagrees"},
		{"id-mismatch", "Implement\n\nStory-ID: other", "register", "Story-ID trailer disagrees"},
		{"unlinked", "Blind implementation", "", "run is not linked to a story; link it before merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, _ := sprintGradeFixture(t)
			attemptRepo, err := gogit.PlainOpen(dir)
			if err != nil {
				t.Fatal(err)
			}
			attemptTree, err := attemptRepo.Worktree()
			if err != nil {
				t.Fatal(err)
			}
			if err := attemptTree.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: plumbing.NewHash(base)}); err != nil {
				t.Fatal(err)
			}
			head := sprintGradeCommit(t, dir, "code.go", "blind implementation", tc.message)
			storyRoot := t.TempDir()
			story, err := todopkg.Add(todopkg.RepoStore(storyRoot), "implement", "body", "p1", nil, "", "")
			if err != nil {
				t.Fatal(err)
			}
			story.ID, story.Seq, story.Sprint = "4474b08d8299", 1217, 331
			if tc.link == "story" {
				story.Weave = 1
			}
			if _, err = todopkg.RepoStore(storyRoot).Save(story); err != nil {
				t.Fatal(err)
			}
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
			queue, err := weaveQueueDir(storyRoot)
			if err != nil {
				t.Fatal(err)
			}
			item := &weaveItem{ID: 1, Owner: "agent-a", Workspace: dir, Branch: "master", BoothForkURL: fork, BaseSHA: base, Created: time.Now().UTC()}
			if tc.link == "register" {
				item.Register = story.ID
			}
			if err := sprintGradePushAttempt(context.Background(), item, ""); err != nil {
				t.Fatal(err)
			}
			if err = saveWeaveQueue(queue, &weaveQueue{Root: storyRoot, NextID: 2, Items: []*weaveItem{item}}); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Base(storyRoot)
			run := repo + "#1"
			s := &weaveStory{ID: 331, StoryRoots: []string{storyRoot}, Lease: &weaveStoryLease{Holder: "agent-manager", TokenHash: sprintLeaseTokenHash("secret"), At: time.Now()}, Runs: []sprintRun{{Repo: repo, Queue: filepath.Base(queue), ID: 1, Born: item.Created}}, Arena: &sprintArena{Repos: []arenaRepo{{Repo: repo, Base: base}}}}
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
			if tc.want != "" {
				out, code := runSprint(t, "merge", "331", "--run", run, "--into", target)
				if code == 0 || !strings.Contains(out, tc.want) {
					t.Fatalf("merge: %d %s; want %s", code, out, tc.want)
				}
				ref, _ := r.Head()
				if ref.Hash().String() != base {
					t.Fatal("target moved on rejection")
				}
				return
			}
			for _, name := range []string{"a-dirty", "b-dirty", "c-dirty", "d-dirty", "e-dirty", "f-dirty"} {
				if err := os.WriteFile(filepath.Join(target, name), []byte("dirty"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, code := runSprint(t, "merge", "331", "--run", run, "--into", target)
			if code == 0 || !strings.Contains(out, "merge target is not clean") {
				t.Fatalf("dirty: %d %s", code, out)
			}
			for _, name := range []string{"a-dirty", "b-dirty", "c-dirty", "d-dirty", "e-dirty"} {
				if !strings.Contains(out, name) {
					t.Fatalf("missing dirty path %s: %s", name, out)
				}
			}
			if strings.Contains(out, "f-dirty") {
				t.Fatalf("more than five paths: %s", out)
			}
			for _, name := range []string{"a-dirty", "b-dirty", "c-dirty", "d-dirty", "e-dirty", "f-dirty"} {
				if err := os.Remove(filepath.Join(target, name)); err != nil {
					t.Fatal(err)
				}
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
			ref, err := r.Head()
			if err != nil {
				t.Fatal(err)
			}
			merged, err := r.CommitObject(ref.Hash())
			if err != nil {
				t.Fatal(err)
			}
			if len(merged.ParentHashes) != 2 || merged.ParentHashes[1].String() != head {
				t.Fatalf("parents: %v", merged.ParentHashes)
			}
			for _, trailer := range []string{"Sprint: #331", "Story: #1217", "Story-ID: 4474b08d8299", "Agent: agent-a"} {
				if !strings.Contains(merged.Message, trailer) {
					t.Fatalf("missing %s: %s", trailer, merged.Message)
				}
			}
			original, err := r.CommitObject(plumbing.NewHash(head))
			if err != nil {
				t.Fatal(err)
			}
			if original.Message != tc.message || original.Author.Name != "Attempt Author" {
				t.Fatal("attempt changed")
			}
		})
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
	_, err = sprintGradeMerge(context.Background(), target, dir, "master", nil, sprintGradeEvent{Commit: head, Verdict: "pass"}, "agent-a", nil)
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

func TestGradeBoothStoredHostCredential(t *testing.T) {
	dir := t.TempDir()
	user := "booth-1-a1b2c3d4-7"
	fork := "http://localhost:3000/" + user + "/repo.git"
	file, err := boothCredentialFile(dir, 7, fork, user, "secret")
	if err != nil {
		t.Fatal(err)
	}
	// git credential-store rewrites credentials at host scope by default.
	if err = os.WriteFile(file, []byte("http://"+user+":secret@localhost%3a3000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	item := &weaveItem{ID: 7, Branch: "agent/work", BoothUser: user, BoothForkURL: fork}
	if _, auth, err := sprintGradeSource(item, dir); err != nil || auth == nil {
		t.Fatalf("host credential: %v", err)
	}
	item.BoothUser = "booth-other"
	if _, _, err := sprintGradeSource(item, dir); err == nil {
		t.Fatal("accepted another booth")
	}
}

func TestGradeAllowTestChangeFlag(t *testing.T) {
	cmd := newSprintGradeCommands()[0]
	if err := cmd.ParseFlags([]string{"--allow-test-change", "tests/**", "--allow-test-change", "*_test.go"}); err != nil {
		t.Fatal(err)
	}
}

func TestGradeBoothPublishAndMerge(t *testing.T) {
	dir, base, _ := sprintGradeFixture(t)
	// Track one original skill so filtering must preserve the pinned base.
	base = sprintGradeCommit(t, dir, ".agents/skills/original/SKILL.md", "base skill", "base skills")
	fork := filepath.Join(t.TempDir(), "fork.git")
	if _, err := gogit.PlainClone(fork, true, &gogit.CloneOptions{URL: dir}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	tr, err := gogit.PlainClone(target, false, &gogit.CloneOptions{URL: dir})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tr.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.User.Name, cfg.User.Email = "Manager", "manager@example.invalid"
	if err := tr.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	sprintGradeCommit(t, dir, ".agents/skills/original/SKILL.md", "provisioned", "provision skills")
	sprintGradeCommit(t, dir, ".claude/skills/added/SKILL.md", "provisioned", "provision more skills")
	sprintGradeCommit(t, dir, "pkg/code_test.go", "updated", "Update tests\n\nSprint: #331\nStory: #1217\nStory-ID: 4474b08d8299\n")
	head := sprintGradeCommit(t, dir, ".agents/skills/extra/SKILL.md", "more scaffolding", "weave(auto): provisioned skills")
	item := &weaveItem{ID: 1, ArenaSprint: 331, Workspace: dir, Branch: "master", BoothForkURL: fork, BaseSHA: base}
	if err := sprintGradePushAttempt(context.Background(), item, ""); err != nil {
		t.Fatal(err)
	}
	local, _ := gogit.PlainOpen(dir)
	ref, _ := local.Head()
	if ref.Hash().String() != head {
		t.Fatal("publication changed workspace HEAD")
	}
	ev, err := sprintGradeAttempt(context.Background(), 331, "repo#1", item, "", base, "exit 0", nil, time.Second*5)
	if err != nil || ev.Verdict != "tamper" {
		t.Fatalf("default protection: %+v %v", ev, err)
	}
	ev, err = sprintGradeAttempt(context.Background(), 331, "repo#1", item, "", base, "exit 0", nil, time.Second*5, "pkg/*_test.go")
	if err != nil || ev.Verdict != "pass" {
		t.Fatalf("allowed test update: %+v %v", ev, err)
	}
	if len(ev.AllowTestChange) != 1 {
		t.Fatal("allowance missing from event")
	}
	published, _ := gogit.PlainOpen(fork)
	c, err := published.CommitObject(plumbing.NewHash(ev.Commit))
	if err != nil {
		t.Fatal(err)
	}
	for c.Hash.String() != base {
		tree, err := c.Tree()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tree.FindEntry(".claude/skills/added/SKILL.md"); err == nil {
			t.Fatal("provisioned skill published")
		}
		f, err := tree.File(".agents/skills/original/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := f.Contents()
		if body != "base skill" {
			t.Fatal("tracked skill changed")
		}
		c, err = c.Parent(0)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = sprintGradeMerge(context.Background(), target, fork, "attempt", nil, ev, "agent-a", map[string]string{"Sprint": "#331", "Story": "#1217", "Story-ID": "4474b08d8299"}); err != nil {
		t.Fatal(err)
	}
	item.BoothForkURL = filepath.Join(t.TempDir(), "missing")
	if err := sprintGradePushAttempt(context.Background(), item, ""); err == nil {
		t.Fatal("push failure swallowed")
	}
}

func TestGradeAllowTestChangeScope(t *testing.T) {
	dir, base, _ := sprintGradeFixture(t)
	head := sprintGradeCommit(t, dir, "pkg/code_test.go", "updated", "update")
	r, _ := gogit.PlainOpen(dir)
	bc, _ := r.CommitObject(plumbing.NewHash(base))
	ac, _ := r.CommitObject(plumbing.NewHash(head))
	for _, tc := range []struct {
		allow   string
		tamper  bool
		invalid bool
	}{
		{"tests/**", true, false}, {"pkg/*_test.go", false, false}, {"[", false, true},
	} {
		got, err := sprintGradeTamper(bc, ac, nil, tc.allow)
		if (err != nil) != tc.invalid || got != tc.tamper {
			t.Fatalf("%q: %t %v", tc.allow, got, err)
		}
	}
}
