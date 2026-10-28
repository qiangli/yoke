package src_test

// M2 acceptance (red/green, local fixtures only — no network): branch + PR
// open + merge lands on local only; failing tests refuse merges;
// protected paths need approvals (merge as modified-protected); core paths
// are always refused; patch export/apply round-trips across homes; Upgrade
// rebases cleanly and aborts with paths + untouched local on conflict;
// State values.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/src"
)

const m2AuthorName = "m2-test"
const m2AuthorEmail = "m2-test@example.com"

// writeWork writes rel under dir, loosening the read-only bit first so
// protected-manifest files stay editable in fixtures.
func writeWork(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p); err == nil && st.Mode().Perm()&0o200 == 0 {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWork(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// commitWork stages everything and commits with an explicit identity.
func commitWork(t *testing.T, dir, msg string) string {
	t.Helper()
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := ygit.Commit(ygit.CommitOptions{
		RepoPath: dir, Message: msg,
		AuthorName: m2AuthorName, AuthorEmail: m2AuthorEmail,
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return rev.Hash
}

func resolve(t *testing.T, dir, rev string) string {
	t.Helper()
	s, err := ygit.ResolveCommit(dir, rev)
	if err != nil {
		t.Fatalf("resolve %q: %v", rev, err)
	}
	return s
}

// fetchAt fetches the yoke fixture at a pinned commit.
func fetchAt(t *testing.T, home, sha string) {
	t.Helper()
	t.Setenv("BASHY_SRC_COMMIT", sha)
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// pushSeedCommit clones the bare seed, adds files, and pushes back,
// returning the new tip SHA. It simulates the next upstream release.
func pushSeedCommit(t *testing.T, home string, files map[string]string, msg string) string {
	t.Helper()
	seed := filepath.Join(home, "seed", "yoke.git")
	work := filepath.Join(t.TempDir(), "upstream")
	if _, err := gogit.PlainClone(work, false, &gogit.CloneOptions{URL: seed}); err != nil {
		t.Fatalf("clone seed: %v", err)
	}
	for rel, body := range files {
		writeWork(t, work, rel, body)
	}
	sha := commitWork(t, work, msg)
	if _, err := ygit.Push(ygit.PushOptions{RepoPath: work}); err != nil {
		t.Fatalf("push: %v", err)
	}
	return sha
}

// openSkillPR branches off local, changes a skill-tier file, and opens a PR.
func openSkillPR(t *testing.T, home, branch, title, testCmd string) src.PR {
	t.Helper()
	if err := src.CreateBranch(home, "yoke", branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	dir := destOf(home)
	writeWork(t, dir, "pkg/weave/engine.go", "package weave\n// "+branch+"\n")
	commitWork(t, dir, "skill change on "+branch)
	pr, err := src.OpenPR(home, "yoke", branch, title, testCmd)
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	return pr
}

func TestM2BranchPropenMergeLocalOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	dir := destOf(home)

	pristineBefore := resolve(t, dir, "pristine")
	pr := openSkillPR(t, home, "feature-a", "Add a skill", "")
	if pr.Base != "local" || pr.Head != "feature-a" || pr.Author != "test-agent" || pr.State != "open" {
		t.Fatalf("PR record = %+v, want base=local head=feature-a author=test-agent state=open", pr)
	}
	if pr.HeadSHA == "" || pr.HeadSHA == pristineBefore {
		t.Fatalf("PR head SHA = %q, want a new commit past pristine", pr.HeadSHA)
	}

	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	got, err := src.GetPR(home, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "merged" {
		t.Errorf("PR state = %q, want merged", got.State)
	}

	// Landed on local only: pristine ref untouched.
	if sha := resolve(t, dir, "pristine"); sha != pristineBefore {
		t.Errorf("pristine moved: %s -> %s", pristineBefore, sha)
	}
	localSHA := resolve(t, dir, "local")
	if localSHA == pristineBefore {
		t.Error("local did not advance past pristine")
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: "local"}); err != nil {
		t.Fatal(err)
	}
	if body := readWork(t, dir, "pkg/weave/engine.go"); !strings.Contains(body, "feature-a") {
		t.Errorf("local lacks the merged change: %q", body)
	}

	st, err := src.State(home, "yoke")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != src.StateModified || st.Base != pristineBefore || st.Local != localSHA {
		t.Errorf("State = %+v, want {modified %s %s}", st, pristineBefore, localSHA)
	}
}

func TestM2MergeRefusesFailingTest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)

	pr := openSkillPR(t, home, "feature-bad", "Breaking change", "false")
	if pr.TestResult != "fail" {
		t.Fatalf("TestResult = %q, want fail", pr.TestResult)
	}
	if _, err := src.MergePR(home, pr.ID); err == nil || !strings.Contains(err.Error(), "test") {
		t.Fatalf("MergePR err = %v, want a test refusal", err)
	}

	pr2 := openSkillPR(t, home, "feature-good", "Good change", "true")
	if pr2.TestResult != "pass" {
		t.Fatalf("TestResult = %q, want pass", pr2.TestResult)
	}
	if _, err := src.MergePR(home, pr2.ID); err != nil {
		t.Fatalf("MergePR with passing test: %v", err)
	}
}

func TestM2ProtectedNeedsApproval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	dir := destOf(home)

	if err := src.CreateBranch(home, "yoke", "guard-edit"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, dir, "pkg/weave/weave_story_commit.go", "package weave\n// guard edit\n")
	commitWork(t, dir, "guard edit")
	pr, err := src.OpenPR(home, "yoke", "guard-edit", "Guard edit", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.MergePR(home, pr.ID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "approv") {
		t.Fatalf("MergePR err = %v, want an approval refusal", err)
	}

	if _, err := src.WriteApproval(home, src.ApprovalRecord{
		Repo: "yoke", Paths: []string{"pkg/weave/weave_story_commit.go"},
		Approver: "test-approver", Reason: "reviewed",
	}); err != nil {
		t.Fatalf("WriteApproval: %v", err)
	}
	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatalf("MergePR with approval: %v", err)
	}
	st, err := src.State(home, "yoke")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != src.StateModifiedProtected {
		t.Errorf("State = %q, want modified-protected", st.State)
	}
}

// The browser sensitive-input guard is a protected set (S512.5): a skill-tier
// change to the shared validators, the gate, or the extension's guard files
// (tab pin, field check, in-page verify) — say, dropping the SSN check — is
// refused at merge until a `bashy approve`d src approval covers
// exactly that path.
func TestM2BrowserGuardChangeNeedsApproval(t *testing.T) {
	for _, rel := range []string{
		"pkg/sensitive/validators.go",
		"pkg/browser/guard/gate.go",
		"pkg/browser/live/extension/background.js",
		"pkg/browser/live/extension/page.js",
		"pkg/browser/live/extension/guard.js",
	} {
		t.Run(rel, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("BASHY_AGENT", "skill-agent")
			_, newSHA := seedFixture(t, home)
			fetchAt(t, home, newSHA)
			dir := destOf(home)
			if err := src.CreateBranch(home, "yoke", "weaken-guard"); err != nil {
				t.Fatal(err)
			}
			writeWork(t, dir, rel, "package x\n// the SSN check is gone\n")
			commitWork(t, dir, "weaken the guard")
			pr, err := src.OpenPR(home, "yoke", "weaken-guard", "Weaken guard", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := src.MergePR(home, pr.ID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "approv") {
				t.Fatalf("MergePR err = %v, want an approval refusal", err)
			}
			// An approval for some OTHER path does not cover it.
			if _, err := src.WriteApproval(home, src.ApprovalRecord{
				Repo: "yoke", Paths: []string{"pkg/weave/weave_story_commit.go"}, Approver: "op", Reason: "unrelated",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := src.MergePR(home, pr.ID); err == nil {
				t.Fatal("an unrelated approval let the guard change merge")
			}
			if _, err := src.WriteApproval(home, src.ApprovalRecord{
				Repo: "yoke", Paths: []string{rel}, Approver: "op", Reason: "reviewed",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := src.MergePR(home, pr.ID); err != nil {
				t.Fatalf("MergePR with approval: %v", err)
			}
		})
	}
}

func TestM2CoreAlwaysRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	dir := destOf(home)

	if err := src.CreateBranch(home, "yoke", "core-edit"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, dir, "pkg/atlas/atlas.go", "package atlas\n// core edit\n")
	commitWork(t, dir, "core edit")
	pr, err := src.OpenPR(home, "yoke", "core-edit", "Core edit", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.MergePR(home, pr.ID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "core") {
		t.Fatalf("MergePR err = %v, want a core refusal", err)
	}
	// Even a blanket approval cannot unlock a core path.
	if _, err := src.WriteApproval(home, src.ApprovalRecord{
		Repo: "yoke", Paths: []string{""},
		Approver: "test-approver", Reason: "blanket",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.MergePR(home, pr.ID); err == nil {
		t.Fatal("MergePR with blanket approval succeeded on a core path, want refusal")
	}
}

func TestM2PatchRoundTripAcrossHomes(t *testing.T) {
	homeA := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	_, newSHA := seedFixture(t, homeA)
	fetchAt(t, homeA, newSHA)

	pr := openSkillPR(t, homeA, "patch-src", "Patch source", "")
	patch, err := src.ExportPatch(homeA, pr.ID)
	if err != nil {
		t.Fatalf("ExportPatch: %v", err)
	}
	if !strings.Contains(patch, "pkg/weave/engine.go") || !strings.Contains(patch, "patch-src") {
		t.Errorf("patch missing the change:\n%s", patch)
	}
	patchFile := filepath.Join(t.TempDir(), "change.patch")
	if err := os.WriteFile(patchFile, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}

	// A second host with its own checkout applies the patch file.
	homeB := t.TempDir()
	_, newSHAB := seedFixture(t, homeB)
	fetchAt(t, homeB, newSHAB)
	prB, err := src.ApplyPatch(homeB, "yoke", patchFile, "Applied from file")
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if prB.State != "open" || prB.Head == "patch-src" {
		t.Errorf("applied PR = %+v, want a fresh open PR on a new branch", prB)
	}
	if _, err := src.MergePR(homeB, prB.ID); err != nil {
		t.Fatalf("MergePR applied: %v", err)
	}
	dirB := destOf(homeB)
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dirB, Branch: "local"}); err != nil {
		t.Fatal(err)
	}
	if body := readWork(t, dirB, "pkg/weave/engine.go"); !strings.Contains(body, "patch-src") {
		t.Errorf("second host lacks the patched change: %q", body)
	}
}

func TestM2UpgradeRebasesCleanly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	oldSHA, _ := seedFixture(t, home)
	fetchAt(t, home, oldSHA)
	dir := destOf(home)

	pr := openSkillPR(t, home, "pre-upgrade", "Before upgrade", "")
	if _, err := src.MergePR(home, pr.ID); err != nil {
		t.Fatal(err)
	}
	localBefore := resolve(t, dir, "local")

	upstreamSHA := pushSeedCommit(t, home, map[string]string{"UPSTREAM.md": "# upstream\n"}, "upstream release")
	conflicts, err := src.Upgrade(home, "yoke", upstreamSHA)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("Upgrade = %v, %v; want clean", conflicts, err)
	}
	if sha := resolve(t, dir, "pristine"); sha != upstreamSHA {
		t.Errorf("pristine = %s, want %s", sha, upstreamSHA)
	}
	localAfter := resolve(t, dir, "local")
	if localAfter == localBefore || localAfter == upstreamSHA {
		t.Errorf("local = %s, want a replayed commit past %s", localAfter, upstreamSHA)
	}
	anc, err := ygit.IsAncestor(dir, upstreamSHA, localAfter)
	if err != nil || !anc {
		t.Errorf("new pristine is not an ancestor of local: anc=%v err=%v", anc, err)
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: "local"}); err != nil {
		t.Fatal(err)
	}
	if body := readWork(t, dir, "pkg/weave/engine.go"); !strings.Contains(body, "pre-upgrade") {
		t.Errorf("local change lost across upgrade: %q", body)
	}
	if body := readWork(t, dir, "UPSTREAM.md"); !strings.Contains(body, "upstream") {
		t.Errorf("upstream change missing after upgrade: %q", body)
	}
	st, err := src.State(home, "yoke")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != src.StateModified || st.Base != upstreamSHA || st.Local != localAfter {
		t.Errorf("State = %+v, want {modified %s %s}", st, upstreamSHA, localAfter)
	}
}

func TestM2UpgradeConflictAbortsUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	oldSHA, _ := seedFixture(t, home)
	fetchAt(t, home, oldSHA)
	dir := destOf(home)

	if err := src.CreateBranch(home, "yoke", "clash"); err != nil {
		t.Fatal(err)
	}
	writeWork(t, dir, "README.md", "# fixture clash\n")
	commitWork(t, dir, "local clash")
	clashPR, err := src.OpenPR(home, "yoke", "clash", "Clash", "")
	if err != nil {
		t.Fatal(err)
	}
	// Only local is rebased, so the clash must ride local to collide.
	if _, err := src.MergePR(home, clashPR.ID); err != nil {
		t.Fatalf("MergePR clash: %v", err)
	}
	localTip := resolve(t, dir, "local")
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: "local"}); err != nil {
		t.Fatal(err)
	}
	clashBody := readWork(t, dir, "README.md")

	upstreamSHA := pushSeedCommit(t, home, map[string]string{"README.md": "# fixture upstream\n"}, "upstream clash")
	conflicts, err := src.Upgrade(home, "yoke", upstreamSHA)
	if err == nil {
		t.Fatal("Upgrade succeeded across a conflicting edit, want an abort")
	}
	if len(conflicts) == 0 {
		t.Error("Upgrade reported no conflicting paths")
	} else if conflicts[0] != "README.md" {
		t.Errorf("conflicts = %q, want [README.md]", conflicts)
	}

	// Local and its files are exactly as they were; pristine never moved.
	if sha := resolve(t, dir, "local"); sha != localTip {
		t.Errorf("local moved across an aborted upgrade: %s -> %s", localTip, sha)
	}
	if sha := resolve(t, dir, "pristine"); sha != oldSHA {
		t.Errorf("pristine moved across an aborted upgrade: %s", sha)
	}
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: "local"}); err != nil {
		t.Fatal(err)
	}
	if body := readWork(t, dir, "README.md"); body != clashBody {
		t.Errorf("worktree changed across an aborted upgrade: %q", body)
	}
}

