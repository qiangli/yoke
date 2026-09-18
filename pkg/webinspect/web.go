package webinspect

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/sdlc"
	"github.com/spf13/cobra"
)

func NewWebCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "web", Short: "web inspection helpers"}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newInspectCmd())
	return cmd
}

func newInspectCmd() *cobra.Command {
	var target string
	var absent, present []string
	var asJSON bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "inspect --url URL",
		Short: "inspect a URL or file with present/absent text checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := sdlc.VerifyContent(cmd.Context(), sdlc.VerifyOptions{
				Target:  target,
				Present: present,
				Absent:  absent,
				Timeout: timeout,
			})
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", res.Status, res.Target)
				for _, c := range res.Checks {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s %q: %s\n", c.Kind, c.Text, c.Status)
				}
				if len(res.Checks) == 0 && err == nil {
					fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(res.Target))
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "URL to inspect")
	cmd.Flags().StringVar(&target, "file", "", "local file to inspect")
	cmd.Flags().StringArrayVar(&absent, "absent", nil, "text that must be absent")
	cmd.Flags().StringArrayVar(&present, "present", nil, "text that must be present")
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "HTTP timeout")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
