package fleet

// `infra inspect` / `infra list --provider` / `infra destroy` — the
// lifecycle verbs over a provider adapter (S522.2 rework 3).
//
// `infra provision` could create a host and nothing could then look at it
// or take it away: the adapters have carried inspect/list/destroy since
// S472, but no front door reached them, so cleaning up a Lightsail
// satellite meant the AWS console. These three verbs are that front door,
// and they are PROVIDER-AGNOSTIC — they speak only the contract, so they
// work for digitalocean and for any provider added later without a line
// of per-provider code here. DigitalOcean's own `infra trash`/`restore`
// (its soft-delete workflow) are untouched.
//
// Two things make them safe to point at a real account:
//
//   - they go through the DISPATCHER, not straight at an adapter, so a
//     destroy is journaled, the credential is scrubbed from what is
//     recorded, and a replayed destroy is answered from the journal
//     instead of deleting twice;
//   - `--yes` is the A6 destroy confirmation, bound to one resource id and
//     one idempotency key. Without it the provider is never called.
//
// `infra list` keeps its original meaning when no provider is named: it
// lists provider PACKAGES. With `--provider` it lists what that provider
// is actually running, which is the question an operator has when they
// type it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/enroll"
)

// lifecycleFlags are the flags the lifecycle verbs share. The shape
// mirrors `infra provision`: the provider is named positionally or with
// --provider, the region defaults per provider, and --json is machine
// output.
type lifecycleFlags struct {
	providerFlag   string
	name           string
	region         string
	owner          string
	credentialRef  string
	key            string
	userDataBucket string
	yes            bool
	asJSON         bool
	account        accountFlag
}

func (f *lifecycleFlags) bind(cmd *cobra.Command, verb string) {
	cmd.Flags().StringVar(&f.providerFlag, "provider", "", "provider package to use, as an alternative to the PROVIDER argument")
	cmd.Flags().StringVar(&f.name, "name", "", "provider resource name or id")
	cmd.Flags().StringVar(&f.region, "region", "", "provider region ("+defaultsHelp(func(d provisionDefaults) string { return d.Region })+")")
	cmd.Flags().StringVar(&f.owner, "owner", "local", "ownership tag scope the resource must carry")
	cmd.Flags().StringVar(&f.credentialRef, "credential-ref", "", "pkg/secrets ref for the provider credential (default the provider name)")
	f.account.bind(cmd)
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "machine-readable output")
	if verb == "destroy" {
		cmd.Flags().StringVar(&f.key, "key", "", "idempotency key (default infra-destroy-<name>)")
		cmd.Flags().BoolVar(&f.yes, "yes", false, "confirm the destroy (required; it is the per-operation authorization)")
		cmd.Flags().StringVar(&f.userDataBucket, "userdata-bucket", "",
			"bucket the first-boot script was staged in, when the create did not use the default")
	}
}

// resolve looks the named provider up, fills the per-provider region
// default and opens a dispatcher over it.
func (f *lifecycleFlags) resolve(ctx context.Context, verb string, args []string, opts []Option) (*provider.Dispatcher, error) {
	name, err := providerName(verb, args, f.providerFlag)
	if err != nil {
		return nil, err
	}
	p, ok := New(opts...).InfraProvider(name)
	if !ok {
		return nil, fmt.Errorf("fleet: no infra provider %q", name)
	}
	if f.region == "" {
		f.region = infraDefaults(p.Manifest.Name).Region
	}
	// Both refusals happen HERE — before the dispatcher is opened and so
	// before any network call: two answers to "which credential" is a
	// conflict, and an account that cannot drive this provider is
	// unsupported.
	if err := f.account.credentialRefConflict(f.credentialRef); err != nil {
		return nil, err
	}
	sel, err := resolveInfraAccount(ctx, &f.account, p.Manifest.Name)
	if err != nil {
		return nil, err
	}
	return openProviderDispatcher(ctx, p, f.credentialRef, builtinOpenOptions{
		Owner: f.owner, Region: f.region, UserDataBucket: f.userDataBucket,
		Account: sel,
	})
}

