// Package digitalocean is the reference provider adapter (S472.4a): a
// FormBuiltin provider.Adapter over the DigitalOcean v2 API.
//
// It talks HTTP to a configurable base URL (tests point it at fakedo; no
// doctl is needed). Every resource it creates carries the bashy-lease tags
// (provider.LeaseTags): the lease marker, the owner, the hashed idempotency
// key and, for a temporary-lifecycle authorization, the temporary marker
// with its deadline/work tag. Those tags are the only state: a replayed
// create finds the droplet by its idempotency tag instead of creating a
// second one, and every operation on an existing droplet first checks that
// it carries this owner's tags, so a foreign or untagged droplet is never
// touched.
//
// The token is used for the adapter's lifetime and never persisted; how it
// was obtained (pkg/secrets, a one-shot user grant) belongs to the caller.
package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// DefaultBaseURL is the public DigitalOcean API.
const DefaultBaseURL = "https://api.digitalocean.com"

// Name is the provider name in the adapter's manifest.
const Name = "digitalocean"

// Config configures an adapter.
type Config struct {
	// BaseURL is the API root without /v2; empty means DefaultBaseURL.
	BaseURL string
	// Token is the DigitalOcean API token (bearer).
	Token string
	// Owner scopes ownership: it is written as the owner tag at create and
	// required on every droplet the adapter acts on. Must satisfy
	// provider.ValidOwner.
	Owner string
	// HTTPClient defaults to a client with a 60s timeout.
	HTTPClient *http.Client
	// PollInterval is the wait between action-status polls (default 2s).
	PollInterval time.Duration
	// Now stamps offer observation times; default time.Now.
	Now func() time.Time
}

// Manifest is the adapter's manifest: every v1 operation, no volumes/spot.
func Manifest() provider.Manifest {
	caps := make([]provider.Capability, 0, len(provider.Ops()))
	for _, op := range provider.Ops() {
		caps = append(caps, provider.Capability(op))
	}
	return provider.Manifest{
		Name:            Name,
		Kind:            provider.KindInfra,
		Form:            provider.FormBuiltin,
		ProtocolVersion: provider.ProtocolVersion,
		Summary:         "DigitalOcean droplets over the v2 API",
		Capabilities:    caps,
	}
}

// New returns the adapter, wrapped in provider.Guard so capability,
// idempotency-key and destroy-authorization checks run before any API call.
func New(cfg Config) (provider.Adapter, error) {
	if cfg.Token == "" {
		return nil, &provider.Error{Code: provider.ErrInvalidCredentials, Message: "digitalocean: token is required"}
	}
	if !provider.ValidOwner(cfg.Owner) {
		return nil, &provider.Error{Code: provider.ErrInvalidRequest, Message: fmt.Sprintf("digitalocean: owner %q must match [A-Za-z0-9_-]{1,64}", cfg.Owner)}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &provider.Error{Code: provider.ErrInvalidRequest, Message: fmt.Sprintf("digitalocean: invalid base url %q", cfg.BaseURL)}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return provider.Guard(&adapter{cfg: cfg}), nil
}

type adapter struct{ cfg Config }

func (a *adapter) Manifest() provider.Manifest { return Manifest() }

func (a *adapter) Do(ctx context.Context, req provider.Request) (provider.Result, error) {
	switch req.Op {
	case provider.OpValidate:
		return a.validate(ctx, req)
	case provider.OpCapacity:
		return a.capacity(ctx, req)
	case provider.OpPlan:
		return a.plan(ctx, req)
	case provider.OpCreate:
		return a.create(ctx, req)
	case provider.OpInspect:
		d, res, err := a.owned(ctx, req)
		if err != nil {
			return res, err
		}
		return ok(req.Op, &provider.Result{Resource: d.resource()})
	case provider.OpList:
		ds, err := a.listByTag(ctx, provider.OwnerTag(a.cfg.Owner))
		if err != nil {
			return fail(req.Op, err)
		}
		out := []provider.Resource{}
		for _, d := range ds {
			if provider.Owned(d.Tags, a.cfg.Owner) {
				out = append(out, *d.resource())
			}
		}
		return ok(req.Op, &provider.Result{Resources: out})
	case provider.OpStart:
		return a.action(ctx, req, "power_on", "active", "")
	case provider.OpStop:
		return a.action(ctx, req, "power_off", "off", "")
	case provider.OpReboot:
		return a.action(ctx, req, "reboot", "", "")
	case provider.OpResize:
		return a.action(ctx, req, "resize", "", req.Spec.Size)
	case provider.OpDestroy:
		return a.destroy(ctx, req)
	}
	return provider.Failure(req.Op, provider.ErrUnsupportedCapability, "digitalocean: operation %q", req.Op)
}

// ---- DO wire types ----

type droplet struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	SizeSlug  string    `json:"size_slug"`
	Region    slug      `json:"region"`
	Tags      []string  `json:"tags"`
	VolumeIDs []string  `json:"volume_ids"`
	Networks  networks  `json:"networks"`
	CreatedAt time.Time `json:"created_at"`
}

