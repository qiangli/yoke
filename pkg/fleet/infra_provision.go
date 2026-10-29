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
	var budgetName, userDataBucket string
	var dryRun, asJSON bool
	// `--account` names the PROVIDER identity (a DigitalOcean team, an AWS
	// profile). It is deliberately unrelated to --master/--token, which are
	// the cloudbox session this provision enrolls against, and to --profile,
	// which is the satellite's workload profile — so none of those three is
	// a conflict here.
	var acct accountFlag
	cmd := &cobra.Command{
		Use:   "provision [PROVIDER]",
		Short: "Provision a satellite host via an enrollment bundle (executable path)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := providerName("provision", args, providerFlag)
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
			// Both account refusals land before the dry-run print and
			// before the master session is resolved, so neither a network
			// call nor an output line precedes them.
			if err := acct.credentialRefConflict(credentialRef); err != nil {
				return err
			}
			sel, aerr := resolveInfraAccount(cmd.Context(), &acct, p.Manifest.Name)
			if aerr != nil {
				return aerr
			}
			openOpts := builtinOpenOptions{
				Owner: owner, Region: region,
				Sprint: sprint, Story: story, TTL: ttl,
				BudgetCap: budgetCap, BudgetPerInstance: budgetPerInstance,
				BudgetName: budgetName, UserDataBucket: userDataBucket,
				Account: sel,
			}
			_, isBuiltin := builtinFor(p)
			if dryRun {
				return runProvisionDryRun(cmd, dryRunRequest{
					Provider: p.Manifest.Name, Satellite: satellite,
					Region: region, Size: size, Image: image,
					Profile: profile, IdempotencyKey: key,
					Name: hostName, Endpoint: endpoint, Owner: owner,
					CredentialRef: credentialRef, Builtin: isBuiltin,
				}, openOpts, asJSON)
			}
			base, session := ResolveCloud(master, token)
			if session == "" {
				return fmt.Errorf("fleet: no master session (pair with `bashy login`, set $BASHY_FLEET_TOKEN / $BASHY_API_KEY, or pass --token)")
			}
			ctx := context.Background()
			d, oerr := openProviderDispatcher(ctx, p, credentialRef, openOpts)
			if oerr != nil {
				return oerr
			}
			out := cmd.OutOrStdout()
			// The address a provider reserved for this host BEFORE the
			// enrollment bundle was requested, so the endpoint the
			// bundle was rendered with can be reported next to the host
			// it belongs to.
			var reserved provider.Reservation
			res, perr := enroll.ProvisionExecutable(ctx, enroll.Client{BaseURL: base, Session: session}, d,
				enroll.ProvisionRequest{
					SatelliteID: satellite,
					Endpoint:    endpoint,
					Spec: provider.Spec{
						Name: hostName, Region: region, Size: size, Image: image,
					},
					IdempotencyKey: key,
					Progress:       func(s string) { fmt.Fprintln(out, s) },
					Reserved:       func(r provider.Reservation) { reserved = r },
				})
			if perr != nil {
				return perr
			}
			if res.Resource != nil {
				if asJSON {
					row := resourceRow(*res.Resource)
					if reserved.Held() {
						row["reserved_address"] = reserved.Address
						row["reserved_address_name"] = reserved.Name
					}
					if ep := provisionEndpoint(endpoint, reserved); ep != "" {
						row["endpoint"] = ep
					}
					return json.NewEncoder(out).Encode(row)
				}
				fmt.Fprintf(out, "provisioned %s (%s)\n", res.Resource.ID, res.Resource.State)
				if len(res.Resource.PublicIPs) > 0 {
					fmt.Fprintf(out, "address: %s\n", strings.Join(res.Resource.PublicIPs, ", "))
				}
				if ep := provisionEndpoint(endpoint, reserved); ep != "" {
					fmt.Fprintf(out, "endpoint: %s\n", ep)
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
	acct.bind(cmd)
	cmd.Flags().StringVar(&key, "key", "", "idempotency key (default infra-provision-<satellite>)")
	cmd.Flags().StringVar(&sprint, "sprint", "", "sprint number recorded as a governance tag on created resources")
	cmd.Flags().StringVar(&story, "story", "", "story number recorded as a governance tag on created resources")
	cmd.Flags().StringVar(&ttl, "ttl", "", "ttl recorded as a governance tag on created resources")
	cmd.Flags().Float64Var(&budgetCap, "budget-cap", 0,
		fmt.Sprintf("monthly spend cap in USD the create must stay within (0 = the account budget's own limit, else the provider default, $%.0f for aws)", awsprovider.DefaultMonthlyCapUSD))
	cmd.Flags().Float64Var(&budgetPerInstance, "budget-per-instance", 0,
		fmt.Sprintf("per-instance monthly ceiling in USD (0 = the provider default, $%.0f for aws)", awsprovider.DefaultPerInstanceUSD))
	// The budget is the PRIMARY spend source for aws, and its own limit
	// is the cap when --budget-cap is not given (the account's cost
	// explorer may have no data at all; see pkg/provider/aws/budgets.go).
	cmd.Flags().StringVar(&budgetName, "budget-name", "",
		fmt.Sprintf("account budget read for month-to-date spend and, without --budget-cap, for the cap itself (default %q for aws)", awsprovider.DefaultBudgetName))
	// The master's first-boot script is larger than EC2's 16 KiB
	// user-data limit and does not compress into it either, so the
	// adapter stages it in S3 and launches a fetch bootstrap instead
	// (pkg/provider/aws/s3.go). The bucket is per account and region and
	// is provisioned out of band; this names a different one.
	cmd.Flags().StringVar(&userDataBucket, "userdata-bucket", "",
		"bucket an oversize first-boot script is staged in (default $"+awsprovider.EnvUserDataBucket+
			", else "+awsprovider.UserDataBucketPrefix+"<account-id>-<region> for aws)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate and print the exact request plus the size and budget preflight; create nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// provisionEndpoint is the endpoint the enrollment bundle was actually
// rendered for: the caller's own --endpoint, or the endpoint of the
// address reserved before the bundle was requested.
//
// It is deliberately NOT derived from the created host's address when
// neither applies. A provider that reserves nothing was enrolled with no
// endpoint hint at all, and printing one computed after the fact would
// claim the master knows an endpoint it was never told.
func provisionEndpoint(flag string, reserved provider.Reservation) string {
	if flag != "" {
		return flag
	}
	return reserved.Endpoint
}

// providerName resolves which provider an infra verb addresses. The
// provider is the command's positional argument, and --provider names the
// same thing: callers and runbooks write it both ways, so both are
// accepted. Giving both is only an error when they DISAGREE — a command
// that names one provider twice is unambiguous, and a command that names
// two is a mistake worth stopping on rather than silently picking one.
func providerName(verb string, args []string, flag string) (string, error) {
	var arg string
	if len(args) == 1 {
		arg = strings.TrimSpace(args[0])
	}
	flag = strings.TrimSpace(flag)
	switch {
	case arg != "" && flag != "" && arg != flag:
		return "", fmt.Errorf("fleet: infra %s names two providers (%q and --provider %q); name one", verb, arg, flag)
	case arg != "":
		return arg, nil
	case flag != "":
		return flag, nil
	}
	return "", fmt.Errorf("fleet: infra %s needs a provider (`infra %s PROVIDER` or --provider PROVIDER); `infra list` shows them, built-in: %s",
		verb, verb, strings.Join(BuiltinInfraProviders(), ", "))
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
	cred, err := resolveProviderCredentialFor(ref, openOpts.Account)
	if err != nil {
		// A dry run tolerates a missing AMBIENT credential — it creates
		// nothing, so reporting the skip is more useful than refusing. An
		// explicitly named account is different: the operator asked for
		// that identity, and skipping would quietly answer about another.
		if openOpts.Account != nil {
			return dryRunPreflight{}, err
		}
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
