// The EC2 GPU worker adapter (S546.2): a FormBuiltin provider.Adapter
// over the EC2 query API, reached through `bashy infra provision aws`
// (or `--provider aws-ec2-gpu`) for on-demand GPU workers.
//
// It shares the package's stdlib SigV4 signer (sigv4.go), the S522
// S3-staged presigned user-data bootstrap (userdata.go, s3.go), the
// dhnt:managed=true tag discipline (tags.go) and the Budgets-first spend
// ladder (budgets.go) with the Lightsail adapter, and adds what EC2 GPU
// workers need on top:
//
//   - EC2 is spoken DIRECTLY as a query-protocol service (Action +
//     Version form parameters, XML answers) through the shared
//     queryClient; only the error envelope differs (<Response><Errors>
//     rather than STS's <ErrorResponse>), which is the parseErr hook.
//   - A resource is addressed by INSTANCE ID (i-...). Resource.ID is the
//     id, verbatim; the Name tag carries the human name.
//   - Only the dhnt-automation-ec2-gpu allowlist may launch: g6.xlarge,
//     g6.2xlarge, g6e.xlarge, g5.xlarge (gpuTypes). Anything else is
//     refused before any API call, as is a volume over 200 GB.
//   - Every launch tags BOTH the instance and its gp3 volume with
//     dhnt:managed=true in TagSpecifications (the IAM policy keys
//     RunInstances and Stop/Start/Reboot/Terminate on it), sets
//     InstanceInitiatedShutdownBehavior=stop so a guest poweroff stops
//     rather than terminates the worker, and never touches security
//     groups (the default SG is egress-only; the worker dials out),
//     volumes (no CreateVolume/AttachVolume) or spot (no market
//     options).
//   - Create runs TWO money preflights plus a quota preflight
//     (ec2budget.go): the hourly session model against
//     dhnt-monthly-cap, the session cost against dhnt-gpu-daily, and
//     the type's vCPUs against the Running On-Demand G quota. All
//     three fail closed.
//   - A start that answers InsufficientInstanceCapacity is reported
//     with its AZ: the worker's gp3 volume is AZ-bound, so the adapter
//     never recreates the worker elsewhere to route around it.
//
// The adapter is pinned to us-west-2: the region, the AZ default, the
// spec region and the Service Quotas scope are all that region, and
// anything else is refused before any API call.
package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// EC2Name is the provider name in the EC2 adapter's manifest. It differs
// from the Lightsail adapter's Name so the two can be registered side
// by side in one dispatcher.
const EC2Name = "aws-ec2-gpu"

// EC2Region is the only region the EC2 adapter operates in: the one the
// Sprint 546 automation principal is allowed to act in.
const EC2Region = "us-west-2"

// ServiceEC2 is the SigV4 service name for EC2.
const ServiceEC2 = "ec2"

// ec2APIVersion is the EC2 API version the query protocol requires.
const ec2APIVersion = "2016-11-15"

// ServiceServiceQuotas is the SigV4 service name for Service Quotas.
const ServiceServiceQuotas = "servicequotas"

// serviceQuotasTargetPrefix is the Service Quotas X-Amz-Target prefix.
const serviceQuotasTargetPrefix = "ServiceQuotasV20190624."

// QuotaRunningOnDemandG is the Service Quotas code for "Running
// On-Demand G and VT instances", the quota class G-type workers draw
// from. It was 0 on 2026-10-30 with an 8-vCPU increase PENDING, which
// is why the quota preflight exists: a create past it would fail at
// RunInstances time instead of being refused up front.
const QuotaRunningOnDemandG = "L-DB2E81BA"

// ec2Endpoint is the regional EC2 endpoint for a region.
func ec2Endpoint(region string) string {
	return "https://ec2." + region + ".amazonaws.com/"
}

// serviceQuotasEndpoint is the regional Service Quotas endpoint. Quotas
// are per-region, so this is the worker's region, not us-east-1.
func serviceQuotasEndpoint(region string) string {
	return "https://service-quotas." + region + ".amazonaws.com/"
}

// ---- GPU catalog (operator-reviewed 2026-10-30, us-west-2) ----

// gpuType is one allowlisted worker shape: its on-demand hourly price,
// the vCPUs its quota draw costs, and the device for capacity offers.
type gpuType struct {
	hourlyUSD float64
	vcpu      int
	ramGB     float64
	accel     provider.Accelerator
}

// gpuTypes is the dhnt-automation-ec2-gpu allowlist. g6.2xlarge is
// priced at 2x g6.xlarge (same L4 device, double the vCPU/RAM); the
// others are the reviewed us-west-2 on-demand rates.
var gpuTypes = map[string]gpuType{
	"g6.xlarge": {
		hourlyUSD: 0.805, vcpu: 4, ramGB: 16,
		accel: provider.Accelerator{Vendor: "NVIDIA", Model: "L4", MemoryBytes: 24 * provider.GiB},
	},
	"g6.2xlarge": {
		hourlyUSD: 1.61, vcpu: 8, ramGB: 32,
		accel: provider.Accelerator{Vendor: "NVIDIA", Model: "L4", MemoryBytes: 24 * provider.GiB},
	},
	"g6e.xlarge": {
		hourlyUSD: 1.861, vcpu: 4, ramGB: 16,
		accel: provider.Accelerator{Vendor: "NVIDIA", Model: "L40S", MemoryBytes: 48 * provider.GiB},
	},
	"g5.xlarge": {
		hourlyUSD: 1.006, vcpu: 4, ramGB: 16,
		accel: provider.Accelerator{Vendor: "NVIDIA", Model: "A10G", MemoryBytes: 24 * provider.GiB},
	},
}

