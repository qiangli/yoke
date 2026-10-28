package fleet

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/secrets"
)

// Built-in provider adapters (S472.4a DigitalOcean, S522.2 AWS).
//
// A built-in entry's behavior ships linked into the binary as a Go
// adapter: it is never dispatched to the declarative engine and never
// spawns a program. This file is the ONE place that knows which names are
// built in and how to open each one, so `infra provision` carries no
// per-provider branching: it looks a name up here, takes that provider's
// flag defaults, and asks it for an adapter.
//
// Everything a provider differs about lives in its entry:
//
//   - FLAG DEFAULTS. A DigitalOcean size slug is not a Lightsail bundle id
//     and a DO region is not an AWS one, so each provider names its own
//     defaults and the flags take them unless the caller said otherwise.
//   - CREDENTIALS. The dispatcher resolves ONE credential string, which it
//     then scrubs from every journal record and log line. For a provider
//     that needs more than one value (AWS wants a key pair), the SECRET is
//     the dispatcher credential and the non-secret half is read from the
//     environment where the adapter is built. Nothing here prints a
//     credential, and the resolver returns values it never logs.
//   - ENDPOINT. Each entry's open closure reads its provider's
//     package-level base-URL variable below, so a test can point one
//     provider at a fake. Those variables are the ONLY seam: no flag and
//     no environment variable sets them, so production cannot be
//     redirected by accident.
type builtinProvider struct {
	// defaults are the --region/--size/--image defaults for this provider.
	defaults provisionDefaults
	// manifest is the linked-in adapter's manifest.
	manifest func() provider.Manifest
	// credential resolves the dispatcher credential (the secret half).
	credential func(ref string) (string, error)
	// open builds the adapter from the resolved credential.
	open func(opts builtinOpenOptions) (provider.Adapter, error)
}

// provisionDefaults are one provider's region/size/image defaults.
type provisionDefaults struct{ Region, Size, Image string }

// builtinOpenOptions is everything a built-in adapter is opened with.
type builtinOpenOptions struct {
	// Credential is the secret the dispatcher resolved and scrubs. It is
	// filled in by builtinFactory, not by the caller.
	Credential string
	Owner      string
	Region     string
	// Sprint, Story and TTL become governance tags on created resources
	// for providers that support them.
	Sprint, Story, TTL string
	// BudgetCap and BudgetPerInstance override a provider's spend caps;
	// zero means that provider's default.
	BudgetCap, BudgetPerInstance float64
	// BudgetName is the account budget the provider's spend preflight
	// reads (AWS); empty means that provider's default.
	BudgetName string
	// UserDataBucket is the bucket an oversize first-boot script is
	// staged in (AWS); empty means $AWS_USERDATA_BUCKET, else the
	// provider's per-account default.
	UserDataBucket string
}

// digitaloceanBaseURL and awsLightsailBaseURL override the API roots used
// by the built-in adapters. Empty means the real API. Tests point them at
// their fakes; no flag or environment variable sets them, so production
// can never be redirected by accident.
var (
	digitaloceanBaseURL string
	awsLightsailBaseURL string
	// awsCostExplorerURL, awsBudgetsURL and awsSTSURL are the budget
	// preflight's endpoint seams, held separately because Cost Explorer,
	// AWS Budgets and STS are different services from Lightsail even
	// though one fake serves all of them in tests.
	awsCostExplorerURL string
	awsBudgetsURL      string
	awsSTSURL          string
	// awsS3URL is the user-data staging bucket's endpoint seam. S3 is a
	// different service again, and a different protocol: the object is
	// the URL, not an X-Amz-Target.
	awsS3URL string
)

