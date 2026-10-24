package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/prune"
)

// OwnerClient uses the same URL and credential ladder as other Cloudbox clients.
type OwnerClient struct {
	BaseURL, Token string
	HTTP           *http.Client
}

func NewOwnerClient(base, token string) (OwnerClient, error) {
	base, token = ResolveCloud(base, token)
	if token == "" {
		return OwnerClient{}, fmt.Errorf("fleet: no cloudbox token (set $BASHY_FLEET_TOKEN or pass --token)")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return OwnerClient{BaseURL: base, Token: token, HTTP: httpClient()}, nil
}

func (c OwnerClient) request(ctx context.Context, method, path string, body any, out any) error {
	_, err := c.requestStatus(ctx, method, path, body, out)
	return err
}

func (c OwnerClient) requestStatus(ctx context.Context, method, path string, body any, out any) (int, error) {
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, input)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = httpClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, fmt.Errorf("not found or not yours")
		}
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if resp.StatusCode == http.StatusConflict && e.Error != "" {
			return resp.StatusCode, fmt.Errorf("%s", e.Error)
		}
		if e.Error != "" {
			return resp.StatusCode, fmt.Errorf("fleet: %s %s: %d: %s", method, path, resp.StatusCode, e.Error)
		}
		if e.Message != "" {
			return resp.StatusCode, fmt.Errorf("fleet: %s %s: %d: %s", method, path, resp.StatusCode, e.Message)
		}
		return resp.StatusCode, fmt.Errorf("fleet: %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("fleet: %s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

type HostPruneEntry struct {
	ID, Name, Reason string
	LastSeen         *time.Time `json:"last_seen"`
}

func (c OwnerClient) StaleHosts(ctx context.Context, older time.Duration) ([]HostPruneEntry, error) {
	var env struct {
		Hosts []HostPruneEntry `json:"hosts"`
	}
	err := c.request(ctx, "GET", "/api/v1/hosts?stale="+url.QueryEscape(older.String()), nil, &env)
	return env.Hosts, err
}
func (c OwnerClient) DeleteHost(ctx context.Context, name string) error {
	p := url.PathEscape(name)
	return c.request(ctx, "DELETE", "/api/hosts/"+p+"?confirm="+url.QueryEscape(name)+"&purge=true", nil, nil)
}

type SatellitePruneEntry struct {
	ID, Endpoint, Reason string
	LastContact          *time.Time `json:"last_contact"`
}
type ProvisionPruneEntry struct{ ID, Name, State, ResourceState, SatelliteID, Reason string }
type SatelliteEntry struct{ ID, Name, Endpoint, State string }
type TransferResult struct {
	SatelliteID   string    `json:"satellite_id"`
	From          string    `json:"from"`
	To            string    `json:"to"`
	TransferredAt time.Time `json:"transferred_at"`
	Note          string    `json:"note"`
}

func (c OwnerClient) PruneSatellites(ctx context.Context) ([]SatellitePruneEntry, []ProvisionPruneEntry, error) {
	var env struct {
		Satellites   []SatellitePruneEntry `json:"satellites"`
		Provisioning []ProvisionPruneEntry `json:"provisioning"`
	}
	err := c.request(ctx, "GET", "/api/v1/satellites?prune=1", nil, &env)
	return env.Satellites, env.Provisioning, err
}
func (c OwnerClient) Satellites(ctx context.Context) ([]SatelliteEntry, error) {
	var env struct {
		Satellites []SatelliteEntry `json:"satellites"`
	}
	err := c.request(ctx, "GET", "/api/v1/satellites", nil, &env)
	return env.Satellites, err
}
func (c OwnerClient) Transfer(ctx context.Context, id, to string) (TransferResult, error) {
	var res TransferResult
	err := c.request(ctx, "POST", "/api/v1/satellites/"+url.PathEscape(id)+"/transfer", map[string]string{"to": to}, &res)
	return res, err
}
func (c OwnerClient) DestroyLost(ctx context.Context, id string) error {
	return c.request(ctx, "POST", "/api/satellites/"+url.PathEscape(id)+"/destroy-lost", map[string]any{}, nil)
}
func (c OwnerClient) DestroyProvision(ctx context.Context, id string) error {
	status, err := c.requestStatus(ctx, "POST", "/api/v1/provisioning/requests/"+url.PathEscape(id)+"/destroy", map[string]any{}, nil)
	if err != nil {
		return err
	}
	if status == http.StatusAccepted {
		return fmt.Errorf("destroy requested for provisioning %s; owner approval is required before removal", id)
	}
	return nil
}

// HostPruner stores the duration selected by DryRun so Apply can recheck it.
type HostPruner struct {
	Server, Token string
	Older         time.Duration
	Client        *OwnerClient
}

func (*HostPruner) Name() string { return "host" }
func (p *HostPruner) client() (OwnerClient, error) {
	if p.Client != nil {
		return *p.Client, nil
	}
	return NewOwnerClient(p.Server, p.Token)
}
func (p *HostPruner) DryRun(ctx context.Context, opt prune.Options) ([]prune.Item, error) {
	d := opt.Older
	if d == 0 {
		d = 336 * time.Hour
	}
	p.Older = d
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	rows, err := c.StaleHosts(ctx, d)
	if err != nil {
		return nil, err
	}
	items := make([]prune.Item, 0, len(rows))
	for _, r := range rows {
		if r.Name == "" || r.Reason == "" {
			return nil, fmt.Errorf("fleet: invalid host prune row")
		}
		items = append(items, prune.Item{Noun: "host", ID: r.Name, Reason: r.Reason})
	}
	return items, nil
}
func (p *HostPruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	c, err := p.client()
	if err != nil {
		return prune.Counts{}, err
	}
	d := p.Older
	if d == 0 {
		d = 336 * time.Hour
	}
	count := prune.Counts{}
	for _, item := range items {
		rows, err := c.StaleHosts(ctx, d)
		if err != nil {
			return count, err
		}
		found := false
		for _, r := range rows {
			if r.Name == item.ID {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if err := c.DeleteHost(ctx, item.ID); err != nil {
			return count, err
		}
		count.Removed++
	}
	return count, nil
}

type SatellitePruner struct {
	Master, Token string
	Client        *OwnerClient
}

func (*SatellitePruner) Name() string { return "satellite" }
func (p *SatellitePruner) client() (OwnerClient, error) {
	if p.Client != nil {
		return *p.Client, nil
	}
	return NewOwnerClient(p.Master, p.Token)
}
func (p *SatellitePruner) DryRun(ctx context.Context, _ prune.Options) ([]prune.Item, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	sats, provs, err := c.PruneSatellites(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]prune.Item, 0, len(sats)+len(provs))
	for _, s := range sats {
		if s.ID == "" || s.Reason == "" {
			return nil, fmt.Errorf("fleet: invalid satellite prune row")
		}
		items = append(items, prune.Item{Noun: "satellite", ID: "satellite:" + s.ID, Reason: s.Reason})
	}
	for _, r := range provs {
		if r.ID == "" || r.Reason == "" {
			return nil, fmt.Errorf("fleet: invalid provisioning prune row")
		}
		items = append(items, prune.Item{Noun: "satellite", ID: "provisioning:" + r.ID, Reason: r.Reason})
	}
	return items, nil
}
func (p *SatellitePruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	c, err := p.client()
	if err != nil {
		return prune.Counts{}, err
	}
	count := prune.Counts{}
	for _, item := range items {
		sats, provs, err := c.PruneSatellites(ctx)
		if err != nil {
			return count, err
		}
		found := false
		var apply func(context.Context, string) error
		id := ""
		if strings.HasPrefix(item.ID, "satellite:") {
			id = strings.TrimPrefix(item.ID, "satellite:")
			apply = c.DestroyLost
			for _, s := range sats {
				if s.ID == id {
					found = true
					break
				}
			}
		} else if strings.HasPrefix(item.ID, "provisioning:") {
			id = strings.TrimPrefix(item.ID, "provisioning:")
			apply = c.DestroyProvision
			for _, r := range provs {
				if r.ID == id {
					found = true
					break
				}
			}
		}
		if !found {
			continue
		}
		if err := apply(ctx, id); err != nil {
			return count, err
		}
		count.Removed++
	}
	return count, nil
}
