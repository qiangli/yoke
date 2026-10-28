// Package fakelightsail is a deterministic in-process stand-in for the AWS
// Lightsail and Cost Explorer JSON APIs, for testing pkg/provider/aws.
//
// It is a recorded-response server, not a mock: it speaks the real wire
// protocol (POST "/" with X-Amz-Target, application/x-amz-json-1.1), keeps
// real state so a create is visible to the next list, and — importantly —
// VERIFIES THE SIGV4 SIGNATURE of every request it answers. An adapter that
// signed the wrong canonical request, forgot a header or sent the wrong
// payload hash fails here rather than only against the live account.
//
// Instance states advance the way Lightsail's do: a create lands "pending"
// and reaches "running" on a later read, a stop goes through "stopping".
// That is what exercises the adapter's wait loops instead of letting a
// synchronous fake hide them.
package fakelightsail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider/aws"
)

// Test credentials. They are the AWS documentation's example key pair, so
// nothing here resembles a real secret.
const (
	TestAccessKeyID     = "AKIDEXAMPLE"
	TestSecretAccessKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	// TestRegion is what the fake pretends to be. It matches the region
	// the conformance suite's specs name.
	TestRegion = "r1"
	// TestBundleID and TestBlueprintID are what the conformance suite's
	// specs name (spec.size / spec.image).
	TestBundleID    = "s-1vcpu-1gb"
	TestBlueprintID = "img"
)

// TestCredentials is the key pair the fake accepts.
func TestCredentials() aws.Credentials {
	return aws.Credentials{AccessKeyID: TestAccessKeyID, SecretAccessKey: TestSecretAccessKey}
}

// Fault is an injected failure mode for one target.
type Fault int

const (
	// FaultLoseResponse performs the side effect and then answers 500:
	// the caller cannot tell whether the operation took effect.
	FaultLoseResponse Fault = iota + 1
	// FaultAccessDenied answers the IAM refusal AWS sends.
	FaultAccessDenied
	// FaultThrottle answers ThrottlingException.
	FaultThrottle
	// FaultServiceError answers a 500 ServiceException before any side
	// effect.
	FaultServiceError
	// FaultNotFound answers NotFoundException, the way a read of a
	// just-created resource does before AWS has propagated it.
	FaultNotFound
)

type fault struct {
	kind Fault
	n    int
}

// Instance is the fake's view of one instance.
type Instance struct {
	Name        string
	State       string
	BundleID    string
	BlueprintID string
	UserData    string
	Tags        []aws.Tag
	PublicIP    string
	StaticIP    bool
	Ports       []map[string]any
	CreatedAt   float64
}

// StaticIP is the fake's view of one static IP.
type StaticIP struct {
	Name       string
	IPAddress  string
	AttachedTo string
}

// Server is the fake. It answers both the Lightsail and the Cost Explorer
// targets, so one URL serves an adapter's whole surface.
type Server struct {
	*httptest.Server
	// URL is the endpoint to point Config.BaseURL and
	// Config.CostExplorerURL at.
	URL string

	mu        sync.Mutex
	instances map[string]*Instance
	staticIPs map[string]*StaticIP
	faults    map[string]*fault
	// MonthToDate is the unblended-cost-before-credits figure Cost
	// Explorer reports, in USD.
	monthToDate float64
	// bundlePrice is the monthly price GetBundles reports for
	// TestBundleID.
	bundles []map[string]any

	creates  int
	deletes  int
	requests int
	losses   int
	nextIP   int
	// LastCreate records the last CreateInstances request body, so a test
	// can assert on exactly what went on the wire.
	lastCreate map[string]any
	lastPorts  map[string]any
	badSigs    int
	// observers see every decoded request, so a test can assert on the
	// exact document that went on the wire for one target.
	observers []func(target string, body map[string]any)
	// scopes records the "<service>/<region>" each operation was signed
	// for, so a test can assert the signing scope directly.
	scopes map[string]string
}

