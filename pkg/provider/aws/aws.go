// Package aws is the AWS Lightsail provider adapter (S522.2): a
// FormBuiltin provider.Adapter over the Lightsail JSON API, reached
// through `bashy infra provision aws` (or `--provider aws`).
//
// It mirrors the DigitalOcean reference adapter's discipline and adds what
// AWS needs on top:
//
//   - The Lightsail API is spoken DIRECTLY (sigv4.go + lightsail.go). There
//     is no AWS SDK here: pkg/provider is a nested stdlib-only module, and
//     the whole signer is ~200 lines pinned by AWS's published test vectors.
//   - A resource is addressed by NAME. Lightsail has no numeric instance
//     id, so Resource.ID is the instance name, verbatim.
//   - Every created resource carries the contract's bashy-lease tags AND
//     dhnt:managed=true (tags.go). The automation IAM policy only permits
//     deleting dhnt:managed=true resources, so destroy refuses an untagged
//     instance with a clear message instead of an opaque AccessDenied.
//   - Create runs a BUDGET PREFLIGHT first (budget.go) and refuses to spend
//     over the operator's cap. It fails closed.
//   - Create passes Spec.UserData through byte-identical, wrapping it
//     losslessly only when it exceeds EC2's 16 KiB launch limit
//     (userdata.go), allocates and attaches a static IP named
//     "<instance>-ip" so the satellite's endpoint survives a stop/start,
//     and replaces the instance's public ports with the satellite firewall.
//   - Lightsail is x86_64 only, which is what the satellite installer
//     needs; offers.go reports that arch explicitly rather than leaving it
//     unknown.
//
// Credentials are held for the adapter's lifetime, never persisted and
// never logged: the key id appears only where AWS itself puts it (the
// Authorization header), and the secret only inside the HMAC chain.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// Name is the provider name in the adapter's manifest.
const Name = "aws"

// DefaultBundleID and DefaultBlueprintID are the satellite defaults: a
// 2 vCPU / 2 GB Lightsail plan and Ubuntu 24.04 LTS.
const (
	DefaultBundleID    = "small_3_0"
	DefaultBlueprintID = "ubuntu_24_04"
)

// staticIPSuffix names the static IP belonging to an instance. Lightsail
// static IPs cannot carry tags, so this name is the only link back to the
// instance that owns one, and destroy releases only this one.
const staticIPSuffix = "-ip"

// Config configures an adapter.
type Config struct {
	// Credentials are the AWS access keys. Required.
	Credentials Credentials
	// Region is the Lightsail region. Empty means $AWS_REGION, else
	// DefaultRegion.
	Region string
	// AvailabilityZone is the AZ to create in. Empty derives it from the
	// region (Region + "a"); Lightsail requires an explicit AZ at create.
	AvailabilityZone string
	// Owner scopes ownership (provider.ValidOwner): written as the owner
	// tag at create and required on every instance the adapter acts on.
	Owner string
	// BaseURL overrides the Lightsail endpoint (tests point it at
	// fakelightsail). Empty means the regional endpoint.
	BaseURL string
	// CostExplorerURL overrides the Cost Explorer endpoint (tests). Empty
	// means CostExplorerEndpoint.
	CostExplorerURL string
	// Budget is the spend policy enforced before any create.
	Budget Budget
	// Sprint, Story and TTL become the dhnt:sprint / dhnt:story /
	// dhnt:ttl tags when set.
	Sprint, Story, TTL string
	// Ports is the public-port set applied after create; nil means
	// DefaultSatellitePorts. SkipFirewall leaves Lightsail's defaults.
	Ports        []PortRule
	SkipFirewall bool
	// SkipStaticIP omits the "<name>-ip" static IP allocation.
	SkipStaticIP bool
	// HTTPClient defaults to a client with a 60s timeout.
	HTTPClient *http.Client
	// PollInterval is the wait between state polls (default 2s).
	PollInterval time.Duration
	// WaitTimeout bounds a create's wait for "running" (default 5m).
	WaitTimeout time.Duration
	// Now is the clock (request signing, offer observation times).
	Now func() time.Time
}

// Manifest is the adapter's manifest. Lightsail has no in-place bundle
// change, so resize is deliberately absent: an undeclared operation is
// refused by Guard before any API call instead of failing as a 400.
func Manifest() provider.Manifest {
	caps := []provider.Capability{}
	for _, op := range provider.Ops() {
		if op == provider.OpResize {
			continue
		}
		caps = append(caps, provider.Capability(op))
	}
	return provider.Manifest{
		Name:            Name,
		Kind:            provider.KindInfra,
		Form:            provider.FormBuiltin,
		ProtocolVersion: provider.ProtocolVersion,
		Summary:         "AWS Lightsail instances over the Lightsail JSON API (stdlib SigV4, budget preflight)",
		Capabilities:    caps,
	}
}

