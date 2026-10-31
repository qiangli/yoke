// Package fakelightsail is a deterministic in-process stand-in for the AWS
// Lightsail and Cost Explorer JSON APIs, for testing pkg/provider/aws.
//
// It answers FIVE services on one URL — Lightsail, Cost Explorer, AWS
// Budgets, STS and S3 — because one adapter talks to all five and a test
// wants one endpoint to point at. The signature check below is what keeps
// that honest: each operation must be signed for ITS OWN service and
// region.
//
// S3 is the odd one out and is routed by METHOD, not by X-Amz-Target: a
// PUT/DELETE/GET of /<bucket>/<key> is an object operation (S522.2
// rework 2's user-data staging). A PUT or DELETE is checked the usual
// way, against its Authorization header; a GET is a PRESIGNED fetch with
// no header at all, so its query signature is recomputed instead — which
// is the only way a test can tell a correct presigned URL from a
// plausible-looking one.
//
// It is a recorded-response server, not a mock: it speaks the real wire
// protocol (POST "/" with X-Amz-Target, application/x-amz-json-1.1 — or
// the query protocol for STS, which is form-encoded and answers XML),
// keeps real state so a create is visible to the next list, and —
// importantly — VERIFIES THE SIGV4 SIGNATURE of every request it answers. An adapter that
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
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// TestRegion is the only supported satellite region.
	TestRegion = "us-west-2"
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
	// FaultDataUnavailable answers HTTP 400 DataUnavailableException —
	// what Cost Explorer really answers on an account whose cost data has
	// never been ingested. It is the live failure that made the budget
	// the primary spend source (S522.2 rework 1).
	FaultDataUnavailable
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
	// pre is the sequence of states GetInstance reports before the
	// instance advances on its own (SetInitialStates).
	pre []string
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
	// budget is what DescribeBudget answers, or nil for an account with
	// no such budget (NotFoundException). The default budget has no
	// evaluated spend, so spend still falls back to Cost Explorer.
	budget       *Budget
	bucketStatus int
	rootIdentity bool

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
	// calls counts how many times each operation was called, so a test
	// can assert that a source was NOT consulted (the budget path must
	// not touch Cost Explorer) and that a cached lookup happened once.
	calls map[string]int
	// scopes records the "<service>/<region>" each operation was signed
	// for, so a test can assert the signing scope directly.
	scopes map[string]string
	// objects is the fake S3: the staged user-data objects, keyed
	// "<bucket>/<key>".
	objects map[string]*S3Object
	// initialStates is the state sequence a newly created instance
	// reports before it advances on its own (SetInitialStates).
	initialStates []string
	// stuck freezes every in-flight instance where it is (SetStuck).
	stuck bool
}

// New starts a fake seeded with the standard catalog and closes it when
// the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		instances:    map[string]*Instance{},
		staticIPs:    map[string]*StaticIP{},
		faults:       map[string]*fault{},
		scopes:       map[string]string{},
		calls:        map[string]int{},
		objects:      map[string]*S3Object{},
		monthToDate:  3.50,
		budget:       &Budget{Name: aws.DefaultBudgetName, LimitUSD: 50, NoSpend: true},
		bucketStatus: http.StatusOK,
		nextIP:       10,
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

// SetInitialStates makes a newly created instance report states in turn,
// one per read, before it advances on its own. It is how a test reproduces
// what Lightsail really does after CreateInstances: the record exists
// before its state does, so the first reads answer with an EMPTY state
// name and several more answer "pending".
//
//	s.SetInitialStates("", "pending", "pending")
func (s *Server) SetInitialStates(states ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialStates = append([]string(nil), states...)
}

// SetStuck freezes in-flight instances: nothing advances, so an instance
// created while it is set stays "pending" for as long as the test cares
// to poll. It is how a test reaches a wait budget running out.
func (s *Server) SetStuck(stuck bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stuck = stuck
}

// Budget is the fake's view of the account's AWS Budget.
type Budget struct {
	// Name is the budget name DescribeBudget matches.
	Name string
	// LimitUSD is BudgetLimit, the operator's cap.
	LimitUSD float64
	// ActualSpendUSD is CalculatedSpend.ActualSpend, month-to-date.
	ActualSpendUSD float64
	// NoSpend omits CalculatedSpend entirely, the way a budget AWS has
	// not evaluated yet looks.
	NoSpend bool
	// IncludeCredit is CostTypes.IncludeCredit. The real
	// dhnt-monthly-cap budget sets it false, so the spend figure is
	// before credits.
	IncludeCredit bool
	// Unit overrides the money unit ("USD" when empty).
	Unit string
}

// SetBucketStatus sets the status returned by HeadBucket (200 by default).
func (s *Server) SetBucketStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucketStatus = status
}

