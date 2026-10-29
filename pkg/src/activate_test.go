package src_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

const (
	s525a7Alice = "device:alice"
	s525a7Bob   = "device:bob"
	s525a7App   = "demoapp"
)

func s525a7AllowGate() state.ActivationGate {
	return state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{Verdict: "pass", EvidenceID: "ev-test"}, nil
	})
}

func s525a7Write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func s525a7Commit(t *testing.T, dir, msg string) string {
	t.Helper()
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath: dir, Message: msg,
		AuthorName: "s525a7", AuthorEmail: "s525a7@example.com",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return rev.Hash
}

func s525a7Checkout(t *testing.T, dir, branch string, create bool) {
	t.Helper()
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: branch, Create: create}); err != nil {
		t.Fatalf("checkout %s: %v", branch, err)
	}
}

// s525a7Fixture builds a work repo with release history and per-principal
// candidate commits, then clones it to home/src/demoapp with pristine at
// the old release. Layout:
//
//	P1: base release (pristine)
//	P2: child of P1, base.txt moves to the new release text
//	A:  child of P1, alice.txt gains the alice feature
//	B:  child of P1, bob.txt gains the bob feature
//	P3: child of P2, alice.txt moves again (conflicts with A's replay)
func s525a7Fixture(t *testing.T, home string) (dest, p1, p2, a, b, p3 string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if _, err := ygit.Init(ygit.InitOptions{Path: work}); err != nil {
		t.Fatalf("init: %v", err)
	}
	s525a7Write(t, work, "base.txt", "base1\n")
	s525a7Write(t, work, "alice.txt", "alice1\n")
	s525a7Write(t, work, "bob.txt", "bob1\n")
	p1 = s525a7Commit(t, work, "p1 release")

	if _, _, err := ygit.Branch(ygit.BranchOptions{RepoPath: work, Name: "side-a"}); err != nil {
		t.Fatalf("branch side-a: %v", err)
	}
	if _, _, err := ygit.Branch(ygit.BranchOptions{RepoPath: work, Name: "side-b"}); err != nil {
		t.Fatalf("branch side-b: %v", err)
	}
	s525a7Checkout(t, work, "side-a", false)
	s525a7Write(t, work, "alice.txt", "alice2-feature\n")
	a = s525a7Commit(t, work, "alice feature")
	s525a7Checkout(t, work, "side-b", false)
	s525a7Write(t, work, "bob.txt", "bob2-feature\n")
	b = s525a7Commit(t, work, "bob feature")
	s525a7Checkout(t, work, "master", false)
	s525a7Write(t, work, "base.txt", "base2-release\n")
	p2 = s525a7Commit(t, work, "p2 release")
	s525a7Write(t, work, "alice.txt", "alice3-release\n")
	p3 = s525a7Commit(t, work, "p3 release")

	dest = filepath.Join(home, "src", s525a7App)
	if _, err := gogit.PlainClone(dest, false, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", plumbing.NewHash(p1))); err != nil {
		t.Fatalf("pristine: %v", err)
	}
	return dest, p1, p2, a, b, p3
}

