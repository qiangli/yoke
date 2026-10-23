package src

import (
	"fmt"

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
  src check        flag working-tree edits under core or protected paths`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newShowCmd())
	cmd.AddCommand(newFetchCmd())
	cmd.AddCommand(newCheckCmd())
	return cmd
}

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <cmd>",
		Short: "Show a command's source repo, Go package, class, and release commit",
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
