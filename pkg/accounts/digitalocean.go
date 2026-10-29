package accounts

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	do "github.com/qiangli/yoke/pkg/provider/digitalocean"
)

// DigitalOceanAdapter uses explicit per-connection credential references.
// Client resolves a reference at execution time; secret stores can supply this
// hook without ever putting token values in inventory records.
type DigitalOceanAdapter struct {
	ManagedAdapter
	Connections []Connection
	Client      func(context.Context, Connection) (*do.Client, error)
	// DropletKeys supplies previously proven associations, keyed by credential
	// reference then droplet ID. The API does not report installed SSH keys.
	DropletKeys map[string]map[int][]string
	// Materializer resolves secret: and env: references; nil uses the real
	// vault and environment.
	Materializer *Materializer
	configure    func(*do.ClientConfig)
}

func NewDigitalOceanAdapter() *DigitalOceanAdapter {
	return &DigitalOceanAdapter{ManagedAdapter: ManagedAdapter{ProviderName: "digitalocean"}}
}
func (*DigitalOceanAdapter) Name() string { return "digitalocean" }
func (*DigitalOceanAdapter) Capabilities() Capability {
	return Discover | Verify | Resources | Usage | Billing
}
func (a *DigitalOceanAdapter) client(ctx context.Context, c Connection) (*do.Client, error) {
	if !ValidCredentialRef(c.CredentialRef) || strings.HasPrefix(c.CredentialRef, "ssh-key:") {
		return nil, ErrUnsupported
	}
	if a.Client != nil {
		v, e := a.Client(ctx, c)
		if e != nil || v == nil {
			return nil, errCloudRead
		}
		return v, nil
	}
	m := DefaultMaterializer()
	if a.Materializer != nil {
		m = *a.Materializer
	}
	cred, e := m.Materialize(ctx, Selection{CredentialRef: c.CredentialRef}, DigitalOcean)
	if e != nil || cred.Token == "" {
		return nil, errCloudRead
	}
	cfg := do.ClientConfig{Token: cred.Token}
	if a.configure != nil {
		a.configure(&cfg)
	}
	v, e := do.NewClient(cfg)
	if e != nil {
		return nil, errCloudRead
	}
	return v, nil
}
func (a *DigitalOceanAdapter) Discover(_ context.Context) ([]Observation, error) {
	// Discovery is local-only: an environment variable is configuration, not
	// proof of a team identity. Verification is an explicit refresh.
	cs := a.Connections
	if len(cs) == 0 {
		// Both spellings count as configuration: pkg/fleet's provider path
		// reads $DIGITALOCEAN_ACCESS_TOKEN, so a host that only set that
		// one used to be invisible to discovery.
		for _, name := range DigitalOceanEnvNames() {
			if strings.TrimSpace(os.Getenv(name)) != "" {
				cs = []Connection{{ID: "env", CredentialRef: "env:" + name}}
				break
			}
		}
	}
	var out []Observation
	for _, c := range cs {
		if !ValidCredentialRef(c.CredentialRef) {
			return nil, ErrUnsupported
		}
		out = append(out, Observation{SourceKey: c.CredentialRef, Account: Account{Category: DigitalOcean, Provider: a.Name(), Connections: []Connection{c}, Status: Unresolved, Source: "configuration"}})
	}
	return out, nil
}
func (a *DigitalOceanAdapter) verified(ctx context.Context, acct Account) (*do.Client, *do.AccountInfo, Connection, error) {
	c, e := cloudConnection(acct, a.Name())
	if e != nil {
		return nil, nil, c, e
	}
	client, e := a.client(ctx, c)
	if e != nil {
		return nil, nil, c, e
	}
	info, e := client.Account(ctx)
	if e != nil {
		return nil, nil, c, errCloudRead
	}
	if info.Team.UUID == "" {
		return nil, nil, c, ErrUnsupported
	}
	if acct.ProviderAccountID != "" && acct.ProviderAccountID != info.Team.UUID {
		return nil, nil, c, errCloudIdentity
	}
	// The call succeeded, so this token belongs to this team: record the
	// identity it proved before the caller does anything else with it. Every
	// capability goes through here, so one hook covers verify, resources,
	// usage and billing.
	ObserveSuccess(ctx, DigitalOcean, Facts{
		Adapter: a.Name(), Provider: a.Name(), ProviderAccountID: info.Team.UUID,
		Team: info.Team.Name, Name: info.Team.Name, Email: info.Email,
		ConnectionID: c.ID, Endpoint: c.Endpoint, Profile: c.Profile,
		CredentialRef: c.CredentialRef, AuthMethod: c.AuthMethod,
		Source: "digitalocean/account",
	})
	return client, info, c, nil
}
func (a *DigitalOceanAdapter) Verify(ctx context.Context, acct Account) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, info, c, e := a.verified(ctx, acct)
	if e != nil {
		return Observation{}, e
	}
	now := time.Now().UTC()
	out := Account{Category: DigitalOcean, Provider: a.Name(), ProviderAccountID: info.Team.UUID, Team: info.Team.Name, Name: info.Team.Name, Email: info.Email, Connections: []Connection{c}, Status: Resolved, Source: "digitalocean/account", ObservedAt: &now}
	// Preserve last-good enrichment if a narrower permission is denied.
	out.Resources = acct.Resources
	out.SSHKeys = acct.SSHKeys
	resources, keys, e := a.inventory(ctx, client, c)
	if e != nil {
		out.LastError = "partial_resources"
	} else {
		out.Resources = resources
		out.SSHKeys = keys
	}
	return Observation{Account: out, SourceKey: c.CredentialRef}, nil
}
func (a *DigitalOceanAdapter) inventory(ctx context.Context, c *do.Client, conn Connection) ([]ResourceRef, []SSHKeyRef, error) {
	projects, e := c.ListProjects(ctx)
	if e != nil {
		return nil, nil, errCloudRead
	}
	droplets, e := c.ListDroplets(ctx)
	if e != nil {
		return nil, nil, errCloudRead
	}
	keys, e := c.AccountKeys(ctx)
	if e != nil {
		return nil, nil, errCloudRead
	}
	var out []ResourceRef
	var ssh []SSHKeyRef
	for _, p := range projects {
		out = append(out, ResourceRef{ID: p.ID, Kind: "project", Name: p.Name, Source: "digitalocean/projects"})
	}
	known := map[string]bool{}
	for _, k := range keys {
		if k.Fingerprint != "" {
			known[k.Fingerprint] = true
			ssh = append(ssh, SSHKeyRef{Fingerprint: k.Fingerprint, Name: k.Name})
		}
	}
	regions := map[string]bool{}
	for _, d := range droplets {
		id := strconv.Itoa(d.ID)
		out = append(out, ResourceRef{ID: id, Kind: "droplet", Name: d.Name, Source: "digitalocean/droplets"})
		if d.Region.Slug != "" {
			regions[d.Region.Slug] = true
		}
		for _, fp := range a.DropletKeys[conn.CredentialRef][d.ID] {
			if known[fp] {
				out = append(out, ResourceRef{ID: id + "/" + fp, Kind: "droplet-ssh-key", Source: "local-observation"})
			}
		}
	}
	for _, r := range sortedKeys(regions) {
		out = append(out, ResourceRef{ID: r, Kind: "region", Source: "digitalocean/droplets"})
	}
	return out, ssh, nil
}
func (a *DigitalOceanAdapter) Resources(ctx context.Context, acct Account) ([]ResourceRef, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, _, conn, e := a.verified(ctx, acct)
	if e != nil {
		return nil, e
	}
	rs, _, e := a.inventory(ctx, c, conn)
	return rs, e
}
func (a *DigitalOceanAdapter) Usage(ctx context.Context, acct Account, period string) ([]Metric, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if e := cloudPeriod(period); e != nil {
		return nil, e
	}
	_, info, _, e := a.verified(ctx, acct)
	if e != nil {
		return nil, e
	}
	return []Metric{{Name: "droplet_limit", Value: info.DropletLimit, Unit: "count", Source: "digitalocean/account", Classification: ProviderReported, ObservedAt: time.Now().UTC(), Period: period, Coverage: Partial, Limitations: []string{"Point-in-time quota limit."}}}, nil
}
func (a *DigitalOceanAdapter) Billing(ctx context.Context, acct Account, period string) ([]Metric, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if e := cloudPeriod(period); e != nil {
		return nil, e
	}
	c, _, _, e := a.verified(ctx, acct)
	if e != nil {
		return nil, e
	}
	now := time.Now().UTC()
	b, err := c.Balance(ctx)
	names := []string{"balance", "month_to_date_balance", "month_to_date_usage"}
	out := make([]Metric, 0, 3)
	var values []string
	if err == nil {
		values = []string{b.AccountBalance, b.MonthToDateBalance, b.MonthToDateUsage}
		if at, e := time.Parse(time.RFC3339, b.GeneratedAt); e == nil {
			now = at.UTC()
		}
	}
	for i, name := range names {
		m := cloudMoney(name, "digitalocean/balance", period, now)
		if i > 0 {
			cloudMonth(&m, now)
		} else {
			m.Limitations = []string{"Point-in-time account balance."}
		}
		if err == nil {
			m.Value = cloudNumber(values[i])
		}
		if m.Value == nil {
			m.Limitations = append(m.Limitations, "Balance unavailable or access denied.")
		}
		out = append(out, m)
	}
	return out, nil
}
