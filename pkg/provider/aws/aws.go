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
//     losslessly when it exceeds EC2's 16 KiB launch limit and STAGING it
//     in S3 behind a presigned-fetch bootstrap when even the wrapper does
//     not fit (userdata.go, s3.go), allocates and attaches a static IP named
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
	"sync"
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
	// S3URL overrides the S3 endpoint for the user-data staging bucket
	// (tests). Empty means the real virtual-hosted bucket endpoint.
	S3URL string
	// UserDataBucket is the bucket a too-large first-boot script is
	// staged in. Empty means $AWS_USERDATA_BUCKET, else
	// UserDataBucketName(accountID, region).
	UserDataBucket string
	// PresignTTL is how long the staged script's presigned GET stays
	// valid (default UserDataPresignTTL).
	PresignTTL time.Duration
	// BudgetsURL and STSURL override the AWS Budgets and STS endpoints
	// (tests). Empty means the real global endpoints. They are separate
	// fields because they are separate services, even though one fake
	// serves all of them in tests.
	BudgetsURL, STSURL string
	// AccountID is the account the budget belongs to. Empty asks STS
	// GetCallerIdentity for it, once (sts.go).
	AccountID string
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
	a, err := newAdapter(cfg)
	if err != nil {
		return nil, err
	}
	return provider.Guard(a), nil
}

func newAdapter(cfg Config) (*adapter, error) {
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
	budgetsURL := cfg.BudgetsURL
	if budgetsURL == "" {
		budgetsURL = BudgetsEndpoint
	}
	stsURL := cfg.STSURL
	if stsURL == "" {
		stsURL = STSEndpoint
	}
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = UserDataPresignTTL
	}
	endpoints := []string{lsURL, ceURL, budgetsURL, stsURL}
	if cfg.S3URL != "" {
		// S3 is addressed per-OBJECT, so there is no client to build
		// here: only the override has to be well-formed, and the bucket
		// (which may need an STS lookup) is resolved on first use.
		endpoints = append(endpoints, cfg.S3URL)
	}
	for _, u := range endpoints {
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
	// Budgets and STS are global too, and for the same reason signed for
	// us-east-1 rather than the instance's region.
	a.budgets = &client{
		endpoint: budgetsURL, region: BudgetsRegion, service: ServiceBudgets,
		prefix: budgetsTargetPrefix, creds: cfg.Credentials,
		httpClient: cfg.HTTPClient, now: a.now,
	}
	a.sts = &queryClient{
		endpoint: stsURL, region: STSRegion, service: ServiceSTS,
		version: stsAPIVersion, creds: cfg.Credentials,
		httpClient: cfg.HTTPClient, now: a.now,
	}
	return a, nil
}

type adapter struct {
	cfg     Config
	ls      *client
	ce      *client
	budgets *client
	sts     *queryClient
	// account caches the STS account id lookup (sts.go).
	account accountCache
	// staging caches the resolved user-data staging bucket client.
	staging stagingCache
}

// stagingCache memoizes the S3 staging client. Resolving it can cost an
// STS call (the default bucket name carries the account id), and a create
// already makes enough calls. Only a SUCCESSFUL resolution is cached.
type stagingCache struct {
	mu     sync.Mutex
	client *s3Client
}

// stagingClient returns the S3 client for the user-data staging bucket,
// resolving the bucket name from the config, then the environment, then
// the account id and region.
func (a *adapter) stagingClient(ctx context.Context) (*s3Client, error) {
	a.staging.mu.Lock()
	defer a.staging.mu.Unlock()
	if a.staging.client != nil {
		return a.staging.client, nil
	}
	bucket := strings.TrimSpace(a.cfg.UserDataBucket)
	if bucket == "" {
		bucket = strings.TrimSpace(os.Getenv(EnvUserDataBucket))
	}
	if bucket == "" {
		id, err := a.AccountID(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws: cannot name the user-data staging bucket without the account id: %w", err)
		}
		bucket = UserDataBucketName(id, a.cfg.Region)
	}
	c := &s3Client{
		root: s3ObjectRoot(bucket, a.cfg.Region, a.cfg.S3URL), bucket: bucket,
		region: a.cfg.Region, creds: a.cfg.Credentials,
		httpClient: a.cfg.HTTPClient, now: a.now,
	}
	a.staging.client = c
	return c, nil
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
	// UserDataBytes is what would go on the wire INLINE, and the two
	// flags say which of the three layers carried it: raw (both false),
	// the gzip+base64 wrapper (UserDataWrapped), or an S3 object behind
	// a fetch bootstrap (UserDataStaged, which names the bucket too).
	UserDataBytes     int    `json:"user_data_bytes"`
	UserDataWrapped   bool   `json:"user_data_wrapped"`
	UserDataStaged    bool   `json:"user_data_staged,omitempty"`
	UserDataBucket    string `json:"user_data_bucket,omitempty"`
	UserDataScriptLen int    `json:"user_data_script_bytes,omitempty"`
	// UserDataSHA256 is the staged script's digest — the one the fetch
	// bootstrap verifies — so an operator can check what a create would
	// put in the bucket against what the master rendered.
	UserDataSHA256 string   `json:"user_data_sha256,omitempty"`
	StaticIPName   string   `json:"static_ip_name,omitempty"`
	OpenPorts      []string `json:"open_ports,omitempty"`
	Budget         Verdict  `json:"budget"`
}

