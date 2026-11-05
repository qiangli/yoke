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

	"github.com/qiangli/coreutils/pkg/lockfile"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/shape/verify"
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
// ShapePR records submission provenance without changing normal merge gates.
type ShapePR struct {
	Kind            string `json:"kind,omitempty"`
	SubmittedBy     string `json:"submitted_by"`
	Submission      string `json:"submission"`
	SourceRevision  string `json:"source_revision"`
	Candidate       string `json:"candidate"`
	Actor           string `json:"actor"`
	RepresentedUser string `json:"represented_user"`
	Item            string `json:"item"`
	TrackingRoot    string `json:"tracking_root"`
	Evidence        string `json:"evidence"`
	Run             int64  `json:"run"`
	RunRepo         string `json:"run_repo"`
}

type PR struct {
	Shape            *ShapePR      `json:"shape,omitempty"`
	ID               string        `json:"id"`
	Repo             string        `json:"repo"`
	Title            string        `json:"title"`
	Author           string        `json:"author"`
	AuthorVendor     string        `json:"author_vendor,omitempty"`
	Profile          string        `json:"profile,omitempty"`
	Suites           []SuiteResult `json:"suites,omitempty"`
	SuiteComplete    bool          `json:"suite_complete,omitempty"`
	CoreRepair       bool          `json:"core_repair,omitempty"`
	Trigger          string        `json:"trigger,omitempty"`
	Reviewer         string        `json:"reviewer,omitempty"`
	ReviewVerdict    string        `json:"review_verdict,omitempty"`
	ReviewEvidence   string        `json:"review_evidence,omitempty"`
	Base             string        `json:"base"`
	Head             string        `json:"head"`
	HeadSHA          string        `json:"head_sha"`
	TestCmd          string        `json:"test_cmd,omitempty"`
	TestResult       string        `json:"test_result,omitempty"`
	TestOutput       string        `json:"test_output,omitempty"`
	State            string        `json:"state"`
	TouchedProtected bool          `json:"touched_protected,omitempty"`
	CreatedAt        string        `json:"created_at"`
	MergedSHA        string        `json:"merged_sha,omitempty"`
	MergedAt         string        `json:"merged_at,omitempty"`
	ClosedAt         string        `json:"closed_at,omitempty"`
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
	// A crash while persisting a proposal must not leave a truncated record
	// that prevents idempotent submission recovery or blinds ListPRs.
	f, err := os.CreateTemp(prsDir(home), ".pr-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0644); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), prPath(home, pr.ID))
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
// given it runs now, at the recorded head SHA in an isolated clone, and
// the pass/fail result is stored on the record; MergePR refuses a PR whose
// recorded test failed. A PR with no test command carries no result and is
// gated on verification evidence and policy alone.
func OpenPR(home, repo, branch, title, testCmd string) (PR, error) {
	lock, err := lockfile.AcquireWithin(filepath.Join(prsDir(home), ".open.lock"), 30*time.Second, lockfile.Holder{Name: agentName(), Intent: "open source proposal"})
	if err != nil {
		return PR{}, err
	}
	defer lock.Release()
	return openPRUnlocked(home, repo, branch, title, testCmd, nil)
}

// OpenShapePR records one idempotent proposal for a shaping submission.
// It does not grant review approval, waive checks or merge the proposal.
func OpenShapePR(home, repo, branch, title string, shape ShapePR) (PR, error) {
	if shape.Submission == "" {
		return PR{}, fmt.Errorf("src: shaping proposal requires a submission identity")
	}
	lock, err := lockfile.AcquireWithin(filepath.Join(prsDir(home), ".open.lock"), 30*time.Second, lockfile.Holder{Name: shape.Actor, Intent: "open shaped source proposal"})
	if err != nil {
		return PR{}, err
	}
	defer lock.Release()
	existing, err := ListPRs(home, repo)
	if err != nil {
		return PR{}, err
	}
	for _, pr := range existing {
		if pr.Shape != nil && pr.Shape.Submission == shape.Submission {
			return pr, nil
		}
	}
	pr, err := openPRUnlocked(home, repo, branch, title, "", &shape)
	if err != nil {
		return pr, err
	}
	return pr, nil
}

func openPRUnlocked(home, repo, branch, title, testCmd string, shape *ShapePR) (PR, error) {
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
		Shape: shape,
		ID:    id, Repo: repo, Title: title, Author: agentName(), AuthorVendor: vendor(agentName()),
		Base: localBranch, Head: branch, HeadSHA: headSHA,
		State: PRStateOpen, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if shape != nil && shape.SubmittedBy != "" {
		pr.Author = shape.SubmittedBy
		pr.AuthorVendor = vendor(pr.Author)
	}
	if testCmd != "" {
		pr.TestCmd = testCmd
		out, ok := runPRTest(dir, headSHA, testCmd)
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

// runPRTest executes the author's own test command at headSHA and reports
// pass/fail with truncated combined output.
//
// It runs in an isolated clone detached at that commit, never in the live
// checkout: the checkout is the version the user is running, and a test
// that can edit it is not a test of the candidate. The environment is
// scrubbed the same way a contained run's is, so the command sees a
// minimal PATH and a HOME inside the clone rather than the operator's.
// A clone that cannot be made is a failed test, not a skipped one.
func runPRTest(dir, headSHA, cmd string) (string, bool) {
	clone, err := verify.IsolatedClone(dir, headSHA)
	if err != nil {
		return "src: could not isolate the candidate for testing: " + err.Error(), false
	}
	defer os.RemoveAll(clone)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}
	c.Dir = clone
	if runtime.GOOS != "windows" {
		// The scrub builds a POSIX PATH/HOME/TMPDIR. Imposing that on
		// cmd.exe would break the shell rather than contain it, so on
		// Windows the command keeps the parent environment for now.
		c.Env = contain.Scrub(clone, nil, nil)
	}
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err = c.Run()
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

// MergePR merges the recorded candidate into local. Core path changes require
// a complete passing suite, independent different-vendor review, and sealed
// verification. Other paths retain the established protected-path policy.
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
		var missing []string
		if !suitePass(pr) {
			missing = append(missing, "full-suite pass")
		}
		if pr.ReviewVerdict != "pass" || pr.Reviewer == "" || pr.AuthorVendor == "" || vendor(pr.Reviewer) == pr.AuthorVendor {
			missing = append(missing, "different-vendor pass review")
		}
		baseSHA, e := mergeBase(dir, localBranch, pr.HeadSHA)
		if e != nil {
			return PR{}, e
		}
		if e = requireEvidence(home, dir, pr, baseSHA); e != nil {
			missing = append(missing, "sealed verify: "+e.Error())
		}
		if len(missing) > 0 {
			return PR{}, fmt.Errorf("src: refusing core PR %s: missing %s", id, strings.Join(missing, "; "))
		}
	}
	var prot []string
	for _, f := range files {
		if PathClass(pr.Repo, f) == ClassProtected {
			prot = append(prot, f)
		}
	}
	if !covered && len(core) == 0 {
		return PR{}, fmt.Errorf("src: refusing PR %s: protected path(s) %s need an approval record (see the approve verb)",
			id, strings.Join(prot, ", "))
	}
	// Policy said yes; evidence still has to. The override is checked
	// first only so an operator who already reviewed the whole change is
	// not sent to re-verify something they have signed off.
	waived, err := approvalsCoverAll(home, pr.Repo, files)
	if err != nil {
		return PR{}, err
	}
	if !waived && len(core) == 0 {
		baseSHA, err := mergeBase(dir, localBranch, pr.HeadSHA)
		if err != nil {
			return PR{}, fmt.Errorf("src: PR %s head %s: %w", id, shortSHA(pr.HeadSHA), err)
		}
		if err := requireEvidence(home, dir, pr, baseSHA); err != nil {
			return PR{}, err
		}
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
	pr.CoreRepair = len(core) > 0
	if err := savePR(home, pr); err != nil {
		restore()
		return PR{}, err
	}
	restore()
	return pr, nil
}
