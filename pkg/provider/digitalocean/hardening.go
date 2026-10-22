package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Client is a DigitalOcean API client for reserved IPs, firewalls, projects,
// and droplet hardening operations.
type Client struct {
	baseURL      string
	token        string
	httpClient   *http.Client
	pollInterval time.Duration
}

// ClientConfig configures a Client.
type ClientConfig struct {
	BaseURL      string
	Token        string
	HTTPClient   *http.Client
	PollInterval time.Duration
}

// NewClient returns a new Client. Token is required.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("digitalocean: token is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("digitalocean: invalid base url %q", cfg.BaseURL)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Client{
		baseURL:      cfg.BaseURL,
		token:        cfg.Token,
		httpClient:   cfg.HTTPClient,
		pollInterval: cfg.PollInterval,
	}, nil
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &transportError{err: err}
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &apiError{status: resp.StatusCode, id: e.ID, msg: e.Message}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// IsNotFound reports whether err represents an HTTP 404 response.
func IsNotFound(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status == http.StatusNotFound
	}
	return false
}

// --- Reserved IPs ---

// Slug is DO's {"slug": ...} representation for regions and images.
type Slug struct {
	Slug string `json:"slug"`
}

// ReservedIP represents a DigitalOcean reserved/floating IP.
type ReservedIP struct {
	IP      string          `json:"ip"`
	Region  Slug            `json:"region"`
	Droplet *DropletSummary `json:"droplet"`
	Locked  bool            `json:"locked,omitempty"`
}

// DropletSummary is the summary of a droplet attached to a reserved IP.
type DropletSummary struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// CreateReservedIPRequest specifies the parameters for creating a reserved IP.
type CreateReservedIPRequest struct {
	Region    string `json:"region,omitempty"`
	DropletID int    `json:"droplet_id,omitempty"`
}

// ListReservedIPs returns all reserved IPs in the account.
func (c *Client) ListReservedIPs(ctx context.Context) ([]ReservedIP, error) {
	var out struct {
		ReservedIPs []ReservedIP `json:"reserved_ips"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/reserved_ips", nil, &out); err != nil {
		return nil, err
	}
	return out.ReservedIPs, nil
}

// GetReservedIP returns the reserved IP with the given address.
func (c *Client) GetReservedIP(ctx context.Context, ip string) (*ReservedIP, error) {
	var out struct {
		ReservedIP ReservedIP `json:"reserved_ip"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/reserved_ips/"+url.PathEscape(ip), nil, &out); err != nil {
		return nil, err
	}
	return &out.ReservedIP, nil
}

// CreateReservedIP allocates a new reserved IP.
func (c *Client) CreateReservedIP(ctx context.Context, req CreateReservedIPRequest) (*ReservedIP, error) {
	var out struct {
		ReservedIP ReservedIP `json:"reserved_ip"`
	}
	if err := c.call(ctx, http.MethodPost, "/v2/reserved_ips", req, &out); err != nil {
		return nil, err
	}
	return &out.ReservedIP, nil
}

// AssignReservedIP attaches a reserved IP to a droplet.
func (c *Client) AssignReservedIP(ctx context.Context, ip string, dropletID int) error {
	in := map[string]any{"type": "assign", "droplet_id": dropletID}
	var out struct {
		Action struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
		} `json:"action"`
	}
	if err := c.call(ctx, http.MethodPost, "/v2/reserved_ips/"+url.PathEscape(ip)+"/actions", in, &out); err != nil {
		return err
	}
	return c.waitAction(ctx, out.Action.ID, out.Action.Status)
}

// UnassignReservedIP detaches a reserved IP from its droplet.
func (c *Client) UnassignReservedIP(ctx context.Context, ip string) error {
	in := map[string]any{"type": "unassign"}
	var out struct {
		Action struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
		} `json:"action"`
	}
	if err := c.call(ctx, http.MethodPost, "/v2/reserved_ips/"+url.PathEscape(ip)+"/actions", in, &out); err != nil {
		return err
	}
	return c.waitAction(ctx, out.Action.ID, out.Action.Status)
}

func (c *Client) waitAction(ctx context.Context, id int, status string) error {
	if status == "completed" || id == 0 {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.pollInterval):
		}
		var out struct {
			Action struct {
				Status string `json:"status"`
			} `json:"action"`
		}
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/v2/actions/%d", id), nil, &out); err != nil {
			return err
		}
		switch out.Action.Status {
		case "completed":
			return nil
		case "errored":
			return fmt.Errorf("digitalocean: action %d failed", id)
		}
	}
}

// --- Cloud Firewalls ---

// Firewall represents a DigitalOcean cloud firewall.
type Firewall struct {
	ID            string         `json:"id,omitempty"`
	Name          string         `json:"name"`
	Status        string         `json:"status,omitempty"`
	InboundRules  []FirewallRule `json:"inbound_rules"`
	OutboundRules []FirewallRule `json:"outbound_rules"`
	DropletIDs    []int          `json:"droplet_ids"`
	Tags          []string       `json:"tags"`
	CreatedAt     string         `json:"created_at,omitempty"`
}

