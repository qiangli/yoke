// SigV4 request signing, stdlib only (S522.2).
//
// pkg/provider is a nested stdlib-only module, so there is no AWS SDK here:
// this file is the whole signer. It implements AWS Signature Version 4 for
// the non-S3 services this adapter talks to (Lightsail and Cost Explorer,
// both application/x-amz-json-1.1 POSTs to "/"), and it is pinned by the
// published AWS SigV4 test-suite vectors in sigv4_test.go — canonical
// request, string to sign and Authorization header are all exported so a
// known-answer test can compare each stage, not just the end result.
//
// Credential material never reaches a log line or an error message here:
// the only thing this file writes into a header is the access key id (which
// AWS itself puts in the clear in Authorization) and the derived signature.
package aws

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Algorithm is the SigV4 algorithm name.
const Algorithm = "AWS4-HMAC-SHA256"

// terminator ends the credential scope.
const terminator = "aws4_request"

// EmptyPayloadHash is the hex SHA-256 of the empty body.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials is one set of AWS access keys. SessionToken is set only for
// temporary (STS) credentials.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Valid reports whether both halves of the key pair are present.
func (c Credentials) Valid() bool {
	return strings.TrimSpace(c.AccessKeyID) != "" && c.SecretAccessKey != ""
}

// unsignedHeaders are never signed: the Go transport adds or rewrites them
// after this signer runs, so including them would break every signature.
// AWS ignores headers that are not in SignedHeaders.
var unsignedHeaders = map[string]bool{
	"authorization":     true,
	"user-agent":        true,
	"content-length":    true,
	"expect":            true,
	"connection":        true,
	"accept-encoding":   true,
	"x-amzn-trace-id":   true,
	"transfer-encoding": true,
}

// uriEncode is RFC 3986 percent-encoding with the AWS unreserved set.
// encodeSlash is false only for path segments of services that keep "/".
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalPath is the request's absolute path, "/" when empty. The
// adapter's endpoints are all "/", and this signer is not used for S3, so
// the path is taken as already-encoded (EscapedPath) rather than
// re-normalized.
func canonicalPath(req *http.Request) string {
	p := req.URL.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

// canonicalQuery is the query string sorted by key then value, with both
// percent-encoded. A parameter with no value keeps an empty value.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		// The raw query is already percent-encoded on the wire; decode
		// nothing, but normalize the encoding of the pieces we know.
		out = append(out, uriEncode(decodePercent(k), true)+"="+uriEncode(decodePercent(v), true))
	}
	sort.Strings(out)
	return strings.Join(out, "&")
}