// resolveInfraAccount resolves `--account` for one provider: only the category
// that provider belongs to may drive it, so a DigitalOcean account named for
// an AWS provision is refused by name rather than used as a token.
func resolveInfraAccount(ctx context.Context, flag *accountFlag, providerName string) (*AccountSelection, error) {
	if !flag.given() {
		return nil, nil
	}
	category, ok := accountCategoryForProvider(providerName)
	if !ok {
		return nil, accountFail(accountCodeUnsupported,
			"provider "+providerName+" has no account category; --account is not supported for it")
	}
	return flag.resolve(ctx, accountOpInfra, category)
}

// openProviderDispatcher resolves a loaded provider's credential and
// opens a dispatcher over it. It is the ONE place provision and the
// lifecycle verbs open a provider, so they cannot drift in how a
// credential is resolved or an adapter is built.
//
// A built-in entry dispatches to the linked-in Go adapter, never to the
// declarative engine and never to a spawned program.
func openProviderDispatcher(ctx context.Context, p InfraProvider, credentialRef string, o builtinOpenOptions) (*provider.Dispatcher, error) {
	ref := credentialRef
	if ref == "" {
		ref = p.Manifest.Name
	}
	cred, err := resolveProviderCredentialFor(ref, o.Account)
	if err != nil {
		return nil, err
	}
	manifest := p.Manifest
	builtins := map[string]provider.BuiltinFactory{}
	var mutating time.Duration
	if b, isBuiltin := builtinFor(p); isBuiltin {
		manifest = b.manifest()
		manifest.Name = p.Manifest.Name
		builtins[manifest.Name] = builtinFactory(p.Manifest.Name, o)
		mutating = b.mutatingTimeout
	}
	return provider.Open(ctx, manifest, provider.Options{
		Owner:       o.Owner,
		Exec:        provider.ExecAllowed, // user-owned compute only
		Credentials: provider.StaticCredential(cred),
		Builtins:    builtins,
		// A create that waits for the host to be usable needs more than
		// a read's budget; a provider that says nothing keeps the
		// dispatcher default.
		MutatingTimeout: mutating,
	})
}

// newInfraInspect reads one provider resource.
func newInfraInspect(opts []Option) *cobra.Command {
	var f lifecycleFlags
	cmd := &cobra.Command{
		Use:   "inspect [PROVIDER] --name NAME",
		Short: "Show one provider resource (state, region, size, addresses, tags)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(f.name) == "" {
				return fmt.Errorf("fleet: infra inspect needs --name")
			}
			ctx := context.Background()
			d, err := f.resolve(ctx, "inspect", args, opts)
			if err != nil {
				return err
			}
			res, err := d.Do(ctx, provider.Request{
				ProtocolVersion: provider.ProtocolVersion,
				Op:              provider.OpInspect,
				ResourceID:      f.name,
			})
			if err != nil {
				return err
			}
			if res.Resource == nil {
				return fmt.Errorf("fleet: provider %s answered inspect without a resource", d.Manifest().Name)
			}
			out := cmd.OutOrStdout()
			if f.asJSON {
				row := resourceRow(*res.Resource)
				if ep := resourceEndpoint(*res.Resource); ep != "" {
					row["endpoint"] = ep
				}
				return writeJSON(out, row)
			}
			printResource(out, *res.Resource)
			return nil
		},
	}
	f.bind(cmd, "inspect")
	return cmd
}