// gpuTypeNames lists the allowlist in a stable order for messages.
func gpuTypeNames() []string {
	out := make([]string, 0, len(gpuTypes))
	for n := range gpuTypes {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// EBS and session pricing facts for the plan (operator-reviewed
// 2026-10-30, us-west-2).
const (
	// EBSGBMonthUSD is the gp3 price per GB-month.
	EBSGBMonthUSD = 0.08
	// ThroughputBaselineMBs is the gp3 throughput every volume gets
	// for free.
	ThroughputBaselineMBs = 125
	// ThroughputMBsMonthUSD is the price per provisioned MB/s-month
	// ABOVE the baseline, billed even while the worker is stopped.
	ThroughputMBsMonthUSD = 0.04
	// PublicIPv4HourUSD is the auto-assigned public IPv4 charge,
	// billed only while the worker runs.
	PublicIPv4HourUSD = 0.005
)

// Worker disk and session defaults.
const (
	// DefaultVolumeSizeGB is the gp3 root volume at launch. Models
	// live on the worker's own volume (the guard grants no
	// CreateVolume/AttachVolume), so this is room for weights plus
	// the runner, within the 200 GB IAM ceiling.
	DefaultVolumeSizeGB = 100
	// MaxVolumeSizeGB is the IAM ceiling: volumes above it are
	// refused before any API call.
	MaxVolumeSizeGB = 200
	// DefaultThroughputMBs is the gp3 baseline: no surcharge.
	DefaultThroughputMBs = ThroughputBaselineMBs
	// MaxThroughputMBs is the gp3 ceiling the plan prices (1,000
	// MB/s at ~+$35/mo).
	MaxThroughputMBs = 1000
	// DefaultSessionHours is the session window the hourly preflight
	// prices: rate x hours plus the EBS month.
	DefaultSessionHours = 8.0
	// DefaultGPUMonthlyCapUSD is the monthly cap when neither
	// --budget-cap nor the dhnt-monthly-cap budget's own limit names
	// one. The Lightsail $24/mo per-instance ceiling is NOT applied
	// to GPU workers: every GPU type fails it, so the hourly model
	// replaces it.
	DefaultGPUMonthlyCapUSD = 400.0
	// DailyBudgetName is the AWS Budget the daily preflight reads.
	DailyBudgetName = "dhnt-gpu-daily"
)

// ebsMonthly prices one gp3 volume for a month: size plus provisioned
// throughput above the 125 MB/s baseline.
func ebsMonthly(volumeGB, throughputMBs int) float64 {
	m := float64(volumeGB) * EBSGBMonthUSD
	if extra := throughputMBs - ThroughputBaselineMBs; extra > 0 {
		m += float64(extra) * ThroughputMBsMonthUSD
	}
	return m
}

// ---- config and construction ----

// EC2Config configures a GPU worker adapter. It embeds the Lightsail
// Config for everything the two adapters share (credentials, owner,
// the Cost Explorer / Budgets / STS / S3 endpoints, the user-data
// staging bucket, the spend policy, governance tags, HTTP and polling
// behavior) and adds what only the GPU path needs.
//
// Inherited fields the EC2 adapter does NOT use: BaseURL (the
// Lightsail endpoint), Ports and SkipFirewall (no security-group
// writes: the default SG stays egress-only), SkipStaticIP (EC2 has no
// static-IP step; the auto-assigned public IPv4 only lives while the
// worker runs).
type EC2Config struct {
	Config
	// EC2URL overrides the EC2 endpoint (tests point it at fakeec2).
	// Empty means the regional endpoint for the pinned region.
	EC2URL string
	// ServiceQuotasURL overrides the Service Quotas endpoint
	// (tests). Empty means the regional endpoint.
	ServiceQuotasURL string
	// QuotaCode is the On-Demand quota the preflight reads. Empty
	// means QuotaRunningOnDemandG.
	QuotaCode string
	// DailyBudget is the AWS Budget the daily preflight reads.
	// Empty means DailyBudgetName.
	DailyBudget string
	// SessionHours is the session window the hourly preflight
	// prices. Zero means DefaultSessionHours.
	SessionHours float64
	// VolumeSizeGB is the gp3 root volume at launch, 1..200. Zero
	// means DefaultVolumeSizeGB.
	VolumeSizeGB int
	// ThroughputMBs is the provisioned gp3 throughput, 125..1000.
	// Zero means DefaultThroughputMBs.
	ThroughputMBs int
}

// gpuParams are the resolved EC2-only settings.
type gpuParams struct {
	quotaCode   string
	dailyBudget string
	hours       float64
	volumeGB    int
	throughput  int
}

// EC2Manifest is the GPU adapter's manifest. Only the seven worker
// operations are declared: resize is refused (changing type on a
// stopped worker is a new worker: the IAM policy grants no
// ModifyInstanceAttribute) and so is reboot (stop/start covers it),
// by Guard before any API call.
func EC2Manifest() provider.Manifest {
	return provider.Manifest{
		Name:            EC2Name,
		Kind:            provider.KindInfra,
		Form:            provider.FormBuiltin,
		ProtocolVersion: provider.ProtocolVersion,
		Summary:         "AWS EC2 GPU workers in us-west-2 over the EC2 query API (stdlib SigV4, hourly budget + daily + quota preflights)",
		Capabilities: []provider.Capability{
			provider.Capability(provider.OpValidate),
			provider.Capability(provider.OpCapacity),
			provider.Capability(provider.OpPlan),
			provider.Capability(provider.OpCreate),
			provider.Capability(provider.OpInspect),
			provider.Capability(provider.OpList),
			provider.Capability(provider.OpStart),
			provider.Capability(provider.OpStop),
			provider.Capability(provider.OpDestroy),
		},
	}
}

// NewEC2 returns the GPU worker adapter, wrapped in provider.Guard.
func NewEC2(cfg EC2Config) (provider.Adapter, error) {
	base, err := newAdapter(cfg.Config)
	if err != nil {
		return nil, err
	}
	if base.cfg.Region != EC2Region {
		return nil, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: this adapter is pinned to region %s, not %s", EC2Region, base.cfg.Region)}
	}
	g := gpuParams{
		quotaCode:   strings.TrimSpace(cfg.QuotaCode),
		dailyBudget: strings.TrimSpace(cfg.DailyBudget),
		hours:       cfg.SessionHours,
		volumeGB:    cfg.VolumeSizeGB,
		throughput:  cfg.ThroughputMBs,
	}
	if g.quotaCode == "" {
		g.quotaCode = QuotaRunningOnDemandG
	}
	if g.dailyBudget == "" {
		g.dailyBudget = DailyBudgetName
	}
	if g.hours <= 0 {
		g.hours = DefaultSessionHours
	}
	if g.volumeGB <= 0 {
		g.volumeGB = DefaultVolumeSizeGB
	}
	if g.throughput <= 0 {
		g.throughput = DefaultThroughputMBs
	}
	if g.volumeGB < 1 || g.volumeGB > MaxVolumeSizeGB {
		return nil, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: volume size %d GB is outside 1..%d (the IAM policy caps volumes at %d GB)",
				g.volumeGB, MaxVolumeSizeGB, MaxVolumeSizeGB)}
	}
	if g.throughput < ThroughputBaselineMBs || g.throughput > MaxThroughputMBs {
		return nil, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: throughput %d MB/s is outside %d..%d",
				g.throughput, ThroughputBaselineMBs, MaxThroughputMBs)}
	}
	if g.hours <= 0 || g.hours > 24*31 {
		return nil, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: session hours %.1f is not a plausible session window", g.hours)}
	}
	ec2URL := strings.TrimSpace(cfg.EC2URL)
	if ec2URL == "" {
		ec2URL = ec2Endpoint(base.cfg.Region)
	}
	sqURL := strings.TrimSpace(cfg.ServiceQuotasURL)
	if sqURL == "" {
		sqURL = serviceQuotasEndpoint(base.cfg.Region)
	}
	for _, u := range []string{ec2URL, sqURL} {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return nil, &provider.Error{Code: provider.ErrInvalidRequest, Message: fmt.Sprintf("aws-ec2-gpu: invalid endpoint %q", u)}
		}
	}
	e := &ec2Adapter{adapter: base, gpu: g}
	e.ec2 = &queryClient{
		endpoint: ec2URL, region: base.cfg.Region, service: ServiceEC2,
		version: ec2APIVersion, creds: base.cfg.Credentials,
		httpClient: base.cfg.HTTPClient, now: base.now, parseErr: ec2Error,
	}
	e.squota = &client{
		endpoint: sqURL, region: base.cfg.Region, service: ServiceServiceQuotas,
		prefix: serviceQuotasTargetPrefix, creds: base.cfg.Credentials,
		httpClient: base.cfg.HTTPClient, now: base.now,
	}
	return provider.Guard(e), nil
}

