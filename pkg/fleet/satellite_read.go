package fleet

// `satellite list` / `satellite show` — read-only daily-operation rods
// (S598 story 299f93c16dd2).
//
// Both verbs speak exactly one Bearer [REDACTED] endpoint already served by the
// master:
//
//	GET /api/v1/satellites -> {"satellites": [{id, name, endpoint, state}]}
//
// `show` selects from the list response; there is no detail/status lookup
// and no host-assignment inference. The JSON envelopes mirror the wire:
// `list --json` emits {"satellites":[...]}, `show --json` emits
// {"satellite":{...}}. Empty lists succeed. Reads issue no mutations.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// selectSatellite resolves a `show` key against the list response. An exact
// ID match always wins; otherwise the key must match exactly one name.
// Missing and ambiguous keys fail explicitly instead of guessing.
func selectSatellite(rows []SatelliteEntry, key string) (SatelliteEntry, error) {
	var zero SatelliteEntry
	for _, r := range rows {
		if r.ID == key {
			return r, nil
		}
	}
	var matches []SatelliteEntry
	for _, r := range rows {
		if r.Name == key {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return zero, fmt.Errorf("not found or not yours")
	case 1:
		return matches[0], nil
	default:
		return zero, fmt.Errorf("fleet: satellite %q is ambiguous (%d satellites share that name); pass an ID", key, len(matches))
	}
}

// NewSatelliteListCmd builds the `satellite list` verb (alias `ls`).
func NewSatelliteListCmd() *cobra.Command {
	var master, token string
	var asJSON bool
	c := &cobra.Command{
		Use:     "list",
		Short:   "List the satellites visible to the caller",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := NewOwnerClient(master, token)
			if err != nil {
				return err
			}
			rows, err := client.Satellites(cmd.Context())
			if err != nil {
				return err
			}
			if rows == nil {
				rows = []SatelliteEntry{}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{"satellites": rows})
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "no satellites")
				return nil
			}
			fmt.Fprintln(out, "ID\tNAME\tENDPOINT\tSTATE")
			for _, r := range rows {
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\n",
					r.ID, dashIfEmpty(r.Name), dashIfEmpty(r.Endpoint), dashIfEmpty(r.State))
			}
			return nil
		},
	}
	c.Flags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	c.Flags().StringVar(&token, "token", "", "owner Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY, else the paired credential)")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

// NewSatelliteShowCmd builds the `satellite show <id|name>` verb. The record
// comes from the list response; only ID, name, endpoint and state surface.
func NewSatelliteShowCmd() *cobra.Command {
	var master, token string
	var asJSON bool
	c := &cobra.Command{
		Use:   "show <id|name>",
		Short: "Show one satellite by ID or unique exact name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := strings.TrimSpace(args[0])
			if key == "" {
				return fmt.Errorf("fleet: satellite show needs an ID or name")
			}
			client, err := NewOwnerClient(master, token)
			if err != nil {
				return err
			}
			rows, err := client.Satellites(cmd.Context())
			if err != nil {
				return err
			}
			sat, err := selectSatellite(rows, key)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{"satellite": sat})
			}
			fmt.Fprintf(out, "id: %s\n", sat.ID)
			fmt.Fprintf(out, "name: %s\n", dashIfEmpty(sat.Name))
			fmt.Fprintf(out, "endpoint: %s\n", dashIfEmpty(sat.Endpoint))
			fmt.Fprintf(out, "state: %s\n", dashIfEmpty(sat.State))
			return nil
		},
	}
	c.Flags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	c.Flags().StringVar(&token, "token", "", "owner Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY, else the paired credential)")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}
