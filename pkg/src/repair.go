package src

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/spf13/cobra"
)

// HealRecord is the small, independent part of an outpost heal ledger that
// source repair consumes. Unknown ledger fields remain available in the brief.
type HealRecord struct {
	ID       string          `json:"id"`
	App      string          `json:"app"`
	Incident string          `json:"incident"`
	Time     string          `json:"time"`
	Rung     string          `json:"rung"`
	Kind     string          `json:"kind"`
	Result   string          `json:"result"`
	Evidence json.RawMessage `json:"evidence"`
}

type RepairOptions struct {
	FromHeal    string
	Incident    string
	ID          string
	Agent       string
	ReviewAgent string
	Profile     string
	Release     bool
	// Delegate and Runner let local integrations provide a contained agent and
	// a trusted verifier. The CLI uses bashy delegate and the host verifier.
	Delegate func(context.Context, string, string, string) error
	Runner   verify.Runner
}

type RepairResult struct {
	PR      PR
	Version string
	Branch  string
	Brief   string
}

func readHeal(o RepairOptions) (HealRecord, error) {
	if (o.FromHeal == "") == (o.Incident == "") {
		return HealRecord{}, fmt.Errorf("src: specify exactly one of --from-heal or --incident")
	}
	path := o.FromHeal
	if path == "" {
		path = o.Incident
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if o.Incident == "" || !json.Valid([]byte(o.Incident)) {
			return HealRecord{}, err
		}
		b = []byte(o.Incident)
	}
	lines := bytes.Split(b, []byte{'\n'})
	if o.Incident != "" {
		lines = [][]byte{b}
	}
	var matches []HealRecord
	for n, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec HealRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return HealRecord{}, fmt.Errorf("src: heal record line %d: %w", n+1, err)
		}
		if o.ID == "" || rec.ID == o.ID {
			matches = append(matches, rec)
		}
	}
	if len(matches) != 1 {
		return HealRecord{}, fmt.Errorf("src: expected one heal record, found %d (select with --id)", len(matches))
	}
	r := matches[0]
	if r.Kind != "code" {
		return HealRecord{}, fmt.Errorf("src: heal %q is kind %q; only kind code may enter core repair", r.ID, r.Kind)
	}
	if r.Result != "reported" || r.Rung != "code" {
		return HealRecord{}, fmt.Errorf("src: heal %q must be a reported code-rung incident", r.ID)
	}
	if r.ID == "" {
		return HealRecord{}, fmt.Errorf("src: reported code heal has no record id")
	}
	return r, nil
}

func repairBrief(r HealRecord) string {
	return fmt.Sprintf("Core repair for heal record %s (app %s, incident %s, time %s).\nIncident evidence: %s\nRestore correct behavior. Add a red/green regression test: demonstrate it fails before the fix and passes after. Do not change public behavior beyond the fix. Do not commit; src repair will commit the result.\n", r.ID, r.App, r.Incident, r.Time, r.Evidence)
}

