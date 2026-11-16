package broker

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/broker/door"
)

func newSelfServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "self-service",
		Short: "let members of chosen circles mint their own door key (off by default)",
		Long: `With self-service on, an active mutual member of an allowed circle runs
` + "`bashy llm key URL`" + ` and gets their own key, peer:self-<email>, bound to
their email and circle — no owner step. Every request re-checks membership,
so removing someone from the circle disables their key. ` + "`off`" + ` stops new
mints and rotations; existing keys keep working until revoked. Revoke is
for a leaked key: the member may mint again while still in an allowed circle.

` + "`on`" + ` turns on the signed cloud identity of this host's outpost app "llm" and
stores its SSO secret here (owner-only). A satellite served by an extra
outpost instance needs ` + "`outpost-profile up NAME --app llm`" + ` re-run afterwards.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newSelfServiceOnCmd(), newSelfServiceOffCmd(), newSelfServiceStatusCmd())
	return cmd
}

func selfServicePath() (string, error) {
	p, err := peersPath()
	if err != nil {
		return "", err
	}
	return SelfServiceFile(p), nil
}

func newSelfServiceOnCmd() *cobra.Command {
	var circles []string
	var dryRun bool
	c := &cobra.Command{
		Use:   "on --circle C [--circle C2]",
		Short: "allow members of these circles to mint their own key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var clean []string
			for _, c := range circles {
				for _, part := range strings.Split(c, ",") {
					if part = strings.TrimSpace(part); part != "" {
						clean = append(clean, part)
					}
				}
			}
			if len(clean) == 0 {
				return fmt.Errorf("llm self-service on: name at least one --circle")
			}
			path, err := selfServicePath()
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "would allow self-service for circles %s and enable signed identity on outpost app %q\n", strings.Join(clean, ","), doorServiceName)
				return nil
			}
			if err := ensureLocalAppTrust(cmd.Context(), door.Port(), true); err != nil {
				return fmt.Errorf("llm self-service on: publish outpost app %q with signed identity: %w", doorServiceName, err)
			}
			out, err := outpostRun(cmd.Context(), nil, "apps", "secret", doorServiceName)
			if err != nil {
				return fmt.Errorf("llm self-service on: read the app's SSO secret: %w", err)
			}
			secret := strings.TrimSpace(string(out))
			if secret == "" {
				return fmt.Errorf("llm self-service on: outpost returned no SSO secret for %q", doorServiceName)
			}
			ss := SelfService{Enabled: true, Circles: clean, SSOSecret: secret, Updated: time.Now().UTC()}
			if err := SaveSelfService(path, ss); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "self-service on for circles %s\n", strings.Join(clean, ","))
			return nil
		},
	}
	c.Flags().StringSliceVar(&circles, "circle", nil, "allowed circle (repeatable or comma-separated)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show the change without making it")
	return c
}

func newSelfServiceOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "stop new self-service mints; existing keys keep working",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := selfServicePath()
			if err != nil {
				return err
			}
			ss, err := LoadSelfService(path)
			if err != nil {
				return err
			}
			ss.Enabled, ss.Updated = false, time.Now().UTC()
			if err := SaveSelfService(path, ss); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "self-service off (existing keys keep working; `llm peer revoke NAME` removes one)")
			return nil
		},
	}
}

func newSelfServiceStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "show whether self-service is on and which circles may mint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := selfServicePath()
			if err != nil {
				return err
			}
			ss, err := LoadSelfService(path)
			if err != nil {
				return err
			}
			var keys []string
			if pp, err := peersPath(); err == nil {
				if ps, err := LoadPeerShares(pp); err == nil {
					for _, n := range ps.Names() {
						if strings.HasPrefix(n, SelfPeerPrefix) {
							keys = append(keys, PeerPrincipalPrefix+n)
						}
					}
				}
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
					"enabled": ss.Enabled, "circles": ss.Circles, "identity_configured": ss.SSOSecret != "", "keys": keys,
				})
			}
			state := "off"
			if ss.Enabled {
				state = "on"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "self-service %s; circles: %s; keys: %d\n", state, strings.Join(ss.Circles, ","), len(keys))
			for _, k := range keys {
				fmt.Fprintln(cmd.OutOrStdout(), "  "+k)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}
