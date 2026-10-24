package src

import (
	"fmt"

	"github.com/qiangli/yoke/pkg/prune"
	"github.com/spf13/cobra"
)

// NewSrcCmd returns the `src` cobra command tree — the host-agnostic entry
// point the embedding shell mounts (e.g. `bashy src`; M1b owns the wiring).
func NewSrcCmd() *cobra.Command { return newSrcCmd() }

func newSrcCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "src",
		Short: "Source provenance: show a command's source, fetch pinned OSS checkouts, check tree integrity",
		Long: `src maps every command to its source and guards the trees it fetches.

  src show <cmd>   print repo, Go package path, class, and release commit
  src fetch [repo] pin $BASHY_HOME/src/<repo> at the release commit (default yoke)
  src check        flag working-tree edits under core or protected paths
  src branch <repo> <name>
                   create a topic branch off local (seeded from pristine)
  src status [repo]
                   show branch pins and the pristine|modified|modified-protected state
  src diff <repo> [--base pristine|local]
                   diff the base tip to HEAD
  src log [repo]   show recent commits
  src pr open <repo> <branch> --title T [--test CMD]
  src pr show <id> | src pr list [--repo R] | src pr merge <id> | src pr close <id>
  src patch export <id>              print the PR's patch to stdout
  src patch apply <repo> <file> [--title T]
                   apply a patch file on a fresh branch and open a PR
  src upgrade <repo> <commit>
				   move pristine to the release commit and rebase local onto it
  src prune        list stale derived builds; --apply removes them`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newShowCmd())
	cmd.AddCommand(newFetchCmd())
	cmd.AddCommand(newCheckCmd())
	cmd.AddCommand(newBranchCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newDiffCmd())
	cmd.AddCommand(newLogCmd())
	cmd.AddCommand(newPrCmd())
	cmd.AddCommand(newPatchCmd())
	cmd.AddCommand(newUpgradeCmd())
	cmd.AddCommand(prune.NewNounCommand(Pruner{}))
	return cmd
}

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <cmd>",
		Short: "Show a command's source repo, Go package, class, release commit, and protected parts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := Show(args[0])
			if err != nil {
				return err
			}
			pkg := info.Package
			if pkg == "" {
				pkg = "(mounted by the embedding shell)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "command: %s\nrepo: %s\npackage: %s\nclass: %s\ncommit: %s\n",
				info.Command, info.Repo, pkg, info.Class, info.Commit)
			if len(info.ProtectedParts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "protected_parts: (none)")
			} else {
				for _, p := range info.ProtectedParts {
					fmt.Fprintf(cmd.OutOrStdout(), "protected_part: %s:%s\n", p.Repo, p.Path)
				}
			}
			return nil
		},
	}
}

func newFetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch [repo]",
		Short: "Fetch an OSS repo at the running binary's release commit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "yoke"
			if len(args) == 1 {
				repo = args[0]
			}
			msg, err := Fetch("", repo)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}
}

func newCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Flag working-tree edits under core or protected paths",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			violations, err := Check("")
			if err != nil {
				return err
			}
			for _, v := range violations {
				fmt.Fprintf(cmd.OutOrStdout(), "VIOLATION %s %s:%s\n", v.Kind, v.Repo, v.Path)
			}
			if len(violations) > 0 {
				return fmt.Errorf("src check: %d violation(s)", len(violations))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "clean")
			return nil
		},
	}
}
