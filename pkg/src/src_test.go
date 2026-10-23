package src_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/src"
)

// seedFixture builds a tiny two-commit git repo and publishes it as a bare
// seed at home/seed/yoke.git (the Sprint 386 offline-seed shape). It returns
// the two commit SHAs (old, new). The tree mixes tiers: pkg/atlas is core,
// pkg/weave/weave_story_commit.go is a protected guard rail, pkg/weave's
// other files are skill tier, and README is neutral. No network is touched.
func seedFixture(t *testing.T, home string) (oldSHA, newSHA string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	write := func(rel, body string) {
		p := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/weave/engine.go", "package weave\n")
	write("pkg/weave/weave_story_commit.go", "package weave\n")
	write("pkg/atlas/atlas.go", "package atlas\n")
	write("README.md", "# fixture\n")
	if _, err := ygit.Init(ygit.InitOptions{Path: work}); err != nil {
		t.Fatalf("init: %v", err)
	}
	commit := func(msg string) string {
		if _, err := ygit.Add(ygit.AddOptions{RepoPath: work, All: true}); err != nil {
			t.Fatalf("add: %v", err)
		}
		if _, err := ygit.Commit(ygit.CommitOptions{
			RepoPath: work, Message: msg,
			AuthorName: "src-test", AuthorEmail: "src-test@example.com",
		}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: work})
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		return rev.Hash
	}
	oldSHA = commit("first")
	write("README.md", "# fixture\n\nmore\n")
	newSHA = commit("second")
	seed := filepath.Join(home, "seed", "yoke.git")
	if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gogit.PlainClone(seed, true, &gogit.CloneOptions{URL: work}); err != nil {
		t.Fatalf("seed clone: %v", err)
	}
	return oldSHA, newSHA
}

func destOf(home string) string { return filepath.Join(home, "src", "yoke") }

func TestShowCoreSkillExternal(t *testing.T) {
	t.Setenv("BASHY_SRC_COMMIT", "abc123def456")
	for _, tc := range []struct {
		cmd       string
		repo, pkg string
		class     src.Class
		parts     []string
	}{
		{"dag", "yoke", "github.com/qiangli/yoke/pkg/dag", src.ClassCore, nil},
		{"sprint", "yoke", "github.com/qiangli/yoke/pkg/weave", src.ClassSkill,
			[]string{"pkg/weave/sprint_commit_attribution.go", "pkg/weave/weave_story_commit.go"}},
		{"kb", "yoke", "github.com/qiangli/yoke/pkg/kb", src.ClassSkill, nil},
		{"kubectl", "managed", "managed:kubectl", src.ClassExternal, nil},
		{"ls", "coreutils", "github.com/qiangli/coreutils", src.ClassCore, nil},
	} {
		info, err := src.Show(tc.cmd)
		if err != nil {
			t.Errorf("Show(%q): %v", tc.cmd, err)
			continue
		}
		if info.Repo != tc.repo || info.Package != tc.pkg || info.Class != tc.class || info.Commit != "abc123def456" {
			t.Errorf("Show(%q) = %+v, want repo=%q pkg=%q class=%q commit=abc123def456",
				tc.cmd, info, tc.repo, tc.pkg, tc.class)
		}
		var got []string
		for _, p := range info.ProtectedParts {
			got = append(got, p.Repo+":"+p.Path)
		}
		var want []string
		for _, p := range tc.parts {
			want = append(want, "yoke:"+p)
		}
		if len(got) != len(want) {
			t.Errorf("Show(%q) protected parts = %q, want %q", tc.cmd, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Show(%q) protected parts = %q, want %q", tc.cmd, got, want)
				break
			}
		}
	}
	if _, err := src.Show("nope-not-a-command"); err == nil {
		t.Error("Show(unknown) = nil error, want failure")
	}
}

func TestFetchFromSeedNoOpAndCheckClean(t *testing.T) {
	home := t.TempDir()
	_, sha := seedFixture(t, home)
	t.Setenv("BASHY_SRC_COMMIT", sha)

	msg, err := src.Fetch(home, "yoke")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(msg, "fetched yoke at") {
		t.Errorf("Fetch msg = %q, want a fetch confirmation", msg)
	}
	dest := destOf(home)
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dest})
	if err != nil || rev.Hash != sha {
		t.Fatalf("checkout at %v, %v; want %s", rev, err, sha)
	}
	_, branches, err := ygit.Branch(ygit.BranchOptions{RepoPath: dest})
	if err != nil {
		t.Fatalf("branches: %v", err)
	}
	if !strings.Contains(strings.Join(branches, "\n"), "pristine") {
		t.Errorf("no pristine branch in %q", branches)
	}
	prot := filepath.Join(dest, "pkg", "weave", "weave_story_commit.go")
	if st, err := os.Stat(prot); err != nil || st.Mode().Perm() != 0o444 {
		t.Errorf("protected file mode = %v, %v; want 0444", st, err)
	}
	for _, rel := range []string{"pkg/weave/engine.go", "README.md"} {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if st, err := os.Stat(p); err != nil || st.Mode().Perm()&0o200 == 0 {
			t.Errorf("%s should stay writable: %v, %v", rel, st, err)
		}
	}

	msg2, err := src.Fetch(home, "yoke")
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if !strings.Contains(msg2, "no-op") {
		t.Errorf("second Fetch msg = %q, want a no-op", msg2)
	}

	if vs, err := src.Check(home); err != nil || len(vs) != 0 {
		t.Errorf("Check = %v, %v; want clean", vs, err)
	}
}