// decodePercent reverses percent-encoding without touching "+" (AWS does
// not treat "+" as a space in a canonical query string).
func decodePercent(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				b.Write(n)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// trimHeaderValue trims the value and collapses internal whitespace runs to
// one space, as the canonicalization rules require.
func trimHeaderValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// signedHeaderNames is the sorted, lowercased list of header names that go
// into the signature. "host" is always present, taken from the URL when the
// Host header is not set explicitly.
func signedHeaderNames(req *http.Request) []string {
	seen := map[string]bool{"host": true}
	for name := range req.Header {
		l := strings.ToLower(name)
		if unsignedHeaders[l] {
			continue
		}
		seen[l] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func headerValue(req *http.Request, lower string) string {
	if lower == "host" {
		if req.Host != "" {
			return req.Host
		}
		return req.URL.Host
	}
	return strings.Join(req.Header.Values(lower), ",")
}

// CanonicalRequest renders the canonical request for req with the given hex
// payload hash. It is exported so known-answer tests can pin this stage.
func CanonicalRequest(req *http.Request, payloadHash string) (canonical string, signedHeaders string) {
	names := signedHeaderNames(req)
	var hdr strings.Builder
	for _, n := range names {
		hdr.WriteString(n)
		hdr.WriteByte(':')
		hdr.WriteString(trimHeaderValue(headerValue(req, n)))
		hdr.WriteByte('\n')
	}
	signedHeaders = strings.Join(names, ";")
	canonical = strings.Join([]string{
		req.Method,
		canonicalPath(req),
		canonicalQuery(req.URL.RawQuery),
		hdr.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	return canonical, signedHeaders
}

// CredentialScope is "<yyyymmdd>/<region>/<service>/aws4_request".
func CredentialScope(t time.Time, region, service string) string {
	return strings.Join([]string{t.UTC().Format("20060102"), region, service, terminator}, "/")
}

// StringToSign renders the string to sign for a canonical request. It is
// exported so known-answer tests can pin this stage.
func StringToSign(canonicalRequest string, t time.Time, region, service string) string {
	return strings.Join([]string{
		Algorithm,
		t.UTC().Format("20060102T150405Z"),
		CredentialScope(t, region, service),
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")
}

// SigningKey derives the date/region/service/terminator signing key.
func SigningKey(secret string, t time.Time, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(t.UTC().Format("20060102")))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte(terminator))
}

// AuthorizationHeader computes the full Authorization header value for req.
// payload may be nil for an empty body.
func AuthorizationHeader(req *http.Request, payload []byte, c Credentials, t time.Time, region, service string) string {
	hash := EmptyPayloadHash
	if len(payload) > 0 {
		hash = hexSHA256(payload)
	}
	canonical, signed := CanonicalRequest(req, hash)
	sts := StringToSign(canonical, t, region, service)
	sig := hex.EncodeToString(hmacSHA256(SigningKey(c.SecretAccessKey, t, region, service), []byte(sts)))
	return fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		Algorithm, c.AccessKeyID, CredentialScope(t, region, service), signed, sig)
}

// SignRequest stamps X-Amz-Date (and X-Amz-Security-Token for temporary
// credentials) on req and sets its Authorization header. payload must be
// exactly the body the request will send.
func SignRequest(req *http.Request, payload []byte, c Credentials, t time.Time, region, service string) {
	req.Header.Set("X-Amz-Date", t.UTC().Format("20060102T150405Z"))
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	req.Header.Set("Authorization", AuthorizationHeader(req, payload, c, t, region, service))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- query-string (presigned URL) authentication ----
//
// A presigned URL moves the signature out of the Authorization header and
// into the query string, so something that cannot sign — curl on a host
// at first boot, with no credentials of its own — can still make ONE
// authorized request. S522.2 rework 2 uses it for exactly that: the
// instance's user-data bootstrap fetches the staged first-boot script
// from S3 with a presigned GET, so the only credential-derived material
// that reaches the VM is a signature over one object, scoped to one
// method, and expiring on its own.
//
// Two things differ from header signing. The payload hash is the literal
// UnsignedPayload (the signer never sees the body, and a GET has none),
// and only "host" is signed — a header the fetching client sends anyway.

// UnsignedPayload is the payload-hash placeholder a query-presigned
// request carries in place of a body hash.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// DefaultPresignTTL is how long a presigned URL stays valid when the
// caller names no expiry.
const DefaultPresignTTL = 15 * time.Minute

// MaxPresignTTL is the longest expiry AWS accepts for a SigV4 presigned
// URL (7 days). A longer request is clamped rather than signed into a URL
// the service would reject.
const MaxPresignTTL = 7 * 24 * time.Hour

// canonicalQueryParams renders a canonical query string from params:
// every key and value percent-encoded, the pairs sorted by their ENCODED
// form (which is what the specification sorts on, not the raw names).
func canonicalQueryParams(params map[string]string) string {
	out := make([]string, 0, len(params))
	for k, v := range params {
		out = append(out, uriEncode(k, true)+"="+uriEncode(v, true))
	}
	sort.Strings(out)
	return strings.Join(out, "&")
}

// presign computes the query-string signature for one request. It returns
// each stage — the canonical request, the canonical query (which is also
// the query the final URL carries) and the signature — so a known-answer
// test can pin them individually rather than only the end result.
func presign(method string, u *url.URL, c Credentials, t time.Time, region, service string, expires time.Duration) (canonical, query, signature string) {
	t = t.UTC()
	switch {
	case expires <= 0:
		expires = DefaultPresignTTL
	case expires > MaxPresignTTL:
		expires = MaxPresignTTL
	}
	// Any query parameters already on the URL are signed too, so a
	// caller may presign a request that carries its own.
	params := map[string]string{}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			params[k] = vs[0]
		}
	}
	params["X-Amz-Algorithm"] = Algorithm
	params["X-Amz-Credential"] = c.AccessKeyID + "/" + CredentialScope(t, region, service)
	params["X-Amz-Date"] = t.Format("20060102T150405Z")
	params["X-Amz-Expires"] = strconv.FormatInt(int64(expires/time.Second), 10)
	params["X-Amz-SignedHeaders"] = "host"
	if c.SessionToken != "" {
		params["X-Amz-Security-Token"] = c.SessionToken
	}
	query = canonicalQueryParams(params)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical = strings.Join([]string{
		method,
		path,
		query,
		"host:" + u.Host + "\n",
		"host",
		UnsignedPayload,
	}, "\n")
	signature = hex.EncodeToString(hmacSHA256(
		SigningKey(c.SecretAccessKey, t, region, service),
		[]byte(StringToSign(canonical, t, region, service))))
	return canonical, query, signature
}

// PresignURL returns u with SigV4 query-string authentication appended,
// valid for expires from t. Nothing but the signature is derived from the
// secret key: the URL carries the access key id (as AWS itself does), the
// scope, the expiry and the signature.
func PresignURL(method string, u *url.URL, c Credentials, t time.Time, region, service string, expires time.Duration) string {
	_, query, sig := presign(method, u, c, t, region, service, expires)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return u.Scheme + "://" + u.Host + path + "?" + query + "&X-Amz-Signature=" + sig
}

// PresignedSignature recomputes the signature a presigned URL should
// carry, for a verifier that has the same credentials: it strips the
// X-Amz-Signature parameter, re-signs what is left with the scope the URL
// itself names, and returns the expected hex signature alongside the one
// the URL claimed.
//
// It exists so a test (and the fake S3 in fakelightsail) can check a
// presigned URL the way S3 does — by recomputing it — instead of
// string-matching the generator's own output.
func PresignedSignature(method string, raw string, c Credentials) (want, got string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	got = q.Get("X-Amz-Signature")
	if got == "" {
		return "", "", fmt.Errorf("aws: presigned URL has no X-Amz-Signature")
	}
	amzDate := q.Get("X-Amz-Date")
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return "", "", fmt.Errorf("aws: presigned URL has no usable X-Amz-Date: %w", err)
	}
	cred := strings.Split(q.Get("X-Amz-Credential"), "/")
	if len(cred) != 5 {
		return "", "", fmt.Errorf("aws: presigned URL has no usable X-Amz-Credential")
	}
	region, service := cred[2], cred[3]
	secs, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil {
		return "", "", fmt.Errorf("aws: presigned URL has no usable X-Amz-Expires: %w", err)
	}
	// The session token is taken from the URL, not from c: it is part of
	// what was signed, so a verifier that substituted its own would
	// accept a URL whose token had been swapped.
	c.SessionToken = q.Get("X-Amz-Security-Token")
	// Re-sign what the URL carried apart from the signature itself: the
	// signer puts the X-Amz-* parameters back, and any others ride along.
	for _, k := range []string{"X-Amz-Signature", "X-Amz-Algorithm", "X-Amz-Credential",
		"X-Amz-Date", "X-Amz-Expires", "X-Amz-SignedHeaders", "X-Amz-Security-Token"} {
		q.Del(k)
	}
	bare := *u
	bare.RawQuery = q.Encode()
	_, _, want = presign(method, &bare, c, t, region, service, time.Duration(secs)*time.Second)
	return want, got, nil
}

// PresignExpired reports whether a presigned URL's own X-Amz-Date plus
// X-Amz-Expires is already in the past at now.
func PresignExpired(raw string, now time.Time) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	q := u.Query()
	t, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return true
	}
	secs, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil {
		return true
	}
	return now.UTC().After(t.Add(time.Duration(secs) * time.Second))
}