// New starts a fake seeded with the standard catalog and closes it when
// the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		instances:   map[string]*Instance{},
		staticIPs:   map[string]*StaticIP{},
		faults:      map[string]*fault{},
		scopes:      map[string]string{},
		monthToDate: 3.50,
		nextIP:      10,
	}
	s.bundles = []map[string]any{
		{"bundleId": TestBundleID, "name": "Tiny", "instanceType": "tiny", "price": 5.0,
			"cpuCount": 1, "ramSizeInGb": 1.0, "diskSizeInGb": 40, "transferPerMonthInGb": 2000,
			"isActive": true, "power": 300, "supportedPlatforms": []string{"LINUX_UNIX"}, "publicIpv4AddressCount": 1},
		{"bundleId": "small_3_0", "name": "Small", "instanceType": "small", "price": 12.0,
			"cpuCount": 2, "ramSizeInGb": 2.0, "diskSizeInGb": 60, "transferPerMonthInGb": 3000,
			"isActive": true, "power": 500, "supportedPlatforms": []string{"LINUX_UNIX"}, "publicIpv4AddressCount": 1},
		{"bundleId": "xlarge_3_0", "name": "XLarge", "instanceType": "xlarge", "price": 80.0,
			"cpuCount": 8, "ramSizeInGb": 32.0, "diskSizeInGb": 640, "transferPerMonthInGb": 7000,
			"isActive": true, "power": 3000, "supportedPlatforms": []string{"LINUX_UNIX"}, "publicIpv4AddressCount": 1},
		{"bundleId": "gpu_nvidia_xl_1_0", "name": "GPU XL", "instanceType": "gpu_xl", "price": 20.0,
			"cpuCount": 8, "ramSizeInGb": 32.0, "diskSizeInGb": 640, "transferPerMonthInGb": 7000,
			"isActive": true, "power": 3000, "supportedPlatforms": []string{"LINUX_UNIX"}, "publicIpv4AddressCount": 1},
		{"bundleId": "retired_1_0", "name": "Retired", "instanceType": "retired", "price": 3.5,
			"cpuCount": 1, "ramSizeInGb": 0.5, "diskSizeInGb": 20, "transferPerMonthInGb": 1000,
			"isActive": false, "power": 200, "supportedPlatforms": []string{"LINUX_UNIX"}, "publicIpv4AddressCount": 1},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Server.Close)
	s.URL = s.Server.URL + "/"
	return s
}

// ---- controls ----

// SetMonthToDate sets the month-to-date spend Cost Explorer reports.
func (s *Server) SetMonthToDate(usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.monthToDate = usd
}

// Inject arms a fault on the next n calls to target (the bare operation
// name, e.g. "CreateInstances").
func (s *Server) Inject(target string, kind Fault, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[target] = &fault{kind: kind, n: n}
}

// AddInstance seeds an instance directly, counting no create.
func (s *Server) AddInstance(name, state string, tags ...string) *Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextIP++
	in := &Instance{
		Name: name, State: state, BundleID: TestBundleID, BlueprintID: TestBlueprintID,
		Tags: encode(tags), PublicIP: fmt.Sprintf("192.0.2.%d", s.nextIP),
		CreatedAt: float64(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()),
	}
	s.instances[name] = in
	return in
}

// Instance returns the fake's record for name.
func (s *Server) Instance(name string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return Instance{}, false
	}
	return *in, true
}

// StaticIP returns the fake's record for a static IP name.
func (s *Server) StaticIP(name string) (StaticIP, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ip, ok := s.staticIPs[name]
	if !ok {
		return StaticIP{}, false
	}
	return *ip, true
}

// Scope is the "<service>/<region>" credential scope the given operation
// was signed for, or "" if it was never called.
func (s *Server) Scope(op string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scopes[op]
}

// StaticIPNames lists every allocated static IP, sorted.
func (s *Server) StaticIPNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.staticIPs))
	for n := range s.staticIPs {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// StaticIPCount is how many static IPs are allocated.
func (s *Server) StaticIPCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.staticIPs) }

// Exists reports whether an instance is still stored.
func (s *Server) Exists(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.instances[name]
	return ok
}

// CreateCount, DeleteCount and RequestCount are the conformance counters.
func (s *Server) CreateCount() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.creates }
func (s *Server) DeleteCount() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.deletes }
func (s *Server) RequestCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.requests }

// Losses is how many injected lost-response faults fired.
func (s *Server) Losses() int { s.mu.Lock(); defer s.mu.Unlock(); return s.losses }

// BadSignatures is how many requests failed signature verification. A
// passing test must leave it at zero.
func (s *Server) BadSignatures() int { s.mu.Lock(); defer s.mu.Unlock(); return s.badSigs }

