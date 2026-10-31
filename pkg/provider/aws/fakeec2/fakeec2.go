// Package fakeec2 is a deterministic in-process stand-in for the AWS
// EC2 query API, the Service Quotas JSON API and the S3 object API,
// for testing the EC2 GPU worker adapter (pkg/provider/aws NewEC2).
//
// Budgets, Cost Explorer and STS are NOT served here: the adapter
// talks to those through the shared fake in fakelightsail, and a test
// wires them by pointing Config.BudgetsURL/CostExplorerURL/STSURL at
// one fakelightsail server and EC2Config.EC2URL/ServiceQuotasURL/S3URL
// at this one. Splitting by service keeps each fake's signing scope
// exact: EC2 and Service Quotas sign for us-west-2 here, while S3
// staging signs for the bucket's region through the same split the
// adapter itself uses.
//
// Like its sibling, it is a recorded-response server, not a mock: it
// speaks the real wire protocols (form-encoded query actions with XML
// answers for EC2, application/x-amz-json-1.1 for Service Quotas,
// object PUT/DELETE/GET for S3), keeps real state so a launch is
// visible to the next describe, and VERIFIES THE SIGV4 SIGNATURE of
// every request it answers — header signatures recomputed with
// aws.AuthorizationHeader, presigned GETs with aws.PresignedSignature.
//
// Instance states advance the way EC2's do between reads: a launch
// lands "pending" and reaches "running" on a later read, a stop goes
// through "stopping", a terminate through "shutting-down". That is
// what exercises the adapter's wait loops instead of letting a
// synchronous fake hide them.
package fakeec2

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
	"github.com/qiangli/yoke/pkg/provider/aws/fakelightsail"
)

// TestRegion is the region the fake pretends to be: the pinned
// us-west-2 the adapter refuses anything else for.
const TestRegion = "us-west-2"

// TestAZ is the availability zone launched workers land in.
const TestAZ = TestRegion + "a"

// TestAMI is an image id the fake accepts at launch.
const TestAMI = "ami-0deep123456789012"

// testCredentials is the key pair the fake accepts (the AWS
// documentation's example pair, via the sibling fake).
func testCredentials() aws.Credentials { return fakelightsail.TestCredentials() }

// Fault is an injected failure mode for one action.
type Fault int

const (
	// FaultLoseResponse performs the side effect and then answers 500:
	// the caller cannot tell whether the operation took effect.
	FaultLoseResponse Fault = iota + 1
	// FaultAccessDenied answers the IAM refusal AWS sends.
	FaultAccessDenied
	// FaultThrottle answers a throttled error.
	FaultThrottle
	// FaultServiceError answers a 500 before any side effect.
	FaultServiceError
	// FaultInsufficientCapacity answers InsufficientInstanceCapacity
	// on a start, the way an AZ without GPU headroom does.
	FaultInsufficientCapacity
)

type fault struct {
	kind Fault
	n    int
}

// Instance is the fake's view of one worker.
type Instance struct {
	ID               string
	Name             string
	Type             string
	AMI              string
	State            string
	AZ               string
	UserData         string // as sent on the wire (base64)
	KeyName          string
	ShutdownBehavior string
	VolumeSize       int
	Throughput       int
	Tags             []aws.Tag
	PublicIP         string
	ClientToken      string
	LaunchTime       string
}

// S3Object is one staged object, with the headers it was written
// under so a test can assert the encryption the bucket policy
// requires.
type S3Object struct {
	Bucket      string
	Key         string
	Body        []byte
	ContentType string
	SSE         string
}

