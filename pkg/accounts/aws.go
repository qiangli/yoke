package accounts

import (
	"context"
	"os"
	"strconv"
	"time"

	awsauth "github.com/qiangli/yoke/external/aws"
	awsprovider "github.com/qiangli/yoke/pkg/provider/aws"
)

// AWSAdapter binds each client to one explicit profile/role and region.
// Client may resolve session credentials through the existing credential store;
// its errors are reduced to a safe code before reaching inventory surfaces.
type AWSAdapter struct {
	ManagedAdapter
	Connections []Connection
	Client      func(context.Context, Connection, string) (*awsprovider.InventoryClient, error)
}

func NewAWSAdapter() *AWSAdapter {
	return &AWSAdapter{ManagedAdapter: ManagedAdapter{ProviderName: "aws"}}
}
func (*AWSAdapter) Name() string             { return "aws" }
func (*AWSAdapter) Capabilities() Capability { return Discover | Verify | Resources | Usage | Billing }
func (a *AWSAdapter) Discover(context.Context) ([]Observation, error) {
	cs := a.Connections
	if len(cs) == 0 {
		profiles, err := awsauth.ConfiguredProfiles()
		if err != nil {
			return nil, errCloudRead
		}
		names := map[string]bool{}
		for _, name := range profiles {
			names[name] = true
		}
		if selected, err := awsauth.ResolveCredentials(); err == nil {
			names[selected.Profile] = true
		}
		for _, name := range sortedKeys(names) {
			if !ValidCredentialRef("profile:" + name) {
				continue
			}
			cs = append(cs, Connection{ID: name, Profile: name, CredentialRef: "profile:" + name})
		}
	}
	var out []Observation
	for _, c := range cs {
		if c.Profile == "" || c.CredentialRef != "profile:"+c.Profile || !ValidCredentialRef(c.CredentialRef) {
			return nil, ErrUnsupported
		}
		out = append(out, Observation{SourceKey: c.CredentialRef, Account: Account{Category: AWS, Provider: a.Name(), Connections: []Connection{c}, Status: Unresolved, Source: "configuration"}})
	}
	return out, nil
}
func (a *AWSAdapter) verified(ctx context.Context, acct Account) (*awsprovider.InventoryClient, string, Connection, error) {
	c, e := cloudConnection(acct, a.Name())
	if e != nil {
		return nil, "", c, e
	}
	if c.Profile == "" || c.CredentialRef != "profile:"+c.Profile {
		return nil, "", c, ErrUnsupported
	}
	var client *awsprovider.InventoryClient
	if a.Client != nil {
		client, e = a.Client(ctx, c, acct.Region)
	} else {
		var cfg awsprovider.Config
		cfg.Credentials, e = awsauth.ResolveProfileKeys(c.Profile)
		if e == nil {
			cfg.Region = acct.Region
			if cfg.Region == "" {
				cfg.Region = awsprovider.RegionFromEnv(os.Environ())
			}
			client, e = awsprovider.NewInventoryClient(cfg)
		}
	}
	if e != nil || client == nil {
		return nil, "", c, errCloudRead
	}
	id, e := client.AccountID(ctx)
	if e != nil {
		return nil, "", c, errCloudRead
	}
	if id == "" || (acct.ProviderAccountID != "" && acct.ProviderAccountID != id) {
		return nil, "", c, errCloudIdentity
	}
	if acct.Role != "" {
		role, err := client.PrincipalARN(ctx)
		if err != nil {
			return nil, "", c, errCloudRead
		}
		if role != acct.Role {
			return nil, "", c, errCloudIdentity
		}
	}
	return client, id, c, nil
}
func (a *AWSAdapter) Verify(ctx context.Context, acct Account) (Observation, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, id, c, e := a.verified(ctx, acct)
	if e != nil {
		return Observation{}, e
	}
	role, e := client.PrincipalARN(ctx)
	if e != nil {
		return Observation{}, errCloudRead
	}
	now := time.Now().UTC()
	out := Account{Category: AWS, Provider: a.Name(), ProviderAccountID: id, Connections: []Connection{c}, Role: role, Region: acct.Region, Status: Resolved, Source: "aws/sts", ObservedAt: &now, Resources: acct.Resources}
	rows, e := awsResources(ctx, client)
	if e != nil {
		out.LastError = "partial_resources"
	} else {
		out.Resources = rows
	}
	return Observation{Account: out, SourceKey: c.CredentialRef}, nil
}
func awsResources(ctx context.Context, c *awsprovider.InventoryClient) ([]ResourceRef, error) {
	rows, e := c.Resources(ctx)
	if e != nil {
		return nil, errCloudRead
	}
	var out []ResourceRef
	regions := map[string]bool{}
	for _, r := range rows {
		out = append(out, ResourceRef{ID: r.ID, Name: r.Name, Kind: "lightsail-instance", Source: "aws/lightsail"})
		if r.Region != "" {
			regions[r.Region] = true
		}
	}
	for _, r := range sortedKeys(regions) {
		out = append(out, ResourceRef{ID: r, Kind: "region", Source: "aws/lightsail"})
	}
	return out, nil
}
func (a *AWSAdapter) Resources(ctx context.Context, acct Account) ([]ResourceRef, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, _, _, e := a.verified(ctx, acct)
	if e != nil {
		return nil, e
	}
	return awsResources(ctx, c)
}
func (a *AWSAdapter) Usage(ctx context.Context, acct Account, period string) ([]Metric, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return a.Billing(ctx, acct, period)
}
func (a *AWSAdapter) Billing(ctx context.Context, acct Account, period string) ([]Metric, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if e := cloudPeriod(period); e != nil {
		return nil, e
	}
	c, _, _, e := a.verified(ctx, acct)
	if e != nil {
		return nil, e
	}
	at := time.Now().UTC()
	spend := c.MonthToDateSpend(ctx)
	m := cloudMoney("month_to_date_usage", "aws/"+spend.Source, period, at)
	cloudMonth(&m, at)
	if spend.Known() {
		m.Value = cloudNumber(strconv.FormatFloat(spend.USD, 'g', -1, 64))
	} else {
		m.Source = "aws/budgets-cost-explorer"
		m.Limitations = append(m.Limitations, "Spend unavailable or access denied.")
	}
	if spend.CreditsIncluded {
		m.Limitations = append(m.Limitations, "Provider spend includes credits.")
	}
	if spend.BudgetErr != nil {
		m.Limitations = append(m.Limitations, "Budget source unavailable.")
	}
	limit := cloudMoney("budget_limit", "aws/budget", period, at)
	start := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	limit.WindowStart, limit.WindowEnd = &start, &end
	if spend.CapKnown {
		limit.Value = cloudNumber(strconv.FormatFloat(spend.CapUSD, 'g', -1, 64))
	} else {
		limit.Limitations = append(limit.Limitations, "Budget limit unavailable.")
	}
	limit.Limitations = append(limit.Limitations, "Configured provider budget; not a balance or spend total.")
	return []Metric{m, limit}, nil
}