type slug struct {
	Slug string `json:"slug"`
}

type networks struct {
	V4 []struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	} `json:"v4"`
	V6 []struct {
		IPAddress string `json:"ip_address"`
		Type      string `json:"type"`
	} `json:"v6"`
}

type doAction struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

// State maps a DigitalOcean droplet status onto the normalized state.
func State(status string) provider.State {
	switch status {
	case "new":
		return provider.StatePending
	case "active":
		return provider.StateRunning
	case "off":
		return provider.StateStopped
	case "archive":
		return provider.StateDeleted
	}
	return provider.StateUnknown
}

func (d *droplet) resource() *provider.Resource {
	r := &provider.Resource{
		ID:        strconv.Itoa(d.ID),
		Name:      d.Name,
		State:     State(d.Status),
		RawStatus: d.Status,
		Region:    d.Region.Slug,
		Size:      d.SizeSlug,
		Tags:      slices.Clone(d.Tags),
		CreatedAt: d.CreatedAt,
	}
	for _, n := range d.Networks.V4 {
		if n.Type == "public" {
			r.PublicIPs = append(r.PublicIPs, n.IPAddress)
		}
	}
	for _, n := range d.Networks.V6 {
		if n.Type == "public" {
			r.PublicIPs = append(r.PublicIPs, n.IPAddress)
		}
	}
	return r
}

// retained lists what outlives the droplet itself: attached volumes are
// neither deleted with it nor stop billing when it is powered off.
func (d *droplet) retained() []string {
	var out []string
	for _, v := range d.VolumeIDs {
		out = append(out, "volume:"+v)
	}
	return out
}

// ---- operations ----

func (a *adapter) validate(ctx context.Context, req provider.Request) (provider.Result, error) {
	var out struct {
		Account json.RawMessage `json:"account"`
	}
	if err := a.call(ctx, http.MethodGet, "/v2/account", nil, &out); err != nil {
		return fail(req.Op, err)
	}
	return ok(req.Op, &provider.Result{Data: out.Account})
}

func (a *adapter) capacity(ctx context.Context, req provider.Request) (provider.Result, error) {
	var regions struct {
		Regions json.RawMessage `json:"regions"`
	}
	var sizes struct {
		Sizes json.RawMessage `json:"sizes"`
	}
	if err := a.call(ctx, http.MethodGet, "/v2/regions?per_page=200", nil, &regions); err != nil {
		return fail(req.Op, err)
	}
	if err := a.call(ctx, http.MethodGet, "/v2/sizes?per_page=200", nil, &sizes); err != nil {
		return fail(req.Op, err)
	}
	out := map[string]any{"regions": regions.Regions, "sizes": sizes.Sizes}
	if offers, err := a.offers(ctx, regions.Regions, sizes.Sizes); err == nil {
		out["offers"] = offers
	}
	data, _ := json.Marshal(out)
	return ok(req.Op, &provider.Result{Data: data})
}

// Plan is the Data payload of a plan result.
type Plan struct {
	Name         string   `json:"name"`
	Region       string   `json:"region"`
	Size         string   `json:"size"`
	Image        string   `json:"image"`
	Tags         []string `json:"tags"`
	PriceMonthly float64  `json:"price_monthly"`
	PriceHourly  float64  `json:"price_hourly"`
	Temporary    bool     `json:"temporary"`
}