// Server is the fake.
type Server struct {
	*httptest.Server
	// URL is the endpoint to point EC2Config.EC2URL,
	// EC2Config.ServiceQuotasURL and Config.S3URL at.
	URL string

	mu        sync.Mutex
	instances map[string]*Instance
	byToken   map[string]string
	faults    map[string]*fault
	// quotas is what GetServiceQuota answers, keyed by quota code.
	// It starts with headroom for one 8-vCPU worker (the approved
	// state); a test reproduces the PENDING quota with SetQuota(..., 0).
	quotas map[string]float64
	// objects is the fake S3: staged user-data objects, keyed
	// "<bucket>/<key>".
	objects map[string]*S3Object

	next     int
	creates  int
	deletes  int
	requests int
	losses   int
	badSigs  int
	// lastRun records the last RunInstances form, so a test can
	// assert on exactly what went on the wire.
	lastRun url.Values
	// calls counts how many times each action was called.
	calls map[string]int
	// scopes records the "<service>/<region>" each action was signed
	// for.
	scopes map[string]string
	// stuck freezes every in-flight worker where it is (SetStuck).
	stuck bool
}

// New starts a fake and closes it when the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		instances: map[string]*Instance{},
		byToken:   map[string]string{},
		faults:    map[string]*fault{},
		quotas:    map[string]float64{aws.QuotaRunningOnDemandG: 8},
		objects:   map[string]*S3Object{},
		calls:     map[string]int{},
		scopes:    map[string]string{},
		next:      0,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Server.Close)
	s.URL = s.Server.URL + "/"
	return s
}

// ---- controls ----

// SetQuota makes GetServiceQuota answer value for code.
func (s *Server) SetQuota(code string, value float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotas[code] = value
}

// SetStuck freezes in-flight workers: nothing advances, so a worker
// launched while it is set stays "pending" for as long as the test
// cares to poll.
func (s *Server) SetStuck(stuck bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stuck = stuck
}

// Inject arms a fault on the next n calls to action (e.g.
// "RunInstances", "StartInstances", "GetServiceQuota").
func (s *Server) Inject(action string, kind Fault, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[action] = &fault{kind: kind, n: n}
}

// AddInstance seeds a worker directly, counting no launch. Tags are
// flat contract strings ("k=v" or bare keys), plus the EC2 Name tag
// for name.
func (s *Server) AddInstance(name, state, key string, tags ...string) *Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := fmt.Sprintf("i-fake%04d", s.next)
	in := &Instance{
		ID: id, Name: name, Type: "g6.xlarge", AMI: TestAMI,
		State: state, AZ: TestAZ,
		ShutdownBehavior: "stop",
		VolumeSize:       100, Throughput: 125,
		PublicIP:   fmt.Sprintf("192.0.2.%d", 100+s.next),
		LaunchTime: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	_ = key
	flat := append([]string{"Name=" + name}, tags...)
	in.Tags = encodeTags(flat)
	s.instances[id] = in
	return in
}

// Instance returns the fake's record for id.
func (s *Server) Instance(id string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[id]
	if !ok {
		return Instance{}, false
	}
	return *in, true
}

// Calls is how many times the given action was called.
func (s *Server) Calls(action string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[action]
}

// Scope is the "<service>/<region>" credential scope the given action
// was signed for, or "" if it was never called.
func (s *Server) Scope(action string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scopes[action]
}

// Exists reports whether a worker is still stored (in any state,
// including terminated: EC2 keeps reporting those for a while).
func (s *Server) Exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.instances[id]
	return ok
}

