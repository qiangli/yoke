// STS GetCallerIdentity (S522.2 rework 1): the account id the Budgets API
// needs.
//
// Budgets addresses a budget by (AccountId, BudgetName), and the account id
// is not in the credentials — so it has to be asked for. STS answers that
// with GetCallerIdentity, which every authenticated principal may call: it
// requires no IAM permission at all, so adding this call cannot be the
// thing that denies a preflight.
//
// STS is the ONE service this adapter talks to that is not
// application/x-amz-json-1.1: it speaks the older AWS QUERY protocol —
// form-encoded request, XML answer, an "Action" parameter instead of an
// X-Amz-Target header. That is why this file has its own tiny client
// instead of reusing lightsail.go's. The SIGNER is shared, unchanged: the
// canonical request does not care what the body means, only what bytes it
// is.
//
// The answer is cached for the adapter's lifetime. An account id does not
// change, and a create already makes enough calls.
package aws

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ServiceSTS is the SigV4 service name for STS.
const ServiceSTS = "sts"

// STSRegion is the global STS endpoint's signing region. It is NOT the
// instance's region.
const STSRegion = "us-east-1"

// STSEndpoint is the global STS endpoint.
const STSEndpoint = "https://sts.amazonaws.com/"

// stsAPIVersion is the GetCallerIdentity API version the query protocol
// requires as a parameter.
const stsAPIVersion = "2011-06-15"

// contentTypeForm is the AWS query protocol's content type.
const contentTypeForm = "application/x-www-form-urlencoded; charset=utf-8"

// queryClient is one signed AWS QUERY-protocol endpoint (STS).
type queryClient struct {
	endpoint   string
	region     string
	service    string
	version    string
	creds      Credentials
	httpClient *http.Client
	now        func() time.Time
}

// call POSTs one Action and decodes the XML answer into out.
func (c *queryClient) call(ctx context.Context, action string, out any) error {
	form := url.Values{"Action": {action}, "Version": {c.version}}
	body := []byte(form.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentTypeForm)
	req.Header.Set("Accept", "application/xml")
	if u, perr := url.Parse(c.endpoint); perr == nil && u.Host != "" {
		req.Host = u.Host
	}
	SignRequest(req, body, c.creds, c.now().UTC(), c.region, c.service)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &transportError{err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &transportError{err: err}
	}
	if resp.StatusCode/100 != 2 {
		typ, msg := queryError(data)
		if typ == "" {
			typ = errorType(resp, data)
		}
		if msg == "" {
			msg = errorMessage(data)
		}
		return &callError{status: resp.StatusCode, typ: typ, msg: msg}
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("aws: decode %s: %w", action, err)
	}
	return nil
}

// queryError reads the query protocol's XML error envelope. It returns
// empty strings for a body that is not one, so the caller can fall back to
// the JSON shapes.
func queryError(body []byte) (typ, msg string) {
	var e struct {
		XMLName xml.Name
		Error   struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) != nil {
		return "", ""
	}
	typ, msg = e.Error.Code, e.Error.Message
	if typ == "" {
		typ, msg = e.Code, e.Message
	}
	return shortExceptionName(typ), strings.TrimSpace(msg)
}

// callerIdentity is the GetCallerIdentity answer.
type callerIdentity struct {
	XMLName xml.Name `xml:"GetCallerIdentityResponse"`
	Result  struct {
		Account string `xml:"Account"`
		ARN     string `xml:"Arn"`
		UserID  string `xml:"UserId"`
	} `xml:"GetCallerIdentityResult"`
}

// accountCache memoizes one account id lookup. Only a SUCCESSFUL lookup
// is cached: a cancelled context or a transient STS failure must not
// poison every later preflight on the same adapter.
type accountCache struct {
	mu sync.Mutex
	id string
}

// AccountID returns the account id the credentials belong to, from
// Config.AccountID when the caller supplied one and otherwise from STS
// GetCallerIdentity (asked at most once per adapter).
//
// Only the ACCOUNT id is kept. The caller ARN and user id in the same
// answer name the principal, which is credential-adjacent material this
// package has no use for, so they are dropped here rather than stored
// where a later error message could pick them up.
func (a *adapter) AccountID(ctx context.Context) (string, error) {
	if id := strings.TrimSpace(a.cfg.AccountID); id != "" {
		return id, nil
	}
	a.account.mu.Lock()
	defer a.account.mu.Unlock()
	if a.account.id != "" {
		return a.account.id, nil
	}
	var out callerIdentity
	if err := a.sts.call(ctx, "GetCallerIdentity", &out); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.Result.Account)
	if id == "" {
		return "", fmt.Errorf("aws: sts GetCallerIdentity returned no account id")
	}
	a.account.id = id
	return id, nil
}