func (a *adapter) plan(ctx context.Context, req provider.Request) (provider.Result, error) {
	s := req.Spec
	if s.Region == "" || s.Size == "" || s.Image == "" {
		return provider.Failure(req.Op, provider.ErrInvalidRequest, "plan needs spec.region, spec.size and spec.image")
	}
	var sizes struct {
		Sizes []struct {
			Slug         string   `json:"slug"`
			PriceMonthly float64  `json:"price_monthly"`
			PriceHourly  float64  `json:"price_hourly"`
			Available    bool     `json:"available"`
			Regions      []string `json:"regions"`
		} `json:"sizes"`
	}
	if err := a.call(ctx, http.MethodGet, "/v2/sizes?per_page=200", nil, &sizes); err != nil {
		return fail(req.Op, err)
	}
	for _, z := range sizes.Sizes {
		if z.Slug != s.Size {
			continue
		}
		if !z.Available || !slices.Contains(z.Regions, s.Region) {
			return provider.Failure(req.Op, provider.ErrInvalidRequest, "size %s is not available in region %s", s.Size, s.Region)
		}
		p := Plan{
			Name: dropletName(req), Region: s.Region, Size: s.Size, Image: s.Image,
			Tags:         a.createTags(req),
			PriceMonthly: z.PriceMonthly, PriceHourly: z.PriceHourly,
			Temporary: req.Authorization != nil,
		}
		data, _ := json.Marshal(p)
		return ok(req.Op, &provider.Result{Data: data})
	}
	return provider.Failure(req.Op, provider.ErrInvalidRequest, "unknown size %s", s.Size)
}

func dropletName(req provider.Request) string {
	if req.Spec.Name != "" {
		return req.Spec.Name
	}
	return "bashy-" + strings.TrimPrefix(provider.IdempotencyTag(req.IdempotencyKey), "bashy-idem-")[:12]
}