// Live reports whether a worker is stored and not terminated.
func (s *Server) Live(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[id]
	return ok && in.State != "terminated" && in.State != "shutting-down"
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

// LastRun is the last RunInstances form, so a test can assert on
// exactly what went on the wire: tag specifications, shutdown
// behavior, volume, user data, and what was NOT sent (security
// groups, spot options).
func (s *Server) LastRun() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun
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

func encodeTags(flat []string) []aws.Tag {
	out := make([]aws.Tag, 0, len(flat))
	for _, f := range flat {
		k, v, _ := strings.Cut(f, "=")
		if k == "" {
			continue
		}
		out = append(out, aws.Tag{Key: k, Value: v})
	}
	return out
}

// ---- serving ----

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// S3 is addressed by object and verb, not by Action: anything
	// that is not a POST is an object operation.
	if r.Method != http.MethodPost {
		s.serveS3(w, r)
		return
	}
	body, _ := readAll(r)
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		s.serveQuotas(w, r, body, target)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		queryErrorXML(w, http.StatusBadRequest, "InvalidParameterValue", err.Error())
		return
	}
	action := form.Get("Action")
	if !s.verify(r, body, action) {
		s.mu.Lock()
		s.badSigs++
		s.mu.Unlock()
		queryErrorXML(w, http.StatusForbidden, "InvalidSignatureException",
			"the request signature we calculated does not match the signature you provided")
		return
	}
	s.mu.Lock()
	s.calls[action]++
	s.mu.Unlock()
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		queryErrorXML(w, http.StatusBadRequest, "InvalidParameterValue", "unexpected content type "+ct)
		return
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	// A pre-side-effect fault short-circuits the action entirely.
	switch s.takeFault(action) {
	case FaultAccessDenied:
		queryErrorXML(w, http.StatusForbidden, "UnauthorizedOperation",
			"User: arn:aws:iam::"+fakelightsail.TestAccountID+":user/dhnt-automation is not authorized to perform: ec2:"+action)
		return
	case FaultThrottle:
		queryErrorXML(w, http.StatusBadRequest, "RequestLimitExceeded", "Rate exceeded")
		return
	case FaultServiceError:
		queryErrorXML(w, http.StatusInternalServerError, "InternalFailure", "injected service exception")
		return
	case FaultInsufficientCapacity:
		queryErrorXML(w, http.StatusInternalServerError, "InsufficientInstanceCapacity",
			"There is no capacity for the requested instance type in this Availability Zone")
		return
	case FaultLoseResponse:
		s.mu.Lock()
		s.losses++
		s.mu.Unlock()
		if _, ferr := s.dispatchEC2(action, form); ferr != nil {
			ferr.write(w)
			return
		}
		queryErrorXML(w, http.StatusInternalServerError, "InternalFailure",
			"injected: side effect happened, answer lost")
		return
	}
	known, ferr := s.dispatchEC2(action, form)
	if ferr != nil {
		ferr.write(w)
		return
	}
	if !known {
		queryErrorXML(w, http.StatusBadRequest, "InvalidAction", "unknown action "+action)
		return
	}
	s.replyEC2(w, action, form)
}

// ec2Failure is a side-effect-time refusal (an IAM denial, a bad AMI)
// discovered while dispatching rather than before it.
type ec2Failure struct {
	status int
	code   string
	msg    string
}

func (f *ec2Failure) write(w http.ResponseWriter) {
	queryErrorXML(w, f.status, f.code, f.msg)
}

// replyEC2 writes the 200 answer for an action whose dispatch already
// ran. It is separate so the lost-response fault can run the side
// effect without answering.
func (s *Server) replyEC2(w http.ResponseWriter, action string, form url.Values) {
	switch action {
	case "RunInstances":
		s.mu.Lock()
		id := s.byToken[form.Get("ClientToken")]
		in := s.instances[id]
		s.mu.Unlock()
		if in == nil {
			queryErrorXML(w, http.StatusInternalServerError, "InternalFailure", "launched worker vanished")
			return
		}
		writeXML(w, http.StatusOK, runInstancesDoc(in))
	case "DescribeInstances":
		s.mu.Lock()
		docs, matched := s.describeDoc(form)
		s.mu.Unlock()
		var ids []string
		for i := 1; ; i++ {
			id := form.Get(fmt.Sprintf("InstanceId.%d", i))
			if id == "" {
				break
			}
			ids = append(ids, id)
		}
		if len(ids) > 0 && matched == 0 {
			status, code := unknownIDError(ids[0])
			queryErrorXML(w, status, code, "does not exist: "+ids[0])
			return
		}
		writeXML(w, http.StatusOK, docs)
	case "StartInstances", "StopInstances", "TerminateInstances":
		id := form.Get("InstanceId.1")
		s.mu.Lock()
		in := s.instances[id]
		s.mu.Unlock()
		if in == nil {
			queryErrorXML(w, http.StatusBadRequest, "InvalidInstanceID.NotFound", "does not exist: "+id)
			return
		}
		writeXML(w, http.StatusOK, stateChangeDoc(action, in))
	default:
		queryErrorXML(w, http.StatusBadRequest, "InvalidAction", "unknown action "+action)
	}
}

