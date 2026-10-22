package fleet

// `reproduce --plan` — dry-run what the master metadata can recreate,
// read-only (S490 story 7107fb14).
//
// For each managed resource with a master record, the plan names the action
// that would recreate it through the product path and the record it comes
// from. Resources with NO master record (hand-made) cannot be enumerated
// from here — live provider inventory is outside this read-only plan — so
// they are reported as a detection gap with the procedure, not invented.
//
// The plan reads only Bearer [REDACTED] endpoints that already exist on the master
// (cloudbox hub/internal/server/*.go), reusing the clients in this package:
//
//	GET /api/v1/provisioning/requests(+/:id)  satellites/droplets (+workers)
//	GET /api/v1/hosts + GET /api/v1/peers     host pairings (re-pair list)
//	GET /api/v1/tokens                        token intents (never values)
//
// There is NO --apply in this run: the root refuses anything but --plan,
// and even --plan --apply is rejected as an unknown flag by cobra.
//
// Coverage the wire cannot serve is returned machine-readably in
// gaps (and mirrored in --help): satellite rows, enrollment + escrowed
// recovery keys + checkpoint/etcd locations, DKS bundles and federation,
// registry backends/routes/grants, and the host→satellite mapping. Each
// gap names the missing Bearer [REDACTED] instead of inventing one.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// ReproduceSource is the master record an action comes from: the Bearer
// endpoint plus the record id, so an operator can re-read it by hand.
type ReproduceSource struct {
	Endpoint string `json:"endpoint"`
	Record   string `json:"record"`
}

// ReproduceAction is one recreate step through the product path. Via is
// the command to re-run (never run by this verb); params carry the
// record's recreate dimensions; values that cannot be reproduced (token
// secrets, provider credentials) are named in note, never carried.
type ReproduceAction struct {
	Kind     string          `json:"kind"` // provision | pairing | token
	Resource string          `json:"resource"`
	Via      string          `json:"via"`
	Params   map[string]any  `json:"params,omitempty"`
	From     ReproduceSource `json:"from"`
	Note     string          `json:"note,omitempty"`
}

// ReproduceGap is one dimension the plan cannot cover from master
// metadata. MissingEndpoint names the Bearer [REDACTED] that would close it;
// empty means no endpoint of any kind exists yet.
type ReproduceGap struct {
	Category        string `json:"category"`
	Reason          string `json:"reason"`
	MissingEndpoint string `json:"missing_endpoint,omitempty"`
}

// ReproducePlan is the whole dry-run: recreate actions plus the gaps.
type ReproducePlan struct {
	Scope   string            `json:"scope"` // "all" or a satellite id
	Actions []ReproduceAction `json:"actions"`
	Gaps    []ReproduceGap    `json:"gaps"`
}

// reproduceGaps is the static coverage list: dimensions of the story's
// design (satellites, enrollment/escrow/snapshots, DKS, registry,
// host→satellite mapping) with no Bearer [REDACTED] on the master. Kept next to
// the builder so --plan --json and --help can never drift apart.
func reproduceGaps() []ReproduceGap {
	return []ReproduceGap{
		{
			Category:        "satellite-record",
			Reason:          "satellite rows (endpoint, state, key_state, owner) are session-only; the plan derives satellites from provisioning requests instead",
			MissingEndpoint: "GET /api/v1/satellites",
		},
		{
			Category:        "satellite-detail",
			Reason:          "per-satellite detail (profile, tags, enrollment state, provision/resource ids) has no Bearer [REDACTED]",
			MissingEndpoint: "GET /api/v1/satellites/:id",
		},
		{
			Category:        "host-satellite-mapping",
			Reason:          "which satellite a host is paired with is session-only; hosts list unattributed and --satellite filters provisioning only",
			MissingEndpoint: "GET /api/v1/satellites/assignment (Bearer)",
		},
		{
			Category:        "enrollment-escrow-snapshots",
			Reason:          "enrollment bundles, escrowed recovery-key mode, checkpoint and etcd snapshot locations are session-only or satellite-signed",
			MissingEndpoint: "GET /api/v1/satellites/:id/checkpoints (Bearer)",
		},
		{
			Category:        "dks-membership",
			Reason:          "DKS bundles and federation membership are session-only; cluster members cannot be re-derived here",
			MissingEndpoint: "GET /api/v1/satellites/:id/dks-bundles (Bearer)",
		},
		{
			Category:        "registry",
			Reason:          "registry backends, routes and grants have no master endpoint yet (story 674fedb4); nothing reproducible can name them",
			MissingEndpoint: "GET /api/v1/registry/backends|routes|grants",
		},
		{
			Category: "unrecorded-resource",
			Reason:   "a resource with no master record (hand-made, e.g. s482 alien) is invisible to this plan by construction: diff live provider/host inventory against plan actions by hand and re-create strays through master provisioning, then re-pair per the S490.4 procedure",
		},
	}
}

