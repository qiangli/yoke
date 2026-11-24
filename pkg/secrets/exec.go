package secrets

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func newExecCmd(cfg *Config) *cobra.Command {
	var stdinName, googleRoot string
	cmd := &cobra.Command{
		Use:   "exec NAME [NAME...] -- <cmd> [args]",
		Short: "Run a command with vault credentials scoped to the child",
		Long: `Fetch each named secret and run a command with those values in its
environment. The environment variable is the name upper-cased with '/' and
'-' replaced by '_'; use NAME=ENVVAR to choose a variable explicitly.
Values are never cached or printed by secret exec. --no-cache makes this
unconditional behavior explicit.

  bashy secret exec do/orbit/agent=DIGITALOCEAN_TOKEN -- doctl account get

With --stdin NAME the single named secret is written to the command's
STDIN instead of its environment — the handle route for a command that
reads its credential on stdin (the value never appears in argv, in this
process's output, and the child does not inherit our stdin):

  bashy secret exec --stdin HOST_SSH_PW -- outpost connect HOST --stdin

With --google-root DIR, a bare google/<account>/credentials binding creates
DIR/<account>/credentials.json (0600) until the command exits. DIR must
exist and the account directory must be unused. The child receives DIR as
GWS_CONFIG_ROOT.`,
		Args: func(c *cobra.Command, args []string) error {
			n := c.ArgsLenAtDash()
			if c.Flags().Changed("stdin") {
				if googleRoot != "" {
					return fmt.Errorf("--google-root cannot be combined with --stdin")
				}
				if n != 0 {
					return fmt.Errorf("usage: secret exec --stdin NAME -- <cmd> [args] (--stdin is the only name form)")
				}
				if stdinName == "" || strings.Contains(stdinName, "=") {
					return fmt.Errorf("--stdin takes one secret NAME, not a NAME=ENVVAR binding")
				}
			} else if n < 1 || n >= len(args) {
				return fmt.Errorf("usage: secret exec NAME [NAME...] -- <cmd> [args]")
			}
			if n < 0 || n >= len(args) {
				return fmt.Errorf("usage: secret exec NAME [NAME...] -- <cmd> [args]")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			n := c.ArgsLenAtDash()
			client, err := cfg.ResolveContext(c.Context())
			if err != nil {
				return err
			}
			child := exec.CommandContext(c.Context(), args[n], args[n+1:]...)
			child.Stdout = c.OutOrStdout()
			child.Stderr = c.ErrOrStderr()
			child.Env = child.Environ()
			if stdinName != "" {
				// Handle route for a stdin consumer: resolve the value and
				// write it to the child's stdin pipe ONLY — never argv,
				// never this process's stdin or stdout. The value is
				// fetched before the child starts, so an unknown name
				// launches nothing.
				value, err := client.getContext(c.Context(), stdinName)
				if err != nil {
					return err
				}
				pipe, err := child.StdinPipe()
				if err != nil {
					return err
				}
				if err := child.Start(); err != nil {
					return err
				}
				if _, werr := io.WriteString(pipe, value); werr != nil {
					_ = pipe.Close()
					_ = child.Wait()
					return fmt.Errorf("secret exec --stdin: %w", werr)
				}
				if werr := pipe.Close(); werr != nil {
					_ = child.Wait()
					return fmt.Errorf("secret exec --stdin: %w", werr)
				}
				return child.Wait()
			}
			child.Stdin = c.InOrStdin()
			return client.RunProjected(c.Context(), child, googleRoot, args[:n])
		},
	}
	cmd.Flags().StringVar(&stdinName, "stdin", "", "feed ONE named secret to the command on its stdin (never argv, never output)")
	cmd.Flags().Bool("no-cache", true, "fetch fresh values without reading or writing the cache (always enabled)")
	cmd.Flags().StringVar(&googleRoot, "google-root", "", "project Google credential files into this existing scratch directory")
	return cmd
}