// LastCreate is the last CreateInstances request body.
func (s *Server) LastCreate() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCreate
}

// Observe registers a callback invoked with every decoded request body,
// keyed by the bare operation name. It runs before the operation.
func (s *Server) Observe(fn func(target string, body map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers = append(s.observers, fn)
}

// LastPorts is the last PutInstancePublicPorts request body.
func (s *Server) LastPorts() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPorts
}

func encode(flat []string) []aws.Tag {
	out := make([]aws.Tag, 0, len(flat))
	for _, f := range flat {
		k, v, _ := strings.Cut(f, "=")
		out = append(out, aws.Tag{Key: k, Value: v})
	}
	return out
}

// ---- serving ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// awsError answers the way an AWS JSON service does: HTTP 400 (or 500) with
// the exception type in both the header and the body.
func awsError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("X-Amzn-Errortype", typ+":http://internal.amazon.com/coral/com.amazon.lightsail/")
	writeJSON(w, status, map[string]string{"__type": typ, "code": typ, "message": msg, "docs": "", "tip": ""})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	_, op, _ := strings.Cut(target, ".")
	body, _ := readAll(r)

	if !s.verify(r, body, op) {
		s.mu.Lock()
		s.badSigs++
		s.mu.Unlock()
		awsError(w, http.StatusForbidden, "InvalidSignatureException",
			"the request signature we calculated does not match the signature you provided")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-amz-json-1.1" {
		awsError(w, http.StatusBadRequest, "InvalidInputException", "unexpected content type "+ct)
		return
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	var in map[string]any
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			awsError(w, http.StatusBadRequest, "InvalidInputException", err.Error())
			return
		}
	}

	s.mu.Lock()
	obs := slices.Clone(s.observers)
	s.mu.Unlock()
	for _, fn := range obs {
		fn(op, in)
	}

	// A pre-side-effect fault short-circuits the operation entirely.
	switch s.takeFault(op) {
	case FaultAccessDenied:
		awsError(w, http.StatusBadRequest, "AccessDeniedException",
			"User: arn:aws:iam::123456789012:user/dhnt-automation is not authorized to perform: lightsail:"+op)
		return
	case FaultThrottle:
		awsError(w, http.StatusBadRequest, "ThrottlingException", "Rate exceeded")
		return
	case FaultServiceError:
		awsError(w, http.StatusInternalServerError, "ServiceException", "injected service exception")
		return
	case FaultNotFound:
		awsError(w, http.StatusBadRequest, "NotFoundException", "injected: does not exist yet")
		return
	case FaultLoseResponse:
		// Handled below, after the side effect.
		s.mu.Lock()
		s.losses++
		s.mu.Unlock()
		s.dispatch(op, in, func(int, any) {})
		awsError(w, http.StatusInternalServerError, "ServiceException",
			"injected: side effect happened, answer lost")
		return
	}
	if !s.dispatch(op, in, func(status int, v any) { writeJSON(w, status, v) }) {
		awsError(w, http.StatusBadRequest, "InvalidInputException", "unknown target "+target)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// expectedScope is the SigV4 service and region an operation MUST be
// signed for. Real AWS derives these from the endpoint it is, and gets to
// reject a mismatch; one fake serves both services here, so it has to
// derive them from the target instead — and it does, because signing a
// Cost Explorer call with the instance's region (rather than Cost
// Explorer's global us-east-1) is one of the easiest things to get wrong
// and would otherwise only show up as a live SignatureDoesNotMatch.
func expectedScope(op string) (service, region string) {
	if op == "GetCostAndUsage" {
		return "ce", "us-east-1"
	}
	return "lightsail", TestRegion
}

// verify recomputes the SigV4 signature over the request as received and
// compares it with the Authorization header the adapter sent, and checks
// that the credential scope names the service and region the operation
// belongs to.
func (s *Server) verify(r *http.Request, body []byte, op string) bool {
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return false
	}
	amzDate := r.Header.Get("X-Amz-Date")
	ts, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return false
	}
	// Recover region and service from the credential scope the client
	// claimed, so a signature over the wrong scope still fails.
	i := strings.Index(authz, "Credential=")
	if i < 0 {
		return false
	}
	scope := authz[i+len("Credential="):]
	if j := strings.IndexByte(scope, ','); j >= 0 {
		scope = scope[:j]
	}
	parts := strings.Split(strings.TrimSpace(scope), "/")
	if len(parts) != 5 || parts[0] != TestAccessKeyID {
		return false
	}
	region, service := parts[2], parts[3]
	s.mu.Lock()
	s.scopes[op] = service + "/" + region
	s.mu.Unlock()
	if wantService, wantRegion := expectedScope(op); service != wantService || region != wantRegion {
		return false
	}

	// Rebuild the request as the client signed it: the signer excludes
	// the transport headers, so a faithful comparison just reuses it.
	clone := r.Clone(r.Context())
	clone.Header.Del("Authorization")
	clone.URL.Scheme = "https"
	if clone.URL.Host == "" {
		clone.URL.Host = r.Host
	}
	want := aws.AuthorizationHeader(clone, body, TestCredentials(), ts, region, service)
	return want == authz
}