// BuildReproducePlan joins provisioning requests, paired hosts and token
// intents into one plan. scope is "all" or a satellite id: it filters
// provisioning requests by satellite_id; hosts and tokens carry no
// satellite linkage on the wire, so they ride along marked unattributed
// (never silently dropped, never falsely attributed).
func BuildReproducePlan(scope string, reqs []ProvisionRequest, hosts []PairedHost, tokens []TokenInfo) ReproducePlan {
	plan := ReproducePlan{Scope: scope, Actions: []ReproduceAction{}, Gaps: reproduceGaps()}
	for _, r := range reqs {
		if scope != "all" && r.SatelliteID != scope {
			continue
		}
		plan.Actions = append(plan.Actions, provisionAction(r))
	}
	for _, h := range hosts {
		plan.Actions = append(plan.Actions, pairingAction(h, scope))
	}
	for _, t := range tokens {
		plan.Actions = append(plan.Actions, tokenAction(t, scope))
	}
	sort.Slice(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Kind != plan.Actions[j].Kind {
			return plan.Actions[i].Kind < plan.Actions[j].Kind
		}
		return plan.Actions[i].Resource < plan.Actions[j].Resource
	})
	return plan
}

func provisionAction(r ProvisionRequest) ReproduceAction {
	sat := r.SatelliteID
	if sat == "" {
		sat = r.Name
	}
	if sat == "" {
		sat = r.ID
	}
	via := fmt.Sprintf("bashy infra provision %s --satellite %s --region %s --size %s",
		r.Provider, sat, r.Region, r.Size)
	params := map[string]any{
		"provider": r.Provider, "region": r.Region, "size": r.Size,
		"satellite_id": r.SatelliteID,
	}
	if r.Image != "" {
		params["image"] = r.Image
		via += " --image " + r.Image
	}
	if r.Profile != "" {
		params["profile"] = r.Profile
		via += " --profile " + r.Profile
	}
	if r.Endpoint != "" {
		params["endpoint"] = r.Endpoint
	}
	if r.ResourceID != "" {
		params["provider_resource_id"] = r.ResourceID
	}
	if r.IdempotencyKey != "" {
		params["idempotency_key"] = r.IdempotencyKey
	}
	a := ReproduceAction{
		Kind:     "provision",
		Resource: "satellite/" + sat,
		Via:      via,
		Params:   params,
		From: ReproduceSource{
			Endpoint: "GET /api/v1/provisioning/requests/:id",
			Record:   r.ID,
		},
	}
	if r.State != "" {
		a.Note = "master state at plan time: " + r.State + "; recreate is idempotent on (owner, idempotency_key)"
	}
	if r.NeedsToken {
		a.Note += "; needs a fresh provider token at apply time (vault-injected, never stored in the plan)"
	}
	a.Note = strings.TrimPrefix(a.Note, "; ")
	return a
}

func pairingAction(h PairedHost, scope string) ReproduceAction {
	a := ReproduceAction{
		Kind:     "pairing",
		Resource: "host/" + h.Host,
		Via:      "outpost register --code <owner-minted invite> --server <master> (re-pair per the S490.4 procedure)",
		Params:   map[string]any{"host": h.Host, "online": h.Online, "shared": h.Shared},
		From: ReproduceSource{
			Endpoint: "GET /api/v1/hosts (+ GET /api/v1/peers)",
			Record:   h.Host,
		},
		Note: "invite mint is owner-side; this read-only plan cannot mint one",
	}
	if scope != "all" {
		a.Note += "; satellite unattributed (no host→satellite mapping on the wire)"
	}
	return a
}