// ec2Adapter is the GPU worker adapter. It embeds the Lightsail
// adapter for everything shared — credentials, owner and governance
// tags, the Budgets-first spend ladder, STS account lookup and S3
// user-data staging — and adds the EC2 and Service Quotas clients.
type ec2Adapter struct {
	*adapter
	ec2    *queryClient
	squota *client
	gpu    gpuParams
}

func (e *ec2Adapter) Manifest() provider.Manifest { return EC2Manifest() }

func (e *ec2Adapter) Do(ctx context.Context, req provider.Request) (provider.Result, error) {
	switch req.Op {
	case provider.OpValidate:
		return e.validate(ctx, req)
	case provider.OpCapacity:
		return e.capacity(ctx, req)
	case provider.OpPlan:
		return e.plan(ctx, req)
	case provider.OpCreate:
		return e.create(ctx, req)
	case provider.OpInspect:
		in, res, err := e.owned(ctx, req)
		if err != nil {
			return res, err
		}
		return ok(req.Op, &provider.Result{Resource: e.resource(in)})
	case provider.OpList:
		return e.list(ctx, req)
	case provider.OpStart:
		return e.power(ctx, req, "StartInstances", "running")
	case provider.OpStop:
		return e.power(ctx, req, "StopInstances", "stopped")
	case provider.OpDestroy:
		return e.destroy(ctx, req)
	}
	return provider.Failure(req.Op, provider.ErrUnsupportedCapability, "aws-ec2-gpu: operation %q", req.Op)
}

// ---- EC2 wire types (only the fields this adapter reads) ----

// ec2Tag is one EC2 resource tag. Unlike Lightsail's, the value is a
// nested element, and a tag with no value carries none.
type ec2Tag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

type ec2State struct {
	Code int    `xml:"code"`
	Name string `xml:"name"`
}

type ec2Placement struct {
	AvailabilityZone string `xml:"availabilityZone"`
}

// ec2Instance is one EC2 instance as DescribeInstances reports it.
type ec2Instance struct {
	ID               string       `xml:"instanceId"`
	ImageID          string       `xml:"imageId"`
	Type             string       `xml:"instanceType"`
	State            ec2State     `xml:"instanceState"`
	PrivateDNS       string       `xml:"privateDnsName"`
	PublicIP         string       `xml:"ipAddress"`
	PrivateIP        string       `xml:"privateIpAddress"`
	Placement        ec2Placement `xml:"placement"`
	ShutdownBehavior string       `xml:"instanceInitiatedShutdownBehavior"`
	LaunchTime       string       `xml:"launchTime"`
	Tags             []ec2Tag     `xml:"tagSet>item"`
}

type ec2Reservation struct {
	ID        string        `xml:"reservationId"`
	Instances []ec2Instance `xml:"instancesSet>item"`
}

type describeInstancesResp struct {
	XMLName      xml.Name         `xml:"DescribeInstancesResponse"`
	Reservations []ec2Reservation `xml:"reservationSet>item"`
	NextToken    string           `xml:"nextToken"`
}

type runInstancesResp struct {
	XMLName   xml.Name      `xml:"RunInstancesResponse"`
	Instances []ec2Instance `xml:"instancesSet>item"`
}

// ec2StateChange is one instance in a Start/Stop/Terminate answer.
type ec2StateChange struct {
	ID       string   `xml:"instanceId"`
	Current  ec2State `xml:"currentState"`
	Previous ec2State `xml:"previousState"`
}

type powerResp struct {
	XMLName xml.Name         `xml:"-"`
	Changes []ec2StateChange `xml:"instancesSet>item"`
}