// SetRootIdentity makes STS report the account root rather than the scoped user.
func (s *Server) SetRootIdentity(root bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rootIdentity = root
}

// SetBudget makes DescribeBudget answer with b (matched by name).
func (s *Server) SetBudget(b Budget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budget = &b
}

// ClearBudget removes the budget, so DescribeBudget answers
// NotFoundException the way an account without one does.
func (s *Server) ClearBudget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budget = nil
}

// TestAccountID is the account id STS GetCallerIdentity reports.
const TestAccountID = "123456789012"

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

// AddStaticIP seeds a static IP directly, counting no allocation.
// attachedTo may name an instance (or be empty for a free address), which
// is how a test stages an address that already belongs to someone.
func (s *Server) AddStaticIP(name, attachedTo string) *StaticIP {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextIP++
	ip := &StaticIP{
		Name: name, IPAddress: fmt.Sprintf("198.51.100.%d", s.nextIP), AttachedTo: attachedTo,
	}
	s.staticIPs[name] = ip
	if cur, ok := s.instances[attachedTo]; ok {
		cur.StaticIP = true
		cur.PublicIP = ip.IPAddress
	}
	return ip
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

// Calls is how many times the given operation was called.
func (s *Server) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
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

// notify hands one decoded request to every registered observer. It runs
// before the operation, and for S3 as well as the JSON services, so a
// test can assert the ORDER of calls across both protocols (staging an
// object before the instance that fetches it, for one).
func (s *Server) notify(op string, in map[string]any) {
	s.mu.Lock()
	obs := slices.Clone(s.observers)
	s.mu.Unlock()
	for _, fn := range obs {
		fn(op, in)
	}
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
	// S3 is addressed by object and verb, not by X-Amz-Target: anything
	// that is not a POST is an S3 object operation.
	if r.Method != http.MethodPost {
		s.serveS3(w, r)
		return
	}
	target := r.Header.Get("X-Amz-Target")
	_, op, _ := strings.Cut(target, ".")
	body, _ := readAll(r)
	// A request with no X-Amz-Target is the QUERY protocol (STS): the
	// operation is the form body's Action, and the answer is XML.
	query := op == ""
	if query {
		op = formAction(body)
	}

	if !s.verify(r, body, op) {
		s.mu.Lock()
		s.badSigs++
		s.mu.Unlock()
		awsError(w, http.StatusForbidden, "InvalidSignatureException",
			"the request signature we calculated does not match the signature you provided")
		return
	}
	s.mu.Lock()
	s.calls[op]++
	s.mu.Unlock()
	if query {
		s.serveQuery(w, r, op)
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

	s.notify(op, in)

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
	case FaultDataUnavailable:
		awsError(w, http.StatusBadRequest, "DataUnavailableException",
			"injected: no cost data is available for this account yet")
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

// formAction reads the Action parameter out of a query-protocol body.
func formAction(body []byte) string {
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return ""
	}
	return v.Get("Action")
}

// writeXML answers the query protocol's way.
func writeXML(w http.ResponseWriter, status int, doc string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+doc)
}

// queryErrorXML is the query protocol's error envelope, which is XML and
// not the JSON shape the other three services use.
func queryErrorXML(w http.ResponseWriter, status int, code, msg string) {
	writeXML(w, status, "<ErrorResponse><Error><Type>Sender</Type><Code>"+code+
		"</Code><Message>"+msg+"</Message></Error><RequestId>req-fake</RequestId></ErrorResponse>")
}

// serveQuery answers the STS query-protocol operations.
func (s *Server) serveQuery(w http.ResponseWriter, r *http.Request, action string) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		queryErrorXML(w, http.StatusBadRequest, "InvalidAction", "unexpected content type "+ct)
		return
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	switch s.takeFault(action) {
	case FaultAccessDenied:
		queryErrorXML(w, http.StatusForbidden, "AccessDenied",
			"User: arn:aws:iam::"+TestAccountID+":user/dhnt-automation is not authorized to perform: sts:"+action)
		return
	case FaultServiceError, FaultLoseResponse:
		queryErrorXML(w, http.StatusInternalServerError, "InternalFailure", "injected internal failure")
		return
	case FaultThrottle:
		queryErrorXML(w, http.StatusBadRequest, "Throttling", "Rate exceeded")
		return
	}
	switch action {
	case "GetCallerIdentity":
		s.mu.Lock()
		principal := ":user/dhnt-automation"
		if s.rootIdentity {
			principal = ":root"
		}
		s.mu.Unlock()
		writeXML(w, http.StatusOK,
			"<GetCallerIdentityResponse xmlns=\"https://sts.amazonaws.com/doc/2011-06-15/\">"+
				"<GetCallerIdentityResult>"+
				"<Arn>arn:aws:iam::"+TestAccountID+principal+"</Arn>"+
				"<UserId>AIDAFAKEUSERID</UserId>"+
				"<Account>"+TestAccountID+"</Account>"+
				"</GetCallerIdentityResult>"+
				"<ResponseMetadata><RequestId>req-fake</RequestId></ResponseMetadata>"+
				"</GetCallerIdentityResponse>")
	default:
		queryErrorXML(w, http.StatusBadRequest, "InvalidAction", "unknown action "+action)
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
// reject a mismatch; one fake serves all four services here, so it has to
// derive them from the operation instead — and it does, because signing a
// Cost Explorer call with the instance's region (rather than Cost
// Explorer's global us-east-1) is one of the easiest things to get wrong
// and would otherwise only show up as a live SignatureDoesNotMatch.
func expectedScope(op string) (service, region string) {
	switch op {
	case "GetCostAndUsage":
		return "ce", "us-east-1"
	case "DescribeBudget":
		return "budgets", "us-east-1"
	case "GetCallerIdentity":
		return "sts", "us-east-1"
	case "PutObject", "DeleteObject", "GetObject", "HeadBucket":
		// S3 is regional and signed for the bucket's own region, which
		// for this fake is the instance's region.
		return "s3", TestRegion
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
			pre:       append([]string(nil), s.initialStates...),
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
	case "DescribeBudget":
		s.mu.Lock()
		b := s.budget
		s.mu.Unlock()
		name, _ := in["BudgetName"].(string)
		account, _ := in["AccountId"].(string)
		if account != TestAccountID {
			return s.invalid(reply, "AccountId "+account+" is not this account")
		}
		if b == nil || b.Name != name {
			awsErrorReply(reply, http.StatusBadRequest, "NotFoundException",
				"Unable to get budget: "+name+" - the budget doesn't exist.")
			return true
		}
		unit := b.Unit
		if unit == "" {
			unit = "USD"
		}
		out := map[string]any{
			"BudgetName":  b.Name,
			"BudgetType":  "COST",
			"TimeUnit":    "MONTHLY",
			"BudgetLimit": map[string]string{"Amount": money(b.LimitUSD), "Unit": unit},
			"CostTypes":   map[string]any{"IncludeCredit": b.IncludeCredit, "IncludeRefund": false},
		}
		if !b.NoSpend {
			out["CalculatedSpend"] = map[string]any{
				"ActualSpend":     map[string]string{"Amount": money(b.ActualSpendUSD), "Unit": unit},
				"ForecastedSpend": map[string]string{"Amount": money(b.ActualSpendUSD), "Unit": unit},
			}
		}
		reply(200, map[string]any{"Budget": out})
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

// money renders a USD figure the way AWS does: a decimal STRING, not a
// JSON number.
func money(usd float64) string { return strconv.FormatFloat(usd, 'f', 10, 64) }

// advance moves an in-flight instance one step toward its resting state,
// the way Lightsail does between two reads. Callers hold s.mu.
func (s *Server) advance(in *Instance) {
	if s.stuck {
		return
	}
	if len(in.pre) > 0 {
		in.State, in.pre = in.pre[0], in.pre[1:]
		return
	}
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

// ---- fake S3 (S522.2 rework 2: user-data staging) ----

// S3Object is one staged object, with the headers it was written under so
// a test can assert the encryption the bucket policy requires.
type S3Object struct {
	Bucket      string
	Key         string
	Body        []byte
	ContentType string
	// SSE is the x-amz-server-side-encryption header's value.
	SSE string
}

// S3Object returns the stored object for a bucket and key.
func (s *Server) S3Object(bucket, key string) (S3Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[bucket+"/"+key]
	if !ok {
		return S3Object{}, false
	}
	return *o, true
}

// S3Keys lists the stored objects as "<bucket>/<key>", sorted.
func (s *Server) S3Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// S3ObjectCount is how many objects are stored.
func (s *Server) S3ObjectCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.objects) }

// splitS3Path splits "/<bucket>/<key...>" into its two halves. The fake is
// addressed path-style (the adapter's S3URL override puts the bucket in
// the path) so the bucket name is visible here rather than hidden in an
// httptest host.
func splitS3Path(path string) (bucket, key string, ok bool) {
	p := strings.TrimPrefix(path, "/")
	bucket, key, found := strings.Cut(p, "/")
	if !found || bucket == "" || key == "" {
		return "", "", false
	}
	// The adapter builds keys from a name and hex, so no escaping is in
	// play; unescape anyway so a key that needed it still matches.
	if u, err := url.PathUnescape(key); err == nil {
		key = u
	}
	return bucket, key, true
}

// s3ErrorXML answers the way S3 does: an XML <Error> envelope, not the
// JSON shape the other four services use. An adapter that only parsed
// JSON errors would report "HTTP 403: <xml...>" here, which is why the
// fake insists on the real shape.
func s3ErrorXML(w http.ResponseWriter, status int, code, msg string) {
	writeXML(w, status, "<Error><Code>"+code+"</Code><Message>"+msg+
		"</Message><RequestId>req-fake</RequestId></Error>")
}

// serveS3 answers PutObject, DeleteObject and a presigned GetObject.
func (s *Server) serveS3(w http.ResponseWriter, r *http.Request) {
	op := map[string]string{
		http.MethodHead:   "HeadBucket",
		http.MethodPut:    "PutObject",
		http.MethodDelete: "DeleteObject",
		http.MethodGet:    "GetObject",
	}[r.Method]
	if op == "" {
		s3ErrorXML(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported method "+r.Method)
		return
	}
	bucket, key, ok := splitS3Path(r.URL.EscapedPath())
	if op == "HeadBucket" {
		bucket = strings.Trim(r.URL.Path, "/")
		key, ok = "", bucket != "" && !strings.Contains(bucket, "/")
	}
	if !ok {
		s3ErrorXML(w, http.StatusBadRequest, "InvalidRequest",
			"the fake is addressed path-style: /<bucket>/<key>, got "+r.URL.Path)
		return
	}
	body, _ := readAll(r)

	// A GET is presigned and carries no Authorization header: its
	// signature is in the query string and is verified by recomputing it.
	authorized := false
	if op == "GetObject" {
		authorized = s.verifyPresigned(r, op)
	} else {
		authorized = s.verify(r, body, op)
	}
	if !authorized {
		s.mu.Lock()
		s.badSigs++
		s.mu.Unlock()
		s3ErrorXML(w, http.StatusForbidden, "SignatureDoesNotMatch",
			"the request signature we calculated does not match the signature you provided")
		return
	}
	s.mu.Lock()
	s.calls[op]++
	s.requests++
	s.mu.Unlock()
	s.notify(op, map[string]any{"bucket": bucket, "key": key, "bytes": len(body)})

	switch s.takeFault(op) {
	case FaultAccessDenied:
		s3ErrorXML(w, http.StatusForbidden, "AccessDenied", "Access Denied")
		return
	case FaultServiceError, FaultLoseResponse:
		s3ErrorXML(w, http.StatusInternalServerError, "InternalError", "injected internal error")
		return
	case FaultThrottle:
		s3ErrorXML(w, http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate.")
		return
	case FaultNotFound:
		s3ErrorXML(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}

	path := bucket + "/" + key
	switch op {
	case "HeadBucket":
		s.mu.Lock()
		status := s.bucketStatus
		s.mu.Unlock()
		w.WriteHeader(status)
	case "PutObject":
		// The staging bucket's policy requires server-side encryption,
		// so a write without the header is a refusal, not a success.
		sse := r.Header.Get("X-Amz-Server-Side-Encryption")
		if sse == "" {
			s3ErrorXML(w, http.StatusBadRequest, "InvalidRequest",
				"this bucket requires server-side encryption")
			return
		}
		s.mu.Lock()
		s.objects[path] = &S3Object{
			Bucket: bucket, Key: key, Body: slices.Clone(body),
			ContentType: r.Header.Get("Content-Type"), SSE: sse,
		}
		s.mu.Unlock()
		w.Header().Set("ETag", "\"fake\"")
		w.WriteHeader(http.StatusOK)
	case "DeleteObject":
		// S3's DeleteObject is idempotent: deleting a key that is not
		// there is a 204, not an error.
		s.mu.Lock()
		delete(s.objects, path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "GetObject":
		s.mu.Lock()
		o, found := s.objects[path]
		s.mu.Unlock()
		if !found {
			s3ErrorXML(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Header().Set("Content-Type", o.ContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(o.Body)
	}
}

// verifyPresigned checks a presigned URL the way S3 does: recompute the
// signature over everything but X-Amz-Signature and compare, then check
// the URL has not expired. It records the scope the URL named so a test
// can assert it, and refuses a scope that is not S3's.
func (s *Server) verifyPresigned(r *http.Request, op string) bool {
	full := "https://" + r.Host + r.URL.RequestURI()
	q := r.URL.Query()
	cred := strings.Split(q.Get("X-Amz-Credential"), "/")
	if len(cred) != 5 || cred[0] != TestAccessKeyID {
		return false
	}
	s.mu.Lock()
	s.scopes[op] = cred[3] + "/" + cred[2]
	s.mu.Unlock()
	if wantService, wantRegion := expectedScope(op); cred[3] != wantService || cred[2] != wantRegion {
		return false
	}
	want, got, err := aws.PresignedSignature(r.Method, full, TestCredentials())
	if err != nil || want != got {
		return false
	}
	// The expiry is checked against the URL's own X-Amz-Date rather than
	// the wall clock, so a test that signs at a fixed time still passes.
	t, terr := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	return terr == nil && !aws.PresignExpired(full, t.Add(time.Second))
}
