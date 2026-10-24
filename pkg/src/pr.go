package src

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	ygit "github.com/qiangli/yoke/git"
)

// PR states: a record is open until merged into local or closed unmerged.
const (
	PRStateOpen   = "open"
	PRStateMerged = "merged"
	PRStateClosed = "closed"
)

// PR is one local pull request: a topic branch proposed against `local`.
// Records live as JSON under $BASHY_HOME/src/.prs/<id>.json so hosts can
// exchange them by file (see patch export/apply). Merges only ever touch
// `local` — never `pristine` and never any remote.
type PR struct {
	ID               string `json:"id"`
	Repo             string `json:"repo"`
	Title            string `json:"title"`
	Author           string `json:"author"`
	Base             string `json:"base"`
	Head             string `json:"head"`
	HeadSHA          string `json:"head_sha"`
	TestCmd          string `json:"test_cmd,omitempty"`
	TestResult       string `json:"test_result,omitempty"`
	TestOutput       string `json:"test_output,omitempty"`
	State            string `json:"state"`
	TouchedProtected bool   `json:"touched_protected,omitempty"`
	CreatedAt        string `json:"created_at"`
	MergedSHA        string `json:"merged_sha,omitempty"`
	MergedAt         string `json:"merged_at,omitempty"`
	ClosedAt         string `json:"closed_at,omitempty"`
}

// prsDir is $BASHY_HOME/src/.prs/.
func prsDir(home string) string {
	if home == "" {
		home = Home()
	}
	return filepath.Join(SrcDir(home), ".prs")
}

func prPath(home, id string) string { return filepath.Join(prsDir(home), id+".json") }

func savePR(home string, pr PR) error {
	if err := os.MkdirAll(prsDir(home), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(prPath(home, pr.ID), append(raw, '\n'), 0o644)
}

// nextPRID allocates the next integer id for home.
func nextPRID(home string) (string, error) {
	list, err := ListPRs(home, "")
	if err != nil {
		return "", err
	}
	max := 0
	for _, pr := range list {
		if n, err := strconv.Atoi(pr.ID); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1), nil
}

// OpenPR records branch as a pull request against `local`. When testCmd is
// given it runs now in the checkout and the pass/fail result is stored on
// the record; MergePR refuses a PR whose recorded test failed. A PR with
// no test command carries no result and merges on policy gates alone.
func OpenPR(home, repo, branch, title, testCmd string) (PR, error) {
	if strings.TrimSpace(title) == "" {
		return PR{}, fmt.Errorf("src: pr open needs --title")
	}
	dir, err := repoDir(home, repo)
	if err != nil {
		return PR{}, err
	}
	if err := EnsureLocal(home, repo); err != nil {
		return PR{}, err
	}
	if ok, err := branchExists(dir, branch); err != nil {
		return PR{}, err
	} else if !ok {
		return PR{}, fmt.Errorf("src: branch %q does not exist in %s", branch, repo)
	}
	headSHA, err := ygit.ResolveCommit(dir, branch)
	if err != nil {
		return PR{}, err
	}
	baseSHA, err := mergeBase(dir, localBranch, headSHA)
	if err != nil {
		return PR{}, fmt.Errorf("src: merge base of %s and %s: %w", localBranch, shortSHA(headSHA), err)
	}
	hasDiff := false
	if baseSHA != headSHA {
		text, err := diffText(dir, baseSHA, headSHA)
		if err != nil {
			return PR{}, err
		}
		if strings.TrimSpace(text) != "" {
			hasDiff = true
		}
	}
	if !hasDiff {
		return PR{}, fmt.Errorf("src: branch %q has no diff against base %s", branch, localBranch)
	}
	id, err := nextPRID(home)
	if err != nil {
		return PR{}, err
	}
	pr := PR{
		ID: id, Repo: repo, Title: title, Author: agentName(),
		Base: localBranch, Head: branch, HeadSHA: headSHA,
		State: PRStateOpen, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if testCmd != "" {
		pr.TestCmd = testCmd
		out, ok := runPRTest(dir, testCmd)
		pr.TestOutput = out
		if ok {
			pr.TestResult = "pass"
		} else {
			pr.TestResult = "fail"
		}
	}
	if err := savePR(home, pr); err != nil {
		return PR{}, err
	}
	return pr, nil
}

// runPRTest executes cmd in dir and reports pass/fail with truncated
// combined output. This runs the author's own test command, not git.
func runPRTest(dir, cmd string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}
	c.Dir = dir
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	out := buf.String()
	const maxOutput = 4000
	if len(out) > maxOutput {
		out = out[:maxOutput] + "\n…(truncated)"
	}
	return out, err == nil
}

// GetPR reads one PR record by id.
func GetPR(home, id string) (PR, error) {
	raw, err := os.ReadFile(prPath(home, id))
	if err != nil {
		if os.IsNotExist(err) {
			return PR{}, fmt.Errorf("src: no PR %q", id)
		}
		return PR{}, err
	}
	var pr PR
	if err := json.Unmarshal(raw, &pr); err != nil {
		return PR{}, fmt.Errorf("src: corrupt PR %s: %w", id, err)
	}
	return pr, nil
}

// ListPRs reads every PR record, oldest first, optionally filtered to one
// repo.
func ListPRs(home, repo string) ([]PR, error) {
	entries, err := os.ReadDir(prsDir(home))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []PR
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if _, err := strconv.Atoi(id); err != nil {
			continue
		}
		pr, err := GetPR(home, id)
		if err != nil {
			return nil, err
		}
		if repo != "" && pr.Repo != repo {
			continue
		}
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool {
		ii, _ := strconv.Atoi(out[i].ID)
		jj, _ := strconv.Atoi(out[j].ID)
		return ii < jj
	})
	return out, nil
}

