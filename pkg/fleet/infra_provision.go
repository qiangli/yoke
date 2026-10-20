package fleet

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/enroll"
	"github.com/qiangli/yoke/pkg/secrets"
)

// `infra provision` — thin CLI over pkg/provider/enroll (S472.13b item 1,
// executable path). It runs on user-owned compute only: the dispatcher
// opens ExecAllowed, the provider credential is resolved from a
// pkg/secrets reference (never a manifest field, never sent to the
// master), and the master session comes from the existing bashy
// login/bearer ladder (fleet.ResolveCloud: --token >
// $BASHY_FLEET_TOKEN > $BASHY_API_KEY > paired outpost via `bashy login`;
// --master overrides $BASHY_CLOUDBOX_URL).
func newInfraProvision(opts []Option) *cobra.Command {
	var satellite, master, token, endpoint string
	var region, size, image, hostName, owner, credentialRef, key string
	cmd := &cobra.Command{
		Use:   "provision PROVIDER",
		Short: "Provision a satellite host via an enrollment bundle (executable path)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			p, ok := New(opts...).InfraProvider(name)
			if !ok {
				return fmt.Errorf("fleet: no infra provider %q", name)
			}
			if satellite == "" {
				return fmt.Errorf("fleet: infra provision needs --satellite")
			}
			if key == "" {
				key = "infra-provision-" + satellite
			}
			if hostName == "" {
				hostName = satellite
			}
			ref := credentialRef
			if ref == "" {
				ref = p.Manifest.Name
			}
			cred, err := resolveProviderCredential(ref)
			if err != nil {
				return err
			}
			base, session := ResolveCloud(master, token)
			if session == "" {
				return fmt.Errorf("fleet: no master session (pair with `bashy login`, set $BASHY_FLEET_TOKEN / $BASHY_API_KEY, or pass --token)")
			}
			manifest := p.Manifest
			if p.Builtin {
				// A built-in entry dispatches to the linked-in Go
				// adapter, never the declarative engine and never a
				// spawned program.
				manifest = digitalocean.Manifest()
				manifest.Name = p.Manifest.Name
			}
			ctx := context.Background()
			d, err := provider.Open(ctx, manifest, provider.Options{
				Owner:       owner,
				Exec:        provider.ExecAllowed, // user-owned compute only
				Credentials: provider.StaticCredential(cred),
				Builtins: map[string]provider.BuiltinFactory{
					manifest.Name: func(_ context.Context, _ provider.Manifest, credential string) (provider.Adapter, error) {
						return digitalocean.New(digitalocean.Config{Token: credential, Owner: owner})
					},
				},
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			res, err := enroll.ProvisionExecutable(ctx, enroll.Client{BaseURL: base, Session: session}, d,
				enroll.ProvisionRequest{
					SatelliteID: satellite,
					Endpoint:    endpoint,
					Spec: provider.Spec{
						Name: hostName, Region: region, Size: size, Image: image,
					},
					IdempotencyKey: key,
					Progress:       func(s string) { fmt.Fprintln(out, s) },
				})
			if err != nil {
				return err
			}
			if res.Resource != nil {
				fmt.Fprintf(out, "provisioned %s (%s)\n", res.Resource.ID, res.Resource.State)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&satellite, "satellite", "", "satellite id the owner names at the master (required)")
	cmd.Flags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	cmd.Flags().StringVar(&token, "token", "", "master session bearer (default the bashy login/bearer ladder)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "optional public endpoint hint sent to the master")
	cmd.Flags().StringVar(&region, "region", "r1", "provider region")
	cmd.Flags().StringVar(&size, "size", "s-1vcpu-1gb", "provider size")
	cmd.Flags().StringVar(&image, "image", "img", "provider image")
	cmd.Flags().StringVar(&hostName, "name", "", "host name (default the satellite id)")
	cmd.Flags().StringVar(&owner, "owner", "local", "ownership tag scope")
	cmd.Flags().StringVar(&credentialRef, "credential-ref", "", "pkg/secrets ref for the provider credential (default the provider name)")
	cmd.Flags().StringVar(&key, "key", "", "idempotency key (default infra-provision-<satellite>)")
	return cmd
}

// resolveProviderCredential resolves a provider credential exactly like
// `bashy secret env` would for one ref: the process environment's
// conventional names for the ref (via pkg/secrets), never a manifest
// field and never the master.
func resolveProviderCredential(ref string) (string, error) {
	if kv, ok := secrets.ResolveAgentKey(os.Environ(), ref); ok {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			return kv[i+1:], nil
		}
	}
	return "", fmt.Errorf("fleet: no credential for ref %q (want one of %s in the environment via `bashy secret env`)",
		ref, strings.Join(secrets.CredentialEnvNames(ref), ", "))
}
