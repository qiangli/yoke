package secrets

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func newExecCmd(cfg *Config) *cobra.Command {
	var stdinName string
	cmd := &cobra.Command{
		Use:   "exec NAME [NAME...] -- <cmd> [args]",
		Short: "Run a command with vault secrets in its environment only",
		Long: `Fetch each named secret and run a command with those values in its
environment. The environment variable is the name upper-cased with '/' and
'-' replaced by '_'; use NAME=ENVVAR to choose a variable explicitly.
Values are never cached, written to disk, or printed by secret exec.

  bashy secret exec do/orbit/agent=DIGITALOCEAN_TOKEN -- doctl account get

With --stdin NAME the single named secret is written to the command's
STDIN instead of its environment — the handle route for a command that
reads its credential on stdin (the value never appears in argv, in this
process's output, and the child does not inherit our stdin):

  bashy secret exec --stdin HOST_SSH_PW -- outpost connect HOST --stdin`,
		Args: func(c *cobra.Command, args []string) error {
			n := c.ArgsLenAtDash()
			if c.Flags().Changed("stdin") {
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
			client, err := cfg.Resolve()
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
				value, err := client.Get(stdinName)
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
			bindings := make([]binding, 0, n)
			for _, arg := range args[:n] {
				name, local, explicit := strings.Cut(arg, "=")
				if !explicit {
					local = strings.ToUpper(strings.NewReplacer("/", "_", "-", "_").Replace(name))
				}
				if name == "" || !validName(local) {
					return fmt.Errorf("invalid secret binding %q: use NAME=ENVVAR with a valid environment variable", arg)
				}
				bindings = append(bindings, binding{local: local, ref: name, isRef: true})
			}
			for _, bd := range bindings {
				value, err := client.Get(bd.ref)
				if err != nil {
					return err
				}
				// os/exec keeps the last entry for a duplicate environment key.
				child.Env = append(child.Env, bd.local+"="+value)
			}
			return child.Run()
		},
	}
	cmd.Flags().StringVar(&stdinName, "stdin", "", "feed ONE named secret to the command on its stdin (never argv, never output)")
	return cmd
}
