// The Lightsail JSON API client (S522.2): the application/x-amz-json-1.1
// protocol spoken directly over net/http, signed by sigv4.go.
//
// Every call is POST "/" to lightsail.<region>.amazonaws.com with
// X-Amz-Target: Lightsail_20161128.<Operation>; the operation name is the
// only routing information. Errors come back as HTTP 400 with a JSON body
// naming the exception type, which errors.go maps onto the normalized
// provider error codes.
package aws

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

	"github.com/qiangli/yoke/pkg/provider"
)

// ServiceLightsail is the SigV4 service name for Lightsail.
const ServiceLightsail = "lightsail"

// targetPrefix is the X-Amz-Target prefix: the Lightsail API's wire
// version (2016-11-28) is spelled 20161128 in the target.
const targetPrefix = "Lightsail_20161128."

// contentTypeJSON11 is the AWS JSON 1.1 protocol content type.
const contentTypeJSON11 = "application/x-amz-json-1.1"

// DefaultRegion is the region the adapter uses when neither the caller nor
// $AWS_REGION names one. us-west-2 is the only region the Sprint 522
// automation principal is allowed to act in.
const DefaultRegion = "us-west-2"

// lightsailEndpoint is the regional endpoint for a region.
func lightsailEndpoint(region string) string {
	return "https://lightsail." + region + ".amazonaws.com/"
}

// ---- Lightsail wire types (only the fields this adapter reads) ----

// Tag is one Lightsail resource tag. Lightsail allows a key with no value,
// which is how the bashy-lease marker tags are carried.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

type resourceLocation struct {
	AvailabilityZone string `json:"availabilityZone"`
	RegionName       string `json:"regionName"`
}

type instanceState struct {
	Code int    `json:"code"`
	Name string `json:"name"`
}

type instanceHardware struct {
	CPUCount    int     `json:"cpuCount"`
	RAMSizeInGb float64 `json:"ramSizeInGb"`
}

// instance is a Lightsail instance. Lightsail addresses instances by
// NAME, so the contract's Resource.ID is the instance name.
type instance struct {
	Name             string           `json:"name"`
	ARN              string           `json:"arn"`
	CreatedAt        float64          `json:"createdAt"` // Unix seconds, fractional
	Location         resourceLocation `json:"location"`
	BlueprintID      string           `json:"blueprintId"`
	BundleID         string           `json:"bundleId"`
	State            instanceState    `json:"state"`
	PublicIPAddress  string           `json:"publicIpAddress"`
	PrivateIPAddress string           `json:"privateIpAddress"`
	IPv6Addresses    []string         `json:"ipv6Addresses"`
	IsStaticIP       bool             `json:"isStaticIp"`
	Hardware         instanceHardware `json:"hardware"`
	Tags             []Tag            `json:"tags"`
}

// bundle is one Lightsail plan. Price is the monthly price in USD.
type bundle struct {
	BundleID             string   `json:"bundleId"`
	Name                 string   `json:"name"`
	InstanceType         string   `json:"instanceType"`
	Price                float64  `json:"price"`
	CPUCount             int      `json:"cpuCount"`
	RAMSizeInGb          float64  `json:"ramSizeInGb"`
	DiskSizeInGb         int      `json:"diskSizeInGb"`
	TransferPerMonthInGb int      `json:"transferPerMonthInGb"`
	IsActive             bool     `json:"isActive"`
	Power                int      `json:"power"`
	SupportedPlatforms   []string `json:"supportedPlatforms"`
	PublicIPv4Count      int      `json:"publicIpv4AddressCount"`
}

// blueprint is one Lightsail machine image.
type blueprint struct {
	BlueprintID string `json:"blueprintId"`
	Name        string `json:"name"`
	Group       string `json:"group"`
	Type        string `json:"type"`
	IsActive    bool   `json:"isActive"`
	Platform    string `json:"platform"`
	Version     string `json:"version"`
	MinPower    int    `json:"minPower"`
}

type region struct {
	Name              string `json:"name"`
	DisplayName       string `json:"displayName"`
	ContinentCode     string `json:"continentCode"`
	AvailabilityZones []struct {
		ZoneName string `json:"zoneName"`
		State    string `json:"state"`
	} `json:"availabilityZones"`
}

// staticIP is a Lightsail static (reserved) IP. Lightsail static IPs
// cannot carry tags, which is why destroy identifies ours by name.
type staticIP struct {
	Name       string           `json:"name"`
	ARN        string           `json:"arn"`
	IPAddress  string           `json:"ipAddress"`
	AttachedTo string           `json:"attachedTo"`
	IsAttached bool             `json:"isAttached"`
	Location   resourceLocation `json:"location"`
}