// ec2Error reads EC2's XML error envelope. EC2 nests the error one
// level deeper than STS does (<Response><Errors><Error> rather than
// <ErrorResponse><Error>), which is why the shared query client calls
// this through its parseErr hook instead of queryError.
func ec2Error(body []byte) (typ, msg string) {
	var e struct {
		XMLName xml.Name `xml:"Response"`
		Errors  struct {
			Error struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Error"`
		} `xml:"Errors"`
	}
	if xml.Unmarshal(body, &e) != nil {
		return "", ""
	}
	return shortExceptionName(e.Errors.Error.Code), strings.TrimSpace(e.Errors.Error.Message)
}

// ec2StateName maps an EC2 state name onto the normalized state. EC2's
// own names are reported verbatim in Resource.RawStatus.
func ec2StateName(name string) provider.State {
	switch strings.ToLower(name) {
	case "pending":
		return provider.StatePending
	case "running":
		return provider.StateRunning
	case "stopping":
		// In flight, not yet resting: pending, not stopped.
		return provider.StatePending
	case "stopped":
		return provider.StateStopped
	case "shutting-down":
		return provider.StateDeleting
	case "terminated":
		return provider.StateDeleted
	}
	return provider.StateUnknown
}

// ---- request parameters ----

// addEC2Tags encodes flat contract tags as one TagSpecification: EC2
// addresses tags by (specification, resource type), so the instance
// and its volume each get the full set — including dhnt:managed=true
// on both, which the IAM policy requires before it will run, stop or
// terminate anything.
func addEC2Tags(p url.Values, n int, resourceType string, flat []string) {
	p.Set(fmt.Sprintf("TagSpecification.%d.ResourceType", n), resourceType)
	i := 0
	for _, s := range flat {
		k, v, _ := strings.Cut(s, "=")
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		i++
		p.Set(fmt.Sprintf("TagSpecification.%d.Tag.%d.Key", n, i), k)
		// EC2 accepts an empty tag value; emitting Value always
		// keeps key-only lease markers encodable.
		p.Set(fmt.Sprintf("TagSpecification.%d.Tag.%d.Value", n, i), v)
	}
}

// resolveGPUSpec validates the create/plan spec: the region must be the
// pinned one, the size must be on the allowlist, and the image must be
// an AMI id. Everything decidable without asking AWS is decided here,
// so a refused spec costs no API calls.
func (e *ec2Adapter) resolveGPUSpec(req provider.Request) (gpuType, *provider.Error) {
	s := req.Spec
	if s == nil || s.Region == "" || s.Size == "" || s.Image == "" {
		return gpuType{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: %s needs spec.region (%s), spec.size (one of %s) and spec.image (an AMI id)",
				req.Op, EC2Region, strings.Join(gpuTypeNames(), ", "))}
	}
	if s.Region != EC2Region {
		return gpuType{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: this adapter is pinned to region %s; spec.region %q would need an adapter opened for that region",
				EC2Region, s.Region)}
	}
	t, ok := gpuTypes[s.Size]
	if !ok {
		return gpuType{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: instance type %q is not on the dhnt-automation-ec2-gpu allowlist (%s)",
				s.Size, strings.Join(gpuTypeNames(), ", "))}
	}
	if !strings.HasPrefix(s.Image, "ami-") {
		return gpuType{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws-ec2-gpu: spec.image %q is not an AMI id", s.Image)}
	}
	return t, nil
}

// ---- EC2 reads ----

// describeOne returns the instance with id, or a normalized not_found
// when EC2 has no such instance. No format check precedes the call: an
// id EC2 will not shape-match names no instance either.
func (e *ec2Adapter) describeOne(ctx context.Context, id string) (*ec2Instance, error) {
	p := url.Values{}
	p.Set("InstanceId.1", id)
	var out describeInstancesResp
	if err := e.ec2.callWith(ctx, "DescribeInstances", p, &out); err != nil {
		return nil, err
	}
	for _, r := range out.Reservations {
		for i := range r.Instances {
			return &r.Instances[i], nil
		}
	}
	return nil, &provider.Error{Code: provider.ErrNotFound, Message: "aws-ec2-gpu: instance " + id + " not found"}
}

// describeAll pages through DescribeInstances. There is no server-side
// tag filter on this path on purpose: the worker fleet is small, and
// ownership is applied by the caller from the tags, the same way the
// Lightsail adapter does it.
func (e *ec2Adapter) describeAll(ctx context.Context) ([]ec2Instance, error) {
	var all []ec2Instance
	token := ""
	for page := 0; page < 100; page++ {
		p := url.Values{}
		p.Set("MaxResults", "100")
		if token != "" {
			p.Set("NextToken", token)
		}
		var out describeInstancesResp
		if err := e.ec2.callWith(ctx, "DescribeInstances", p, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Reservations {
			all = append(all, r.Instances...)
		}
		if out.NextToken == "" {
			return all, nil
		}
		token = out.NextToken
	}
	return all, &provider.Error{Code: provider.ErrProvider, Message: "aws-ec2-gpu: DescribeInstances did not terminate"}
}

// decodeEC2Tags converts EC2 tags back to flat contract strings.
func decodeEC2Tags(tags []ec2Tag) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t.Key == "" {
			continue
		}
		if t.Value == "" {
			out = append(out, t.Key)
			continue
		}
		out = append(out, t.Key+"="+t.Value)
	}
	return out
}

// nameTag reads the Name tag EC2 consoles show.
func nameTag(tags []ec2Tag) string {
	for _, t := range tags {
		if t.Key == "Name" {
			return t.Value
		}
	}
	return ""
}

// resource maps an EC2 instance onto a contract resource.
func (e *ec2Adapter) resource(in *ec2Instance) *provider.Resource {
	r := &provider.Resource{
		ID:        in.ID,
		Name:      nameTag(in.Tags),
		State:     ec2StateName(in.State.Name),
		RawStatus: in.State.Name,
		Region:    regionOf(in.Placement.AvailabilityZone),
		Size:      in.Type,
		Tags:      decodeEC2Tags(in.Tags),
	}
	if r.Name == "" {
		r.Name = r.ID
	}
	if r.Region == "" {
		r.Region = EC2Region
	}
	if in.PublicIP != "" {
		r.PublicIPs = append(r.PublicIPs, in.PublicIP)
	}
	if t, err := time.Parse(time.RFC3339, in.LaunchTime); err == nil {
		r.CreatedAt = t.UTC()
	}
	return r
}