// azPattern matches a full availability zone ("us-west-2a"), as opposed to
// a region ("us-west-2").
var azPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d[a-z]$`)

// regionOf strips an availability zone's trailing letter.
func regionOf(s string) string {
	if azPattern.MatchString(s) {
		return s[:len(s)-1]
	}
	return s
}

// New returns the adapter, wrapped in provider.Guard so capability,
// idempotency-key and destroy-authorization checks run before any API call.
func New(cfg Config) (provider.Adapter, error) {
	if !cfg.Credentials.Valid() {
		return nil, &provider.Error{Code: provider.ErrInvalidCredentials,
			Message: "aws: " + EnvAccessKeyID + " and " + EnvSecretAccessKey + " are both required"}
	}
	if !provider.ValidOwner(cfg.Owner) {
		return nil, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws: owner %q must match [A-Za-z0-9_-]{1,64}", cfg.Owner)}
	}
	if cfg.Region == "" {
		cfg.Region = strings.TrimSpace(os.Getenv(EnvRegion))
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	if cfg.AvailabilityZone == "" {
		if azPattern.MatchString(cfg.Region) {
			cfg.AvailabilityZone = cfg.Region
		} else {
			cfg.AvailabilityZone = cfg.Region + "a"
		}
	}
	cfg.Region = regionOf(cfg.Region)
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = 5 * time.Minute
	}
	if cfg.Ports == nil {
		cfg.Ports = DefaultSatellitePorts()
	}
	lsURL := cfg.BaseURL
	if lsURL == "" {
		lsURL = lightsailEndpoint(cfg.Region)
	}
	ceURL := cfg.CostExplorerURL
	if ceURL == "" {
		ceURL = CostExplorerEndpoint
	}
	for _, u := range []string{lsURL, ceURL} {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return nil, &provider.Error{Code: provider.ErrInvalidRequest, Message: fmt.Sprintf("aws: invalid endpoint %q", u)}
		}
	}
	a := &adapter{cfg: cfg}
	a.ls = &client{
		endpoint: lsURL, region: cfg.Region, service: ServiceLightsail,
		prefix: targetPrefix, creds: cfg.Credentials,
		httpClient: cfg.HTTPClient, now: a.now,
	}
	// Cost Explorer is global: its endpoint and signing region are
	// us-east-1 no matter where the instance lands.
	a.ce = &client{
		endpoint: ceURL, region: CostExplorerRegion, service: ServiceCostExplorer,
		prefix: costExplorerTargetPrefix, creds: cfg.Credentials,
		httpClient: cfg.HTTPClient, now: a.now,
	}
	return provider.Guard(a), nil
}

type adapter struct {
	cfg Config
	ls  *client
	ce  *client
}

func (a *adapter) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}
	return time.Now()
}

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
		in, res, err := a.owned(ctx, req)
		if err != nil {
			return res, err
		}
		return ok(req.Op, &provider.Result{Resource: a.resource(ctx, in)})
	case provider.OpList:
		return a.list(ctx, req)
	case provider.OpStart:
		return a.power(ctx, req, "StartInstance", "running")
	case provider.OpStop:
		return a.power(ctx, req, "StopInstance", "stopped")
	case provider.OpReboot:
		return a.power(ctx, req, "RebootInstance", "running")
	case provider.OpDestroy:
		return a.destroy(ctx, req)
	}
	return provider.Failure(req.Op, provider.ErrUnsupportedCapability, "aws: operation %q", req.Op)
}

// ---- resource mapping ----

// resource maps a Lightsail instance onto a contract resource. When the
// instance has a static IP, the "<name>-ip" address is reported too (and
// first), so a caller reading PublicIPs gets the stable endpoint rather
// than whatever Lightsail happens to report.
func (a *adapter) resource(ctx context.Context, in *instance) *provider.Resource {
	r := &provider.Resource{
		ID:        in.Name,
		Name:      in.Name,
		State:     State(in.State.Name),
		RawStatus: in.State.Name,
		Region:    in.Location.RegionName,
		Size:      in.BundleID,
		Tags:      decodeTags(in.Tags),
	}
	if r.Region == "" {
		r.Region = regionOf(in.Location.AvailabilityZone)
	}
	if in.CreatedAt > 0 {
		sec := int64(in.CreatedAt)
		r.CreatedAt = time.Unix(sec, int64((in.CreatedAt-float64(sec))*1e9)).UTC()
	}
	if in.IsStaticIP {
		if sip, err := a.getStaticIP(ctx, in.Name+staticIPSuffix); err == nil && sip.IPAddress != "" {
			r.PublicIPs = append(r.PublicIPs, sip.IPAddress)
		}
	}
	for _, ip := range append([]string{in.PublicIPAddress}, in.IPv6Addresses...) {
		if ip != "" && !slices.Contains(r.PublicIPs, ip) {
			r.PublicIPs = append(r.PublicIPs, ip)
		}
	}
	return r
}

// retained lists what keeps costing money independently of the instance.
// A Lightsail static IP is free while attached to an instance and BILLED
// once it is not, so an instance that is gone leaves a billable IP behind
// unless destroy released it.
func retainedStaticIP(name string) []string { return []string{"static-ip:" + name + staticIPSuffix} }

// ---- operations ----

func (a *adapter) validate(ctx context.Context, req provider.Request) (provider.Result, error) {
	// GetRegions is the cheapest authenticated Lightsail read and needs
	// no pre-existing resource, so it proves the credential without
	// assuming anything about the account's contents.
	var out struct {
		Regions []region `json:"regions"`
	}
	if err := a.ls.call(ctx, "GetRegions", map[string]any{"includeAvailabilityZones": true}, &out); err != nil {
		return fail(req.Op, err)
	}
	names := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		names = append(names, r.Name)
	}
	data, _ := json.Marshal(map[string]any{
		"region":            a.cfg.Region,
		"availability_zone": a.cfg.AvailabilityZone,
		"regions":           names,
		"access_key_id":     redactKeyID(a.cfg.Credentials.AccessKeyID),
	})
	return ok(req.Op, &provider.Result{Data: data})
}

func (a *adapter) capacity(ctx context.Context, req provider.Request) (provider.Result, error) {
	bundles, err := a.bundles(ctx)
	if err != nil {
		return fail(req.Op, err)
	}
	blueprints, err := a.blueprints(ctx)
	if err != nil {
		return fail(req.Op, err)
	}
	var regions struct {
		Regions []region `json:"regions"`
	}
	if err := a.ls.call(ctx, "GetRegions", map[string]any{"includeAvailabilityZones": true}, &regions); err != nil {
		return fail(req.Op, err)
	}
	out := map[string]any{
		"region":     a.cfg.Region,
		"bundles":    bundles,
		"blueprints": blueprints,
		"regions":    regions.Regions,
		"offers":     a.offers(bundles),
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fail(req.Op, err)
	}
	return ok(req.Op, &provider.Result{Data: data})
}

// Plan is the Data payload of a plan result: what a create would do, what
// it would cost, and what the budget preflight decides about it.
type Plan struct {
	Provider         string   `json:"provider"`
	Name             string   `json:"name"`
	Region           string   `json:"region"`
	AvailabilityZone string   `json:"availability_zone"`
	BundleID         string   `json:"bundle_id"`
	BlueprintID      string   `json:"blueprint_id"`
	Arch             string   `json:"arch"`
	Tags             []string `json:"tags"`
	PriceMonthly     float64  `json:"price_monthly"`
	PriceHourly      float64  `json:"price_hourly"`
	Temporary        bool     `json:"temporary"`
	// UserDataBytes is what would go on the wire, and UserDataWrapped
	// says whether it had to be compressed to fit EC2's launch limit.
	UserDataBytes   int      `json:"user_data_bytes"`
	UserDataWrapped bool     `json:"user_data_wrapped"`
	StaticIPName    string   `json:"static_ip_name,omitempty"`
	OpenPorts       []string `json:"open_ports,omitempty"`
	Budget          Verdict  `json:"budget"`
}

func (a *adapter) plan(ctx context.Context, req provider.Request) (provider.Result, error) {
	b, pe := a.resolveSpec(ctx, req)
	if pe != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: pe}, pe
	}
	ud, err := WrapUserData(req.Spec.UserData)
	if err != nil {
		return provider.Failure(req.Op, provider.ErrInvalidRequest, "%v", err)
	}
	name := instanceName(req)
	p := Plan{
		Provider: Name, Name: name,
		Region: a.cfg.Region, AvailabilityZone: a.cfg.AvailabilityZone,
		BundleID: b.BundleID, BlueprintID: req.Spec.Image, Arch: ArchX8664,
		Tags:            a.createTags(req),
		PriceMonthly:    b.Price,
		PriceHourly:     hourly(b.Price),
		Temporary:       req.Authorization != nil,
		UserDataBytes:   len(ud),
		UserDataWrapped: Wrapped(ud),
	}
	if !a.cfg.SkipStaticIP {
		p.StaticIPName = name + staticIPSuffix
	}
	if !a.cfg.SkipFirewall {
		p.OpenPorts = describePorts(a.cfg.Ports)
	}
	// The plan is where --dry-run learns the budget verdict, so run the
	// same preflight the create will run; it allocates nothing.
	v, be := a.BudgetPreflight(ctx, b)
	p.Budget = v
	data, merr := json.Marshal(p)
	if merr != nil {
		return fail(req.Op, merr)
	}
	if be != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Data: data, Error: be}, be
	}
	return ok(req.Op, &provider.Result{Data: data})
}

// resolveSpec validates the create/plan spec against the live catalog: the
// region must be the one this adapter is pinned to, the blueprint must
// exist and be active, and the bundle must exist, be active and support
// Linux. It returns the bundle so the caller has its price.
func (a *adapter) resolveSpec(ctx context.Context, req provider.Request) (bundle, *provider.Error) {
	s := req.Spec
	if s.Region == "" || s.Size == "" || s.Image == "" {
		return bundle{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws: %s needs spec.region, spec.size (a Lightsail bundle id) and spec.image (a blueprint id)", req.Op)}
	}
	if r := regionOf(s.Region); r != a.cfg.Region {
		return bundle{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws: this adapter is pinned to region %s; spec.region %q would need an adapter opened for that region", a.cfg.Region, s.Region)}
	}
	bps, err := a.blueprints(ctx)
	if err != nil {
		return bundle{}, normalize(err)
	}
	var found bool
	for _, bp := range bps {
		if bp.BlueprintID != s.Image {
			continue
		}
		if !bp.IsActive {
			return bundle{}, &provider.Error{Code: provider.ErrInvalidRequest,
				Message: fmt.Sprintf("aws: blueprint %s is inactive and cannot be used for a new instance", s.Image)}
		}
		found = true
		break
	}
	if !found {
		return bundle{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws: unknown blueprint %q in region %s (list them with `bashy aws lightsail get-blueprints --region %s`)", s.Image, a.cfg.Region, a.cfg.Region)}
	}
	bundles, err := a.bundles(ctx)
	if err != nil {
		return bundle{}, normalize(err)
	}
	for _, b := range bundles {
		if b.BundleID != s.Size {
			continue
		}
		if !b.IsActive {
			return bundle{}, &provider.Error{Code: provider.ErrInvalidRequest,
				Message: fmt.Sprintf("aws: bundle %s is not available in region %s", s.Size, a.cfg.Region)}
		}
		if len(b.SupportedPlatforms) > 0 && !slices.Contains(b.SupportedPlatforms, "LINUX_UNIX") {
			return bundle{}, &provider.Error{Code: provider.ErrInvalidRequest,
				Message: fmt.Sprintf("aws: bundle %s does not support LINUX_UNIX blueprints", s.Size)}
		}
		return b, nil
	}
	return bundle{}, &provider.Error{Code: provider.ErrInvalidRequest,
		Message: fmt.Sprintf("aws: unknown bundle %q in region %s (list them with `bashy aws lightsail get-bundles --region %s`)", s.Size, a.cfg.Region, a.cfg.Region)}
}

// instanceNamePattern is Lightsail's name charset (\w[\w\-]*\w).
var instanceNamePattern = regexp.MustCompile(`^\w[\w\-]*\w$`)

// instanceName is the Lightsail instance name for a request: the caller's
// name, or one derived from the idempotency tag.
func instanceName(req provider.Request) string {
	if req.Spec != nil && req.Spec.Name != "" {
		return req.Spec.Name
	}
	return "bashy-" + strings.TrimPrefix(provider.IdempotencyTag(req.IdempotencyKey), "bashy-idem-")[:12]
}

func (a *adapter) createTags(req provider.Request) []string {
	tags := provider.LeaseTags(a.cfg.Owner, req.IdempotencyKey, req.Authorization)
	for _, t := range append(a.dhntTags(), req.Spec.Tags...) {
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	for k, v := range req.Spec.Labels {
		kv := k
		if v != "" {
			kv = k + "=" + v
		}
		if !slices.Contains(tags, kv) {
			tags = append(tags, kv)
		}
	}
	slices.Sort(tags[3:]) // deterministic order after the lease triple
	return tags
}

// create is idempotent on req.IdempotencyKey: an existing instance tagged
// with the key is returned instead of creating another. A create whose
// answer may have been lost is reconciled by that tag before the failure
// is reported, so at most one instance results.
//
// After the instance exists, create finishes the host: it waits for
// "running", replaces the public ports with the satellite firewall, and
// allocates + attaches the "<name>-ip" static IP. Those steps are
// idempotent too, so a replay converges rather than duplicating.
func (a *adapter) create(ctx context.Context, req provider.Request) (provider.Result, error) {
	name := instanceName(req)
	if !instanceNamePattern.MatchString(name) {
		return provider.Failure(req.Op, provider.ErrInvalidRequest,
			"aws: instance name %q must match Lightsail's \\w[\\w\\-]*\\w", name)
	}
	if existing, err := a.findByKey(ctx, req.IdempotencyKey); err != nil {
		return fail(req.Op, err)
	} else if existing != nil {
		return a.finish(ctx, req, existing.Name)
	}
	b, pe := a.resolveSpec(ctx, req)
	if pe != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: pe}, pe
	}
	if _, be := a.BudgetPreflight(ctx, b); be != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: be}, be
	}
	ud, err := WrapUserData(req.Spec.UserData)
	if err != nil {
		return provider.Failure(req.Op, provider.ErrInvalidRequest, "%v", err)
	}
	in := map[string]any{
		"instanceNames":    []string{name},
		"availabilityZone": a.cfg.AvailabilityZone,
		"blueprintId":      req.Spec.Image,
		"bundleId":         b.BundleID,
		"tags":             encodeTags(a.createTags(req)),
	}
	if len(ud) > 0 {
		in["userData"] = string(ud)
	}
	if len(req.Spec.SSHKeys) > 0 {
		// Lightsail takes one key pair name, not a list.
		in["keyPairName"] = req.Spec.SSHKeys[0]
	}
	var out struct {
		Operations []operation `json:"operations"`
	}
	cerr := a.ls.call(ctx, "CreateInstances", in, &out)
	if cerr == nil {
		cerr = opsError(out.Operations)
	}
	if cerr != nil {
		if !maybeApplied(cerr) || ctx.Err() != nil {
			return fail(req.Op, cerr)
		}
		// The create may have taken effect with the answer lost:
		// reconcile by idempotency tag before reporting a failure.
		if found, ferr := a.findByKey(ctx, req.IdempotencyKey); ferr == nil && found != nil {
			return a.finish(ctx, req, found.Name)
		}
		return fail(req.Op, cerr)
	}
	return a.finish(ctx, req, name)
}

// finish brings a created instance to its serving shape: running, firewalled
// and holding its static IP. Every step is idempotent, so it is safe on a
// replay and on an instance that was reconciled rather than created here.
//
// It takes a NAME, not an instance, because a create's own answer is not a
// readable instance: Lightsail answers CreateInstances with an operation
// record, and a GetInstance issued immediately after can still answer
// NotFound. waitState is told to tolerate that here.
func (a *adapter) finish(ctx context.Context, req provider.Request, name string) (provider.Result, error) {
	cur, err := a.waitState(ctx, name, anyRestingState, tolerateMissing)
	if err != nil {
		return fail(req.Op, err)
	}
	if cur.State.Name != "running" {
		// A replay against an instance the operator has since stopped:
		// the create already happened, so report it. Waiting for
		// "running" would hang out the whole wait budget, and starting it
		// is the start operation's job, not create's.
		return ok(req.Op, &provider.Result{Resource: a.resource(ctx, cur)})
	}
	if !a.cfg.SkipFirewall {
		if err := a.putPorts(ctx, cur.Name); err != nil {
			return fail(req.Op, err)
		}
	}
	if !a.cfg.SkipStaticIP {
		if err := a.ensureStaticIP(ctx, cur.Name); err != nil {
			return fail(req.Op, err)
		}
		if cur, err = a.getInstance(ctx, cur.Name); err != nil {
			return fail(req.Op, err)
		}
	}
	return ok(req.Op, &provider.Result{Resource: a.resource(ctx, cur)})
}

func (a *adapter) list(ctx context.Context, req provider.Request) (provider.Result, error) {
	all, err := a.allInstances(ctx)
	if err != nil {
		return fail(req.Op, err)
	}
	out := []provider.Resource{}
	for i := range all {
		if provider.Owned(decodeTags(all[i].Tags), a.cfg.Owner) {
			out = append(out, *a.resource(ctx, &all[i]))
		}
	}
	return ok(req.Op, &provider.Result{Resources: out})
}

// owned fetches req.ResourceID and refuses unless it carries this owner's
// lease tags, so a foreign or untagged instance is never acted on.
func (a *adapter) owned(ctx context.Context, req provider.Request) (*instance, provider.Result, error) {
	if !instanceNamePattern.MatchString(req.ResourceID) {
		r, e := provider.Failure(req.Op, provider.ErrInvalidRequest, "aws: invalid instance name %q", req.ResourceID)
		return nil, r, e
	}
	in, err := a.getInstance(ctx, req.ResourceID)
	if err != nil {
		r, e := fail(req.Op, err)
		return nil, r, e
	}
	if !provider.Owned(decodeTags(in.Tags), a.cfg.Owner) {
		r, e := provider.Failure(req.Op, provider.ErrNotOwned,
			"aws: instance %s does not carry the %s and %s tags; refusing to act on it",
			req.ResourceID, provider.TagLease, provider.OwnerTag(a.cfg.Owner))
		return nil, r, e
	}
	return in, provider.Result{}, nil
}

// power runs a start/stop/reboot and waits for the resting state. A stop
// reports ongoing cost: a Lightsail bundle is billed by the month whether
// the instance runs or not, and the static IP keeps its charge too.
func (a *adapter) power(ctx context.Context, req provider.Request, op, want string) (provider.Result, error) {
	in, res, err := a.owned(ctx, req)
	if err != nil {
		return res, err
	}
	// A reboot always acts; a start/stop already in its target state is a
	// replay and issues nothing.
	alreadyThere := req.Op != provider.OpReboot && in.State.Name == want
	if !alreadyThere {
		var out struct {
			Operations []operation `json:"operations"`
		}
		if err := a.ls.call(ctx, op, map[string]any{"instanceName": in.Name}, &out); err != nil {
			return fail(req.Op, err)
		}
		if err := opsError(out.Operations); err != nil {
			return fail(req.Op, err)
		}
		if in, err = a.waitState(ctx, in.Name, want); err != nil {
			return fail(req.Op, err)
		}
	}
	r := &provider.Result{Resource: a.resource(ctx, in)}
	if req.Op == provider.OpStop {
		retained := []string{"instance:" + in.Name}
		if in.IsStaticIP {
			retained = append(retained, retainedStaticIP(in.Name)...)
		}
		r.Cost = &provider.CostReport{
			Ongoing:  true,
			Retained: retained,
			Note:     "a stopped Lightsail instance keeps its disk, its static IP and its full monthly plan charge until destroyed",
		}
	}
	return ok(req.Op, r)
}

// destroy deletes an instance this owner created, after the A6 check and
// the dhnt:managed gate, then releases the instance's static IP.
//
// The managed gate is checked HERE rather than left to AWS: the automation
// IAM policy permits DeleteInstance only on resources tagged
// dhnt:managed=true, so without this check an untagged instance would fail
// as an opaque AccessDeniedException instead of saying what is wrong.
//
// Order matters. The static IP is read BEFORE the delete (afterwards
// Lightsail has cleared its attachment, so there is nothing left to
// identify it by) and released AFTER it, so a delete that fails has not
// already given away the address. A release that fails is reported as
// ongoing cost rather than swallowed: an unattached Lightsail static IP is
// billed.
func (a *adapter) destroy(ctx context.Context, req provider.Request) (provider.Result, error) {
	in, res, err := a.owned(ctx, req)
	if err != nil {
		return res, err
	}
	r := a.resource(ctx, in)
	if e := provider.AuthorizeDestroy(req, *r); e != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e, Cost: provider.RefusedDestroyCost()}, e
	}
	if !Managed(r.Tags) {
		e := &provider.Error{Code: provider.ErrNotOwned, Message: fmt.Sprintf(
			"aws: instance %s is not tagged %s; the automation IAM policy permits deleting only tagged resources, so this destroy would be denied by AWS",
			in.Name, ManagedTag)}
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: e, Cost: provider.RefusedDestroyCost()}, e
	}

	ipName := in.Name + staticIPSuffix
	attached := false
	if in.IsStaticIP {
		if sip, serr := a.getStaticIP(ctx, ipName); serr == nil && sip.AttachedTo == in.Name {
			attached = true
		}
	}

	var out struct {
		Operations []operation `json:"operations"`
	}
	if err := a.ls.call(ctx, "DeleteInstance", map[string]any{"instanceName": in.Name}, &out); err != nil {
		return fail(req.Op, err)
	}
	if err := opsError(out.Operations); err != nil {
		return fail(req.Op, err)
	}
	r.State = provider.StateDeleted
	cost := &provider.CostReport{}
	if attached {
		if rerr := a.releaseStaticIP(ctx, ipName); rerr != nil {
			cost.Ongoing = true
			cost.Retained = retainedStaticIP(in.Name)
			cost.Note = fmt.Sprintf("the instance is deleted but its static IP %s could not be released (%v), and an unattached Lightsail static IP is billed; release it with `bashy aws lightsail release-static-ip --static-ip-name %s`", ipName, normalize(rerr), ipName)
		}
	}
	return ok(req.Op, &provider.Result{Resource: r, Cost: cost})
}

// ---- Lightsail reads ----

func (a *adapter) getInstance(ctx context.Context, name string) (*instance, error) {
	var out struct {
		Instance instance `json:"instance"`
	}
	if err := a.ls.call(ctx, "GetInstance", map[string]any{"instanceName": name}, &out); err != nil {
		return nil, err
	}
	if out.Instance.Name == "" {
		return nil, &provider.Error{Code: provider.ErrNotFound, Message: "aws: instance " + name + " not found"}
	}
	return &out.Instance, nil
}

// allInstances pages through GetInstances. Lightsail has no server-side
// tag filter for instances, so ownership is applied by the caller.
func (a *adapter) allInstances(ctx context.Context) ([]instance, error) {
	var all []instance
	token := ""
	for page := 0; page < 1000; page++ {
		in := map[string]any{}
		if token != "" {
			in["pageToken"] = token
		}
		var out struct {
			Instances     []instance `json:"instances"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if err := a.ls.call(ctx, "GetInstances", in, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Instances...)
		if out.NextPageToken == "" {
			return all, nil
		}
		token = out.NextPageToken
	}
	return all, &provider.Error{Code: provider.ErrProvider, Message: "aws: GetInstances did not terminate"}
}