// takeFault consumes one armed fault for op, if any.
func (s *Server) takeFault(op string) Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.faults[op]
	if !ok || f.n <= 0 {
		return 0
	}
	f.n--
	return f.kind
}

// dispatch runs one operation. reply is the answer writer (a no-op for the
// lost-response fault, which performs the side effect but sends nothing).
func (s *Server) dispatch(op string, in map[string]any, reply func(int, any)) bool {
	switch op {
	case "GetRegions":
		reply(200, map[string]any{"regions": []map[string]any{{
			"name": TestRegion, "displayName": "Region One", "continentCode": "NA",
			"availabilityZones": []map[string]string{{"zoneName": TestRegion + "a", "state": "available"}},
		}}})
	case "GetBundles":
		s.mu.Lock()
		b := s.bundles
		s.mu.Unlock()
		reply(200, map[string]any{"bundles": b})
	case "GetBlueprints":
		reply(200, map[string]any{"blueprints": []map[string]any{
			{"blueprintId": TestBlueprintID, "name": "Fixture Linux", "group": "fixture",
				"type": "os", "isActive": true, "platform": "LINUX_UNIX", "version": "1", "minPower": 0},
			{"blueprintId": "ubuntu_24_04", "name": "Ubuntu 24.04 LTS", "group": "ubuntu",
				"type": "os", "isActive": true, "platform": "LINUX_UNIX", "version": "24.04", "minPower": 0},
			{"blueprintId": "ubuntu_16_04", "name": "Ubuntu 16.04 LTS", "group": "ubuntu",
				"type": "os", "isActive": false, "platform": "LINUX_UNIX", "version": "16.04", "minPower": 0},
		}})
	case "GetInstance":
		name, _ := in["instanceName"].(string)
		s.mu.Lock()
		cur, ok := s.instances[name]
		if ok {
			s.advance(cur)
		}
		var v map[string]any
		if ok {
			v = s.wire(cur)
		}
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "instance "+name)
		}
		reply(200, map[string]any{"instance": v})
	case "GetInstances":
		s.mu.Lock()
		out := make([]map[string]any, 0, len(s.instances))
		for _, cur := range sortedInstances(s.instances) {
			s.advance(cur)
			out = append(out, s.wire(cur))
		}
		s.mu.Unlock()
		reply(200, map[string]any{"instances": out})
	case "CreateInstances":
		names, _ := in["instanceNames"].([]any)
		if len(names) != 1 {
			return s.invalid(reply, "instanceNames must name exactly one instance")
		}
		name, _ := names[0].(string)
		s.mu.Lock()
		if _, exists := s.instances[name]; exists {
			s.mu.Unlock()
			return s.invalid(reply, "instance "+name+" already exists")
		}
		s.lastCreate = in
		s.nextIP++
		ud, _ := in["userData"].(string)
		bundleID, _ := in["bundleId"].(string)
		blueprintID, _ := in["blueprintId"].(string)
		cur := &Instance{
			Name: name, State: "pending", BundleID: bundleID, BlueprintID: blueprintID,
			UserData: ud, Tags: wireTags(in["tags"]),
			PublicIP:  fmt.Sprintf("192.0.2.%d", s.nextIP),
			CreatedAt: float64(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC).Unix()),
		}
		s.instances[name] = cur
		s.creates++
		s.mu.Unlock()
		reply(200, map[string]any{"operations": []map[string]any{
			{"id": "op-create-" + name, "resourceName": name, "resourceType": "Instance",
				"status": "Succeeded", "operationType": "CreateInstance", "isTerminal": true},
		}})
	case "StartInstance", "StopInstance", "RebootInstance":
		name, _ := in["instanceName"].(string)
		s.mu.Lock()
		cur, ok := s.instances[name]
		if ok {
			switch op {
			case "StartInstance", "RebootInstance":
				cur.State = "pending"
			case "StopInstance":
				cur.State = "stopping"
			}
		}
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "instance "+name)
		}
		reply(200, map[string]any{"operations": []map[string]any{
			{"id": "op-" + op, "resourceName": name, "status": "Succeeded", "operationType": op, "isTerminal": true},
		}})
	case "DeleteInstance":
		name, _ := in["instanceName"].(string)
		s.mu.Lock()
		cur, ok := s.instances[name]
		if ok {
			// The automation IAM policy only permits deleting resources
			// tagged dhnt:managed=true; an untagged one is a refusal from
			// AWS, not a success.
			if !hasManaged(cur.Tags) {
				s.mu.Unlock()
				awsErrorReply(reply, http.StatusBadRequest, "AccessDeniedException",
					"not authorized to perform lightsail:DeleteInstance on an untagged resource")
				return true
			}
			delete(s.instances, name)
			s.deletes++
			if ip, has := s.staticIPs[name+"-ip"]; has {
				ip.AttachedTo = ""
			}
		}
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "instance "+name)
		}
		reply(200, map[string]any{"operations": []map[string]any{
			{"id": "op-delete-" + name, "resourceName": name, "status": "Succeeded",
				"operationType": "DeleteInstance", "isTerminal": true},
		}})
	case "PutInstancePublicPorts":
		name, _ := in["instanceName"].(string)
		s.mu.Lock()
		cur, ok := s.instances[name]
		if ok {
			s.lastPorts = in
			cur.Ports = nil
			if infos, has := in["portInfos"].([]any); has {
				for _, pi := range infos {
					if m, isMap := pi.(map[string]any); isMap {
						cur.Ports = append(cur.Ports, m)
					}
				}
			}
		}
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "instance "+name)
		}
		reply(200, map[string]any{"operation": map[string]any{
			"id": "op-ports-" + name, "resourceName": name, "status": "Succeeded",
			"operationType": "PutInstancePublicPorts", "isTerminal": true,
		}})
	case "AllocateStaticIp":
		name, _ := in["staticIpName"].(string)
		s.mu.Lock()
		if _, exists := s.staticIPs[name]; !exists {
			s.nextIP++
			s.staticIPs[name] = &StaticIP{Name: name, IPAddress: fmt.Sprintf("198.51.100.%d", s.nextIP)}
		}
		s.mu.Unlock()
		reply(200, map[string]any{"operations": []map[string]any{
			{"id": "op-allocate-" + name, "resourceName": name, "status": "Succeeded",
				"operationType": "AllocateStaticIp", "isTerminal": true},
		}})
	case "GetStaticIp":
		name, _ := in["staticIpName"].(string)
		s.mu.Lock()
		ip, ok := s.staticIPs[name]
		var v map[string]any
		if ok {
			v = map[string]any{
				"name": ip.Name, "ipAddress": ip.IPAddress,
				"attachedTo": ip.AttachedTo, "isAttached": ip.AttachedTo != "",
				"location": map[string]string{"regionName": TestRegion, "availabilityZone": TestRegion + "a"},
			}
		}
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "static ip "+name)
		}
		reply(200, map[string]any{"staticIp": v})
	case "AttachStaticIp":
		ipName, _ := in["staticIpName"].(string)
		instName, _ := in["instanceName"].(string)
		s.mu.Lock()
		ip, ok := s.staticIPs[ipName]
		cur, hasInst := s.instances[instName]
		if ok && hasInst {
			ip.AttachedTo = instName
			cur.StaticIP = true
			cur.PublicIP = ip.IPAddress
		}
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "static ip "+ipName)
		}
		if !hasInst {
			return s.notFound(reply, "instance "+instName)
		}
		reply(200, map[string]any{"operations": []map[string]any{
			{"id": "op-attach-" + ipName, "resourceName": ipName, "status": "Succeeded",
				"operationType": "AttachStaticIp", "isTerminal": true},
		}})
	case "DetachStaticIp":
		ipName, _ := in["staticIpName"].(string)
		s.mu.Lock()
		ip, ok := s.staticIPs[ipName]
		if ok {
			if cur, has := s.instances[ip.AttachedTo]; has {
				cur.StaticIP = false
			}
			ip.AttachedTo = ""
		}
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "static ip "+ipName)
		}
		reply(200, map[string]any{"operations": []map[string]any{
			{"id": "op-detach-" + ipName, "resourceName": ipName, "status": "Succeeded",
				"operationType": "DetachStaticIp", "isTerminal": true},
		}})
	case "ReleaseStaticIp":
		ipName, _ := in["staticIpName"].(string)
		s.mu.Lock()
		_, ok := s.staticIPs[ipName]
		delete(s.staticIPs, ipName)
		s.mu.Unlock()
		if !ok {
			return s.notFound(reply, "static ip "+ipName)
		}
		reply(200, map[string]any{"operations": []map[string]any{
			{"id": "op-release-" + ipName, "resourceName": ipName, "status": "Succeeded",
				"operationType": "ReleaseStaticIp", "isTerminal": true},
		}})
	case "GetCostAndUsage":
		s.mu.Lock()
		mtd := s.monthToDate
		s.mu.Unlock()
		period, _ := in["TimePeriod"].(map[string]any)
		reply(200, map[string]any{"ResultsByTime": []map[string]any{{
			"TimePeriod": period,
			"Total": map[string]any{"UnblendedCost": map[string]string{
				"Amount": strconv.FormatFloat(mtd, 'f', 10, 64), "Unit": "USD",
			}},
			"Estimated": true,
		}}})
	default:
		return false
	}
	return true
}

