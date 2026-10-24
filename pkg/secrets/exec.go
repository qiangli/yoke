package secrets

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func newExecCmd(cfg *Config) *cobra.Command {
	return &cobra.Command{
		Use:   "exec NAME [NAME...] -- <cmd> [args]",
		Short: "Run a command with vault secrets in its environment only",
		Long: `Fetch each named secret and run a command with those values in its
environment. The environment variable is the name upper-cased with '/' and
'-' replaced by '_'; use NAME=ENVVAR to choose a variable explicitly.
Values are never cached, written to disk, or printed by secret exec.

  bashy secret exec do/orbit/agent=DIGITALOCEAN_TOKEN -- doctl account get`,
		Args: func(c *cobra.Command, args []string) error {
			if n := c.ArgsLenAtDash(); n < 1 || n >= len(args) {
				return fmt.Errorf("usage: secret exec NAME [NAME...] -- <cmd> [args]")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			n := c.ArgsLenAtDash()
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
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			child := exec.CommandContext(c.Context(), args[n], args[n+1:]...)
			child.Stdin = c.InOrStdin()
			child.Stdout = c.OutOrStdout()
			child.Stderr = c.ErrOrStderr()
			child.Env = child.Environ()
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
}
