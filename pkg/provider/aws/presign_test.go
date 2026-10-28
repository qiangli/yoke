package aws

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// SigV4 QUERY-STRING (presigned URL) known-answer test.
//
// The vector is AWS's own published one for a presigned S3 GET
// ("Create a presigned URL for Amazon S3 using an AWS SDK" / the
// sigv4-query-string-auth signature example): the documentation states
// the canonical request, its hash, and the final signature, and all three
// are checked here rather than only the end result — a signer that gets
// the right answer from the wrong canonical request is a signer that will
// break on the next parameter.
const (
	vecPresignKeyID  = "AKIAIOSFODNN7EXAMPLE"
	vecPresignSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	vecPresignURL    = "https://examplebucket.s3.amazonaws.com/test.txt"
	// The documented canonical request for this vector.
	vecPresignCanonical = "GET\n" +
		"/test.txt\n" +
		"X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host\n" +
		"host:examplebucket.s3.amazonaws.com\n" +
		"\n" +
		"host\n" +
		"UNSIGNED-PAYLOAD"
	// The documented hash of that canonical request.
	vecPresignCanonicalHash = "3bfa292879f6447bbcda7001decf97f4a54dc650c8942174ae0a9121cf58ad04"
	// The documented signature.
	vecPresignSignature = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
)

func vecPresignTime() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }

func TestPresignMatchesTheAWSVector(t *testing.T) {
	u, err := url.Parse(vecPresignURL)
	if err != nil {
		t.Fatal(err)
	}
	creds := Credentials{AccessKeyID: vecPresignKeyID, SecretAccessKey: vecPresignSecret}
	canonical, query, sig := presign("GET", u, creds, vecPresignTime(), "us-east-1", ServiceS3, 24*time.Hour)
	if canonical != vecPresignCanonical {
		t.Fatalf("canonical request:\n got %q\nwant %q", canonical, vecPresignCanonical)
	}
	if got := hexSHA256([]byte(canonical)); got != vecPresignCanonicalHash {
		t.Fatalf("canonical request hash = %s, want %s", got, vecPresignCanonicalHash)
	}
	if sig != vecPresignSignature {
		t.Fatalf("signature = %s, want %s", sig, vecPresignSignature)
	}
	// The canonical query is also the query the URL carries, so the
	// signature covers exactly what the client will send.
	full := PresignURL("GET", u, creds, vecPresignTime(), "us-east-1", ServiceS3, 24*time.Hour)
	want := vecPresignURL + "?" + query + "&X-Amz-Signature=" + vecPresignSignature
	if full != want {
		t.Fatalf("presigned URL:\n got %s\nwant %s", full, want)
	}
}

// A verifier with the same credentials recomputes the same signature from
// the URL alone — which is what S3 does, and what the fake S3 in
// fakelightsail does.
func TestPresignedSignatureRecomputesFromTheURL(t *testing.T) {
	u, _ := url.Parse(vecPresignURL)
	creds := Credentials{AccessKeyID: vecPresignKeyID, SecretAccessKey: vecPresignSecret}
	full := PresignURL("GET", u, creds, vecPresignTime(), "us-east-1", ServiceS3, 24*time.Hour)
	want, got, err := PresignedSignature("GET", full, creds)
	if err != nil {
		t.Fatal(err)
	}
	if want != got || want != vecPresignSignature {
		t.Fatalf("recomputed %s, URL carried %s, want %s", want, got, vecPresignSignature)
	}
	// A tampered path must not verify: the signature covers the object.
	tampered := strings.Replace(full, "/test.txt?", "/secret.txt?", 1)
	want, got, err = PresignedSignature("GET", tampered, creds)
	if err != nil {
		t.Fatal(err)
	}
	if want == got {
		t.Fatal("a presigned URL whose object was swapped must not verify")
	}
	// So must a tampered method: the signature covers the verb too.
	want, got, err = PresignedSignature("PUT", full, creds)
	if err != nil {
		t.Fatal(err)
	}
	if want == got {
		t.Fatal("a presigned GET must not authorize a PUT")
	}
}

// Temporary credentials put the session token in the query string, and it
// is SIGNED (it is part of the canonical query), so it cannot be swapped.
func TestPresignCarriesAndSignsASessionToken(t *testing.T) {
	u, _ := url.Parse(vecPresignURL)
	creds := Credentials{AccessKeyID: vecPresignKeyID, SecretAccessKey: vecPresignSecret, SessionToken: "FQoGZXIvYXdzEBYa/EXAMPLE"}
	full := PresignURL("GET", u, creds, vecPresignTime(), "us-east-1", ServiceS3, time.Hour)
	if !strings.Contains(full, "X-Amz-Security-Token=FQoGZXIvYXdzEBYa%2FEXAMPLE") {
		t.Fatalf("the session token must be in the query, percent-encoded: %s", full)
	}
	want, got, err := PresignedSignature("GET", full, creds)
	if err != nil || want != got {
		t.Fatalf("a presigned URL with a session token must verify: %v %s %s", err, want, got)
	}
	// The same URL with a different token does not verify.
	swapped := strings.Replace(full, "FQoGZXIvYXdzEBYa%2FEXAMPLE", "SOMEOTHERTOKEN", 1)
	want, got, _ = PresignedSignature("GET", swapped, creds)
	if want == got {
		t.Fatal("the session token is part of the canonical query and must be signed")
	}
}

// The expiry is the URL's own, and AWS's 7-day maximum is clamped rather
// than signed into a URL the service would reject.
func TestPresignExpiry(t *testing.T) {
	u, _ := url.Parse(vecPresignURL)
	creds := Credentials{AccessKeyID: vecPresignKeyID, SecretAccessKey: vecPresignSecret}
	at := vecPresignTime()
	full := PresignURL("GET", u, creds, at, "us-east-1", ServiceS3, 30*time.Minute)
	if !strings.Contains(full, "X-Amz-Expires=1800") {
		t.Fatalf("expiry not in the URL: %s", full)
	}
	if PresignExpired(full, at.Add(29*time.Minute)) {
		t.Fatal("a URL inside its window must not read as expired")
	}
	if !PresignExpired(full, at.Add(31*time.Minute)) {
		t.Fatal("a URL past its window must read as expired")
	}
	// Zero means the default, and over the maximum is clamped.
	if d := PresignURL("GET", u, creds, at, "us-east-1", ServiceS3, 0); !strings.Contains(d, "X-Amz-Expires=900") {
		t.Fatalf("zero expiry should take the default: %s", d)
	}
	if c := PresignURL("GET", u, creds, at, "us-east-1", ServiceS3, 30*24*time.Hour); !strings.Contains(c, "X-Amz-Expires=604800") {
		t.Fatalf("an over-long expiry should clamp to 7 days: %s", c)
	}
	if PresignExpired("not a url at all %%", at) != true {
		t.Fatal("an unparseable URL must read as expired, not as valid")
	}
}
