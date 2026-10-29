package aws

import "context"

// InventoryClient exposes only read operations over the existing provider.
// Credentials are supplied per client; no global profile is switched.
type InventoryClient struct{ a *adapter }

func NewInventoryClient(cfg Config) (*InventoryClient, error) {
	cfg.AccountID = "" // inventory must verify ownership with STS
	if cfg.Owner == "" {
		cfg.Owner = "accounts"
	}
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return &InventoryClient{a: a}, nil
}
func (c *InventoryClient) AccountID(ctx context.Context) (string, error) { return c.a.AccountID(ctx) }
func (c *InventoryClient) MonthToDateSpend(ctx context.Context) Spend {
	return c.a.MonthToDateSpend(ctx)
}

type InventoryResource struct{ ID, Name, Region string }

func (c *InventoryClient) Resources(ctx context.Context) ([]InventoryResource, error) {
	rows, err := c.a.allInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]InventoryResource, 0, len(rows))
	for _, r := range rows {
		id := r.ARN
		if id == "" {
			id = r.Name
		}
		out = append(out, InventoryResource{ID: id, Name: r.Name, Region: r.Location.RegionName})
	}
	return out, nil
}

// PrincipalARN returns the principal proven by the same STS identity lookup.
func (c *InventoryClient) PrincipalARN(ctx context.Context) (string, error) {
	if _, err := c.a.AccountID(ctx); err != nil {
		return "", err
	}
	c.a.account.mu.Lock()
	defer c.a.account.mu.Unlock()
	return c.a.account.arn, nil
}
