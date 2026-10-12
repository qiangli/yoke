package git

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestLineDiffRealHistory replays the last commits of real repositories
// (LINEDIFF_REAL_REPOS, comma-separated paths) and compares, file by
// file, host git's patch hunks with the engine's and with the production
// stagedPatch path. It reports the identical share per repository and
// fails below LINEDIFF_REAL_MIN percent (default 98).
func TestLineDiffRealHistory(t *testing.T) {
	repos := os.Getenv("LINEDIFF_REAL_REPOS")
	if repos == "" {
		t.Skip("set LINEDIFF_REAL_REPOS to replay real history")
	}
	minPct := 98.0
	if v := os.Getenv("LINEDIFF_REAL_MIN"); v != "" {
		if _, err := fmt.Sscan(v, &minPct); err != nil {
			t.Fatal(err)
		}
	}
	hg := func(dir string, args ...string) string {
		out, _ := osexec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		return string(out)
	}
	for _, dir := range strings.Split(repos, ",") {
		total, engine, prod := 0, 0, 0
		for _, c := range strings.Fields(hg(dir, "rev-list", "--no-merges", "-n", "150", "HEAD")) {
			for _, f := range strings.Split(strings.TrimSpace(hg(dir, "diff-tree", "--no-commit-id", "-r", "--diff-filter=M", "--name-only", c)), "\n") {
				if f == "" {
					continue
				}
				a, b := hg(dir, "show", c+"^:"+f), hg(dir, "show", c+":"+f)
				if strings.IndexByte(a, 0) >= 0 || strings.IndexByte(b, 0) >= 0 || len(a)+len(b) > 400000 {
					continue
				}
				want := hg(dir, "diff", "--no-color", "--no-ext-diff", "--no-renames", c+"^", c, "--", f)
				i := strings.Index(want, "\n@@ ")
				if i < 0 {
					continue
				}
				want = want[i+1:]
				total++
				la := diffSplitLines(a)
				if newLineDiff(la, diffSplitLines(b), true).unified(3, funcNamer(la)) == want {
					engine++
				}
				if stagedHunks(t, a, b) == want {
					prod++
				}
			}
		}
		pe, pp := 100*float64(engine)/float64(max(total, 1)), 100*float64(prod)/float64(max(total, 1))
		t.Logf("%s: %d files; engine identical %d (%.1f%%); stagedPatch identical %d (%.1f%%)", filepath.Base(dir), total, engine, pe, prod, pp)
		if pp < minPct {
			t.Errorf("%s: stagedPatch identical %.1f%% < %.1f%%", dir, pp, minPct)
		}
	}
}

// stagedHunks renders a file change through stagedPatch on a scratch
// repository, headers stripped.
func stagedHunks(t *testing.T, a, b string) string {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		return "init-error"
	}
	wt, _ := repo.Worktree()
	os.WriteFile(filepath.Join(dir, "f"), []byte(a), 0o644)
	wt.Add("f")
	if _, err := wt.Commit("a", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}}); err != nil {
		return "commit-error"
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte(b), 0o644)
	wt.Add("f")
	p, err := stagedPatch(repo, "", nil, true)
	if err != nil {
		return "patch-error"
	}
	if i := strings.Index(p, "\n@@ "); i >= 0 {
		return p[i+1:]
	}
	return p
}