func tokenAction(t TokenInfo, scope string) ReproduceAction {
	via := fmt.Sprintf("bashy token create --name %s --scope %s --ttl <ttl> --to-secret <vault-name>",
		t.Name, strings.Join(t.Scopes, ","))
	a := ReproduceAction{
		Kind:     "token",
		Resource: "token/" + t.Name,
		Via:      via,
		Params:   map[string]any{"scopes": t.Scopes, "expires_at": t.ExpiresAt},
		From: ReproduceSource{
			Endpoint: "GET /api/v1/tokens",
			Record:   t.ID,
		},
		Note: "intent only: values are never stored and cannot be reproduced — re-mint with a fresh TTL into the vault",
	}
	if scope != "all" {
		a.Note += "; satellite unattributed (tokens carry no satellite linkage)"
	}
	return a
}

// NewReproduceCmd builds the `reproduce` verb tree. --plan is the only
// mode in this run; anything else (including a future --apply) is
// refused, loudly.
func NewReproduceCmd() *cobra.Command {
	var master, token, satellite string
	var asJSON, plan, all bool
	c := &cobra.Command{
		Use:   "reproduce",
		Short: "Dry-run what master metadata can recreate (read-only)",
		Long: "Dry-run what master metadata can recreate (read-only).\n\n" +
			"`reproduce --plan` lists, for each managed resource with a master\n" +
			"record, the action that would recreate it through the product path\n" +
			"and the record it comes from. Dimensions the Bearer [REDACTED] cannot serve\n" +
			"(satellite rows, escrow/snapshots, DKS, registry, host→satellite\n" +
			"mapping) are reported under gaps, each naming the missing endpoint.\n" +
			"There is no --apply in this run.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !plan {
				return fmt.Errorf("fleet: reproduce is plan-only in this run (pass --plan; --apply lands later behind S489 guards)")
			}
			if satellite != "" && all {
				return fmt.Errorf("fleet: pass --satellite ID or --all, not both")
			}
			scope := "all"
			if satellite != "" {
				scope = satellite
			}
			return runReproducePlan(cmd, master, token, scope, asJSON)
		},
	}
	c.PersistentFlags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	c.PersistentFlags().StringVar(&token, "token", "", "master Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY, else the paired credential)")
	c.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&plan, "plan", false, "list recreate actions + gaps (the only mode in this run)")
	c.Flags().StringVar(&satellite, "satellite", "", "plan one satellite only (filters provisioning by satellite_id)")
	c.Flags().BoolVar(&all, "all", false, "plan every managed resource (the default)")
	return c
}

func runReproducePlan(cmd *cobra.Command, master, token, scope string, asJSON bool) error {
	provs, err := NewProvisioningClient(master, token)
	if err != nil {
		return err
	}
	reqs, _, err := provs.List()
	if err != nil {
		return err
	}
	paired, err := NewPairedHostsClient(master, token)
	if err != nil {
		return err
	}
	hosts, _, err := paired.ListHosts()
	if err != nil {
		return err
	}
	peers, _, err := paired.ListPeers()
	if err != nil {
		return err
	}
	toks, err := NewTokenClient(master, token)
	if err != nil {
		return err
	}
	tinfos, err := toks.List()
	if err != nil {
		return err
	}
	plan := BuildReproducePlan(scope, reqs, joinPaired(hosts, peers), tinfos)
	out := cmd.OutOrStdout()
	if asJSON {
		return writeJSON(out, plan)
	}
	fmt.Fprintf(out, "scope: %s\n", plan.Scope)
	fmt.Fprintln(out, "actions:")
	if len(plan.Actions) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else {
		for _, a := range plan.Actions {
			fmt.Fprintf(out, "  - [%s] %s\n    via: %s\n    from: %s record %s\n",
				a.Kind, a.Resource, a.Via, a.From.Endpoint, a.From.Record)
			if a.Note != "" {
				fmt.Fprintf(out, "    note: %s\n", a.Note)
			}
		}
	}
	fmt.Fprintln(out, "gaps:")
	for _, g := range plan.Gaps {
		if g.MissingEndpoint != "" {
			fmt.Fprintf(out, "  - %s: %s (missing: %s)\n", g.Category, g.Reason, g.MissingEndpoint)
		} else {
			fmt.Fprintf(out, "  - %s: %s\n", g.Category, g.Reason)
		}
	}
	return nil
}