func (a *adapter) createTags(req provider.Request) []string {
	tags := provider.LeaseTags(a.cfg.Owner, req.IdempotencyKey, req.Authorization)
	for _, t := range req.Spec.Tags {
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	return tags
}

// findByKey returns this owner's droplet created under key, if any.
func (a *adapter) findByKey(ctx context.Context, key string) (*droplet, error) {
	ds, err := a.listByTag(ctx, provider.IdempotencyTag(key))
	if err != nil {
		return nil, err
	}
	var mine []droplet
	for _, d := range ds {
		if provider.Owned(d.Tags, a.cfg.Owner) {
			mine = append(mine, d)
		}
	}
	switch len(mine) {
	case 0:
		return nil, nil
	case 1:
		return &mine[0], nil
	}
	ids := make([]string, len(mine))
	for i, d := range mine {
		ids[i] = strconv.Itoa(d.ID)
	}
	return nil, &provider.Error{Code: provider.ErrNeedsManualReconcile, Message: fmt.Sprintf("idempotency key matches %d droplets (%s)", len(mine), strings.Join(ids, ", "))}
}

// create is idempotent on req.IdempotencyKey: an existing droplet tagged
// with the key is returned instead of creating another. When the create
// call itself fails in a way that may have taken effect (a 5xx, a lost
// response, a timeout), the key is looked up again before reporting the
// failure, so a create that was accepted is reported as success and a
// later replay finds it either way: no orphan.
func (a *adapter) create(ctx context.Context, req provider.Request) (provider.Result, error) {
	s := req.Spec
	if s.Region == "" || s.Size == "" || s.Image == "" {
		return provider.Failure(req.Op, provider.ErrInvalidRequest, "create needs spec.region, spec.size and spec.image")
	}
	if d, err := a.findByKey(ctx, req.IdempotencyKey); err != nil {
		return fail(req.Op, err)
	} else if d != nil {
		return ok(req.Op, &provider.Result{Resource: d.resource()})
	}
	body := map[string]any{
		"name":   dropletName(req),
		"region": s.Region,
		"size":   s.Size,
		"image":  s.Image,
		"tags":   a.createTags(req),
	}
	if len(s.UserData) > 0 {
		body["user_data"] = string(s.UserData) // opaque, passed through unread
	}
	var out struct {
		Droplet droplet `json:"droplet"`
	}
	err := a.call(ctx, http.MethodPost, "/v2/droplets", body, &out)
	if err == nil {
		return ok(req.Op, &provider.Result{Resource: out.Droplet.resource()})
	}
	if !maybeApplied(err) || ctx.Err() != nil {
		return fail(req.Op, err)
	}
	if d, ferr := a.findByKey(ctx, req.IdempotencyKey); ferr == nil && d != nil {
		return ok(req.Op, &provider.Result{Resource: d.resource()})
	}
	return fail(req.Op, err)
}

// owned fetches req.ResourceID and refuses unless it carries this
// owner's lease tags.
func (a *adapter) owned(ctx context.Context, req provider.Request) (*droplet, provider.Result, error) {
	id, err := strconv.Atoi(req.ResourceID)
	if err != nil || id <= 0 {
		r, e := provider.Failure(req.Op, provider.ErrInvalidRequest, "invalid droplet id %q", req.ResourceID)
		return nil, r, e
	}
	var out struct {
		Droplet droplet `json:"droplet"`
	}
	if err := a.call(ctx, http.MethodGet, "/v2/droplets/"+strconv.Itoa(id), nil, &out); err != nil {
		r, e := fail(req.Op, err)
		return nil, r, e
	}
	if !provider.Owned(out.Droplet.Tags, a.cfg.Owner) {
		r, e := provider.Failure(req.Op, provider.ErrNotOwned, "droplet %d does not carry the %s and %s tags; refusing to act on it", id, provider.TagLease, provider.OwnerTag(a.cfg.Owner))
		return nil, r, e
	}
	return &out.Droplet, provider.Result{}, nil
}

// action runs a droplet action and waits for it to finish. When the
// droplet is already in want (a replay, or stop of a stopped droplet) no
// action is issued. Stop and start only ever power the droplet off/on:
// they never delete, and stop reports that a powered-off droplet keeps
// billing.
func (a *adapter) action(ctx context.Context, req provider.Request, typ, want, size string) (provider.Result, error) {
	d, res, err := a.owned(ctx, req)
	if err != nil {
		return res, err
	}
	done := (want != "" && d.Status == want) || (typ == "resize" && d.SizeSlug == size)
	if !done {
		body := map[string]any{"type": typ}
		if size != "" {
			body["size"] = size
		}
		var out struct {
			Action doAction `json:"action"`
		}
		if err := a.call(ctx, http.MethodPost, "/v2/droplets/"+req.ResourceID+"/actions", body, &out); err != nil {
			return fail(req.Op, err)
		}
		if err := a.wait(ctx, out.Action); err != nil {
			return fail(req.Op, err)
		}
		if d, res, err = a.owned(ctx, req); err != nil {
			return res, err
		}
	}
	r := &provider.Result{Resource: d.resource()}
	if req.Op == provider.OpStop {
		r.Cost = &provider.CostReport{
			Ongoing:  true,
			Retained: append([]string{"droplet:" + req.ResourceID}, d.retained()...),
			Note:     "a powered-off droplet keeps its disk, IPs and full hourly charge until destroyed",
		}
	}
	return ok(req.Op, r)
}

func (a *adapter) wait(ctx context.Context, act doAction) error {
	for {
		switch act.Status {
		case "completed":
			return nil
		case "errored":
			return &provider.Error{Code: provider.ErrProvider, Message: fmt.Sprintf("action %d (%s) errored", act.ID, act.Type)}
		}
		select {
		case <-ctx.Done():
			return ctxErr(ctx)
		case <-time.After(a.cfg.PollInterval):
		}
		var out struct {
			Action doAction `json:"action"`
		}
		if err := a.call(ctx, http.MethodGet, "/v2/actions/"+strconv.Itoa(act.ID), nil, &out); err != nil {
			return err
		}
		act = out.Action
	}
}

// destroy deletes a droplet this owner created, after the A6 check
// against its recorded tags. The cost report names what DigitalOcean keeps
// (and bills) after the droplet is gone.
func (a *adapter) destroy(ctx context.Context, req provider.Request) (provider.Result, error) {
	d, res, err := a.owned(ctx, req)
	if err != nil {
		return res, err
	}
	if e := provider.AuthorizeDestroy(req, *d.resource()); e != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e, Cost: provider.RefusedDestroyCost()}, e
	}
	if err := a.call(ctx, http.MethodDelete, "/v2/droplets/"+req.ResourceID, nil, nil); err != nil {
		return fail(req.Op, err)
	}
	r := d.resource()
	r.State = provider.StateDeleted
	retained := d.retained()
	cost := &provider.CostReport{Ongoing: len(retained) > 0, Retained: retained}
	if cost.Ongoing {
		cost.Note = "volumes are not deleted with the droplet and keep billing"
	}
	return ok(req.Op, &provider.Result{Resource: r, Cost: cost})
}