// restingEC2 reports whether an EC2 state name is a resting state.
func restingEC2(name string) bool {
	switch strings.ToLower(name) {
	case "running", "stopped", "terminated":
		return true
	}
	return false
}

// waitEC2State polls DescribeInstances until the instance reports
// want, or the wait budget runs out. tolerateMissing keeps waiting
// while the id is not yet visible, which a just-launched instance is
// until EC2 has propagated it.
func (e *ec2Adapter) waitEC2State(ctx context.Context, id, want string, opts ...waitOption) (*ec2Instance, error) {
	deadline := e.now().Add(e.cfg.WaitTimeout)
	missingOK := slices.Contains(opts, tolerateMissing)
	unknownOK := slices.Contains(opts, tolerateUnknown)
	for {
		in, err := e.describeOne(ctx, id)
		if err != nil {
			if !missingOK || normalize(err).Code != provider.ErrNotFound {
				return nil, err
			}
			in = &ec2Instance{ID: id, State: ec2State{Name: "pending"}}
		}
		st := ec2StateName(in.State.Name)
		inFlight := !restingEC2(in.State.Name) || (unknownOK && st == provider.StateUnknown)
		switch {
		case want == anyRestingState:
			if !inFlight {
				return in, nil
			}
		case strings.EqualFold(in.State.Name, want):
			return in, nil
		}
		if !e.now().Before(deadline) {
			target := want
			if target == anyRestingState {
				target = "a resting state"
			}
			return nil, &provider.Error{Code: provider.ErrTimeout, Retryable: true,
				Message: fmt.Sprintf("aws-ec2-gpu: instance %s is %q after waiting %s for %s", id, in.State.Name, e.cfg.WaitTimeout, target)}
		}
		select {
		case <-ctx.Done():
			return nil, ctxErr(ctx)
		case <-time.After(e.cfg.PollInterval):
		}
	}
}

// ---- operations ----

func (e *ec2Adapter) validate(ctx context.Context, req provider.Request) (provider.Result, error) {
	// DescribeInstances is the cheapest EC2 read this adapter is
	// certain to be allowed: every other operation needs it, so it
	// proves the credential without assuming anything else.
	p := url.Values{}
	p.Set("MaxResults", "5")
	var out describeInstancesResp
	if err := e.ec2.callWith(ctx, "DescribeInstances", p, &out); err != nil {
		return fail(req.Op, err)
	}
	n := 0
	for _, r := range out.Reservations {
		n += len(r.Instances)
	}
	data, _ := json.Marshal(map[string]any{
		"region":            EC2Region,
		"availability_zone": e.cfg.AvailabilityZone,
		"instances_visible": n,
		"access_key_id":     redactKeyID(e.cfg.Credentials.AccessKeyID),
	})
	return ok(req.Op, &provider.Result{Data: data})
}

func (e *ec2Adapter) capacity(ctx context.Context, req provider.Request) (provider.Result, error) {
	at := e.now().UTC()
	offers := make([]provider.Offer, 0, len(gpuTypes))
	quota, qerr := e.getServiceQuota(ctx, e.gpu.quotaCode)
	for _, name := range gpuTypeNames() {
		t := gpuTypes[name]
		o := provider.Offer{
			Region:      EC2Region,
			SKU:         name,
			Purchase:    provider.PurchaseOnDemand,
			CPU:         provider.CPU{Arch: ArchX8664, Count: t.vcpu},
			MemoryBytes: int64(t.ramGB * float64(provider.GiB)),
			Accelerators: []provider.Accelerator{
				{Vendor: t.accel.Vendor, Model: t.accel.Model, MemoryBytes: t.accel.MemoryBytes, Fraction: 1},
			},
			Storage: []provider.Storage{{
				Kind: "boot", SizeBytes: int64(e.gpu.volumeGB) * provider.GiB, Persistent: true,
			}},
			Network: provider.Network{Known: false},
			Price: provider.Price{
				Known: true, USDPerHour: t.hourlyUSD,
				USDPerMonth: t.hourlyUSD * 730, ObservedAt: at,
			},
			Availability:  provider.Availability{State: provider.AvailAvailable, ObservedAt: at},
			Qualification: provider.Qualification{Qualified: true},
			Lifecycle: provider.Lifecycle{
				StopKeepsBilling: true, StopKeepsDisk: true, ReleaseDeletes: true,
				Note: "a stopped worker bills no compute; its gp3 volume (and throughput above 125 MB/s) keeps billing monthly until terminated, which deletes the volume; the auto-assigned public IPv4 is released on stop and bills $0.005/h only while running",
			},
		}
		// The quota is per-offer headroom the catalog cannot show:
		// an offer the account cannot launch is reported, not
		// hidden, with the quota named.
		if qerr == nil && quota < float64(t.vcpu) {
			o.Availability = provider.Availability{State: provider.AvailUnavailable, ObservedAt: at,
				Reason: fmt.Sprintf("quota %s allows %.0f vCPUs, this type needs %d", e.gpu.quotaCode, quota, t.vcpu)}
		}
		offers = append(offers, o)
	}
	out := map[string]any{
		"region": EC2Region, "offers": offers,
		"quota_code": e.gpu.quotaCode,
	}
	if qerr == nil {
		out["quota_value"] = quota
	} else {
		out["quota_error"] = normalize(qerr).Message
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fail(req.Op, err)
	}
	return ok(req.Op, &provider.Result{Data: data})
}