func (a *adapter) plan(ctx context.Context, req provider.Request) (provider.Result, error) {
	b, pe := a.resolveSpec(ctx, req)
	if pe != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: pe}, pe
	}
	name := instanceName(req)
	// A plan STAGES NOTHING: it renders what the create would send, so a
	// --dry-run reports the real inline size and the bucket it would
	// write to without putting an object there.
	ud, err := a.prepareUserData(ctx, name, req.Spec.UserData, false)
	if err != nil {
		return fail(req.Op, err)
	}
	p := Plan{
		Provider: Name, Name: name,
		Region: a.cfg.Region, AvailabilityZone: a.cfg.AvailabilityZone,
		BundleID: b.BundleID, BlueprintID: req.Spec.Image, Arch: ArchX8664,
		Tags:            a.createTags(req, ud.Key),
		PriceMonthly:    b.Price,
		PriceHourly:     hourly(b.Price),
		Temporary:       req.Authorization != nil,
		UserDataBytes:   len(ud.Inline),
		UserDataWrapped: Wrapped(ud.Inline),
		UserDataStaged:  ud.Staged(),
		UserDataBucket:  ud.Bucket,
	}
	if ud.Staged() {
		p.UserDataScriptLen = len(req.Spec.UserData)
		p.UserDataSHA256 = ud.SHA256
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

// createTags is the tag set for one create. stagedKey, when non-empty,
// adds the dhnt:userdata-key tag that tells a later destroy which S3
// object to delete (the IAM policy grants no s3:ListBucket, so the tag is
// the only record).
func (a *adapter) createTags(req provider.Request, stagedKey string) []string {
	tags := provider.LeaseTags(a.cfg.Owner, req.IdempotencyKey, req.Authorization)
	dhnt := a.dhntTags()
	if stagedKey != "" {
		dhnt = append(dhnt, TagUserDataKey+"="+stagedKey)
	}
	for _, t := range append(dhnt, req.Spec.Tags...) {
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
	// The user data is settled BEFORE CreateInstances: a script too large
	// to go inline is staged in S3 here, so the request carries a
	// bootstrap that already has an object to fetch.
	ud, uerr := a.prepareUserData(ctx, name, req.Spec.UserData, true)
	if uerr != nil {
		return fail(req.Op, uerr)
	}
	in := map[string]any{
		"instanceNames":    []string{name},
		"availabilityZone": a.cfg.AvailabilityZone,
		"blueprintId":      req.Spec.Image,
		"bundleId":         b.BundleID,
		"tags":             encodeTags(a.createTags(req, ud.Key)),
	}
	if len(ud.Inline) > 0 {
		in["userData"] = string(ud.Inline)
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
// NotFound, or answer with a record whose state is not populated yet.
// waitState is told to tolerate both here.
//
// An instance that never reaches "running" is a FAILED create, not a
// create to report: the firewall and the static IP are the difference
// between a satellite and a machine with Lightsail's defaults (ssh and
// http open to the whole internet, no stable address), so the error names
// the steps that did not run rather than letting a caller read "created"
// as "ready".
func (a *adapter) finish(ctx context.Context, req provider.Request, name string) (provider.Result, error) {
	cur, err := a.waitState(ctx, name, anyRestingState, tolerateMissing, tolerateUnknown)
	if err != nil {
		return fail(req.Op, a.unfinished(name, err))
	}
	if cur.State.Name != "running" {
		// A replay against an instance the operator has since stopped:
		// the create already happened, so report it. Starting it is the
		// start operation's job, not create's.
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

// unfinished turns a failed wait for "running" into an error that says
// what was left undone, so the instance can be finished by hand or by a
// replay instead of being believed ready.
//
// The error is deliberately NOT retryable and not a timeout, whatever the
// cause was. A timeout is AMBIGUOUS to the dispatcher — the provider may
// or may not have acted — so it reconciles it by listing the idempotency
// tag, finds the instance that does exist, and reports the create as
// SUCCEEDED. That is the live bug restated: the instance may well be
// real, the host is not finished, and the caller must hear the second
// half. A plain
// provider error is reported as the failure it is; re-running the same
// create finishes the host, because create is idempotent on its key.
func (a *adapter) unfinished(name string, err error) error {
	var left []string
	if !a.cfg.SkipFirewall {
		left = append(left, "the satellite firewall (PutInstancePublicPorts) was NOT applied, so Lightsail's defaults are still in place")
	}
	if !a.cfg.SkipStaticIP {
		left = append(left, "the static ip "+name+staticIPSuffix+" was NOT attached")
	}
	e := normalize(err)
	if len(left) == 0 {
		return e
	}
	return &provider.Error{Code: provider.ErrProvider, Retryable: false, Message: fmt.Sprintf(
		"%s; the host is unfinished: %s. Re-run the same create to finish it, or destroy it",
		e.Message, strings.Join(left, "; "))}
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

// allocateStaticIP allocates "<name>-ip" and reads it back, so the
// caller has the address AWS assigned and not just an operation record.
func (a *adapter) allocateStaticIP(ctx context.Context, ipName string) (*staticIP, error) {
	var out struct {
		Operations []operation `json:"operations"`
	}
	if err := a.ls.call(ctx, "AllocateStaticIp", map[string]any{"staticIpName": ipName}, &out); err != nil {
		return nil, err
	}
	if err := opsError(out.Operations); err != nil {
		return nil, err
	}
	return a.getStaticIP(ctx, ipName)
}

// ensureStaticIP allocates "<name>-ip" if needed and attaches it to the
// instance, so the satellite's sslip.io endpoint survives a stop/start
// (Lightsail's ordinary public IP does not). On the provisioning path the
// address has usually been reserved already (reserve.go), and then this
// only has the attach left to do.
func (a *adapter) ensureStaticIP(ctx context.Context, name string) error {
	ipName := name + staticIPSuffix
	sip, err := a.getStaticIP(ctx, ipName)
	if err != nil {
		if normalize(err).Code != provider.ErrNotFound {
			return err
		}
		if sip, err = a.allocateStaticIP(ctx, ipName); err != nil {
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

const (
	// tolerateMissing keeps waiting while GetInstance answers NotFound,
	// which a just-created instance does until AWS has propagated it.
	// Without it a create would fail on an instance that in fact exists.
	tolerateMissing waitOption = iota + 1
	// tolerateUnknown keeps waiting while the instance reports a state
	// this adapter does not map — including the EMPTY state a Lightsail
	// record carries for the first moments after CreateInstances, before
	// the state field is populated.
	//
	// It is load-bearing on the create path. Without it an unmapped
	// state counts as a resting state, so the wait ends on the first
	// read, and create then skips the firewall and the static IP and
	// reports a host in state "unknown" as provisioned — which is what
	// the first live zoo-aws create did.
	tolerateUnknown
)

// waitState polls GetInstance until the instance reports want, or the
// wait budget runs out. Lightsail's own operation records say when an API
// call finished, not when the machine reached a state, so the instance's
// own state is what is polled.
func (a *adapter) waitState(ctx context.Context, name, want string, opts ...waitOption) (*instance, error) {
	deadline := a.now().Add(a.cfg.WaitTimeout)
	missingOK := slices.Contains(opts, tolerateMissing)
	unknownOK := slices.Contains(opts, tolerateUnknown)
	for {
		in, err := a.getInstance(ctx, name)
		if err != nil {
			if !missingOK || normalize(err).Code != provider.ErrNotFound {
				return nil, err
			}
			in = &instance{Name: name, State: instanceState{Name: "pending"}}
		}
		st := State(in.State.Name)
		// An unmapped state is in flight when the caller says so. The
		// empty state name a fresh record carries is one of those, and
		// it is deliberately NOT compared against want: anyRestingState
		// is the empty string too, and the two meeting would end the
		// wait the instant AWS answered with an unpopulated record.
		inFlight := st == provider.StatePending || (unknownOK && st == provider.StateUnknown)
		switch {
		case want == anyRestingState:
			if !inFlight {
				return in, nil
			}
		case in.State.Name == want:
			return in, nil
		}
		if st == provider.StateError {
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
