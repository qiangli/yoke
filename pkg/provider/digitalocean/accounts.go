package digitalocean

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// AccountInfo is the identity reported for the explicitly supplied token.
// UUID identifies the user; Team.UUID identifies the billing/resource owner.
type AccountInfo struct {
	UUID         string   `json:"uuid"`
	Email        string   `json:"email"`
	Status       string   `json:"status"`
	DropletLimit *float64 `json:"droplet_limit"`
	Team         struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"team"`
}
type AccountKey struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}
type AccountBalance struct {
	AccountBalance     string `json:"account_balance"`
	MonthToDateBalance string `json:"month_to_date_balance"`
	MonthToDateUsage   string `json:"month_to_date_usage"`
	GeneratedAt        string `json:"generated_at"`
}

func (c *Client) Account(ctx context.Context) (*AccountInfo, error) {
	var out struct {
		Account AccountInfo `json:"account"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/account", nil, &out); err != nil {
		return nil, err
	}
	return &out.Account, nil
}
func (c *Client) Balance(ctx context.Context) (*AccountBalance, error) {
	var out AccountBalance
	if err := c.call(ctx, http.MethodGet, "/v2/customers/my/balance", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// accountPages never follows a provider link to another endpoint or host.
func accountPages[T any](ctx context.Context, c *Client, path, field string) ([]T, error) {
	var all []T
	for page := 1; page <= 1000; page++ {
		var out struct {
			Keys     []T `json:"ssh_keys"`
			Droplets []T `json:"droplets"`
			Projects []T `json:"projects"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=200&page=%d", path, page), nil, &out); err != nil {
			return nil, err
		}
		switch field {
		case "keys":
			all = append(all, out.Keys...)
		case "projects":
			all = append(all, out.Projects...)
		default:
			all = append(all, out.Droplets...)
		}
		if out.Links.Pages.Next == "" {
			return all, nil
		}
		u, err := url.Parse(out.Links.Pages.Next)
		if err != nil || u.Path != path {
			return nil, fmt.Errorf("digitalocean: invalid pagination")
		}
	}
	return nil, fmt.Errorf("digitalocean: pagination limit exceeded")
}
func (c *Client) AccountKeys(ctx context.Context) ([]AccountKey, error) {
	return accountPages[AccountKey](ctx, c, "/v2/account/keys", "keys")
}
func (c *Client) ListDroplets(ctx context.Context) ([]DropletDetail, error) {
	return accountPages[DropletDetail](ctx, c, "/v2/droplets", "droplets")
}