func bashyRepairDelegate(ctx context.Context, dir, agent, brief string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "bashy", "delegate", "--json", "--agent", agent, "--cwd", dir, brief)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("src: delegate %s: %w: %s", agent, err, strings.TrimSpace(string(out)))
	}
	var result struct {
		ExitCode int `json:"exit_code"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return fmt.Errorf("src: delegate returned unreadable JSON: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("src: delegate %s exited %d", agent, result.ExitCode)
	}
	return nil
}

// Repair takes one reported code incident through a local PR and, when
// requested, the existing suite, review, verify, merge and release gates.
func Repair(ctx context.Context, home, repo string, o RepairOptions) (RepairResult, error) {
	var result RepairResult
	if o.Agent == "" {
		o.Agent = strings.TrimSpace(os.Getenv("BASHY_AGENT"))
	}
	rec, err := readHeal(o)
	if err != nil {
		return result, err
	}
	if o.Agent == "" {
		return result, fmt.Errorf("src: repair needs --agent <delegate binding>")
	}
	if o.Release && (o.Profile == "" || o.ReviewAgent == "") {
		return result, fmt.Errorf("src: --release needs --profile and --review-agent")
	}
	dir, err := repoDir(home, repo)
	if err != nil {
		return result, err
	}
	if o.Release {
		if upstream, e := ConfiguredUpstream(home, repo); e != nil {
			return result, e
		} else if upstream == "" {
			return result, fmt.Errorf("src: --release needs a configured upstream")
		}
	}
	if err := requireClean(dir); err != nil {
		return result, err
	}
	sum := sha256.Sum256([]byte(rec.ID))
	branch := fmt.Sprintf("repair/%x", sum[:8])
	result.Branch, result.Brief = branch, repairBrief(rec)
	if err := CreateBranch(home, repo, branch); err != nil {
		return result, err
	}
	delegate := o.Delegate
	if delegate == nil {
		delegate = bashyRepairDelegate
	}
	if err := delegate(ctx, dir, o.Agent, result.Brief); err != nil {
		return result, err
	}
	_, changes, err := ygit.Status(dir)
	if err != nil {
		return result, err
	}
	if len(changes) == 0 {
		return result, fmt.Errorf("src: delegate made no repair")
	}
	if _, err := ygit.Add(ygit.AddOptions{RepoPath: dir, All: true}); err != nil {
		return result, err
	}
	if _, err := ygit.Commit(ygit.CommitOptions{RepoPath: dir, Message: "Repair core for heal " + rec.ID, AuthorName: o.Agent, AuthorEmail: "repair@localhost"}); err != nil {
		return result, err
	}
	pr, err := OpenPR(home, repo, branch, "Core repair: "+rec.ID, "")
	if err != nil {
		return result, err
	}
	pr.Author, pr.AuthorVendor, pr.Trigger, pr.Profile = o.Agent, vendor(o.Agent), rec.ID, o.Profile
	pr.CoreRepair = true
	if err := savePR(home, pr); err != nil {
		return result, err
	}
	result.PR = pr
	if o.ReviewAgent == "" && !o.Release {
		return result, nil
	}
	if o.Profile == "" {
		return result, fmt.Errorf("src: pipeline needs --profile")
	}
	if result.PR, err = RunPRSuite(home, pr.ID, o.Profile); err != nil {
		return result, err
	}
	if result.PR, err = ReviewPR(home, pr.ID, o.ReviewAgent); err != nil {
		return result, err
	}
	proof, err := VerifyPR(ctx, home, pr.ID, o.Runner)
	if err != nil {
		return result, err
	}
	if proof.Verdict != evidence.VerdictPass {
		return result, fmt.Errorf("src: PR %s sealed verify refused: %s", pr.ID, proof.Reason)
	}
	if result.PR, err = MergePR(home, pr.ID); err != nil {
		return result, err
	}
	if o.Release {
		result.Version, err = ReleaseCore(home, repo, rec.ID)
	}
	return result, err
}

func newRepairCmd() *cobra.Command {
	var o RepairOptions
	c := &cobra.Command{Use: "repair <repo>", Short: "Intake a reported code heal as a core repair PR", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		res, err := Repair(cmd.Context(), "", args[0], o)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "opened core repair PR %s from %s", res.PR.ID, filepath.Base(o.FromHeal))
		if res.Version != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "; released %s", res.Version)
		}
		fmt.Fprintln(cmd.OutOrStdout())
		return nil
	}}
	c.Flags().StringVar(&o.FromHeal, "from-heal", "", "outpost heals.jsonl ledger")
	c.Flags().StringVar(&o.Incident, "incident", "", "one incident JSON file")
	c.Flags().StringVar(&o.ID, "id", "", "heal record id in the ledger")
	c.Flags().StringVar(&o.Agent, "agent", "", "delegate author binding")
	c.Flags().StringVar(&o.ReviewAgent, "review-agent", "", "independent reviewer binding")
	c.Flags().StringVar(&o.Profile, "profile", "", "full-suite profile JSON")
	c.Flags().BoolVar(&o.Release, "release", false, "merge and release after all gates pass")
	return c
}