// GPUPlan is the Data payload of a GPU plan result: what a create
// would do, what it would cost by the hour, and what the three
// preflights decide about it.
type GPUPlan struct {
	Provider         string   `json:"provider"`
	Name             string   `json:"name"`
	Region           string   `json:"region"`
	AvailabilityZone string   `json:"availability_zone"`
	InstanceType     string   `json:"instance_type"`
	ImageID          string   `json:"image_id"`
	Arch             string   `json:"arch"`
	Tags             []string `json:"tags"`
	PriceHourlyUSD   float64  `json:"price_hourly_usd"`
	SessionHours     float64  `json:"session_hours"`
	SessionCostUSD   float64  `json:"session_cost_usd"`
	VolumeGB         int      `json:"volume_gb"`
	VolumeType       string   `json:"volume_type"`
	ThroughputMBs    int      `json:"throughput_mbs"`
	EBSMonthlyUSD    float64  `json:"ebs_monthly_usd"`
	// PublicIPv4HourlyUSD is billed only while the worker runs; the
	// address is released on stop, so a stopped worker pays none of it.
	PublicIPv4HourlyUSD float64      `json:"public_ipv4_hourly_usd"`
	ShutdownBehavior    string       `json:"shutdown_behavior"`
	Temporary           bool         `json:"temporary"`
	UserDataBytes       int          `json:"user_data_bytes"`
	UserDataWrapped     bool         `json:"user_data_wrapped"`
	UserDataStaged      bool         `json:"user_data_staged,omitempty"`
	UserDataBucket      string       `json:"user_data_bucket,omitempty"`
	UserDataScriptLen   int          `json:"user_data_script_bytes,omitempty"`
	UserDataSHA256      string       `json:"user_data_sha256,omitempty"`
	Preflight           GPUPreflight `json:"preflight"`
}

func (e *ec2Adapter) plan(ctx context.Context, req provider.Request) (provider.Result, error) {
	t, pe := e.resolveGPUSpec(req)
	if pe != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: pe}, pe
	}
	name := instanceName(req)
	// A plan STAGES NOTHING: it renders what the create would send,
	// so a --dry-run reports the real inline size and the bucket it
	// would write to without putting an object there.
	ud, err := e.prepareUserData(ctx, name, req.Spec.UserData, false)
	if err != nil {
		return fail(req.Op, err)
	}
	p := GPUPlan{
		Provider: EC2Name, Name: name,
		Region: EC2Region, AvailabilityZone: e.cfg.AvailabilityZone,
		InstanceType: req.Spec.Size, ImageID: req.Spec.Image, Arch: ArchX8664,
		Tags:                e.createTags(req, ud.Key),
		PriceHourlyUSD:      t.hourlyUSD,
		SessionHours:        e.gpu.hours,
		SessionCostUSD:      t.hourlyUSD * e.gpu.hours,
		VolumeGB:            e.gpu.volumeGB,
		VolumeType:          "gp3",
		ThroughputMBs:       e.gpu.throughput,
		EBSMonthlyUSD:       ebsMonthly(e.gpu.volumeGB, e.gpu.throughput),
		PublicIPv4HourlyUSD: PublicIPv4HourUSD,
		ShutdownBehavior:    "stop",
		Temporary:           req.Authorization != nil,
		UserDataBytes:       len(ud.Inline),
		UserDataWrapped:     Wrapped(ud.Inline),
		UserDataStaged:      ud.Staged(),
		UserDataBucket:      ud.Bucket,
	}
	if ud.Staged() {
		p.UserDataScriptLen = len(req.Spec.UserData)
		p.UserDataSHA256 = ud.SHA256
	}
	// The plan is where --dry-run learns the preflight verdicts, so
	// run the same three the create will run; they allocate nothing.
	v, be := e.gpuPreflight(ctx, t, req.Spec.Size)
	p.Preflight = v
	data, merr := json.Marshal(p)
	if merr != nil {
		return fail(req.Op, merr)
	}
	if be != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Data: data, Error: be}, be
	}
	return ok(req.Op, &provider.Result{Data: data})
}

// create is idempotent on req.IdempotencyKey: an existing worker tagged
// with the key is returned instead of launching another. A create whose
// answer may have been lost carries ClientToken=key AND is reconciled
// by that tag before the failure is reported, so at most one worker
// results.
func (e *ec2Adapter) create(ctx context.Context, req provider.Request) (provider.Result, error) {
	name := instanceName(req)
	if existing, err := e.findByKey(ctx, req.IdempotencyKey); err != nil {
		return fail(req.Op, err)
	} else if existing != nil {
		return e.finishCreate(ctx, req, existing.ID)
	}
	t, pe := e.resolveGPUSpec(req)
	if pe != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: pe}, pe
	}
	if _, be := e.gpuPreflight(ctx, t, req.Spec.Size); be != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: be}, be
	}
	// The user data is settled BEFORE RunInstances: a script too large
	// to go inline is staged in S3 here, so the request carries a
	// bootstrap that already has an object to fetch.
	ud, uerr := e.prepareUserData(ctx, name, req.Spec.UserData, true)
	if uerr != nil {
		return fail(req.Op, uerr)
	}
	p := url.Values{}
	p.Set("ImageId", req.Spec.Image)
	p.Set("InstanceType", req.Spec.Size)
	p.Set("MinCount", "1")
	p.Set("MaxCount", "1")
	p.Set("InstanceInitiatedShutdownBehavior", "stop")
	// The idempotency key rides as the EC2 client token AND as a tag:
	// the token dedupes a retried RunInstances, the tag reconciles a
	// lost answer.
	p.Set("ClientToken", req.IdempotencyKey)
	if len(ud.Inline) > 0 {
		// EC2 takes user data base64-encoded; the 16 KiB limit
		// applies to the raw bytes Fit already enforced.
		p.Set("UserData", base64.StdEncoding.EncodeToString(ud.Inline))
	}
	if len(req.Spec.SSHKeys) > 0 {
		p.Set("KeyName", req.Spec.SSHKeys[0])
	}
	tags := e.createTags(req, ud.Key)
	addEC2Tags(p, 1, "instance", append([]string{"Name=" + name}, tags...))
	addEC2Tags(p, 2, "volume", tags)
	// The models live on the worker's own gp3 volume: one root disk
	// at launch, deleted with the worker so no orphan bills. No
	// SubnetId and no SecurityGroupIds: the default VPC and its
	// egress-only default SG are exactly what an Outpost-dialing
	// worker needs, and the policy grants no SG writes.
	p.Set("BlockDeviceMapping.1.DeviceName", "/dev/sda1")
	p.Set("BlockDeviceMapping.1.Ebs.VolumeSize", fmt.Sprintf("%d", e.gpu.volumeGB))
	p.Set("BlockDeviceMapping.1.Ebs.VolumeType", "gp3")
	p.Set("BlockDeviceMapping.1.Ebs.Throughput", fmt.Sprintf("%d", e.gpu.throughput))
	p.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")
	// A public address for the dial-out; it lives only while the
	// worker runs ($0.005/h) and is released on stop.
	p.Set("NetworkInterface.1.DeviceIndex", "0")
	p.Set("NetworkInterface.1.AssociatePublicIpAddress", "true")
	var out runInstancesResp
	cerr := e.ec2.callWith(ctx, "RunInstances", p, &out)
	if cerr != nil {
		if !maybeApplied(cerr) || ctx.Err() != nil {
			return fail(req.Op, cerr)
		}
		// The launch may have taken effect with the answer lost:
		// reconcile by idempotency tag before reporting a failure.
		if found, ferr := e.findByKey(ctx, req.IdempotencyKey); ferr == nil && found != nil {
			return e.finishCreate(ctx, req, found.ID)
		}
		return fail(req.Op, cerr)
	}
	if len(out.Instances) == 0 || out.Instances[0].ID == "" {
		return fail(req.Op, fmt.Errorf("aws-ec2-gpu: RunInstances answered with no instance"))
	}
	return e.finishCreate(ctx, req, out.Instances[0].ID)
}