// prDiffFiles returns the repo-relative paths the PR introduces: the diff
// from the merge base of local and the recorded head SHA to that SHA.
// Pinning the recorded SHA (not the moving branch tip) keeps the recorded
// test result meaningful.
func prDiffFiles(dir string, pr PR) ([]string, error) {
	base, err := mergeBase(dir, localBranch, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("src: PR %s head %s: %w", pr.ID, shortSHA(pr.HeadSHA), err)
	}
	if base == pr.HeadSHA {
		return nil, nil
	}
	return changedFiles(dir, base, pr.HeadSHA)
}

// ClosePR closes an open PR without merging.
func ClosePR(home, id string) (PR, error) {
	pr, err := GetPR(home, id)
	if err != nil {
		return PR{}, err
	}
	if pr.State != PRStateOpen {
		return PR{}, fmt.Errorf("src: PR %s is %s, not open", id, pr.State)
	}
	pr.State = PRStateClosed
	pr.ClosedAt = time.Now().UTC().Format(time.RFC3339)
	if err := savePR(home, pr); err != nil {
		return PR{}, err
	}
	return pr, nil
}

// MergePR merges the PR's recorded head SHA into `local` and records the
// merge. It refuses, changing nothing, when the recorded test failed, when
// the diff touches a core path (always), or when it touches a protected
// path without an approval covering it. `pristine` and all remotes are
// never touched; no fetch, no push.
func MergePR(home, id string) (PR, error) {
	pr, err := GetPR(home, id)
	if err != nil {
		return PR{}, err
	}
	if pr.State != PRStateOpen {
		return PR{}, fmt.Errorf("src: PR %s is %s, not open", id, pr.State)
	}
	if pr.TestResult == "fail" {
		return PR{}, fmt.Errorf("src: refusing PR %s: recorded test failed (%s)", id, pr.TestCmd)
	}
	dir, err := repoDir(home, pr.Repo)
	if err != nil {
		return PR{}, err
	}
	if err := requireClean(dir); err != nil {
		return PR{}, err
	}
	files, err := prDiffFiles(dir, pr)
	if err != nil {
		return PR{}, err
	}
	covered, core, err := approvalsCoverProtected(home, pr.Repo, files)
	if err != nil {
		return PR{}, err
	}
	if len(core) > 0 {
		return PR{}, fmt.Errorf("src: refusing PR %s: core path(s) %s are immutable (no approval can unlock them)",
			id, strings.Join(core, ", "))
	}
	var prot []string
	for _, f := range files {
		if PathClass(pr.Repo, f) == ClassProtected {
			prot = append(prot, f)
		}
	}
	if !covered {
		return PR{}, fmt.Errorf("src: refusing PR %s: protected path(s) %s need an approval record (see the approve verb)",
			id, strings.Join(prot, ", "))
	}

	cur, err := currentBranch(dir)
	if err != nil {
		return PR{}, err
	}
	if err := unlockRepo(dir, pr.Repo); err != nil {
		return PR{}, err
	}
	restore := func() {
		if cur != "" {
			_, _ = ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: cur})
		}
		_ = relockRepo(dir, pr.Repo)
	}
	if err := checkoutUnlocked(home, dir, pr.Repo, localBranch); err != nil {
		restore()
		return PR{}, err
	}
	if _, err := ygit.Merge(ygit.MergeOptions{RepoPath: dir, Ref: pr.HeadSHA}); err != nil {
		restore()
		var ce *ygit.ConflictError
		if errors.As(err, &ce) && len(ce.Files) > 0 {
			return PR{}, fmt.Errorf("src: PR %s conflicts on %s: %w", id, strings.Join(ce.Files, ", "), err)
		}
		return PR{}, fmt.Errorf("src: merge PR %s: %w", id, err)
	}
	mergedSHA, err := ygit.ResolveCommit(dir, localBranch)
	if err != nil {
		restore()
		return PR{}, err
	}
	pr.State = PRStateMerged
	pr.MergedSHA = mergedSHA
	pr.MergedAt = time.Now().UTC().Format(time.RFC3339)
	pr.TouchedProtected = len(prot) > 0
	if err := savePR(home, pr); err != nil {
		restore()
		return PR{}, err
	}
	restore()
	return pr, nil
}
