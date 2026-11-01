package src

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

type SuiteResult struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Dir        string `json:"dir,omitempty"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Complete   bool   `json:"complete"`
	Truncated  bool   `json:"truncated,omitempty"`
	Output     string `json:"output,omitempty"`
}
type SuiteProfile struct {
	Commands  []SuiteResult `json:"commands"`
	Results   []SuiteResult `json:"results"`
	Suites    []SuiteResult `json:"suites"`
	Complete  bool          `json:"complete"`
	Candidate string        `json:"candidate,omitempty"`
}

func suitePass(pr PR) bool {
	if !pr.SuiteComplete || len(pr.Suites) == 0 {
		return false
	}
	for _, r := range pr.Suites {
		if !r.Complete || r.Truncated || strings.Contains(r.Output, "…(truncated)") || r.Status != "pass" || r.ExitCode != 0 {
			return false
		}
	}
	return true
}
func RunPRSuite(home, id, path string) (PR, error) {
	pr, err := GetPR(home, id)
	if err != nil {
		return pr, err
	}
	if pr.State != PRStateOpen {
		return pr, fmt.Errorf("src: PR %s is not open", id)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return pr, err
	}
	var prof SuiteProfile
	if err = json.Unmarshal(b, &prof); err != nil {
		return pr, err
	}
	pr.Profile = path
	pr.Suites = nil
	pr.SuiteComplete = false
	evidenceResults := prof.Results
	if len(evidenceResults) == 0 {
		evidenceResults = prof.Suites
	}
	if len(evidenceResults) == 0 && len(prof.Commands) > 0 && prof.Commands[0].Status != "" {
		evidenceResults = prof.Commands
	}
	if len(evidenceResults) > 0 {
		if !prof.Complete || (prof.Candidate != "" && prof.Candidate != pr.HeadSHA) {
			return pr, fmt.Errorf("src: suite evidence is partial or stale")
		}
		pr.Suites = evidenceResults
		pr.SuiteComplete = true
	} else {
		if len(prof.Commands) == 0 {
			return pr, fmt.Errorf("src: suite profile has no commands")
		}
		dir, err := repoDir(home, pr.Repo)
		if err != nil {
			return pr, err
		}
		clone, err := verify.IsolatedClone(dir, pr.HeadSHA)
		if err != nil {
			return pr, err
		}
		defer os.RemoveAll(clone)
		for _, entry := range prof.Commands {
			r := SuiteResult{Name: entry.Name, Command: entry.Command, Dir: entry.Dir, Status: "fail", ExitCode: -1}
			if r.Name == "" || r.Command == "" {
				pr.Suites = append(pr.Suites, r)
				continue
			}
			work := filepath.Join(clone, filepath.Clean(entry.Dir))
			if rel, e := filepath.Rel(clone, work); e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				pr.Suites = append(pr.Suites, r)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			shell, args := "sh", []string{"-c", r.Command}
			if runtime.GOOS == "windows" {
				shell, args = "cmd", []string{"/C", r.Command}
			}
			c := exec.CommandContext(ctx, shell, args...)
			c.Dir = work
			var output bytes.Buffer
			c.Stdout = &output
			c.Stderr = &output
			start := time.Now()
			runErr := c.Run()
			r.DurationMS = time.Since(start).Milliseconds()
			r.Complete = ctx.Err() == nil
			if len(output.String()) > 4000 {
				r.Complete = false
				r.Output = output.String()[:4000] + "\n…(truncated)"
			} else {
				r.Output = output.String()
			}
			if runErr == nil && r.Complete {
				r.Status = "pass"
				r.ExitCode = 0
			} else if ee, ok := runErr.(*exec.ExitError); ok {
				r.ExitCode = ee.ExitCode()
			}
			cancel()
			pr.Suites = append(pr.Suites, r)
		}
		pr.SuiteComplete = true
	}
	if err = savePR(home, pr); err != nil {
		return pr, err
	}
	if !suitePass(pr) {
		return pr, fmt.Errorf("src: PR %s full suite failed or incomplete", id)
	}
	return pr, nil
}
func vendor(binding string) string {
	s := strings.ToLower(strings.TrimSpace(binding))
	if s == "" || s == "unknown" {
		return ""
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexAny(s, "-/"); i >= 0 {
		s = s[:i]
	}
	switch s {
	case "codex", "openai":
		return "openai"
	case "claude", "anthropic":
		return "anthropic"
	case "gemini", "google":
		return "google"
	default:
		return s
	}
}

var pairReview = runPairReview

func runPairReview(dir, base, gate, reviewer string) (string, string, error) {
	c := exec.Command("bashy", "pair", "Review this PR independently and test for defects.", "--diff", base, "--agent", reviewer, "--verify", gate, "--json")
	c.Dir = dir
	b, err := c.CombinedOutput()
	var data struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(b, &data) != nil {
		return "harness-error", string(b), err
	}
	v := map[string]string{"held": "pass", "proved": "refuted", "broken-before": "broken-before", "repaired": "broken-before", "unmeasured": "unmeasured"}[data.Outcome]
	if v == "" {
		v = "harness-error"
	}
	return v, string(b), err
}
func ReviewPR(home, id, reviewer string) (PR, error) {
	pr, err := GetPR(home, id)
	if err != nil {
		return pr, err
	}
	if pr.State != PRStateOpen {
		return pr, fmt.Errorf("src: PR %s is not open", id)
	}
	if reviewer == "" || pr.AuthorVendor == "" || vendor(reviewer) == "" || vendor(reviewer) == pr.AuthorVendor {
		return pr, fmt.Errorf("src: reviewer must be a different-vendor agent from author %q", pr.Author)
	}
	dir, err := repoDir(home, pr.Repo)
	if err != nil {
		return pr, err
	}
	gate := pr.TestCmd
	if gate == "" && pr.Profile != "" {
		gate = "bashy src pr verify " + pr.ID + " --profile " + strconv.Quote(pr.Profile)
	}
	if gate == "" {
		return pr, fmt.Errorf("src: PR %s has no gate command", id)
	}
	base, err := mergeBase(dir, localBranch, pr.HeadSHA)
	if err != nil {
		return pr, err
	}
	clone, err := verify.IsolatedClone(dir, pr.HeadSHA)
	if err != nil {
		return pr, err
	}
	defer os.RemoveAll(clone)
	v, out, runErr := pairReview(clone, base, gate, reviewer)
	if runErr != nil && v == "pass" {
		v = "harness-error"
	}
	pr.Reviewer = reviewer
	pr.ReviewVerdict = v
	pr.ReviewEvidence = out
	if err = savePR(home, pr); err != nil {
		return pr, err
	}
	if v != "pass" {
		return pr, fmt.Errorf("src: review %s: %s", id, v)
	}
	return pr, nil
}
func newPrReviewCmd() *cobra.Command {
	var agent string
	c := &cobra.Command{Use: "review <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		p, e := ReviewPR("", a[0], agent)
		if e == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "review %s: %s by %s\n", p.ID, p.ReviewVerdict, p.Reviewer)
		}
		return e
	}}
	c.Flags().StringVar(&agent, "agent", "", "independent reviewer binding")
	_ = c.MarkFlagRequired("agent")
	return c
}
func ReleaseCore(home, repo, trigger string) (string, error) {
	u, err := ConfiguredUpstream(home, repo)
	if err != nil {
		return "", err
	}
	if u == "" {
		return "", fmt.Errorf("src: release-core requires a configured upstream; use src upstream set %s <url-or-path>", repo)
	}
	prs, err := ListPRs(home, repo)
	if err != nil {
		return "", err
	}
	var pr *PR
	for i := range prs {
		p := &prs[i]
		if p.State == PRStateMerged && p.CoreRepair && suitePass(*p) && p.ReviewVerdict == "pass" && p.MergedSHA != "" {
			if pr == nil || p.MergedAt > pr.MergedAt {
				pr = p
			}
		}
	}
	if pr == nil {
		return "", fmt.Errorf("src: no merged, reviewed, full-suite PR for %s", repo)
	}
	dir, err := repoDir(home, repo)
	if err != nil {
		return "", err
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return "", err
	}
	head, err := r.Reference(plumbing.NewBranchReferenceName(localBranch), true)
	if err != nil {
		return "", err
	}
	if head.Hash().String() != pr.MergedSHA {
		return "", fmt.Errorf("src: local branch moved since PR %s merged", pr.ID)
	}
	tags, err := releaseTags(r, "")
	if err != nil {
		return "", err
	}
	if old := releasedAs(tags)[pr.MergedSHA]; old != "" {
		return "", fmt.Errorf("src: core PR %s already released as %s", pr.ID, old)
	}
	version := "v0.0.1"
	if len(tags) > 0 {
		v := tags[0].Version
		parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
		if len(parts) == 3 {
			var patch int
			fmt.Sscanf(parts[2], "%d", &patch)
			version = fmt.Sprintf("v%s.%s.%d", parts[0], parts[1], patch+1)
		}
	}
	if !semver.IsValid(version) {
		return "", fmt.Errorf("src: invalid next version %s", version)
	}
	msg := fmt.Sprintf("Core repair %s\n\nTrigger: %s\nPR: %s\nSuite-Evidence: %s\nReviewer: %s\n", version, trigger, pr.ID, pr.Profile, pr.Reviewer)
	if _, err = r.CreateTag(version, head.Hash(), &gogit.CreateTagOptions{Tagger: &object.Signature{Name: agentName(), Email: "bashy@localhost", When: time.Now()}, Message: msg}); err != nil {
		return "", err
	}
	// Use a dedicated remote bound to this installation's configured upstream.
	name := "bashy-installation"
	if _, err = r.Remote(name); err == nil {
		_ = r.DeleteRemote(name)
	}
	if _, err = r.CreateRemote(&config.RemoteConfig{Name: name, URLs: []string{u}}); err != nil {
		return "", err
	}
	err = r.Push(&gogit.PushOptions{RemoteName: name, RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/local:refs/heads/local"), config.RefSpec("refs/tags/" + version + ":refs/tags/" + version)}})
	if err != nil {
		return version, fmt.Errorf("src: release %s tagged locally but push failed: %w", version, err)
	}
	return version, nil
}
func newReleaseCoreCmd() *cobra.Command {
	var trigger string
	c := &cobra.Command{Use: "release-core <repo>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		v, e := ReleaseCore("", a[0], trigger)
		if e == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "released %s %s to configured upstream\n", a[0], v)
		}
		return e
	}}
	c.Flags().StringVar(&trigger, "trigger", "", "incident or heal id")
	_ = c.MarkFlagRequired("trigger")
	return c
}
