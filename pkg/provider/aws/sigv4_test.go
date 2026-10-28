package aws

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Known-answer vectors from AWS's published Signature Version 4 test
// suite (aws-sig-v4-test-suite): the fixed example credentials, service
// "service" in us-east-1, at 20150830T123600Z. Each case pins all three
// stages — canonical request, string to sign, Authorization header — so a
// regression says which stage broke, not just "signature mismatch".
const (
	suiteAccessKey = "AKIDEXAMPLE"
	suiteSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	suiteRegion    = "us-east-1"
	suiteService   = "service"
)

func suiteTime(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse("20060102T150405Z", "20150830T123600Z")
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

type sigVector struct {
	name    string
	method  string
	url     string
	headers map[string]string
	body    string
	creq    string
	sts     string
	authz   string
}

func sigVectors() []sigVector {
	return []sigVector{
		{
			name:    "get-vanilla",
			method:  http.MethodGet,
			url:     "https://example.amazonaws.com/",
			headers: map[string]string{},
			creq: "GET\n/\n\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\n" +
				"host;x-amz-date\n" +
				EmptyPayloadHash,
			sts: "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n" +
				"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
			authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, " +
				"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name:    "post-vanilla",
			method:  http.MethodPost,
			url:     "https://example.amazonaws.com/",
			headers: map[string]string{},
			creq: "POST\n/\n\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\n" +
				"host;x-amz-date\n" +
				EmptyPayloadHash,
			sts: "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n" +
				"553f88c9e4d10fc9e109e2aeb65f030801b70c2f6468faca261d401ae622fc87",
			authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, " +
				"Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
		},
		{
			// The case that matters most here: a POST with a body, so
			// the payload hash and an extra signed header are both in play.
			name:    "post-x-www-form-urlencoded",
			method:  http.MethodPost,
			url:     "https://example.amazonaws.com/",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			body:    "Param1=value1",
			creq: "POST\n/\n\n" +
				"content-type:application/x-www-form-urlencoded\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\n" +
				"content-type;host;x-amz-date\n" +
				"9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
			sts: "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n" +
				"42a5e5bb34198acb3e84da4f085bb7927f2bc277ca766e6d19c73c2154021281",
			authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=content-type;host;x-amz-date, " +
				"Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
		},
		{
			// Query parameters must be sorted by key, not left in
			// request order.
			name:    "get-vanilla-query-order-key-case",
			method:  http.MethodGet,
			url:     "https://example.amazonaws.com/?Param2=value2&Param1=value1",
			headers: map[string]string{},
			creq: "GET\n/\nParam1=value1&Param2=value2\n" +
				"host:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\n" +
				"host;x-amz-date\n" +
				EmptyPayloadHash,
			sts: "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n" +
				"816cd5b414d056048ba4f7c5386d6e0533120fb1fcfa93762cf0fc39e2cf19e0",
			authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, " +
				"Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
	}
}

func TestSigV4KnownAnswerVectors(t *testing.T) {
	ts := suiteTime(t)
	creds := Credentials{AccessKeyID: suiteAccessKey, SecretAccessKey: suiteSecret}
	for _, v := range sigVectors() {
		t.Run(v.name, func(t *testing.T) {
			req, err := http.NewRequest(v.method, v.url, strings.NewReader(v.body))
			if err != nil {
				t.Fatal(err)
			}
			for k, val := range v.headers {
				req.Header.Set(k, val)
			}
			// X-Amz-Date is part of the canonical request, so stamp it
			// the way SignRequest does before comparing stages.
			req.Header.Set("X-Amz-Date", "20150830T123600Z")

			hash := EmptyPayloadHash
			if v.body != "" {
				hash = hexSHA256([]byte(v.body))
			}
			creq, _ := CanonicalRequest(req, hash)
			if creq != v.creq {
				t.Fatalf("canonical request mismatch\n got: %q\nwant: %q", creq, v.creq)
			}
			if got := StringToSign(creq, ts, suiteRegion, suiteService); got != v.sts {
				t.Fatalf("string to sign mismatch\n got: %q\nwant: %q", got, v.sts)
			}
			if got := AuthorizationHeader(req, []byte(v.body), creds, ts, suiteRegion, suiteService); got != v.authz {
				t.Fatalf("authorization mismatch\n got: %s\nwant: %s", got, v.authz)
			}
		})
	}
}

// SignRequest must produce the same header as AuthorizationHeader and must
// stamp X-Amz-Date itself.
func TestSignRequestStampsDateAndAuthorization(t *testing.T) {
	ts := suiteTime(t)
	req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", strings.NewReader("Param1=value1"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	SignRequest(req, []byte("Param1=value1"), Credentials{AccessKeyID: suiteAccessKey, SecretAccessKey: suiteSecret}, ts, suiteRegion, suiteService)
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, " +
		"Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %s, want %s", got, want)
	}
}

// A session token must be signed, which changes both SignedHeaders and the
// signature: temporary credentials that silently dropped the token would
// authenticate as the base principal.
func TestSignRequestSignsSessionToken(t *testing.T) {
	ts := suiteTime(t)
	mk := func(tok string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", nil)
		if err != nil {
			t.Fatal(err)
		}
		SignRequest(req, nil, Credentials{AccessKeyID: suiteAccessKey, SecretAccessKey: suiteSecret, SessionToken: tok}, ts, suiteRegion, suiteService)
		return req
	}
	withTok := mk("FQoDYXdzEXAMPLETOKEN")
	if got := withTok.Header.Get("X-Amz-Security-Token"); got != "FQoDYXdzEXAMPLETOKEN" {
		t.Fatalf("X-Amz-Security-Token = %q", got)
	}
	if !strings.Contains(withTok.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("session token not in SignedHeaders: %s", withTok.Header.Get("Authorization"))
	}
	if withTok.Header.Get("Authorization") == mk("").Header.Get("Authorization") {
		t.Fatal("session token did not change the signature")
	}
}

// The Go transport sets User-Agent, Content-Length and Accept-Encoding
// after this signer runs; signing them would break every real request.
func TestSignRequestExcludesTransportHeaders(t *testing.T) {
	ts := suiteTime(t)
	req, err := http.NewRequest(http.MethodPost, "https://example.amazonaws.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "whatever/1")
	req.Header.Set("Content-Length", "0")
	req.Header.Set("Accept-Encoding", "gzip")
	SignRequest(req, nil, Credentials{AccessKeyID: suiteAccessKey, SecretAccessKey: suiteSecret}, ts, suiteRegion, suiteService)
	authz := req.Header.Get("Authorization")
	for _, h := range []string{"user-agent", "content-length", "accept-encoding"} {
		if strings.Contains(authz, h) {
			t.Fatalf("%s must not be signed: %s", h, authz)
		}
	}
	if !strings.Contains(authz, "SignedHeaders=host;x-amz-date,") {
		t.Fatalf("SignedHeaders = %s, want host;x-amz-date", authz)
	}
}

// Credentials.Valid is the gate New uses; both halves are required.
func TestCredentialsValid(t *testing.T) {
	for _, tc := range []struct {
		c    Credentials
		want bool
	}{
		{Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}, true},
		{Credentials{AccessKeyID: " ", SecretAccessKey: "s"}, false},
		{Credentials{AccessKeyID: "AKIA"}, false},
		{Credentials{}, false},
	} {
		if got := tc.c.Valid(); got != tc.want {
			t.Fatalf("Valid(%q/%t) = %t, want %t", tc.c.AccessKeyID, tc.c.SecretAccessKey != "", got, tc.want)
		}
	}
}