// findByKey returns this owner's instance created under key, if any. Two
// matches is a reconcile case, never a guess.
func (a *adapter) findByKey(ctx context.Context, key string) (*instance, error) {
	all, err := a.allInstances(ctx)
	if err != nil {
		return nil, err
	}
	tag := provider.IdempotencyTag(key)
	var mine []instance
	for _, in := range all {
		tags := decodeTags(in.Tags)
		if provider.Owned(tags, a.cfg.Owner) && slices.Contains(tags, tag) {
			mine = append(mine, in)
		}
	}
	switch len(mine) {
	case 0:
		return nil, nil
	case 1:
		return &mine[0], nil
	}
	names := make([]string, len(mine))
	for i, in := range mine {
		names[i] = in.Name
	}
	return nil, &provider.Error{Code: provider.ErrNeedsManualReconcile,
		Message: fmt.Sprintf("aws: idempotency key matches %d instances (%s)", len(mine), strings.Join(names, ", "))}
}

func (a *adapter) bundles(ctx context.Context) ([]bundle, error) {
	var all []bundle
	token := ""
	for page := 0; page < 100; page++ {
		in := map[string]any{"includeInactive": true}
		if token != "" {
			in["pageToken"] = token
		}
		var out struct {
			Bundles       []bundle `json:"bundles"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := a.ls.call(ctx, "GetBundles", in, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Bundles...)
		if out.NextPageToken == "" {
			return all, nil
		}
		token = out.NextPageToken
	}
	return all, &provider.Error{Code: provider.ErrProvider, Message: "aws: GetBundles did not terminate"}
}

func (a *adapter) blueprints(ctx context.Context) ([]blueprint, error) {
	var all []blueprint
	token := ""
	for page := 0; page < 100; page++ {
		in := map[string]any{"includeInactive": true}
		if token != "" {
			in["pageToken"] = token
		}
		var out struct {
			Blueprints    []blueprint `json:"blueprints"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := a.ls.call(ctx, "GetBlueprints", in, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Blueprints...)
		if out.NextPageToken == "" {
			return all, nil
		}
		token = out.NextPageToken
	}
	return all, &provider.Error{Code: provider.ErrProvider, Message: "aws: GetBlueprints did not terminate"}
}

// ---- Lightsail writes ----

// putPorts replaces the instance's public-port set. PutInstancePublicPorts
// is declarative (it replaces, not appends), so sending the full intended
// set is what makes a replay converge.
func (a *adapter) putPorts(ctx context.Context, name string) error {
	in := map[string]any{"instanceName": name, "portInfos": wirePorts(a.cfg.Ports)}
	var out struct {
		Operation operation `json:"operation"`
	}
	if err := a.ls.call(ctx, "PutInstancePublicPorts", in, &out); err != nil {
		return err
	}
	return opsError([]operation{out.Operation})
}

func (a *adapter) getStaticIP(ctx context.Context, name string) (*staticIP, error) {
	var out struct {
		StaticIP staticIP `json:"staticIp"`
	}
	if err := a.ls.call(ctx, "GetStaticIp", map[string]any{"staticIpName": name}, &out); err != nil {
		return nil, err
	}
	if out.StaticIP.Name == "" {
		return nil, &provider.Error{Code: provider.ErrNotFound, Message: "aws: static ip " + name + " not found"}
	}
	return &out.StaticIP, nil
}

// ensureStaticIP allocates "<name>-ip" if needed and attaches it to the
// instance, so the satellite's sslip.io endpoint survives a stop/start
// (Lightsail's ordinary public IP does not).
func (a *adapter) ensureStaticIP(ctx context.Context, name string) error {
	ipName := name + staticIPSuffix
	sip, err := a.getStaticIP(ctx, ipName)
	if err != nil {
		if normalize(err).Code != provider.ErrNotFound {
			return err
		}
		var out struct {
			Operations []operation `json:"operations"`
		}
		if aerr := a.ls.call(ctx, "AllocateStaticIp", map[string]any{"staticIpName": ipName}, &out); aerr != nil {
			return aerr
		}
		if oerr := opsError(out.Operations); oerr != nil {
			return oerr
		}
		if sip, err = a.getStaticIP(ctx, ipName); err != nil {
			return err
		}
	}
	if sip.AttachedTo == name {
		return nil
	}
	if sip.AttachedTo != "" {
		return &provider.Error{Code: provider.ErrNotOwned, Message: fmt.Sprintf(
			"aws: static ip %s is attached to %s, not %s; refusing to move it", ipName, sip.AttachedTo, name)}
	}
	var out struct {
		Operations []operation `json:"operations"`
	}
	if err := a.ls.call(ctx, "AttachStaticIp", map[string]any{"staticIpName": ipName, "instanceName": name}, &out); err != nil {
		return err
	}
	return opsError(out.Operations)
}

// releaseStaticIP detaches and releases an IP. Detach runs first because
// Lightsail refuses to release an attached address — but a FAILED detach is
// not itself an error: deleting an instance already detaches its address,
// so a detach of an unattached IP is expected to be rejected. Only the
// RELEASE decides whether the address is still billing, so that is the
// only error that propagates.
func (a *adapter) releaseStaticIP(ctx context.Context, ipName string) error {
	var det struct {
		Operations []operation `json:"operations"`
	}
	_ = a.ls.call(ctx, "DetachStaticIp", map[string]any{"staticIpName": ipName}, &det)
	var rel struct {
		Operations []operation `json:"operations"`
	}
	if err := a.ls.call(ctx, "ReleaseStaticIp", map[string]any{"staticIpName": ipName}, &rel); err != nil {
		if normalize(err).Code == provider.ErrNotFound {
			// Already gone: nothing is billing.
			return nil
		}
		return err
	}
	return opsError(rel.Operations)
}

// ---- waiting ----

// anyRestingState is the waitState target meaning "whatever this instance
// settles into": it waits out an in-flight state without demanding a
// particular destination.
const anyRestingState = ""

// waitOption tunes waitState.
type waitOption int

// tolerateMissing keeps waiting while GetInstance answers NotFound, which
// a just-created instance does until AWS has propagated it. Without it a
// create would fail on an instance that in fact exists.
const tolerateMissing waitOption = iota + 1

// waitState polls GetInstance until the instance reports want, or the
// wait budget runs out. Lightsail's own operation records say when an API
// call finished, not when the machine reached a state, so the instance's
// own state is what is polled.
func (a *adapter) waitState(ctx context.Context, name, want string, opts ...waitOption) (*instance, error) {
	deadline := a.now().Add(a.cfg.WaitTimeout)
	missingOK := slices.Contains(opts, tolerateMissing)
	for {
		in, err := a.getInstance(ctx, name)
		if err != nil {
			if !missingOK || normalize(err).Code != provider.ErrNotFound {
				return nil, err
			}
			in = &instance{Name: name, State: instanceState{Name: "pending"}}
		}
		settled := State(in.State.Name) != provider.StatePending
		if in.State.Name == want || (want == anyRestingState && settled) {
			return in, nil
		}
		if State(in.State.Name) == provider.StateError {
			return nil, &provider.Error{Code: provider.ErrProvider,
				Message: fmt.Sprintf("aws: instance %s is in state %q, not %q", name, in.State.Name, want)}
		}
		if !a.now().Before(deadline) {
			target := want
			if target == anyRestingState {
				target = "a resting state"
			}
			return nil, &provider.Error{Code: provider.ErrTimeout, Retryable: true,
				Message: fmt.Sprintf("aws: instance %s is %q after waiting %s for %s", name, in.State.Name, a.cfg.WaitTimeout, target)}
		}
		select {
		case <-ctx.Done():
			return nil, ctxErr(ctx)
		case <-time.After(a.cfg.PollInterval):
		}
	}
}

// opsError turns a failed Lightsail operation record into an error. A 200
// answer carrying errorCode is still a failure.
func opsError(ops []operation) error {
	for _, o := range ops {
		if o.ErrorCode == "" && !strings.EqualFold(o.Status, "Failed") {
			continue
		}
		msg := o.ErrorDetails
		if msg == "" {
			msg = o.Status
		}
		return &provider.Error{Code: provider.ErrProvider, Message: fmt.Sprintf(
			"aws: %s on %s failed: %s %s", o.OperationType, o.ResourceName, o.ErrorCode, msg)}
	}
	return nil
}

// ---- errors ----

// maybeApplied reports whether a failed mutating call may have taken
// effect anyway: a 5xx, a throttle, or a transport failure with no answer.
func maybeApplied(err error) bool {
	var ce *callError
	if errors.As(err, &ce) {
		return ce.status >= 500 || ce.status == http.StatusTooManyRequests
	}
	var te *transportError
	return errors.As(err, &te)
}

// normalize maps a call error onto a normalized provider error. AWS JSON
// services answer almost everything with HTTP 400, so the EXCEPTION TYPE
// is what carries the meaning, not the status code.
func normalize(err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	var ce *callError
	if errors.As(err, &ce) {
		e := &provider.Error{Message: ce.Error()}
		switch ce.typ {
		case "AccessDeniedException", "UnauthenticatedException",
			"InvalidSignatureException", "MissingAuthenticationTokenException",
			"IncompleteSignatureException", "SignatureDoesNotMatch",
			"UnrecognizedClientException", "DataUnavailableException":
			e.Code = provider.ErrInvalidCredentials
		case "NotFoundException", "ResourceNotFoundException", "DoesNotExistException":
			e.Code = provider.ErrNotFound
		case "ThrottlingException", "TooManyRequestsException",
			"RequestThrottledException", "LimitExceededException":
			e.Code, e.Retryable = provider.ErrRateLimited, true
		case "InvalidInputException", "ValidationException", "InvalidParameterException":
			e.Code = provider.ErrInvalidRequest
		case "OperationFailureException", "ServiceException",
			"AccountSetupInProgressException", "RegionSetupInProgressException",
			"UnauthenticatedException2":
			e.Code, e.Retryable = provider.ErrProvider, true
		default:
			switch {
			case ce.status == http.StatusUnauthorized || ce.status == http.StatusForbidden:
				e.Code = provider.ErrInvalidCredentials
			case ce.status == http.StatusNotFound:
				e.Code = provider.ErrNotFound
			case ce.status == http.StatusTooManyRequests:
				e.Code, e.Retryable = provider.ErrRateLimited, true
			case ce.status >= 500:
				e.Code, e.Retryable = provider.ErrProvider, true
			default:
				e.Code = provider.ErrInvalidRequest
			}
		}
		return e
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		(errors.As(err, &ne) && ne.Timeout()) {
		return &provider.Error{Code: provider.ErrTimeout, Message: "aws: " + err.Error(), Retryable: true}
	}
	var te *transportError
	if errors.As(err, &te) {
		return &provider.Error{Code: provider.ErrProvider, Message: err.Error(), Retryable: true}
	}
	return &provider.Error{Code: provider.ErrProvider, Message: err.Error()}
}

func ctxErr(ctx context.Context) error {
	return &provider.Error{Code: provider.ErrTimeout, Message: "aws: " + ctx.Err().Error(), Retryable: true}
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

// ---- misc ----

// hourly converts a Lightsail monthly plan price to an hourly rate over a
// 730-hour month (AWS's own convention). Lightsail bills by the month but
// prorates by the hour.
func hourly(monthly float64) float64 { return monthly / 730 }

// describePorts renders the firewall for human and JSON output.
func describePorts(rules []PortRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		port := fmt.Sprintf("%d", r.FromPort)
		if r.ToPort != r.FromPort {
			port = fmt.Sprintf("%d-%d", r.FromPort, r.ToPort)
		}
		src := "0.0.0.0/0,::/0"
		if len(r.Aliases) > 0 {
			src = strings.Join(r.Aliases, ",")
		}
		out = append(out, fmt.Sprintf("%s/%s from %s", port, r.Protocol, src))
	}
	return out
}

// redactKeyID shows just enough of an access key id to tell two apart,
// never the whole one. The secret never appears anywhere.
func redactKeyID(id string) string {
	if len(id) <= 4 {
		return "****"
	}
	return "****" + id[len(id)-4:]
}