// FirewallRule is one inbound or outbound firewall rule.
type FirewallRule struct {
	Protocol     string       `json:"protocol"`
	Ports        string       `json:"ports"`
	Sources      *RuleTargets `json:"sources,omitempty"`
	Destinations *RuleTargets `json:"destinations,omitempty"`
}

// RuleTargets defines sources or destinations for a firewall rule.
type RuleTargets struct {
	Addresses        []string `json:"addresses,omitempty"`
	DropletIDs       []int    `json:"droplet_ids,omitempty"`
	LoadBalancerUIDs []string `json:"load_balancer_uids,omitempty"`
	KubernetesIDs    []string `json:"kubernetes_ids,omitempty"`
	Tags             []string `json:"tags,omitempty"`
}

// DefaultOutboundRules returns default outbound rules permitting all egress.
func DefaultOutboundRules() []FirewallRule {
	all := []string{"0.0.0.0/0", "::/0"}
	return []FirewallRule{
		{
			Protocol:     "tcp",
			Ports:        "all",
			Destinations: &RuleTargets{Addresses: all},
		},
		{
			Protocol:     "udp",
			Ports:        "all",
			Destinations: &RuleTargets{Addresses: all},
		},
		{
			Protocol:     "icmp",
			Ports:        "all",
			Destinations: &RuleTargets{Addresses: all},
		},
	}
}

// ParsePortProtocol parses strings like "22/tcp", "443/tcp", "80", "icmp".
func ParsePortProtocol(s string) (ports, proto string, err error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "", "", errors.New("empty port/protocol specification")
	}
	if s == "icmp" {
		return "all", "icmp", nil
	}
	if strings.Contains(s, "/") {
		parts := strings.SplitN(s, "/", 2)
		p1, p2 := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if p1 == "tcp" || p1 == "udp" || p1 == "icmp" {
			proto = p1
			ports = p2
		} else if p2 == "tcp" || p2 == "udp" || p2 == "icmp" {
			proto = p2
			ports = p1
		} else {
			return "", "", fmt.Errorf("invalid port/protocol %q: unknown protocol %q", s, p2)
		}
	} else {
		ports = s
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" && proto != "icmp" {
		return "", "", fmt.Errorf("unsupported protocol %q: must be tcp, udp, or icmp", proto)
	}
	if ports == "" {
		ports = "all"
	}
	return ports, proto, nil
}

// MatchesRule reports whether rule r covers the given protocol, port, and source CIDR.
func (r FirewallRule) Matches(proto, ports, sourceCIDR string) bool {
	if strings.ToLower(r.Protocol) != strings.ToLower(proto) {
		return false
	}
	if strings.ToLower(r.Ports) != strings.ToLower(ports) {
		return false
	}
	if sourceCIDR != "" && r.Sources != nil {
		return slices.Contains(r.Sources.Addresses, sourceCIDR)
	}
	return true
}