// dispatchEC2 runs one action's side effect. It reports whether the
// action is known and any refusal found while running it; answers are
// written by replyEC2.
func (s *Server) dispatchEC2(action string, form url.Values) (bool, *ec2Failure) {
	switch action {
	case "RunInstances", "DescribeInstances", "StartInstances", "StopInstances", "TerminateInstances":
	default:
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch action {
	case "RunInstances":
		token := form.Get("ClientToken")
		if _, ok := s.byToken[token]; ok && token != "" {
			// A retried launch under the same token returns the
			// same worker: no second allocation.
			s.lastRun = form
			return true, nil
		}
		typ := form.Get("InstanceType")
		ami := form.Get("ImageId")
		if !strings.HasPrefix(ami, "ami-") {
			return true, &ec2Failure{status: http.StatusBadRequest, code: "InvalidAMIID.NotFound", msg: "does not exist: " + ami}
		}
		tags := formTags(form, 1)
		volTags := formTags(form, 2)
		if !hasManaged(tags) || !hasManaged(volTags) {
			return true, &ec2Failure{status: http.StatusForbidden, code: "UnauthorizedOperation",
				msg: "not authorized to perform ec2:RunInstances without dhnt:managed=true on the instance and the volume"}
		}
		size, _ := strconv.Atoi(form.Get("BlockDeviceMapping.1.Ebs.VolumeSize"))
		if size > 200 {
			return true, &ec2Failure{status: http.StatusBadRequest, code: "InvalidParameterValue",
				msg: "volume size is over the 200 GB policy ceiling"}
		}
		thr, _ := strconv.Atoi(form.Get("BlockDeviceMapping.1.Ebs.Throughput"))
		s.next++
		id := fmt.Sprintf("i-fake%04d", s.next)
		name := ""
		for _, t := range tags {
			if t.Key == "Name" {
				name = t.Value
			}
		}
		in := &Instance{
			ID: id, Name: name, Type: typ, AMI: ami,
			State: "pending", AZ: TestAZ,
			UserData:         form.Get("UserData"),
			KeyName:          form.Get("KeyName"),
			ShutdownBehavior: form.Get("InstanceInitiatedShutdownBehavior"),
			VolumeSize:       size, Throughput: thr,
			Tags:        tags,
			PublicIP:    fmt.Sprintf("192.0.2.%d", 100+s.next),
			ClientToken: token,
			LaunchTime:  time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		}
		s.instances[id] = in
		if token != "" {
			s.byToken[token] = id
		}
		s.lastRun = form
		s.creates++
	case "DescribeInstances":
		for _, in := range s.instances {
			s.advance(in)
		}
	case "StartInstances", "StopInstances":
		id := form.Get("InstanceId.1")
		in, ok := s.instances[id]
		if !ok {
			return true, &ec2Failure{status: http.StatusBadRequest, code: "InvalidInstanceID.NotFound", msg: "does not exist: " + id}
		}
		if !hasManaged(in.Tags) {
			return true, &ec2Failure{status: http.StatusForbidden, code: "UnauthorizedOperation",
				msg: "not authorized to perform ec2:" + action + " on an untagged worker"}
		}
		switch action {
		case "StartInstances":
			if in.State == "stopped" {
				in.State = "pending"
				in.PublicIP = fmt.Sprintf("192.0.2.%d", 100+s.next)
				s.next++
			}
		case "StopInstances":
			if in.State == "running" || in.State == "pending" {
				in.State = "stopping"
			}
		}
	case "TerminateInstances":
		id := form.Get("InstanceId.1")
		in, ok := s.instances[id]
		if !ok {
			return true, &ec2Failure{status: http.StatusBadRequest, code: "InvalidInstanceID.NotFound", msg: "does not exist: " + id}
		}
		if !hasManaged(in.Tags) {
			return true, &ec2Failure{status: http.StatusForbidden, code: "UnauthorizedOperation",
				msg: "not authorized to perform ec2:TerminateInstances on an untagged worker"}
		}
		if in.State != "terminated" {
			in.State = "shutting-down"
		}
		s.deletes++
	}
	return true, nil
}

// formTags decodes one TagSpecification out of a RunInstances form.
func formTags(form url.Values, n int) []aws.Tag {
	var out []aws.Tag
	pre := fmt.Sprintf("TagSpecification.%d.", n)
	if form.Get(pre+"ResourceType") == "" {
		return nil
	}
	for i := 1; ; i++ {
		k := form.Get(fmt.Sprintf("%sTag.%d.Key", pre, i))
		if k == "" && form.Get(fmt.Sprintf("%sTag.%d.Value", pre, i)) == "" {
			break
		}
		if k == "" {
			continue
		}
		out = append(out, aws.Tag{Key: k, Value: form.Get(fmt.Sprintf("%sTag.%d.Value", pre, i))})
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

// advance moves an in-flight worker one step toward its resting
// state, the way EC2 does between two reads. Callers hold s.mu.
func (s *Server) advance(in *Instance) {
	if s.stuck {
		return
	}
	switch in.State {
	case "pending":
		in.State = "running"
	case "stopping":
		in.State = "stopped"
		in.PublicIP = ""
	case "shutting-down":
		in.State = "terminated"
		in.PublicIP = ""
	}
}

// ---- XML answers ----

func writeXML(w http.ResponseWriter, status int, doc string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+doc)
}

// queryErrorXML is the query protocol's error envelope.
func queryErrorXML(w http.ResponseWriter, status int, code, msg string) {
	writeXML(w, status, "<Response><Errors><Error><Type>Sender</Type><Code>"+code+
		"</Code><Message>"+xmlEscape(msg)+"</Message></Error></Errors><RequestId>req-fake</RequestId></Response>")
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
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

func stateCode(name string) int {
	switch name {
	case "pending":
		return 0
	case "running":
		return 16
	case "shutting-down":
		return 32
	case "terminated":
		return 48
	case "stopping":
		return 64
	case "stopped":
		return 80
	}
	return 0
}

// instanceDoc renders one worker the way DescribeInstances does.
// Callers hold s.mu.
func instanceDoc(in *Instance) string {
	var b strings.Builder
	b.WriteString("<item>")
	fmt.Fprintf(&b, "<instanceId>%s</instanceId>", xmlEscape(in.ID))
	fmt.Fprintf(&b, "<imageId>%s</imageId>", xmlEscape(in.AMI))
	fmt.Fprintf(&b, "<instanceState><code>%d</code><name>%s</name></instanceState>", stateCode(in.State), xmlEscape(in.State))
	fmt.Fprintf(&b, "<instanceType>%s</instanceType>", xmlEscape(in.Type))
	fmt.Fprintf(&b, "<placement><availabilityZone>%s</availabilityZone></placement>", xmlEscape(in.AZ))
	if in.PublicIP != "" {
		fmt.Fprintf(&b, "<ipAddress>%s</ipAddress>", xmlEscape(in.PublicIP))
	}
	fmt.Fprintf(&b, "<privateIpAddress>172.31.0.5</privateIpAddress>")
	fmt.Fprintf(&b, "<instanceInitiatedShutdownBehavior>%s</instanceInitiatedShutdownBehavior>", xmlEscape(in.ShutdownBehavior))
	fmt.Fprintf(&b, "<launchTime>%s</launchTime>", xmlEscape(in.LaunchTime))
	b.WriteString("<tagSet>")
	for _, t := range in.Tags {
		fmt.Fprintf(&b, "<item><key>%s</key>", xmlEscape(t.Key))
		if t.Value != "" {
			fmt.Fprintf(&b, "<value>%s</value>", xmlEscape(t.Value))
		}
		b.WriteString("</item>")
	}
	b.WriteString("</tagSet></item>")
	return b.String()
}

func runInstancesDoc(in *Instance) string {
	return `<RunInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">` +
		`<requestId>req-fake</requestId><ownerId>` + fakelightsail.TestAccountID + `</ownerId><instancesSet>` +
		instanceDoc(in) + `</instancesSet></RunInstancesResponse>`
}

func stateChangeDoc(action string, in *Instance) string {
	root := action + "Response"
	return `<` + root + ` xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">` +
		`<requestId>req-fake</requestId><instancesSet><item>` +
		`<instanceId>` + xmlEscape(in.ID) + `</instanceId>` +
		`<currentState><code>` + strconv.Itoa(stateCode(in.State)) + `</code><name>` + xmlEscape(in.State) + `</name></currentState>` +
		`</item></instancesSet></` + root + `>`
}

// describeDoc renders DescribeInstances: one reservation per worker
// (the adapter reads reservations, not placement), honoring
// InstanceId.N. Callers hold s.mu.
func (s *Server) describeDoc(form url.Values) (string, int) {
	var ids []string
	for i := 1; ; i++ {
		id := form.Get(fmt.Sprintf("InstanceId.%d", i))
		if id == "" {
			break
		}
		ids = append(ids, id)
	}
	var b strings.Builder
	b.WriteString(`<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">`)
	b.WriteString(`<requestId>req-fake</requestId><reservationSet>`)
	n := 0
	emit := func(in *Instance) {
		n++
		fmt.Fprintf(&b, `<item><reservationId>r-fake%04d</reservationId><ownerId>`, n)
		b.WriteString(fakelightsail.TestAccountID + `</ownerId><instancesSet>`)
		b.WriteString(instanceDoc(in))
		b.WriteString(`</instancesSet></item>`)
	}
	if len(ids) == 0 {
		for _, in := range sortedInstances(s.instances) {
			emit(in)
		}
	} else {
		for _, id := range ids {
			if in, ok := s.instances[id]; ok {
				emit(in)
			}
		}
	}
	b.WriteString(`</reservationSet></DescribeInstancesResponse>`)
	return b.String(), n
}

func sortedInstances(m map[string]*Instance) []*Instance {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*Instance, 0, len(ids))
	for _, id := range ids {
		out = append(out, m[id])
	}
	return out
}

// unknownIDError distinguishes the two EC2 answers for a missing id.
// It is consulted by the serve path when a DescribeInstances names
// ids none of which exist.
func unknownIDError(id string) (status int, code string) {
	if strings.HasPrefix(id, "i-") {
		return http.StatusBadRequest, "InvalidInstanceID.NotFound"
	}
	return http.StatusBadRequest, "InvalidInstanceID.Malformed"
}

// ---- signature verification ----

// expectedScope is the SigV4 service and region an action MUST be
// signed for. Real AWS derives these from the endpoint it is; one
// fake serves three services here, so it derives them from the action
// instead.
func expectedScope(action string) (service, region string) {
	switch action {
	case "GetServiceQuota":
		return "servicequotas", TestRegion
	case "PutObject", "DeleteObject", "GetObject":
		return "s3", TestRegion
	}
	return "ec2", TestRegion
}

// verify recomputes the SigV4 signature over the request as received
// and compares it with the Authorization header the adapter sent, and
// checks that the credential scope names the service and region the
// action belongs to.
func (s *Server) verify(r *http.Request, body []byte, action string) bool {
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return false
	}
	amzDate := r.Header.Get("X-Amz-Date")
	ts, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return false
	}
	i := strings.Index(authz, "Credential=")
	if i < 0 {
		return false
	}
	scope := authz[i+len("Credential="):]
	if j := strings.IndexByte(scope, ','); j >= 0 {
		scope = scope[:j]
	}
	parts := strings.Split(strings.TrimSpace(scope), "/")
	if len(parts) != 5 || parts[0] != fakelightsail.TestAccessKeyID {
		return false
	}
	region, service := parts[2], parts[3]
	s.mu.Lock()
	s.scopes[action] = service + "/" + region
	s.mu.Unlock()
	if wantService, wantRegion := expectedScope(action); service != wantService || region != wantRegion {
		return false
	}
	clone := r.Clone(r.Context())
	clone.Header.Del("Authorization")
	clone.URL.Scheme = "https"
	if clone.URL.Host == "" {
		clone.URL.Host = r.Host
	}
	want := aws.AuthorizationHeader(clone, body, testCredentials(), ts, region, service)
	return want == authz
}