// finishCreate waits for "running" and reports the worker. A worker
// that never reaches it is a FAILED create, not a create to report.
func (e *ec2Adapter) finishCreate(ctx context.Context, req provider.Request, id string) (provider.Result, error) {
	in, err := e.waitEC2State(ctx, id, "running", tolerateMissing, tolerateUnknown)
	if err != nil {
		return fail(req.Op, err)
	}
	return ok(req.Op, &provider.Result{Resource: e.resource(in)})
}

func (e *ec2Adapter) list(ctx context.Context, req provider.Request) (provider.Result, error) {
	all, err := e.describeAll(ctx)
	if err != nil {
		return fail(req.Op, err)
	}
	out := []provider.Resource{}
	for i := range all {
		if provider.Owned(decodeEC2Tags(all[i].Tags), e.cfg.Owner) {
			out = append(out, *e.resource(&all[i]))
		}
	}
	return ok(req.Op, &provider.Result{Resources: out})
}

// owned fetches req.ResourceID and refuses unless it carries this
// owner's lease tags, so a foreign or untagged worker is never acted
// on.
func (e *ec2Adapter) owned(ctx context.Context, req provider.Request) (*ec2Instance, provider.Result, error) {
	in, err := e.describeOne(ctx, req.ResourceID)
	if err != nil {
		r, e := fail(req.Op, err)
		return nil, r, e
	}
	if !provider.Owned(decodeEC2Tags(in.Tags), e.cfg.Owner) {
		r, e := provider.Failure(req.Op, provider.ErrNotOwned,
			"aws-ec2-gpu: instance %s does not carry the %s and %s tags; refusing to act on it",
			req.ResourceID, provider.TagLease, provider.OwnerTag(e.cfg.Owner))
		return nil, r, e
	}
	return in, provider.Result{}, nil
}

// findByKey returns this owner's worker launched under key, if any. A
// terminated worker does NOT satisfy the key: the create already
// ended, and a replay must launch a new worker rather than report a
// dead one. Two live matches is a reconcile case, never a guess.
func (e *ec2Adapter) findByKey(ctx context.Context, key string) (*ec2Instance, error) {
	all, err := e.describeAll(ctx)
	if err != nil {
		return nil, err
	}
	tag := provider.IdempotencyTag(key)
	var mine []ec2Instance
	for _, in := range all {
		if st := ec2StateName(in.State.Name); st == provider.StateDeleted || st == provider.StateDeleting {
			continue
		}
		tags := decodeEC2Tags(in.Tags)
		if provider.Owned(tags, e.cfg.Owner) && slices.Contains(tags, tag) {
			mine = append(mine, in)
		}
	}
	switch len(mine) {
	case 0:
		return nil, nil
	case 1:
		return &mine[0], nil
	}
	ids := make([]string, len(mine))
	for i, in := range mine {
		ids[i] = in.ID
	}
	return nil, &provider.Error{Code: provider.ErrNeedsManualReconcile,
		Message: fmt.Sprintf("aws-ec2-gpu: idempotency key matches %d instances (%s)", len(mine), strings.Join(ids, ", "))}
}

// power runs a start/stop and waits for the resting state. A start/stop
// already in its target state is a replay and issues nothing; a call
// already in flight is waited out rather than re-issued (a second
// StartInstances against a pending worker is an IncorrectState).
func (e *ec2Adapter) power(ctx context.Context, req provider.Request, action, want string) (provider.Result, error) {
	in, res, err := e.owned(ctx, req)
	if err != nil {
		return res, err
	}
	// A transitional state for the same destination means someone
	// else's call is already doing it: wait it out.
	inFlight := (want == "running" && in.State.Name == "pending") ||
		(want == "stopped" && in.State.Name == "stopping")
	if !inFlight && !strings.EqualFold(in.State.Name, want) {
		p := url.Values{}
		p.Set("InstanceId.1", in.ID)
		var out powerResp
		if err := e.ec2.callWith(ctx, action, p, &out); err != nil {
			if action == "StartInstances" {
				if ce := e.capacityErr(ctx, in, err); ce != nil {
					return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: ce}, ce
				}
			}
			return fail(req.Op, err)
		}
		if in, err = e.waitEC2State(ctx, in.ID, want); err != nil {
			return fail(req.Op, err)
		}
	} else if inFlight {
		var werr error
		if in, werr = e.waitEC2State(ctx, in.ID, want); werr != nil {
			return fail(req.Op, werr)
		}
	}
	r := &provider.Result{Resource: e.resource(in)}
	if req.Op == provider.OpStop {
		// A stopped worker bills no compute, but the gp3 volume
		// (and throughput above 125 MB/s) keeps its monthly
		// charge until terminated — and the public IPv4 is gone,
		// so the next start dials out from a new address.
		ebs := ebsMonthly(e.gpu.volumeGB, e.gpu.throughput)
		r.Cost = &provider.CostReport{
			Ongoing:  true,
			Retained: []string{"instance:" + in.ID, fmt.Sprintf("ebs:%dGB-gp3", e.gpu.volumeGB)},
			Note: fmt.Sprintf("a stopped worker bills no GPU compute; its %d GB gp3 volume keeps billing $%.2f/mo until terminated, and its auto-assigned public IPv4 was released (a new address is assigned on start)",
				e.gpu.volumeGB, ebs),
		}
	}
	return ok(req.Op, r)
}