// newInfraDestroy deletes one provider resource and everything attached
// to it that would keep billing.
func newInfraDestroy(opts []Option) *cobra.Command {
	var f lifecycleFlags
	cmd := &cobra.Command{
		Use:   "destroy [PROVIDER] --name NAME --yes",
		Short: "Delete one provider resource and release what it holds (requires --yes)",
		Long: "Delete one provider resource this owner created.\n\n" +
			"--yes is the per-operation destroy confirmation: without it the provider is\n" +
			"never called. A resource that does not carry this owner's lease tags, or (on\n" +
			"aws) the dhnt:managed=true tag the automation IAM policy keys deletes on, is\n" +
			"refused and left alone. Whatever the resource holds and would keep billing —\n" +
			"a reserved address, a staged first-boot script — is released too, and every\n" +
			"step is reported.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(f.name) == "" {
				return fmt.Errorf("fleet: infra destroy needs --name")
			}
			if !f.yes {
				return fmt.Errorf("fleet: refusing to destroy %q without --yes; it is the per-operation confirmation and the resource keeps billing until it is given", f.name)
			}
			key := f.key
			if key == "" {
				key = "infra-destroy-" + f.name
			}
			if !provisionKeyPattern.MatchString(key) {
				return fmt.Errorf("fleet: invalid idempotency key %q (want 1-128 chars of [A-Za-z0-9._:-])", key)
			}
			ctx := context.Background()
			d, err := f.resolve(ctx, "destroy", args, opts)
			if err != nil {
				return err
			}
			res, derr := enroll.DestroyExecutable(ctx, d, enroll.DestroyRequest{
				ResourceID:     f.name,
				IdempotencyKey: key,
				// --yes IS the confirmation: it is bound to this one
				// resource and this one key, so it cannot be carried to
				// another resource or replayed as a second delete.
				Authorization: &provider.Authorization{
					Kind:           provider.AuthConfirmation,
					ResourceID:     f.name,
					IdempotencyKey: key,
				},
			})
			out := cmd.OutOrStdout()
			if f.asJSON {
				row := map[string]any{"name": f.name, "provider": d.Manifest().Name}
				if res.Resource != nil {
					row = resourceRow(*res.Resource)
					row["provider"] = d.Manifest().Name
				}
				if rep, ok := decodeDestroySteps(res.Data); ok {
					row["steps"] = rep
				}
				if res.Cost != nil {
					row["cost"] = res.Cost
				}
				if derr != nil {
					row["error"] = derr.Error()
				}
				if jerr := writeJSON(out, row); jerr != nil {
					return jerr
				}
				return derr
			}
			if derr != nil {
				return derr
			}
			fmt.Fprintf(out, "destroy %s (%s):\n", f.name, d.Manifest().Name)
			if rep, ok := decodeDestroySteps(res.Data); ok {
				for _, s := range rep {
					line := fmt.Sprintf("  [%s] %s", s.Status, s.Step)
					if s.Target != "" {
						line += " " + s.Target
					}
					if s.Detail != "" {
						line += ": " + s.Detail
					}
					fmt.Fprintln(out, line)
				}
			}
			state := provider.StateDeleted
			if res.Resource != nil {
				state = res.Resource.State
			}
			fmt.Fprintf(out, "%s: %s\n", f.name, state)
			if c := res.Cost; c != nil && c.Ongoing {
				fmt.Fprintf(out, "warning: still billing: %s\n", strings.Join(c.Retained, ", "))
				if c.Note != "" {
					fmt.Fprintf(out, "warning: %s\n", c.Note)
				}
			}
			return nil
		},
	}
	f.bind(cmd, "destroy")
	return cmd
}

// listProviderResources is `infra list --provider NAME`: what that
// provider is running for this owner, rather than which provider packages
// are installed.
func listProviderResources(cmd *cobra.Command, opts []Option, f *lifecycleFlags) error {
	ctx := context.Background()
	d, err := f.resolve(ctx, "list", nil, opts)
	if err != nil {
		return err
	}
	res, err := d.Do(ctx, provider.Request{
		ProtocolVersion: provider.ProtocolVersion,
		Op:              provider.OpList,
	})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if f.asJSON {
		rows := make([]map[string]any, 0, len(res.Resources))
		for _, r := range res.Resources {
			row := resourceRow(r)
			if ep := resourceEndpoint(r); ep != "" {
				row["endpoint"] = ep
			}
			rows = append(rows, row)
		}
		return writeJSON(out, rows)
	}
	for _, r := range res.Resources {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.State, r.Region, r.Size, strings.Join(r.PublicIPs, ","))
	}
	return nil
}