// operation is one asynchronous Lightsail operation record.
type operation struct {
	ID            string `json:"id"`
	ResourceName  string `json:"resourceName"`
	ResourceType  string `json:"resourceType"`
	Status        string `json:"status"`
	OperationType string `json:"operationType"`
	IsTerminal    bool   `json:"isTerminal"`
	ErrorCode     string `json:"errorCode"`
	ErrorDetails  string `json:"errorDetails"`
}

// portRule is one PutInstancePublicPorts rule.
type portRule struct {
	FromPort        int      `json:"fromPort"`
	ToPort          int      `json:"toPort"`
	Protocol        string   `json:"protocol"`
	CIDRs           []string `json:"cidrs,omitempty"`
	IPv6CIDRs       []string `json:"ipv6Cidrs,omitempty"`
	CIDRListAliases []string `json:"cidrListAliases,omitempty"`
}

// ---- state mapping ----

// State maps a Lightsail instance state name onto the normalized state.
// Lightsail's own names are reported verbatim in Resource.RawStatus.
func State(name string) provider.State {
	switch strings.ToLower(name) {
	case "pending", "rebooting":
		return provider.StatePending
	case "running":
		return provider.StateRunning
	case "stopping":
		// Normalized states have no "stopping": it is in flight, not yet
		// a resting state, so report it as pending rather than stopped.
		return provider.StatePending
	case "stopped":
		return provider.StateStopped
	case "shutting-down", "terminating", "deleting":
		return provider.StateDeleting
	case "terminated", "deleted":
		return provider.StateDeleted
	}
	return provider.StateUnknown
}

// ---- HTTP plumbing ----

// callError is a non-2xx AWS answer. Type is the exception name AWS
// reported (header x-amzn-ErrorType, body __type or code).
type callError struct {
	status int
	typ    string
	msg    string
}

func (e *callError) Error() string {
	if e.typ == "" {
		return fmt.Sprintf("aws: HTTP %d: %s", e.status, e.msg)
	}
	return fmt.Sprintf("aws: HTTP %d %s: %s", e.status, e.typ, e.msg)
}

// transportError is a failure with no HTTP answer: the request may or may
// not have reached AWS.
type transportError struct{ err error }

func (e *transportError) Error() string { return "aws: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// client is one signed AWS JSON endpoint (Lightsail, or Cost Explorer).
type client struct {
	endpoint   string
	region     string
	service    string
	prefix     string // X-Amz-Target prefix
	creds      Credentials
	httpClient *http.Client
	now        func() time.Time
}

// call POSTs one operation and decodes the answer into out (which may be
// nil to discard it).
func (c *client) call(ctx context.Context, op string, in, out any) error {
	body := []byte("{}")
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentTypeJSON11)
	req.Header.Set("X-Amz-Target", c.prefix+op)
	req.Header.Set("Accept", "application/json")
	// Lightsail's endpoint is a fixed host, but tests point the client at
	// an httptest server: sign the host actually being addressed.
	if u, perr := url.Parse(c.endpoint); perr == nil && u.Host != "" {
		req.Host = u.Host
	}
	SignRequest(req, body, c.creds, c.now().UTC(), c.region, c.service)

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
		return &callError{status: resp.StatusCode, typ: errorType(resp, data), msg: errorMessage(data)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("aws: decode %s: %w", op, err)
	}
	return nil
}

// errorType extracts the exception name. AWS JSON protocols put it in the
// x-amzn-ErrorType header and/or the body's __type; Lightsail additionally
// repeats it as "code".
func errorType(resp *http.Response, body []byte) string {
	if h := resp.Header.Get("X-Amzn-Errortype"); h != "" {
		// The header may be "Name:http://internal.amazon.com/..."
		name, _, _ := strings.Cut(h, ":")
		if n := shortExceptionName(name); n != "" {
			return n
		}
	}
	var e struct {
		Type string `json:"__type"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	if n := shortExceptionName(e.Type); n != "" {
		return n
	}
	return shortExceptionName(e.Code)
}

// shortExceptionName strips an AWS exception's namespace prefix
// ("com.amazon.coral.service#AccessDeniedException").
func shortExceptionName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '#'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// errorMessage extracts the human-readable message, accepting both
// capitalizations AWS uses.
func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Upper   string `json:"Message"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Message != "" {
		return e.Message
	}
	if e.Upper != "" {
		return e.Upper
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}