func TestM2StateFreshIsPristine(t *testing.T) {
	home := t.TempDir()
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)
	st, err := src.State(home, "yoke")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != src.StatePristine || st.Base != newSHA || st.Local != newSHA {
		t.Errorf("State = %+v, want {pristine %s %s}", st, newSHA, newSHA)
	}
}

func TestM2SrcRodCoversM2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_AGENT", "test-agent")
	t.Setenv("BASHY_HOME", home)
	_, newSHA := seedFixture(t, home)
	fetchAt(t, home, newSHA)

	run := func(args ...string) (string, error) {
		cmd := src.NewSrcCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}

	if out, err := run("branch", "yoke", "rod-topic"); err != nil || !strings.Contains(out, "rod-topic") {
		t.Errorf("branch = %q, %v", out, err)
	}
	if out, err := run("status", "yoke"); err != nil || !strings.Contains(out, "pristine") {
		t.Errorf("status = %q, %v", out, err)
	}
	if out, err := run("log", "yoke"); err != nil || !strings.Contains(out, "second") {
		t.Errorf("log = %q, %v", out, err)
	}
	if out, err := run("diff", "yoke", "--base", "pristine"); err != nil {
		t.Errorf("diff = %q, %v", out, err)
	}
	if out, err := run("pr", "list"); err != nil {
		t.Errorf("pr list = %q, %v", out, err)
	}
	if _, err := run("pr", "open", "yoke", "rod-topic"); err == nil {
		t.Error("pr open without --title succeeded, want failure")
	}
	if _, err := run("pr", "open", "yoke", "rod-topic", "--title", "Rod PR"); err == nil || !strings.Contains(err.Error(), "no diff") {
		t.Errorf("pr open without diff err = %v, want no diff refusal", err)
	}
	writeWork(t, destOf(home), "pkg/weave/engine.go", "// rod change\n")
	commitWork(t, destOf(home), "rod change")
	if out, err := run("pr", "open", "yoke", "rod-topic", "--title", "Rod PR"); err != nil || !strings.Contains(out, "opened") {
		t.Errorf("pr open = %q, %v", out, err)
	}
	if out, err := run("pr", "list"); err != nil || !strings.Contains(out, "Rod PR") {
		t.Errorf("pr list after open = %q, %v", out, err)
	}
	if out, err := run("revert", "yoke"); err != nil {
		t.Errorf("revert dry-run = %q, %v", out, err)
	}
	if out, err := run("revert", "yoke", "--apply"); err != nil || !strings.Contains(out, "reverted yoke to pristine") {
		t.Errorf("revert apply = %q, %v", out, err)
	}
}