// awsErrorReply writes an AWS error through a dispatch reply func.
func awsErrorReply(reply func(int, any), status int, typ, msg string) {
	reply(status, map[string]string{"__type": typ, "code": typ, "message": msg})
}

func (s *Server) notFound(reply func(int, any), what string) bool {
	awsErrorReply(reply, http.StatusBadRequest, "NotFoundException", "does not exist: "+what)
	return true
}

func (s *Server) invalid(reply func(int, any), msg string) bool {
	awsErrorReply(reply, http.StatusBadRequest, "InvalidInputException", msg)
	return true
}

// advance moves an in-flight instance one step toward its resting state,
// the way Lightsail does between two reads. Callers hold s.mu.
func (s *Server) advance(in *Instance) {
	switch in.State {
	case "pending":
		in.State = "running"
	case "stopping":
		in.State = "stopped"
	}
}

// wire renders an instance in the Lightsail shape. Callers hold s.mu.
func (s *Server) wire(in *Instance) map[string]any {
	tags := make([]map[string]string, 0, len(in.Tags))
	for _, t := range in.Tags {
		tags = append(tags, map[string]string{"key": t.Key, "value": t.Value})
	}
	return map[string]any{
		"name": in.Name, "arn": "arn:aws:lightsail:" + TestRegion + ":123456789012:Instance/" + in.Name,
		"createdAt":   in.CreatedAt,
		"location":    map[string]string{"regionName": TestRegion, "availabilityZone": TestRegion + "a"},
		"blueprintId": in.BlueprintID, "bundleId": in.BundleID,
		"state":            map[string]any{"code": 16, "name": in.State},
		"publicIpAddress":  in.PublicIP,
		"privateIpAddress": "172.26.0.5",
		"isStaticIp":       in.StaticIP,
		"hardware":         map[string]any{"cpuCount": 1, "ramSizeInGb": 1.0},
		"resourceType":     "Instance",
		"tags":             tags,
	}
}

func wireTags(v any) []aws.Tag {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]aws.Tag, 0, len(arr))
	for _, e := range arr {
		m, isMap := e.(map[string]any)
		if !isMap {
			continue
		}
		k, _ := m["key"].(string)
		val, _ := m["value"].(string)
		out = append(out, aws.Tag{Key: k, Value: val})
	}
	return out
}

func hasManaged(tags []aws.Tag) bool {
	for _, t := range tags {
		if t.Key == aws.TagManagedKey && t.Value == aws.TagManagedValue {
			return true
		}
	}
	return false
}

// sortedInstances gives GetInstances a deterministic order.
func sortedInstances(m map[string]*Instance) []*Instance {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	out := make([]*Instance, 0, len(names))
	for _, n := range names {
		out = append(out, m[n])
	}
	return out
}
