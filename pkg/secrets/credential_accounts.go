package secrets

import (
	"encoding/json"
	"slices"

	"github.com/spf13/cobra"
)

// CredentialBindings selects the no-cache exec bindings for the connected
// accounts in a vault listing. Google files are isolated per account. Each
// provider's environment can hold credentials for only one account.
func CredentialBindings(items []Item) (bindings, googleFiles []string) {
	accounts := map[string]map[string][]string{
		"google": {}, "github": {}, "aws": {},
	}
	for _, item := range items {
		provider, account, purpose, ok := credentialPath(item.Name)
		if !ok {
			continue
		}
		switch provider + "/" + purpose {
		case "google/credentials":
			googleFiles = append(googleFiles, item.Name)
		case "google/access-token":
			accounts[provider][account] = []string{item.Name + "=CLOUDSDK_AUTH_ACCESS_TOKEN"}
		case "github/token":
			accounts[provider][account] = []string{item.Name + "=GH_TOKEN"}
		case "aws/access-key-id", "aws/secret-access-key", "aws/session-token":
			if purpose == "session-token" {
				accounts[provider][account] = []string{item.Name + "=AWS_SESSION_TOKEN"}
			} else if _, exists := accounts[provider][account]; !exists {
				accounts[provider][account] = nil
			}
		}
	}
	bindings = append(bindings, googleFiles...)
	for _, provider := range []string{"google", "github", "aws"} {
		if len(accounts[provider]) != 1 {
			continue
		}
		for account, env := range accounts[provider] {
			if provider == "aws" {
				prefix := "aws/" + account + "/"
				env = append([]string{prefix + "access-key-id=AWS_ACCESS_KEY_ID",
					prefix + "secret-access-key=AWS_SECRET_ACCESS_KEY"}, env...)
			}
			bindings = append(bindings, env...)
		}
	}
	return bindings, googleFiles
}

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
