package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/provider"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/enroll"
)

// `infra provision` — thin CLI over pkg/provider/enroll (S472.13b item 1,
// executable path). It runs on user-owned compute only: the dispatcher
// opens ExecAllowed, the provider credential is resolved from a
// pkg/secrets reference (never a manifest field, never sent to the
// master), and the master session comes from the existing bashy
// login/bearer ladder (fleet.ResolveCloud: --token >
// $BASHY_FLEET_TOKEN > $BASHY_API_KEY > paired outpost via `bashy login`;
// --master overrides $BASHY_CLOUDBOX_URL).
//
// It is PROVIDER-AGNOSTIC (S522.2 item 9): which providers are compiled
// in, what their region/size/image defaults are, and how each one is
// opened all live in infra_builtin.go. The flag defaults follow the
// provider named on the command line — a Lightsail bundle id is not a
// DigitalOcean size slug — and a flag the caller set explicitly always
// wins over the provider's default.
func newInfraProvision(opts []Option) *cobra.Command {
	var satellite, master, token, endpoint, providerFlag string
	var region, size, image, hostName, owner, credentialRef, key, profile string
	var sprint, story, ttl string
	var budgetCap, budgetPerInstance float64
	var dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "provision [PROVIDER]",
		Short: "Provision a satellite host via an enrollment bundle (executable path)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := provisionProviderName(args, providerFlag)
			if err != nil {
				return err
			}
			p, ok := New(opts...).InfraProvider(name)
			if !ok {
				return fmt.Errorf("fleet: no infra provider %q", name)
			}
			if satellite == "" {
				return fmt.Errorf("fleet: infra provision needs --satellite")
			}
			// Per-provider flag defaults. --region/--size/--image carry
			// no cobra default, because the right one depends on the
			// provider named on the command line: a Lightsail bundle id
			// is not a DigitalOcean size slug. An unset flag takes the
			// named provider's value here instead.
			defaults := infraDefaults(p.Manifest.Name)
			if region == "" {
				region = defaults.Region
			}
			if size == "" {
				size = defaults.Size
			}
			if image == "" {
				image = defaults.Image
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
			openOpts := builtinOpenOptions{
				Owner: owner, Region: region,
				Sprint: sprint, Story: story, TTL: ttl,
				BudgetCap: budgetCap, BudgetPerInstance: budgetPerInstance,
			}
			builtin, isBuiltin := builtinFor(p)
			if dryRun {
				return runProvisionDryRun(cmd, dryRunRequest{
					Provider: p.Manifest.Name, Satellite: satellite,
					Region: region, Size: size, Image: image,
					Profile: profile, IdempotencyKey: key,
					Name: hostName, Endpoint: endpoint, Owner: owner,
					CredentialRef: credentialRef, Builtin: isBuiltin,
				}, openOpts, asJSON)
			}
			ref := credentialRef
			if ref == "" {
				ref = p.Manifest.Name
			}
			cred, cerr := resolveProviderCredential(ref)
			if cerr != nil {
				return cerr
			}
			base, session := ResolveCloud(master, token)
			if session == "" {
				return fmt.Errorf("fleet: no master session (pair with `bashy login`, set $BASHY_FLEET_TOKEN / $BASHY_API_KEY, or pass --token)")
			}
			manifest := p.Manifest
			builtins := map[string]provider.BuiltinFactory{}
			if isBuiltin {
				// A built-in entry dispatches to the linked-in Go
				// adapter, never the declarative engine and never a
				// spawned program.
				manifest = builtin.manifest()
				manifest.Name = p.Manifest.Name
				builtins[manifest.Name] = builtinFactory(p.Manifest.Name, openOpts)
			}
			ctx := context.Background()
			d, oerr := provider.Open(ctx, manifest, provider.Options{
				Owner:       owner,
				Exec:        provider.ExecAllowed, // user-owned compute only
				Credentials: provider.StaticCredential(cred),
				Builtins:    builtins,
			})
			if oerr != nil {
				return oerr
			}
			out := cmd.OutOrStdout()
			res, perr := enroll.ProvisionExecutable(ctx, enroll.Client{BaseURL: base, Session: session}, d,
				enroll.ProvisionRequest{
					SatelliteID: satellite,
					Endpoint:    endpoint,
					Spec: provider.Spec{
						Name: hostName, Region: region, Size: size, Image: image,
					},
					IdempotencyKey: key,
					Progress:       func(s string) { fmt.Fprintln(out, s) },
				})
			if perr != nil {
				return perr
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
					if len(res.Resource.PublicIPs) > 0 {
						row["public_ips"] = res.Resource.PublicIPs
					}
					return json.NewEncoder(out).Encode(row)
				}
				fmt.Fprintf(out, "provisioned %s (%s)\n", res.Resource.ID, res.Resource.State)
				if len(res.Resource.PublicIPs) > 0 {
					fmt.Fprintf(out, "address: %s\n", strings.Join(res.Resource.PublicIPs, ", "))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&providerFlag, "provider", "", "provider package to use, as an alternative to the PROVIDER argument")
	cmd.Flags().StringVar(&satellite, "satellite", "", "satellite id the owner names at the master (required)")
	cmd.Flags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	cmd.Flags().StringVar(&token, "token", "", "master session bearer (default the bashy login/bearer ladder)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "optional public endpoint hint sent to the master")
	// No cobra default on these three: the default is per-provider and is
	// filled in above. The help text lists every built-in's value,
	// generated from the registry so it cannot drift from the behavior.
	cmd.Flags().StringVar(&region, "region", "", "provider region ("+defaultsHelp(func(d provisionDefaults) string { return d.Region })+")")
	cmd.Flags().StringVar(&size, "size", "", "provider size: a DigitalOcean slug or an AWS Lightsail bundle id ("+defaultsHelp(func(d provisionDefaults) string { return d.Size })+")")
	cmd.Flags().StringVar(&image, "image", "", "provider image: a DigitalOcean image slug or an AWS Lightsail blueprint id ("+defaultsHelp(func(d provisionDefaults) string { return d.Image })+")")
	cmd.Flags().StringVar(&profile, "profile", "", "satellite workload profile: \"\" (master default), \"standard\" or \"third_party\"")
	cmd.Flags().StringVar(&hostName, "name", "", "host name (default the satellite id)")
	cmd.Flags().StringVar(&owner, "owner", "local", "ownership tag scope")
	cmd.Flags().StringVar(&credentialRef, "credential-ref", "", "pkg/secrets ref for the provider credential (default the provider name)")
	cmd.Flags().StringVar(&key, "key", "", "idempotency key (default infra-provision-<satellite>)")
	cmd.Flags().StringVar(&sprint, "sprint", "", "sprint number recorded as a governance tag on created resources")
	cmd.Flags().StringVar(&story, "story", "", "story number recorded as a governance tag on created resources")
	cmd.Flags().StringVar(&ttl, "ttl", "", "ttl recorded as a governance tag on created resources")
	cmd.Flags().Float64Var(&budgetCap, "budget-cap", 0,
		fmt.Sprintf("monthly spend cap in USD the create must stay within (0 = the provider default, $%.0f for aws)", awsprovider.DefaultMonthlyCapUSD))
	cmd.Flags().Float64Var(&budgetPerInstance, "budget-per-instance", 0,
		fmt.Sprintf("per-instance monthly ceiling in USD (0 = the provider default, $%.0f for aws)", awsprovider.DefaultPerInstanceUSD))
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate and print the exact request plus the size and budget preflight; create nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// provisionProviderName resolves which provider to use. The provider is
// the command's positional argument, and --provider names the same thing:
// callers and runbooks write it both ways, so both are accepted. Giving
// both is only an error when they DISAGREE — a command that names one
// provider twice is unambiguous, and a command that names two is a
// mistake worth stopping on rather than silently picking one.
func provisionProviderName(args []string, flag string) (string, error) {
	var arg string
	if len(args) == 1 {
		arg = strings.TrimSpace(args[0])
	}
	flag = strings.TrimSpace(flag)
	switch {
	case arg != "" && flag != "" && arg != flag:
		return "", fmt.Errorf("fleet: infra provision names two providers (%q and --provider %q); name one", arg, flag)
	case arg != "":
		return arg, nil
	case flag != "":
		return flag, nil
	}
	return "", fmt.Errorf("fleet: infra provision needs a provider (`infra provision PROVIDER` or --provider PROVIDER); `infra list` shows them, built-in: %s",
		strings.Join(BuiltinInfraProviders(), ", "))
}

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
// prints. Builtin reports whether the provider resolves to a linked-in Go
// adapter, which is what makes a preflight possible at all.
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
	Builtin        bool   `json:"-"`
}

// dryRunPreflight is the size-availability verdict. Checked is false when
// no credential (or no supporting provider) was available: the request
// still prints, with the skip note, and the command succeeds.
//
// Budget is the provider's spend verdict when it has one (AWS). It is
// reported alongside rather than folded into Note, so `--json` callers can
// read the numbers the refusal was computed from.
type dryRunPreflight struct {
	Checked      bool                 `json:"checked"`
	OK           bool                 `json:"ok"`
	Note         string               `json:"note"`
	PriceMonthly float64              `json:"price_monthly,omitempty"`
	PriceHourly  float64              `json:"price_hourly,omitempty"`
	Budget       *awsprovider.Verdict `json:"budget,omitempty"`
	// Request is the provider-shaped plan the create would send, so a
	// dry-run shows the actual request and not just its flags.
	Request json.RawMessage `json:"provider_request,omitempty"`
}

// runProvisionDryRun validates the exact request, prints it, and runs the
// provider's own plan preflight when a credential is present. It performs
// no enrollment, no master call and no provider create: a size/region
// mismatch that the live path would only discover as a 422 at create
// time — or a spend that would breach the budget cap — fails here instead.
func runProvisionDryRun(cmd *cobra.Command, req dryRunRequest, openOpts builtinOpenOptions, asJSON bool) error {
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
	preflight, err := provisionPreflight(req, ref, openOpts)
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
	if b := preflight.Budget; b != nil {
		fmt.Fprintf(out, "budget: %s\n", b.Reason)
	}
	if len(preflight.Request) > 0 {
		fmt.Fprintf(out, "provider_request: %s\n", preflight.Request)
	}
	return nil
}

// provisionPreflight runs the provider's plan operation: it validates the
// size and image in the region and, for a provider with a spend policy,
// returns that policy's verdict. Without a credential it returns an
// unchecked verdict with a skip note (success: nothing to check against).
// A present credential that the provider rejects, a size missing in the
// region, or a refused budget is an ERROR: the dry-run catches what the
// live path would only surface at create time.
func provisionPreflight(req dryRunRequest, ref string, openOpts builtinOpenOptions) (dryRunPreflight, error) {
	cred, err := resolveProviderCredential(ref)
	if err != nil {
		return dryRunPreflight{Note: "skipped (" + err.Error() + ")"}, nil
	}
	if !req.Builtin {
		return dryRunPreflight{Note: "skipped (the preflight runs only for a built-in provider: " +
			strings.Join(BuiltinInfraProviders(), ", ") + ")"}, nil
	}
	openOpts.Credential = cred
	a, err := openBuiltin(req.Provider, openOpts)
	if err != nil {
		return dryRunPreflight{}, err
	}
	res, planErr := a.Do(context.Background(), provider.Request{
		ProtocolVersion: provider.ProtocolVersion,
		Op:              provider.OpPlan,
		Spec: &provider.Spec{
			Name: req.Name, Region: req.Region, Size: req.Size, Image: req.Image,
		},
	})
	pf := dryRunPreflight{Checked: true, Request: res.Data}
	// The plan payload differs per provider; read the fields they share
	// and the AWS budget verdict when it is there. A payload the plan
	// returned alongside a refusal is still reported: it is what explains
	// the refusal.
	var shared struct {
		PriceMonthly float64              `json:"price_monthly"`
		PriceHourly  float64              `json:"price_hourly"`
		Budget       *awsprovider.Verdict `json:"budget"`
	}
	if len(res.Data) != 0 && json.Unmarshal(res.Data, &shared) == nil {
		pf.PriceMonthly, pf.PriceHourly, pf.Budget = shared.PriceMonthly, shared.PriceHourly, shared.Budget
	}
	if planErr != nil {
		return dryRunPreflight{}, fmt.Errorf("fleet: dry-run preflight: %v", planErr)
	}
	pf.OK = true
	switch {
	case pf.PriceMonthly != 0:
		pf.Note = fmt.Sprintf("size %s is available in region %s ($%.2f/mo)", req.Size, req.Region, pf.PriceMonthly)
	default:
		pf.Note = fmt.Sprintf("size %s is available in region %s", req.Size, req.Region)
	}
	return pf, nil
}