// takeFault consumes one armed fault for action, if any.
func (s *Server) takeFault(action string) Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.faults[action]
	if !ok || f.n <= 0 {
		return 0
	}
	f.n--
	return f.kind
}

// ---- Service Quotas ----

func (s *Server) serveQuotas(w http.ResponseWriter, r *http.Request, body []byte, target string) {
	_, action, _ := strings.Cut(target, ".")
	if !s.verify(r, body, action) {
		s.mu.Lock()
		s.badSigs++
		s.mu.Unlock()
		writeQuotasError(w, http.StatusForbidden, "InvalidSignatureException",
			"the request signature we calculated does not match the signature you provided")
		return
	}
	s.mu.Lock()
	s.calls[action]++
	s.mu.Unlock()
	if ct := r.Header.Get("Content-Type"); ct != "application/x-amz-json-1.1" {
		writeQuotasError(w, http.StatusBadRequest, "InvalidInputException", "unexpected content type "+ct)
		return
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	switch s.takeFault(action) {
	case FaultAccessDenied:
		writeQuotasError(w, http.StatusForbidden, "AccessDeniedException", "not authorized to perform servicequotas:"+action)
		return
	case FaultThrottle:
		writeQuotasError(w, http.StatusBadRequest, "ThrottlingException", "Rate exceeded")
		return
	case FaultServiceError:
		writeQuotasError(w, http.StatusInternalServerError, "ServiceException", "injected service exception")
		return
	}
	if action != "GetServiceQuota" {
		writeQuotasError(w, http.StatusBadRequest, "InvalidInputException", "unknown target "+target)
		return
	}
	var in struct {
		ServiceCode string `json:"ServiceCode"`
		QuotaCode   string `json:"QuotaCode"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeQuotasError(w, http.StatusBadRequest, "InvalidInputException", err.Error())
		return
	}
	s.mu.Lock()
	v, ok := s.quotas[in.QuotaCode]
	s.mu.Unlock()
	if in.ServiceCode != "ec2" || !ok {
		writeQuotasError(w, http.StatusBadRequest, "NoSuchResourceException", "no such quota: "+in.QuotaCode)
		return
	}
	writeQuotasJSON(w, http.StatusOK, map[string]any{"Quota": map[string]any{
		"QuotaCode": in.QuotaCode, "QuotaName": "Running On-Demand G and VT instances",
		"Value": v, "Unit": "None",
	}})
}

func writeQuotasJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeQuotasError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("X-Amzn-Errortype", typ)
	writeQuotasJSON(w, status, map[string]string{"__type": typ, "message": msg})
}

// ---- fake S3 (user-data staging for EC2 workers) ----

// splitS3Path splits "/<bucket>/<key...>" into its two halves. The
// fake is addressed path-style (the adapter's S3URL override puts the
// bucket in the path) so the bucket name is visible here rather than
// hidden in an httptest host.
func splitS3Path(path string) (bucket, key string, ok bool) {
	p := strings.TrimPrefix(path, "/")
	bucket, key, found := strings.Cut(p, "/")
	if !found || bucket == "" || key == "" {
		return "", "", false
	}
	if u, err := url.PathUnescape(key); err == nil {
		key = u
	}
	return bucket, key, true
}

func (s *Server) serveS3(w http.ResponseWriter, r *http.Request) {
	bucket, key, ok := splitS3Path(r.URL.Path)
	if !ok {
		s3ErrorXML(w, http.StatusBadRequest, "InvalidRequest", "bad object path "+r.URL.Path)
		return
	}
	body, _ := readAll(r)
	if authz := r.Header.Get("Authorization"); authz != "" {
		// A signed object call: verify like the query actions.
		op := "PutObject"
		if r.Method == http.MethodDelete {
			op = "DeleteObject"
		}
		if !s.verify(r, body, op) {
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
	} else {
		// A presigned fetch: no header, so the query signature is
		// recomputed instead — the only way a test can tell a
		// correct presigned URL from a plausible-looking one.
		if !s.verifyPresigned(r, "GetObject", w) {
			return
		}
		s.mu.Lock()
		s.calls["GetObject"]++
		s.requests++
		s.mu.Unlock()
	}
	switch r.Method {
	case http.MethodPut:
		s.mu.Lock()
		s.objects[bucket+"/"+key] = &S3Object{
			Bucket: bucket, Key: key, Body: body,
			ContentType: r.Header.Get("Content-Type"),
			SSE:         r.Header.Get("X-Amz-Server-Side-Encryption"),
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, bucket+"/"+key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		s.mu.Lock()
		o, found := s.objects[bucket+"/"+key]
		s.mu.Unlock()
		if !found {
			s3ErrorXML(w, http.StatusNotFound, "NoSuchKey", "no such key "+key)
			return
		}
		if o.ContentType != "" {
			w.Header().Set("Content-Type", o.ContentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(o.Body)
	default:
		s3ErrorXML(w, http.StatusBadRequest, "InvalidRequest", "bad method "+r.Method)
	}
}

// verifyPresigned checks a presigned URL the way S3 does: recompute
// the signature over everything but X-Amz-Signature and compare,
// then check the URL has not expired. It records the scope the URL
// named so a test can assert it, and refuses a scope that is not
// S3's. The expiry is checked against the URL's own X-Amz-Date
// rather than the wall clock, so a test that signs at a fixed time
// still passes.
func (s *Server) verifyPresigned(r *http.Request, op string, w http.ResponseWriter) bool {
	deny := func() bool {
		s3ErrorXML(w, http.StatusForbidden, "SignatureDoesNotMatch", "presigned signature mismatch")
		return false
	}
	full := "https://" + r.Host + r.URL.RequestURI()
	q := r.URL.Query()
	cred := strings.Split(q.Get("X-Amz-Credential"), "/")
	if len(cred) != 5 || cred[0] != fakelightsail.TestAccessKeyID {
		return deny()
	}
	s.mu.Lock()
	s.scopes[op] = cred[3] + "/" + cred[2]
	s.mu.Unlock()
	if wantService, wantRegion := expectedScope(op); cred[3] != wantService || cred[2] != wantRegion {
		return deny()
	}
	want, got, err := aws.PresignedSignature(r.Method, full, testCredentials())
	if err != nil || want != got {
		s.mu.Lock()
		s.badSigs++
		s.mu.Unlock()
		return deny()
	}
	t, terr := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if terr != nil || aws.PresignExpired(full, t.Add(time.Second)) {
		return deny()
	}
	return true
}

func s3ErrorXML(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+`<Error><Code>`+xmlEscape(code)+`</Code><Message>`+xmlEscape(msg)+`</Message></Error>`)
}
