package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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
	var region, size, image, hostName, owner, credentialRef, key, profile string
	var dryRun, asJSON bool
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
			if err := validateProvisionProfile(profile); err != nil {
				return err
			}
			if dryRun {
				return runProvisionDryRun(cmd, dryRunRequest{
					Provider: name, Satellite: satellite,
					Region: region, Size: size, Image: image,
					Profile: profile, IdempotencyKey: key,
					Name: hostName, Endpoint: endpoint, Owner: owner,
					CredentialRef: credentialRef, BuiltinDO: p.Builtin && p.Manifest.Name == digitalocean.Name,
				}, asJSON)
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
				if asJSON {
					row := map[string]any{"id": res.Resource.ID, "state": string(res.Resource.State)}
					if res.Resource.Region != "" {
						row["region"] = res.Resource.Region
					}
					if res.Resource.Size != "" {
						row["size"] = res.Resource.Size
					}
					return json.NewEncoder(out).Encode(row)
				}
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
	cmd.Flags().StringVar(&profile, "profile", "", "satellite workload profile: \"\" (master default), \"standard\" or \"third_party\"")
	cmd.Flags().StringVar(&hostName, "name", "", "host name (default the satellite id)")
	cmd.Flags().StringVar(&owner, "owner", "local", "ownership tag scope")
	cmd.Flags().StringVar(&credentialRef, "credential-ref", "", "pkg/secrets ref for the provider credential (default the provider name)")
	cmd.Flags().StringVar(&key, "key", "", "idempotency key (default infra-provision-<satellite>)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate and print the exact request plus the size preflight; create nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// resolveProviderCredential resolves a provider credential exactly like
// `bashy secret env` would for one ref: the process environment's
// conventional names for the ref (via pkg/secrets), never a manifest
// field and never the master.
//
// For the digitalocean provider it also accepts the vault-injected
// DIGITALOCEAN_ACCESS_TOKEN (the name the vault projects; the generic
// ladder has no _ACCESS_TOKEN shape). The value is returned, never
// printed: callers pass it to the adapter, never to output.
func resolveProviderCredential(ref string) (string, error) {
	if kv, ok := secrets.ResolveAgentKey(os.Environ(), ref); ok {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			return kv[i+1:], nil
		}
	}
	if ref == digitalocean.Name {
		if v := strings.TrimSpace(os.Getenv("DIGITALOCEAN_ACCESS_TOKEN")); v != "" {
			return v, nil
		}
	}
	names := secrets.CredentialEnvNames(ref)
	if ref == digitalocean.Name {
		names = append(names, "DIGITALOCEAN_ACCESS_TOKEN")
	}
	return "", fmt.Errorf("fleet: no credential for ref %q (want one of %s in the environment via `bashy secret env`)",
		ref, strings.Join(names, ", "))
}

// digitaloceanBaseURL overrides the DigitalOcean API root used by the
// provision dry-run preflight. Empty means the public API. Tests point it
// at fakedo; no flag or environment variable sets it, so production can
// never be redirected by accident.
var digitaloceanBaseURL string

// provisionProfiles are the satellite workload contracts the master
// accepts (mirrors cloudbox provisioning validProfile): "" is the master
// default, "standard" is satellite + local Outpost, "third_party" is the
// expose-services-only contract.
var provisionProfiles = map[string]bool{"": true, "standard": true, "third_party": true}

func validateProvisionProfile(profile string) error {
	if !provisionProfiles[profile] {
		return fmt.Errorf("fleet: unknown profile %q (want \"\", \"standard\" or \"third_party\")", profile)
	}
	return nil
}

// provisionKeyPattern is the master's idempotency-key shape
// ([A-Za-z0-9._:-]{1,128}): the dry-run validates it so a bad default or
// a hand-rolled key fails before anything is created.
var provisionKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// dryRunRequest is the exact provision request the dry-run validates and
// prints. BuiltinDO reports whether the provider resolves to the linked-in
// DigitalOcean adapter (the only provider with a sizes preflight here).
type dryRunRequest struct {
	Provider       string `json:"provider"`
	Satellite      string `json:"satellite"`
	Region         string `json:"region"`
	Size           string `json:"size"`
	Image          string `json:"image"`
	Profile        string `json:"profile"`
	IdempotencyKey string `json:"idempotency_key"`
	Name           string `json:"name"`
	Endpoint       string `json:"endpoint,omitempty"`
	Owner          string `json:"owner"`
	CredentialRef  string `json:"credential_ref"`
	BuiltinDO      bool   `json:"-"`
}

// dryRunPreflight is the size-availability verdict. Checked is false when
// no credential (or no supporting provider) was available: the request
// still prints, with the skip note, and the command succeeds.
type dryRunPreflight struct {
	Checked      bool    `json:"checked"`
	OK           bool    `json:"ok"`
	Note         string  `json:"note"`
	PriceMonthly float64 `json:"price_monthly,omitempty"`
	PriceHourly  float64 `json:"price_hourly,omitempty"`
}

// runProvisionDryRun validates the exact request, prints it, and runs the
// size-in-region preflight through the provider's sizes API when a
// credential is present. It performs no enrollment, no master call and no
// provider create: a size/region mismatch that the live path would only
// discover as a 422 at create time fails here instead.
func runProvisionDryRun(cmd *cobra.Command, req dryRunRequest, asJSON bool) error {
	if strings.TrimSpace(req.Region) == "" {
		return fmt.Errorf("fleet: infra provision needs --region")
	}
	if strings.TrimSpace(req.Size) == "" {
		return fmt.Errorf("fleet: infra provision needs --size")
	}
	if strings.TrimSpace(req.Image) == "" {
		return fmt.Errorf("fleet: infra provision needs --image")
	}
	if !provisionKeyPattern.MatchString(req.IdempotencyKey) {
		return fmt.Errorf("fleet: invalid idempotency key %q (want 1-128 chars of [A-Za-z0-9._:-])", req.IdempotencyKey)
	}
	ref := req.CredentialRef
	if ref == "" {
		ref = req.Provider
	}
	preflight, err := provisionPreflight(req, ref)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if asJSON {
		return json.NewEncoder(out).Encode(map[string]any{
			"dry_run":   true,
			"request":   req,
			"preflight": preflight,
		})
	}
	profile := req.Profile
	if profile == "" {
		profile = "(default)"
	}
	fmt.Fprintln(out, "dry-run: creating nothing")
	fmt.Fprintf(out, "provider: %s\n", req.Provider)
	fmt.Fprintf(out, "satellite: %s\n", req.Satellite)
	fmt.Fprintf(out, "region: %s\n", req.Region)
	fmt.Fprintf(out, "size: %s\n", req.Size)
	fmt.Fprintf(out, "image: %s\n", req.Image)
	fmt.Fprintf(out, "profile: %s\n", profile)
	fmt.Fprintf(out, "idempotency_key: %s\n", req.IdempotencyKey)
	fmt.Fprintf(out, "preflight: %s\n", preflight.Note)
	return nil
}

// provisionPreflight checks size availability in region through the
// provider's sizes API. Without a credential it returns an unchecked
// verdict with a skip note (success: nothing to check against). A present
// credential that the provider rejects, or a size missing/unavailable in
// the region, is an error: the dry-run catches the mismatch the live path
// would only surface as a 422 at create time.
func provisionPreflight(req dryRunRequest, ref string) (dryRunPreflight, error) {
	cred, err := resolveProviderCredential(ref)
	if err != nil {
		return dryRunPreflight{Note: "skipped (" + err.Error() + ")"}, nil
	}
	if !req.BuiltinDO {
		return dryRunPreflight{Note: "skipped (size preflight is implemented for the digitalocean provider only)"}, nil
	}
	a, err := digitalocean.New(digitalocean.Config{Token: cred, Owner: req.Owner, BaseURL: digitaloceanBaseURL})
	if err != nil {
		return dryRunPreflight{}, err
	}
	res, err := a.Do(context.Background(), provider.Request{
		ProtocolVersion: provider.ProtocolVersion,
		Op:              provider.OpPlan,
		Spec: &provider.Spec{
			Name: req.Name, Region: req.Region, Size: req.Size, Image: req.Image,
		},
	})
	if err != nil {
		return dryRunPreflight{}, fmt.Errorf("fleet: dry-run preflight: %v", err)
	}
	pf := dryRunPreflight{Checked: true, OK: true}
	if len(res.Data) != 0 {
		var plan digitalocean.Plan
		if json.Unmarshal(res.Data, &plan) == nil {
			pf.PriceMonthly, pf.PriceHourly = plan.PriceMonthly, plan.PriceHourly
		}
	}
	if pf.PriceMonthly != 0 {
		pf.Note = fmt.Sprintf("size %s is available in region %s ($%.2f/mo)", req.Size, req.Region, pf.PriceMonthly)
	} else {
		pf.Note = fmt.Sprintf("size %s is available in region %s", req.Size, req.Region)
	}
	return pf, nil
}