func TestCheckFlagsProtectedAndCoreEdits(t *testing.T) {
	home := t.TempDir()
	_, sha := seedFixture(t, home)
	t.Setenv("BASHY_SRC_COMMIT", sha)
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	dest := destOf(home)
	tamper := func(rel string) {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString("// tampered\n"); err != nil {
			t.Fatal(err)
		}
	}
	tamper("pkg/weave/weave_story_commit.go")
	tamper("pkg/atlas/atlas.go")
	tamper("pkg/weave/engine.go")
	tamper("README.md")

	vs, err := src.Check(home)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := map[string]string{
		"pkg/weave/weave_story_commit.go": "protected",
		"pkg/atlas/atlas.go":              "core",
	}
	if len(vs) != len(want) {
		t.Fatalf("Check = %v, want %d violations (skill-tier and neutral files excluded)", vs, len(want))
	}
	for _, v := range vs {
		if want[v.Path] != v.Kind || v.Repo != "yoke" {
			t.Errorf("violation = %+v, want kind %q", v, want[v.Path])
		}
	}
}

func TestFetchRefusesMismatchAndProprietary(t *testing.T) {
	home := t.TempDir()
	oldSHA, newSHA := seedFixture(t, home)

	// Mismatched commit: the seed has no such object.
	t.Setenv("BASHY_SRC_COMMIT", strings.Repeat("0", 40))
	if _, err := src.Fetch(home, "yoke"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("mismatched Fetch err = %v, want a refusal", err)
	}
	if _, err := os.Stat(destOf(home)); !os.IsNotExist(err) {
		t.Error("mismatched fetch left a partial checkout behind")
	}

	// Existing checkout at another commit is refused, never clobbered.
	t.Setenv("BASHY_SRC_COMMIT", newSHA)
	if _, err := src.Fetch(home, "yoke"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	t.Setenv("BASHY_SRC_COMMIT", oldSHA)
	if _, err := src.Fetch(home, "yoke"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("drifted Fetch err = %v, want a refusal", err)
	}
	rev, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: destOf(home)})
	if err != nil || rev.Hash != newSHA {
		t.Errorf("refused fetch moved HEAD: %v, %v", rev, err)
	}

	// Proprietary repos are refused before any git work.
	if _, err := src.Fetch(home, "acme-internal"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("proprietary Fetch err = %v, want a refusal", err)
	}
}

func TestSrcCmdShowAndCheck(t *testing.T) {
	home := t.TempDir()
	_, sha := seedFixture(t, home)
	t.Setenv("BASHY_SRC_COMMIT", sha)
	t.Setenv("BASHY_HOME", home)

	run := func(args ...string) (string, error) {
		cmd := src.NewSrcCmd()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}

	out, err := run("show", "dag")
	if err != nil {
		t.Fatalf("show dag: %v", err)
	}
	for _, want := range []string{"repo: yoke", "package: github.com/qiangli/yoke/pkg/dag", "class: core", "commit: " + sha, "protected_parts: (none)"} {
		if !strings.Contains(out, want) {
			t.Errorf("show dag output %q missing %q", out, want)
		}
	}
	out, err = run("show", "sprint")
	if err != nil {
		t.Fatalf("show sprint: %v", err)
	}
	for _, want := range []string{"class: skill", "protected_part: yoke:pkg/weave/sprint_commit_attribution.go", "protected_part: yoke:pkg/weave/weave_story_commit.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("show sprint output %q missing %q", out, want)
		}
	}
	if _, err := run("show", "nope-not-a-command"); err == nil {
		t.Error("show unknown: nil error, want failure")
	}

	if out, err := run("fetch", "yoke"); err != nil || !strings.Contains(out, "fetched yoke at") {
		t.Errorf("fetch yoke = %q, %v", out, err)
	}
	if out, err := run("check"); err != nil || !strings.Contains(out, "clean") {
		t.Errorf("check clean = %q, %v", out, err)
	}

	prot := filepath.Join(destOf(home), "pkg", "weave", "weave_story_commit.go")
	if err := os.Chmod(prot, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prot, []byte("package weave\n// tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = run("check")
	if err == nil || !strings.Contains(err.Error(), "violation") {
		t.Errorf("check tampered err = %v (output %q), want a violation exit", err, out)
	}
	if !strings.Contains(out, "VIOLATION protected yoke:pkg/weave/weave_story_commit.go") {
		t.Errorf("check output %q missing the violation line", out)
	}
}
