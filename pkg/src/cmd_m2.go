package src

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/spf13/cobra"
)

// principalEnv is the environment fallback for the principal `src activate`
// acts for: the launcher sets it for the worker it starts.
const principalEnv = "BASHY_PRINCIPAL"

func newActivateCmd() *cobra.Command {
	var principal string
	c := &cobra.Command{
		Use:   "activate <app> <rev>",
		Short: "Activate a revision for a principal: gated, atomic, and recorded",
		Long: `activate checks a revision through the activation gate, records a
commit on the principal's version line, and switches that principal's
active revision to it while keeping the previous revision as
last-known-good. Only the named principal's line and record move.

The principal comes from --principal or $BASHY_PRINCIPAL. Without a wired
gate the command refuses rather than activate ungated.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := strings.TrimSpace(principal)
			if p == "" {
				p = strings.TrimSpace(os.Getenv(principalEnv))
			}
			if p == "" {
				return fmt.Errorf("src: activate needs --principal or $%s", principalEnv)
			}
			res, err := Activate("", args[0], args[1], p, ActivateOptions{Gate: effectiveGate()})
			if err != nil {
				return err
			}
			prev := "(none)"
			if res.Previous != "" {
				prev = shortSHA(res.Previous)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "activated %s for %s at %s (was %s)\n",
				res.App, res.Principal, shortSHA(res.Revision), prev)
			return nil
		},
	}
	c.Flags().StringVar(&principal, "principal", "", "web principal to activate for (default $"+principalEnv+")")
	return c
}

func newBranchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "branch <repo> <name>",
		Short: "Create a topic branch off local and check it out",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := CreateBranch("", args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created branch %s in %s off local\n", args[1], args[0])
			return nil
		},
	}
}

// statusRepos resolves the status targets: one repo or every checkout.
func statusRepos(home, repo string) ([]string, error) {
	if repo != "" {
		if _, err := repoDir(home, repo); err != nil {
			return nil, err
		}
		return []string{repo}, nil
	}
	entries, err := os.ReadDir(SrcDir(home))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if st, err := os.Stat(filepath.Join(SrcDir(home), e.Name(), ".git")); err == nil && st.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [repo]",
		Short: "Show branch pins and the pristine|modified|modified-protected state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var repo string
			if len(args) == 1 {
				repo = args[0]
			}
			repos, err := statusRepos("", repo)
			if err != nil {
				return err
			}
			if len(repos) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no fetched repos")
				return nil
			}
			for _, r := range repos {
				dir, err := repoDir("", r)
				if err != nil {
					return err
				}
				st, err := State("", r)
				if err != nil {
					return err
				}
				branch, err := currentBranch(dir)
				if err != nil {
					return err
				}
				if branch == "" {
					branch = "(detached)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "repo: %s\nbranch: %s\npristine: %s\nlocal: %s\nstate: %s\n",
					r, branch, st.Base, st.Local, st.State)
			}
			return nil
		},
	}
}

func newDiffCmd() *cobra.Command {
	var base, user string
	c := &cobra.Command{
		Use:   "diff <repo> [a [b]]",
		Short: "Diff the base branch tip (pristine|local) to HEAD, or two versions",
		Long: `diff with one repo diffs the base branch tip to HEAD, as
before. With two versions (commits, named labels, or base) it diffs one
version to the other for that principal (--user or $BASHY_PRINCIPAL,
only needed to resolve that principal's labels).`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				return fmt.Errorf("src: diff takes <repo> or <repo> <a> <b>")
			}
			if len(args) == 3 {
				if cmd.Flags().Changed("base") {
					return fmt.Errorf("src: --base applies only to the <repo> form of diff")
				}
				p := strings.TrimSpace(user)
				if p == "" {
					p = strings.TrimSpace(os.Getenv(principalEnv))
				}
				text, err := Diff("", args[0], p, args[1], args[2])
				if err != nil {
					return err
				}
				if strings.TrimSpace(text) == "" {
					fmt.Fprintln(cmd.OutOrStdout(), "(no differences)")
					return nil
				}
				fmt.Fprint(cmd.OutOrStdout(), text)
				if !strings.HasSuffix(text, "\n") {
					fmt.Fprintln(cmd.OutOrStdout())
				}
				return nil
			}
			if base != pristineBranch && base != localBranch {
				return fmt.Errorf("src: --base must be pristine or local, got %q", base)
			}
			dir, err := repoDir("", args[0])
			if err != nil {
				return err
			}
			baseSHA, err := ygit.ResolveCommit(dir, base)
			if err != nil {
				return fmt.Errorf("src: no %s branch in %s (%v)", base, args[0], err)
			}
			head, err := ygit.RevParse(ygit.RevParseOptions{RepoPath: dir})
			if err != nil {
				return err
			}
			text, err := diffText(dir, baseSHA, head.Hash)
			if err != nil {
				return err
			}
			if strings.TrimSpace(text) == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "(no differences)")
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			if !strings.HasSuffix(text, "\n") {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	c.Flags().StringVar(&base, "base", localBranch, "base branch: pristine or local")
	userFlag(c, &user)
	return c
}

func newLogCmd() *cobra.Command {
	var number int
	c := &cobra.Command{
		Use:   "log [repo]",
		Short: "Show recent commits from HEAD",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "yoke"
			if len(args) == 1 {
				repo = args[0]
			}
			dir, err := repoDir("", repo)
			if err != nil {
				return err
			}
			_, entries, err := ygit.Log(ygit.LogOptions{RepoPath: dir, Number: number})
			if err != nil {
				return err
			}
			for _, e := range entries {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", e.Hash, e.Message)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&number, "number", "n", 10, "maximum commits to show")
	return c
}

func newPrCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pr",
		Short: "Local pull requests against the host's own local branch",
	}
	c.AddCommand(newPrOpenCmd(), newPrShowCmd(), newPrListCmd(), newPrVerifyCmd(),
		newPrMergeCmd(), newPrCloseCmd())
	return c
}

func newPrOpenCmd() *cobra.Command {
	var title, testCmd string
	c := &cobra.Command{
		Use:   "open <repo> <branch>",
		Short: "Open a pull request for a topic branch against local",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := OpenPR("", args[0], args[1], title, testCmd)
			if err != nil {
				return err
			}
			test := "no test recorded"
			if pr.TestCmd != "" {
				test = "test " + pr.TestResult + " (" + pr.TestCmd + ")"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "opened PR %s: %s (base %s, head %s @ %s, %s)\n",
				pr.ID, pr.Title, pr.Base, pr.Head, shortSHA(pr.HeadSHA), test)
			return nil
		},
	}
	c.Flags().StringVar(&title, "title", "", "PR title (required)")
	c.Flags().StringVar(&testCmd, "test", "", "test command to run now and record on the PR")
	return c
}

func newPrShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one PR record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := GetPR("", args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nrepo: %s\ntitle: %s\nauthor: %s\nbase: %s\nhead: %s\nhead_sha: %s\nstate: %s\n",
				pr.ID, pr.Repo, pr.Title, pr.Author, pr.Base, pr.Head, pr.HeadSHA, pr.State)
			if pr.TestCmd != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "test_cmd: %s\ntest_result: %s\n", pr.TestCmd, pr.TestResult)
			}
			if pr.MergedSHA != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "merged_sha: %s\n", pr.MergedSHA)
			}
			return nil
		},
	}
}

func newPrListCmd() *cobra.Command {
	var repo string
	c := &cobra.Command{
		Use:   "list",
		Short: "List PR records",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := ListPRs("", repo)
			if err != nil {
				return err
			}
			for _, pr := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%s [%s] %s/%s: %s\n",
					pr.ID, pr.State, pr.Repo, pr.Head, pr.Title)
			}
			return nil
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "only PRs for this repo")
	return c
}

func newPrVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <id>",
		Short: "Verify a PR's candidate against the base contract and seal the result",
		Long: `verify runs the base commit's checks against the PR's recorded head in
an isolated clone, accounts for every contracted function the check set
reaches, and seals the verdict for the resolved principal. Merging reads
that record; a failing verdict is recorded too, and blocks.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := VerifyPR(cmd.Context(), "", args[0], nil)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, c := range rec.Checks {
				fmt.Fprintf(out, "%-8s %s\n", c.Status, c.Name)
			}
			for _, fn := range rec.Uncalled {
				fmt.Fprintf(out, "%-8s %s (no check reached it)\n", "uncalled", fn)
			}
			fmt.Fprintf(out, "verdict: %s for %s at %s\n", rec.Verdict, rec.Binding.Principal, shortSHA(rec.Binding.Candidate))
			if rec.Reason != "" {
				fmt.Fprintf(out, "reason: %s\n", rec.Reason)
			}
			if rec.Verdict != evidence.VerdictPass {
				return fmt.Errorf("src: PR %s is not verified", args[0])
			}
			return nil
		},
	}
}

func newPrMergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <id>",
		Short: "Merge an open PR's head into local (never pristine, never any remote)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := MergePR("", args[0])
			if err != nil {
				return err
			}
			note := ""
			if pr.TouchedProtected {
				note = " (protected paths merged under approval)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "merged PR %s into local at %s%s\n",
				pr.ID, shortSHA(pr.MergedSHA), note)
			return nil
		},
	}
}

func newPrCloseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "close <id>",
		Short: "Close an open PR without merging",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := ClosePR("", args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "closed PR %s\n", pr.ID)
			return nil
		},
	}
}

func newPatchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "patch",
		Short: "Move changes between hosts by patch file",
	}
	c.AddCommand(newPatchExportCmd(), newPatchApplyCmd())
	return c
}

func newPatchExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <id>",
		Short: "Print the PR's patch to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := ExportPatch("", args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			if !strings.HasSuffix(text, "\n") {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
}

func newPatchApplyCmd() *cobra.Command {
	var title string
	c := &cobra.Command{
		Use:   "apply <repo> <file>",
		Short: "Apply a patch file on a fresh branch and open a PR",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pr, err := ApplyPatch("", args[0], args[1], title)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "applied %s to branch %s; opened PR %s\n",
				args[1], pr.Head, pr.ID)
			return nil
		},
	}
	c.Flags().StringVar(&title, "title", "", "PR title (default: Apply <file>)")
	return c
}

func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade <repo> <commit>",
		Short: "Move pristine to the release commit and rebase local onto it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			conflicts, err := Upgrade("", args[0], args[1])
			if err != nil {
				if len(conflicts) > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "conflicts: %s\n", strings.Join(conflicts, ", "))
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "upgraded %s: pristine at %s, local rebased\n",
				args[0], shortSHA(args[1]))
			return nil
		},
	}
}

func newRevertCmd() *cobra.Command {
	var apply bool
	c := &cobra.Command{
		Use:   "revert <repo>",
		Short: "Reset the local src checkout to pristine (base tip)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			res, err := Revert("", repo, apply)
			if err != nil {
				return err
			}
			if apply {
				fmt.Fprintf(cmd.OutOrStdout(), "reverted %s to pristine at %s\n", repo, shortSHA(res.PristineSHA))
				return nil
			}
			if len(res.Commits) == 0 && len(res.Files) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(nothing to discard)")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "would discard:")
			for _, c := range res.Commits {
				fmt.Fprintf(cmd.OutOrStdout(), "  commit %s %s\n", shortSHA(c.Hash), c.Message)
			}
			for _, f := range res.Files {
				fmt.Fprintf(cmd.OutOrStdout(), "  file %s\n", f)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "apply the revert, discarding local changes")
	return c
}