// capacityErr reports an InsufficientInstanceCapacity start failure
// with its AZ. The worker's gp3 volume is AZ-bound to the instance's
// placement, so this is a report, never a recreation elsewhere.
func (e *ec2Adapter) capacityErr(ctx context.Context, in *ec2Instance, err error) *provider.Error {
	pe := normalize(err)
	if !strings.Contains(pe.Message, "InsufficientInstanceCapacity") {
		return nil
	}
	az := in.Placement.AvailabilityZone
	if az == "" {
		az = e.cfg.AvailabilityZone
	}
	return &provider.Error{Code: provider.ErrProvider, Retryable: true, Message: fmt.Sprintf(
		"aws-ec2-gpu: cannot start worker %s in %s: InsufficientInstanceCapacity; its gp3 volume is AZ-bound to %s, so the worker stays stopped here instead of being recreated elsewhere",
		in.ID, az, az)}
}

// StepTerminateInstance is the EC2 destroy step name, as it appears in
// DestroyReport.
const StepTerminateInstance = "terminate-instance"

// destroy terminates a worker this owner launched, after the A6 check
// and the dhnt:managed gate, then deletes its staged first-boot
// script.
//
// The managed gate is checked HERE rather than left to AWS: the
// automation IAM policy permits TerminateInstances only on resources
// tagged dhnt:managed=true, so without this check an untagged worker
// would fail as an opaque UnauthorizedOperation instead of saying
// what is wrong.
//
// Terminate deletes the worker's gp3 volume with it
// (DeleteOnTermination at launch), so unlike the Lightsail destroy
// there is no address or disk left billing afterwards — only the
// staged S3 object, which the bucket's one-day lifecycle rule
// backstops.
//
// A worker that is ALREADY GONE is not an error: destroy is a cleanup
// verb. See destroyGone.
func (e *ec2Adapter) destroy(ctx context.Context, req provider.Request) (provider.Result, error) {
	in, res, err := e.owned(ctx, req)
	if err != nil {
		if normalize(err).Code == provider.ErrNotFound {
			return e.destroyGone(ctx, req)
		}
		return res, err
	}
	r := e.resource(in)
	if er := provider.AuthorizeDestroy(req, *r); er != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: er, Cost: provider.RefusedDestroyCost()}, er
	}
	if !Managed(r.Tags) {
		er := &provider.Error{Code: provider.ErrNotOwned, Message: fmt.Sprintf(
			"aws-ec2-gpu: instance %s is not tagged %s; the automation IAM policy permits terminating only tagged workers, so this destroy would be denied by AWS",
			in.ID, ManagedTag)}
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: er, Cost: provider.RefusedDestroyCost()}, er
	}

	p := url.Values{}
	p.Set("InstanceId.1", in.ID)
	var out powerResp
	if err := e.ec2.callWith(ctx, "TerminateInstances", p, &out); err != nil {
		return fail(req.Op, err)
	}
	if _, err := e.waitEC2State(ctx, in.ID, "terminated"); err != nil {
		return fail(req.Op, err)
	}
	r.State = provider.StateDeleted
	rep := DestroyReport{Instance: in.ID}
	rep.add(StepTerminateInstance, in.ID, StepDone, "terminated; its gp3 volume is deleted with it")
	cost := &provider.CostReport{}
	var notes []string

	switch key := UserDataKey(r.Tags); {
	case key == "":
		rep.add(StepDeleteStagedUserData, "", StepSkipped, "the first-boot script went inline; nothing was staged")
	default:
		if note := e.deleteStagedUserData(ctx, r.Tags); note != "" {
			notes = append(notes, note)
			rep.add(StepDeleteStagedUserData, key, StepFailed, note)
		} else {
			rep.add(StepDeleteStagedUserData, key, StepDone, "")
		}
	}

	cost.Note = strings.Join(notes, "; ")
	return ok(req.Op, &provider.Result{Resource: r, Cost: cost, Data: rep.data()})
}

// destroyGone is the destroy of a worker that is not there: a repeat
// of a destroy that already ran, or a worker an operator terminated
// in the console. It reports success, because the worker is in the
// state the caller asked for.
//
// The A6 confirmation is still required: only the STATIC half of the
// check can run (the resource whose tags the temporary half would read
// is gone). There is no dhnt:managed gate here for the same reason,
// and no volume to account for: DeleteOnTermination took it with the
// worker. The staged object's tag went with the worker too, and the
// IAM policy grants no s3:ListBucket, so the bucket's one-day
// lifecycle rule is the backstop.
func (e *ec2Adapter) destroyGone(ctx context.Context, req provider.Request) (provider.Result, error) {
	if er := provider.CheckDestroyAuthorization(req); er != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Error: er, Cost: provider.RefusedDestroyCost()}, er
	}
	name := req.ResourceID
	rep := DestroyReport{Instance: name}
	rep.add(StepTerminateInstance, name, StepSkipped, "the worker is already gone")
	rep.add(StepDeleteStagedUserData, "", StepSkipped,
		"no staged-script key is recoverable without the worker's tags; the bucket's one-day lifecycle rule expires it")
	return ok(req.Op, &provider.Result{
		Resource: &provider.Resource{
			ID: name, Name: name, State: provider.StateDeleted, Region: EC2Region,
		},
		Cost: &provider.CostReport{},
		Data: rep.data(),
	})
}