// destroyStep is the CLI's view of one reported destroy step. It is
// decoded structurally rather than imported from one provider's package,
// so a second provider reporting the same shape prints the same way.
type destroyStep struct {
	Step   string `json:"step"`
	Target string `json:"target,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// decodeDestroySteps reads the per-step report a provider attached to its
// destroy result. ok is false when there is none: a provider that does
// not report steps, or a journal replay of a destroy performed earlier.
func decodeDestroySteps(data json.RawMessage) ([]destroyStep, bool) {
	if len(data) == 0 {
		return nil, false
	}
	var doc struct {
		Steps []destroyStep `json:"steps"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Steps) == 0 {
		return nil, false
	}
	return doc.Steps, true
}

// resourceRow is one provider resource as a --json record, shared by
// provision, inspect, list and destroy so the same resource reads the
// same way whichever verb printed it.
//
// It carries NO endpoint: an endpoint is a claim about what a host
// answers on, and only the caller knows whether it has one to make —
// provision knows the endpoint the bundle was rendered for, a read verb
// derives one from the address. Folding it in here would have provision
// print a derived endpoint nobody told the master about.
func resourceRow(r provider.Resource) map[string]any {
	row := map[string]any{"id": r.ID, "state": string(r.State)}
	if r.Name != "" {
		row["name"] = r.Name
	}
	if r.Region != "" {
		row["region"] = r.Region
	}
	if r.Size != "" {
		row["size"] = r.Size
	}
	if len(r.PublicIPs) > 0 {
		row["public_ips"] = r.PublicIPs
	}
	if len(r.Tags) > 0 {
		row["tags"] = r.Tags
	}
	if !r.CreatedAt.IsZero() {
		row["created_at"] = r.CreatedAt
	}
	return row
}

// resourceEndpoint is the sslip.io endpoint the resource's first IPv4
// address answers on. For an aws satellite that is exactly the endpoint
// the enrollment bundle was rendered for (the address was reserved and
// the hint derived from it), so a read verb can be used to check them
// against each other. "" when the resource reports no IPv4 address.
func resourceEndpoint(r provider.Resource) string {
	for _, ip := range r.PublicIPs {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			return provider.SslipEndpoint(ip)
		}
	}
	return ""
}

// printResource is the human rendering of one provider resource.
func printResource(w io.Writer, r provider.Resource) {
	// id and name are the same string for a provider that addresses
	// resources by name (Lightsail) and differ for one that does not
	// (a DigitalOcean droplet id), so both are printed.
	fmt.Fprintf(w, "id: %s\n", r.ID)
	name := r.Name
	if name == "" {
		name = r.ID
	}
	fmt.Fprintf(w, "name: %s\n", name)
	fmt.Fprintf(w, "state: %s\n", r.State)
	if r.RawStatus != "" && r.RawStatus != string(r.State) {
		fmt.Fprintf(w, "provider_status: %s\n", r.RawStatus)
	}
	if r.Region != "" {
		fmt.Fprintf(w, "region: %s\n", r.Region)
	}
	if r.Size != "" {
		fmt.Fprintf(w, "size: %s\n", r.Size)
	}
	if len(r.PublicIPs) > 0 {
		fmt.Fprintf(w, "address: %s\n", strings.Join(r.PublicIPs, ", "))
	}
	if ep := resourceEndpoint(r); ep != "" {
		fmt.Fprintf(w, "endpoint: %s\n", ep)
	}
	if !r.CreatedAt.IsZero() {
		fmt.Fprintf(w, "created: %s\n", r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if len(r.Tags) > 0 {
		fmt.Fprintf(w, "tags: %s\n", strings.Join(r.Tags, " "))
	}
}