// ListFirewalls returns all firewalls.
func (c *Client) ListFirewalls(ctx context.Context) ([]Firewall, error) {
	var out struct {
		Firewalls []Firewall `json:"firewalls"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/firewalls", nil, &out); err != nil {
		return nil, err
	}
	return out.Firewalls, nil
}

// GetFirewall returns a firewall by ID.
func (c *Client) GetFirewall(ctx context.Context, id string) (*Firewall, error) {
	var out struct {
		Firewall Firewall `json:"firewall"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/firewalls/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out.Firewall, nil
}

// FindFirewallByName searches firewalls by name.
func (c *Client) FindFirewallByName(ctx context.Context, name string) (*Firewall, error) {
	fws, err := c.ListFirewalls(ctx)
	if err != nil {
		return nil, err
	}
	for _, fw := range fws {
		if fw.Name == name {
			return &fw, nil
		}
	}
	return nil, nil
}

// CreateFirewall creates a new cloud firewall.
func (c *Client) CreateFirewall(ctx context.Context, fw *Firewall) (*Firewall, error) {
	var out struct {
		Firewall Firewall `json:"firewall"`
	}
	if err := c.call(ctx, http.MethodPost, "/v2/firewalls", fw, &out); err != nil {
		return nil, err
	}
	return &out.Firewall, nil
}

// AddDropletsToFirewall associates droplets with a firewall.
func (c *Client) AddDropletsToFirewall(ctx context.Context, fwID string, dropletIDs []int) error {
	in := map[string]any{"droplet_ids": dropletIDs}
	return c.call(ctx, http.MethodPost, "/v2/firewalls/"+url.PathEscape(fwID)+"/droplets", in, nil)
}

// RemoveDropletsFromFirewall removes droplets from a firewall.
func (c *Client) RemoveDropletsFromFirewall(ctx context.Context, fwID string, dropletIDs []int) error {
	in := map[string]any{"droplet_ids": dropletIDs}
	return c.call(ctx, http.MethodDelete, "/v2/firewalls/"+url.PathEscape(fwID)+"/droplets", in, nil)
}

// AddTagsToFirewall associates tags with a firewall.
func (c *Client) AddTagsToFirewall(ctx context.Context, fwID string, tags []string) error {
	in := map[string]any{"tags": tags}
	return c.call(ctx, http.MethodPost, "/v2/firewalls/"+url.PathEscape(fwID)+"/tags", in, nil)
}

// RemoveTagsFromFirewall disassociates tags from a firewall.
func (c *Client) RemoveTagsFromFirewall(ctx context.Context, fwID string, tags []string) error {
	in := map[string]any{"tags": tags}
	return c.call(ctx, http.MethodDelete, "/v2/firewalls/"+url.PathEscape(fwID)+"/tags", in, nil)
}

// AddRulesToFirewall appends inbound rules to a firewall.
func (c *Client) AddRulesToFirewall(ctx context.Context, fwID string, inbound []FirewallRule) error {
	in := map[string]any{"inbound_rules": inbound}
	return c.call(ctx, http.MethodPost, "/v2/firewalls/"+url.PathEscape(fwID)+"/rules", in, nil)
}

// RemoveRulesFromFirewall deletes inbound rules from a firewall.
func (c *Client) RemoveRulesFromFirewall(ctx context.Context, fwID string, inbound []FirewallRule) error {
	in := map[string]any{"inbound_rules": inbound}
	return c.call(ctx, http.MethodDelete, "/v2/firewalls/"+url.PathEscape(fwID)+"/rules", in, nil)
}

// --- Projects ---

// Project represents a DigitalOcean project.
type Project struct {
	ID          string `json:"id"`
	OwnerUUID   string `json:"owner_uuid,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
	Environment string `json:"environment,omitempty"`
	IsDefault   bool   `json:"is_default,omitempty"`
}

// ProjectResource represents a resource assigned to a project.
type ProjectResource struct {
	URN        string `json:"urn"`
	AssignedAt string `json:"assigned_at,omitempty"`
	Status     string `json:"status,omitempty"`
}

// ListProjects returns all projects.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out struct {
		Projects []Project `json:"projects"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/projects", nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// GetProject returns a project by ID.
func (c *Client) GetProject(ctx context.Context, id string) (*Project, error) {
	var out struct {
		Project Project `json:"project"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/projects/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out.Project, nil
}

// FindProjectByNameOrID locates a project by its name or UUID.
func (c *Client) FindProjectByNameOrID(ctx context.Context, nameOrID string) (*Project, error) {
	p, err := c.GetProject(ctx, nameOrID)
	if err == nil {
		return p, nil
	}
	projs, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, proj := range projs {
		if proj.Name == nameOrID || proj.ID == nameOrID {
			return &proj, nil
		}
	}
	return nil, fmt.Errorf("digitalocean: project %q not found", nameOrID)
}

// ListProjectResources returns all resources assigned to a project.
func (c *Client) ListProjectResources(ctx context.Context, projectID string) ([]ProjectResource, error) {
	var out struct {
		Resources []ProjectResource `json:"resources"`
	}
	if err := c.call(ctx, http.MethodGet, "/v2/projects/"+url.PathEscape(projectID)+"/resources", nil, &out); err != nil {
		return nil, err
	}
	return out.Resources, nil
}

// AssignProjectResources assigns resources (by URN, e.g. "do:droplet:123") to a project.
func (c *Client) AssignProjectResources(ctx context.Context, projectID string, urns []string) error {
	in := map[string]any{"resources": urns}
	return c.call(ctx, http.MethodPost, "/v2/projects/"+url.PathEscape(projectID)+"/resources", in, nil)
}

// --- Droplets and Tags ---

// DropletDetail contains droplet attributes retrieved for hardening reconciliation.
type DropletDetail struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Region Slug     `json:"region"`
	Tags   []string `json:"tags"`
}

// GetDroplet fetches droplet details by ID.
func (c *Client) GetDroplet(ctx context.Context, id int) (*DropletDetail, error) {
	var out struct {
		Droplet DropletDetail `json:"droplet"`
	}
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/v2/droplets/%d", id), nil, &out); err != nil {
		return nil, err
	}
	return &out.Droplet, nil
}

// TagDroplet attaches a tag to a droplet.
func (c *Client) TagDroplet(ctx context.Context, tag string, dropletID int) error {
	in := map[string]any{
		"resources": []map[string]string{
			{
				"resource_id":   strconv.Itoa(dropletID),
				"resource_type": "droplet",
			},
		},
	}
	return c.call(ctx, http.MethodPost, "/v2/tags/"+url.PathEscape(tag)+"/resources", in, nil)
}

// UntagDroplet removes a tag from a droplet.
func (c *Client) UntagDroplet(ctx context.Context, tag string, dropletID int) error {
	in := map[string]any{
		"resources": []map[string]string{
			{
				"resource_id":   strconv.Itoa(dropletID),
				"resource_type": "droplet",
			},
		},
	}
	return c.call(ctx, http.MethodDelete, "/v2/tags/"+url.PathEscape(tag)+"/resources", in, nil)
}
