package secrets

import (
	"encoding/json"
	"slices"

	"github.com/spf13/cobra"
)

func newAccountsCmd(cfg *Config) *cobra.Command {
	return &cobra.Command{
		Use:   "accounts",
		Short: "List connected Google, GitHub, and AWS accounts as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := cfg.ResolveContext(cmd.Context())
			if err != nil {
				return err
			}
			items, err := client.listContext(cmd.Context())
			if err != nil {
				return err
			}
			accounts := map[string][]string{"google": {}, "github": {}, "aws": {}}
			for _, item := range items {
				provider, account, _, ok := credentialPath(item.Name)
				if names, supported := accounts[provider]; ok && supported {
					accounts[provider] = append(names, account)
				}
			}
			for provider, names := range accounts {
				slices.Sort(names)
				accounts[provider] = slices.Compact(names)
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(accounts)
		},
	}
}