func (a *adapter) listByTag(ctx context.Context, tag string) ([]droplet, error) {
	var all []droplet
	path := "/v2/droplets?per_page=200&tag_name=" + url.QueryEscape(tag)
	for page := 0; path != "" && page < 1000; page++ {
		var out struct {
			Droplets []droplet `json:"droplets"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}
		if err := a.call(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Droplets...)
		path = ""
		if next := out.Links.Pages.Next; next != "" {
			u, err := url.Parse(next)
			if err != nil {
				return nil, &provider.Error{Code: provider.ErrProvider, Message: "bad pagination link: " + err.Error()}
			}
			// Follow only the path+query: the token goes to BaseURL alone.
			path = u.EscapedPath() + "?" + u.RawQuery
		}
	}
	return all, nil
}

// ---- HTTP ----

// apiError is a non-2xx answer; it may have taken effect only for 5xx.
type apiError struct {
	status int
	id     string
	msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("digitalocean: HTTP %d %s: %s", e.status, e.id, e.msg)
}

// transportError is a failure with no HTTP answer; the request may or may
// not have reached the API.
type transportError struct{ err error }

func (e *transportError) Error() string { return "digitalocean: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// maybeApplied reports whether a failed mutating call may have taken
// effect anyway.
func maybeApplied(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status >= 500
	}
	var te *transportError
	return errors.As(err, &te)
}

func (a *adapter) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, a.cfg.BaseURL+path, body)
	if err != nil {
		return err
	}
	hreq.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	hreq.Header.Set("Accept", "application/json")
	if in != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.cfg.HTTPClient.Do(hreq)
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
	if err := json.Unmarshal(data, out); err != nil {
		return &provider.Error{Code: provider.ErrProvider, Message: fmt.Sprintf("digitalocean: decode %s %s: %v", method, path, err)}
	}
	return nil
}

func ctxErr(ctx context.Context) error {
	return &provider.Error{Code: provider.ErrTimeout, Message: "digitalocean: " + ctx.Err().Error(), Retryable: true}
}

// normalize maps a call error onto a provider.Error.
func normalize(err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	var ae *apiError
	if errors.As(err, &ae) {
		e := &provider.Error{Message: ae.Error()}
		switch {
		case ae.status == http.StatusUnauthorized || ae.status == http.StatusForbidden:
			e.Code = provider.ErrInvalidCredentials
		case ae.status == http.StatusNotFound:
			e.Code = provider.ErrNotFound
		case ae.status == http.StatusTooManyRequests:
			e.Code, e.Retryable = provider.ErrRateLimited, true
		case ae.status >= 500:
			e.Code, e.Retryable = provider.ErrProvider, true
		default:
			e.Code = provider.ErrInvalidRequest
		}
		return e
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &ne) && ne.Timeout()) {
		return &provider.Error{Code: provider.ErrTimeout, Message: err.Error(), Retryable: true}
	}
	var te *transportError
	if errors.As(err, &te) {
		return &provider.Error{Code: provider.ErrProvider, Message: err.Error(), Retryable: true}
	}
	return &provider.Error{Code: provider.ErrProvider, Message: err.Error()}
}

func fail(op provider.Op, err error) (provider.Result, error) {
	e := normalize(err)
	return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: op, Error: e}, e
}

func ok(op provider.Op, r *provider.Result) (provider.Result, error) {
	r.ProtocolVersion = provider.ProtocolVersion
	r.Op = op
	return *r, nil
}