// builtinProviders is the registry, by manifest name.
var builtinProviders = map[string]builtinProvider{
	digitalocean.Name: {
		// The historical defaults, which are the fixture values the
		// existing tests and the declarative scaffold use.
		defaults: provisionDefaults{Region: "r1", Size: "s-1vcpu-1gb", Image: "img"},
		manifest: digitalocean.Manifest,
		credential: func(ref string) (string, error) {
			return resolveEnvCredential(ref, "DIGITALOCEAN_ACCESS_TOKEN")
		},
		open: func(o builtinOpenOptions) (provider.Adapter, error) {
			return digitalocean.New(digitalocean.Config{
				Token: o.Credential, Owner: o.Owner, BaseURL: digitaloceanBaseURL,
			})
		},
	},
	awsprovider.Name: {
		// Lightsail's own vocabulary: a region, a bundle id and a
		// blueprint id. us-west-2 is the only region the Sprint 522
		// automation principal may act in.
		defaults: provisionDefaults{
			Region: awsprovider.DefaultRegion,
			Size:   awsprovider.DefaultBundleID,
			Image:  awsprovider.DefaultBlueprintID,
		},
		manifest: awsprovider.Manifest,
		// AWS needs a key PAIR. The secret is the dispatcher credential
		// (so the dispatcher scrubs it); the key id is read from the
		// environment in open, where it is used and never stored.
		credential: func(ref string) (string, error) {
			return resolveEnvCredential(ref, awsprovider.EnvSecretAccessKey)
		},
		open: func(o builtinOpenOptions) (provider.Adapter, error) {
			creds, perr := awsprovider.CredentialsFromEnv(os.Environ())
			if perr != nil {
				return nil, perr
			}
			// The dispatcher's resolved credential wins for the secret:
			// it is what the journal scrubs.
			if o.Credential != "" {
				creds.SecretAccessKey = o.Credential
			}
			return awsprovider.New(awsprovider.Config{
				Credentials:     creds,
				Region:          o.Region,
				Owner:           o.Owner,
				BaseURL:         awsLightsailBaseURL,
				CostExplorerURL: awsCostExplorerURL,
				BudgetsURL:      awsBudgetsURL,
				STSURL:          awsSTSURL,
				S3URL:           awsS3URL,
				UserDataBucket:  o.UserDataBucket,
				Sprint:          o.Sprint,
				Story:           o.Story,
				TTL:             o.TTL,
				Budget: awsprovider.Budget{
					Name:           o.BudgetName,
					MonthlyCapUSD:  o.BudgetCap,
					PerInstanceUSD: o.BudgetPerInstance,
				},
			})
		},
	},
}

// builtinFor returns the built-in entry for a loaded provider, if its
// manifest is marked builtin and the name is one this binary links in.
func builtinFor(p InfraProvider) (builtinProvider, bool) {
	if !p.Builtin {
		return builtinProvider{}, false
	}
	b, ok := builtinProviders[p.Manifest.Name]
	return b, ok
}

// infraDefaults are the region/size/image defaults for a provider name.
// An unknown (or non-built-in) name keeps the historical defaults, so a
// declarative or executable provider behaves exactly as before.
func infraDefaults(name string) provisionDefaults {
	if b, ok := builtinProviders[name]; ok {
		return b.defaults
	}
	return builtinProviders[digitalocean.Name].defaults
}

// BuiltinInfraProviders lists the compiled-in provider names, sorted.
func BuiltinInfraProviders() []string {
	out := make([]string, 0, len(builtinProviders))
	for name := range builtinProviders {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// defaultsHelp renders one default per built-in provider for a flag's
// help text ("default: aws us-west-2, digitalocean r1"), generated from
// the registry so the help can never drift from the behavior.
func defaultsHelp(pick func(provisionDefaults) string) string {
	parts := make([]string, 0, len(builtinProviders))
	for _, name := range BuiltinInfraProviders() {
		parts = append(parts, name+" "+pick(builtinProviders[name].defaults))
	}
	return "default: " + strings.Join(parts, ", ")
}

// resolveEnvCredential resolves a provider credential exactly like
// `bashy secret env` would for one ref: the process environment's
// conventional names for the ref (via pkg/secrets), plus the extra names
// the vault projects for this provider. It never reaches the master and
// never consults a manifest field.
//
// The value is RETURNED, never printed: callers pass it to an adapter, and
// the error path names only which variables were looked for.
func resolveEnvCredential(ref string, extra ...string) (string, error) {
	if kv, ok := secrets.ResolveAgentKey(os.Environ(), ref); ok {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			return kv[i+1:], nil
		}
	}
	for _, name := range extra {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, nil
		}
	}
	names := append(secrets.CredentialEnvNames(ref), extra...)
	return "", fmt.Errorf("fleet: no credential for ref %q (want one of %s in the environment via `bashy secret env`)",
		ref, strings.Join(names, ", "))
}

// resolveProviderCredential resolves the credential for a provider ref,
// using the built-in entry's resolver when the ref names one.
func resolveProviderCredential(ref string) (string, error) {
	if b, ok := builtinProviders[ref]; ok {
		return b.credential(ref)
	}
	return resolveEnvCredential(ref)
}

// openBuiltin builds a linked-in adapter for a provider name.
func openBuiltin(name string, o builtinOpenOptions) (provider.Adapter, error) {
	b, ok := builtinProviders[name]
	if !ok {
		return nil, fmt.Errorf("fleet: %q is not a built-in provider (built-in: %s)",
			name, strings.Join(BuiltinInfraProviders(), ", "))
	}
	return b.open(o)
}

// builtinFactory adapts openBuiltin to the dispatcher's BuiltinFactory.
func builtinFactory(name string, o builtinOpenOptions) provider.BuiltinFactory {
	return func(_ context.Context, _ provider.Manifest, credential string) (provider.Adapter, error) {
		o.Credential = credential
		return openBuiltin(name, o)
	}
}