func s525a7State(t *testing.T, home, principal string) state.State {
	t.Helper()
	s, err := state.OpenAt(home, s525a7App, principal)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func s525a7Data(t *testing.T, home, principal string) string {
	t.Helper()
	s, err := state.OpenAt(home, s525a7App, principal)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadData("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func s525a7FileAt(t *testing.T, dest, commit, file string) string {
	t.Helper()
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := c.Tree()
	if err != nil {
		t.Fatal(err)
	}
	f, err := tr.File(file)
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestS525A7PerPrincipalActivation is the focused S525.A7 proof on a temp
// src store: an interrupted activation keeps the previous active revision,
// two principals activate concurrently without interference, an upgrade
// that conflicts for one principal records a review version while the
// other principal upgrades, and the private data directories are untouched
// throughout.
func TestS525A7PerPrincipalActivation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	dest, _, _, a, b, p3 := s525a7Fixture(t, home)
	gate := s525a7AllowGate()

	for principal, body := range map[string]string{s525a7Alice: "alice-private\n", s525a7Bob: "bob-private\n"} {
		s, err := state.OpenAt(home, s525a7App, principal)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteData("note.txt", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	dataUntouched := func(stage string) {
		t.Helper()
		if got := s525a7Data(t, home, s525a7Alice); got != "alice-private\n" {
			t.Fatalf("%s: alice data = %q", stage, got)
		}
		if got := s525a7Data(t, home, s525a7Bob); got != "bob-private\n" {
			t.Fatalf("%s: bob data = %q", stage, got)
		}
	}

	// A first activation seeds alice's previous revision.
	if _, err := src.Activate(home, s525a7App, a, s525a7Alice, src.ActivateOptions{Gate: gate}); err != nil {
		t.Fatalf("activate alice: %v", err)
	}

	// An interrupted activation keeps the previous active revision: the
	// switch never runs, so the record still names the earlier revision.
	interrupted := errors.New("simulated crash")
	if _, err := src.Activate(home, s525a7App, b, s525a7Alice,
		src.ActivateOptions{Gate: gate, PreSwitchHook: func() error { return interrupted }}); !errors.Is(err, interrupted) {
		t.Fatalf("interrupted activate err = %v, want the simulated crash", err)
	}
	if st := s525a7State(t, home, s525a7Alice); st.ActiveRevision != a || st.LastKnownGood != "" {
		t.Fatalf("interrupted activate left %+v, want active=%s with no last-known-good", st, a)
	}

	// A refused gate records nothing and switches nothing.
	refuse := state.GateFunc(func(candidate, base, principal string) (state.EvidenceRef, error) {
		return state.EvidenceRef{}, errors.New("no evidence for this candidate")
	})
	if _, err := src.Activate(home, s525a7App, b, s525a7Bob, src.ActivateOptions{Gate: refuse}); err == nil {
		t.Fatal("refused activate = nil error, want refusal")
	}
	if st := s525a7State(t, home, s525a7Bob); st.ActiveRevision != "" {
		t.Fatalf("refused activate switched bob to %s", st.ActiveRevision)
	}

	// Two principals activate concurrently without interference.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = src.Activate(home, s525a7App, a, s525a7Alice, src.ActivateOptions{Gate: gate})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = src.Activate(home, s525a7App, b, s525a7Bob, src.ActivateOptions{Gate: gate})
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent activate %d: %v", i, err)
		}
	}
	if st := s525a7State(t, home, s525a7Alice); st.ActiveRevision != a {
		t.Fatalf("alice active = %s, want %s", st.ActiveRevision, a)
	}
	if st := s525a7State(t, home, s525a7Bob); st.ActiveRevision != b || st.LastKnownGood != "" {
		t.Fatalf("bob state = %+v, want active=%s with no last-known-good", st, b)
	}
	dataUntouched("after activations")

	lines, err := src.PrincipalLines(dest)
	if err != nil || len(lines) != 2 {
		t.Fatalf("version lines = %+v, %v; want one per principal", lines, err)
	}
	for _, ln := range lines {
		if ln.Tip == "" {
			t.Fatalf("version line %+v has no tip", ln)
		}
	}
	if lines[0].Tip == lines[1].Tip {
		t.Fatal("both principals share one version line tip; want independent lines")
	}

	// The naming seam pins a name under the principal's namespace.
	if _, err := src.SetNamedVersion(home, s525a7App, s525a7Alice, "good", a); err != nil {
		t.Fatalf("name version: %v", err)
	}
	if _, err := src.SetNamedVersion(home, s525a7App, s525a7Alice, "good", b); err == nil {
		t.Fatal("renaming a version to another revision = nil error, want refusal")
	}

	// The CLI activates through the wired gate for an explicit principal.
	src.RegisterActivationGate(gate)
	defer src.RegisterActivationGate(nil)
	runCLI := func(args ...string) (string, error) {
		cmd := src.NewSrcCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	out, err := runCLI("activate", s525a7App, a, "--principal", s525a7Alice)
	if err != nil || !strings.Contains(out, "activated "+s525a7App+" for "+s525a7Alice) {
		t.Fatalf("activate CLI = %q, %v", out, err)
	}
	t.Setenv("BASHY_PRINCIPAL", "")
	if _, err := runCLI("activate", s525a7App, a); err == nil {
		t.Fatal("activate CLI without a principal = nil error, want refusal")
	}

	// Upgrade to the release that moved alice.txt: alice's line conflicts
	// and is kept losslessly with a review version, while bob's replays.
	lines, err = src.PrincipalLines(dest)
	if err != nil {
		t.Fatal(err)
	}
	tips := map[string]string{}
	for _, ln := range lines {
		tips[ln.Slug] = ln.Tip
	}
	conflicts, err := src.Upgrade(home, s525a7App, p3)
	if err == nil {
		t.Fatal("upgrade with a principal conflict = nil error, want the review report")
	}
	joined := strings.Join(conflicts, "\n")
	if !strings.Contains(joined, "device-alice") || !strings.Contains(joined, "alice.txt") {
		t.Fatalf("upgrade conflicts = %q, want alice's stuck line on alice.txt", joined)
	}
	if got, err := ygit.ResolveCommit(dest, "pristine"); err != nil || got != p3 {
		t.Fatalf("pristine = %s, %v; want the new release %s", got, err, p3)
	}
	after, err := src.PrincipalLines(dest)
	if err != nil {
		t.Fatal(err)
	}
	afterTips := map[string]string{}
	for _, ln := range after {
		afterTips[ln.Slug] = ln.Tip
	}
	if afterTips["device-alice"] != tips["device-alice"] {
		t.Fatal("alice's conflicted line moved; want it exactly where it was")
	}
	r, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatal(err)
	}
	reviewRef := plumbing.ReferenceName("refs/shaping/device-alice/review/" + p3[:12])
	review, err := r.Storer.Reference(reviewRef)
	if err != nil {
		t.Fatalf("review version %s: %v", reviewRef, err)
	}
	if review.Hash().String() != tips["device-alice"] {
		t.Fatal("review version does not preserve the stuck line tip")
	}
	if afterTips["device-bob"] == tips["device-bob"] {
		t.Fatal("bob's clean line did not advance onto the new release")
	}
	if anc, err := ygit.IsAncestor(dest, p3, afterTips["device-bob"]); err != nil || !anc {
		t.Fatalf("bob's line %s does not descend from the new release (%v)", afterTips["device-bob"], err)
	}
	if got := s525a7FileAt(t, dest, afterTips["device-bob"], "bob.txt"); got != "bob2-feature\n" {
		t.Fatalf("bob's upgraded line carries bob.txt = %q", got)
	}
	if got := s525a7FileAt(t, dest, afterTips["device-bob"], "base.txt"); got != "base2-release\n" {
		t.Fatalf("bob's upgraded line carries base.txt = %q, want the new release text", got)
	}
	if st := s525a7State(t, home, s525a7Alice); st.ActiveRevision != a {
		t.Fatalf("upgrade rewrote alice's active revision to %s", st.ActiveRevision)
	}
	if st := s525a7State(t, home, s525a7Bob); st.ActiveRevision != b {
		t.Fatalf("upgrade rewrote bob's active revision to %s", st.ActiveRevision)
	}
	if _, entries, err := ygit.Status(dest); err != nil || len(entries) != 0 {
		t.Fatalf("worktree dirty after upgrade (%v entries, %v)", len(entries), err)
	}
	if _, branches, err := ygit.Branch(ygit.BranchOptions{RepoPath: dest}); err != nil {
		t.Fatal(err)
	} else {
		for _, br := range branches {
			if strings.Contains(br, "tmp/shaping") {
				t.Fatalf("scratch branch %q left behind", br)
			}
		}
	}
	dataUntouched("after upgrade")
}
