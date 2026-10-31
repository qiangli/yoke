// The S3 object client (S522.2 rework 2): the three calls the user-data
// staging path needs, spoken directly over net/http and signed by
// sigv4.go.
//
// WHY S3 IS HERE AT ALL. EC2 caps user data at 16 KiB and the master's
// first-boot script is ~43 KB and growing; it does not fit raw and it does
// not fit compressed either (see userdata.go). So the adapter does what
// AWS's own documentation prescribes for a large launch script: stage the
// script in S3 and launch with a tiny bootstrap that fetches it. That
// bootstrap runs on a host with no credentials, which is what the
// presigned GET in sigv4.go is for.
//
// S3's protocol is not the JSON one the rest of this adapter speaks: the
// object is the URL, the verb is the operation, the body is the object,
// and errors come back as XML. That is why this file has its own tiny
// client rather than reusing lightsail.go's. The SIGNER is shared and
// unchanged.
//
// WHAT THE IAM POLICY ALLOWS, and therefore what this file may do:
// s3:PutObject / s3:GetObject / s3:DeleteObject on
// dhnt-userdata-<accountId>-<region>/* plus s3:ListBucket on that bucket
// for the prerequisite HeadBucket check; no CreateBucket. This client
// never lists and never creates a bucket:
// the bucket is provisioned out of band (private, public access blocked,
// SSE default, a lifecycle rule expiring objects after one day), and the
// key is remembered on the instance as a TAG, avoiding an object listing.
package aws

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// ServiceS3 is the SigV4 service name for S3.
const ServiceS3 = "s3"

// userDataContentType is what the staged script is stored as. It is
// advisory — the bootstrap fetches bytes and checks a digest — but a
// correct type keeps the object readable in the console.
const userDataContentType = "text/x-shellscript"

// sseAES256 is the server-side-encryption header the staging bucket's
// default already applies. It is sent explicitly anyway so the object is
// encrypted even if the bucket default is ever changed, and so a bucket
// policy that REQUIRES the header accepts the write.
const sseAES256 = "AES256"

// UserDataBucketPrefix is the staging bucket's name prefix: the full
// default name is "dhnt-userdata-<accountId>-<region>".
const UserDataBucketPrefix = "dhnt-userdata-"

// EnvUserDataBucket overrides the staging bucket from the environment.
const EnvUserDataBucket = "AWS_USERDATA_BUCKET"

// UserDataPresignTTL is how long the bootstrap's presigned GET stays
// valid. The bootstrap retries for up to 10 minutes, so 30 gives a slow
// first boot room without leaving a long-lived URL in the instance's
// user data.
const UserDataPresignTTL = 30 * time.Minute

// UserDataKeySuffix is the extension staged objects carry.
const UserDataKeySuffix = ".sh"

// s3Client is one S3 bucket, addressed by its object root.
type s3Client struct {
	// root is everything before the key: the virtual-hosted bucket
	// endpoint in production, or <override>/<bucket> under a test
	// endpoint (see s3ObjectRoot).
	root       string
	bucket     string
	region     string
	creds      Credentials
	httpClient *http.Client
	now        func() time.Time
}

// s3ObjectRoot is the URL prefix every key hangs off.
//
// Production is VIRTUAL-HOSTED addressing —
// https://<bucket>.s3.<region>.amazonaws.com — which is the form AWS
// documents for a presigned URL and the form the known-answer test pins.
// When override names a test endpoint, the bucket becomes the first path
// segment instead (S3's path-style addressing): the fake then sees the
// bucket name it is being asked to write to, which a host-based scheme
// would hide behind an httptest port.
func s3ObjectRoot(bucket, region, override string) string {
	if o := strings.TrimRight(strings.TrimSpace(override), "/"); o != "" {
		return o + "/" + bucket
	}
	return "https://" + bucket + ".s3." + region + ".amazonaws.com"
}

// objectURL is the absolute URL of one key under this bucket.
func (c *s3Client) objectURL(key string) (*url.URL, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("aws: s3: empty object key")
	}
	u, err := url.Parse(c.root + "/" + strings.TrimPrefix(key, "/"))
	if err != nil {
		return nil, fmt.Errorf("aws: s3: bad object url: %w", err)
	}
	return u, nil
}

// do signs and runs one S3 request, reading and discarding the answer.
func (c *s3Client) do(ctx context.Context, method, key string, body []byte, headers map[string]string) error {
	u, err := c.objectURL(key)
	if method == http.MethodHead && key == "" {
		u, err = url.Parse(c.root + "/")
	}
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// S3 requires the payload hash as a SIGNED header, not only inside
	// the signature: a request without it is rejected before the
	// signature is even compared.
	hash := EmptyPayloadHash
	if len(body) > 0 {
		hash = hexSHA256(body)
	}
	req.Header.Set("X-Amz-Content-Sha256", hash)
	req.Host = u.Host
	SignRequest(req, body, c.creds, c.now().UTC(), c.region, ServiceS3)

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
		return &callError{status: resp.StatusCode, typ: s3ErrorType(resp, data), msg: s3ErrorMessage(data)}
	}
	return nil
}

// putObject stores body at key with SSE-S3 encryption.
func (c *s3Client) putObject(ctx context.Context, key string, body []byte, contentType string) error {
	return c.do(ctx, http.MethodPut, key, body, map[string]string{
		"Content-Type":                 contentType,
		"X-Amz-Server-Side-Encryption": sseAES256,
	})
}

// deleteObject removes key. S3's DeleteObject is idempotent: deleting a
// key that is not there succeeds, so a replayed destroy is not an error.
func (c *s3Client) deleteObject(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, key, nil, nil)
}

// presignGet returns a URL that fetches key with no credentials, valid
// for ttl. It performs no request: presigning is local arithmetic.
func (c *s3Client) presignGet(key string, ttl time.Duration) (string, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	return PresignURL(http.MethodGet, u, c.creds, c.now().UTC(), c.region, ServiceS3, ttl), nil
}

// s3ErrorType reads S3's XML error code, falling back to the JSON/header
// shapes the other services use.
func s3ErrorType(resp *http.Response, body []byte) string {
	if typ, _ := queryError(body); typ != "" {
		return typ
	}
	return errorType(resp, body)
}

// s3ErrorMessage reads S3's XML error message, falling back to the JSON
// shapes. A DELETE or PUT answer with no body at all (S3 does that for
// some 403s) reports the status alone, which callError renders.
func s3ErrorMessage(body []byte) string {
	if _, msg := queryError(body); msg != "" {
		return msg
	}
	return errorMessage(body)
}

// UserDataBucketName is the default staging bucket for an account and
// region: one bucket per (account, region), named so an operator reading
// the console can tell what it is for.
func UserDataBucketName(accountID, region string) string {
	return UserDataBucketPrefix + accountID + "-" + region
}

// stagingKey is the object key for one instance's staged script:
// "<instance>/<16 hex>.sh". The random half means a replayed create
// writes a NEW object rather than overwriting the one an earlier,
// still-booting instance is fetching, and the instance prefix is what
// makes an operator's console view legible.
func stagingKey(instance string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("aws: s3: no randomness for an object key: %w", err)
	}
	return instance + "/" + hex.EncodeToString(b[:]) + UserDataKeySuffix, nil
}

// sha256Hex is the hex digest the bootstrap verifies the fetched script
// against.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- the adapter's staging decision ----

// preparedUserData is what one create will send as userData, plus the
// record of anything it staged to get there.
type preparedUserData struct {
	// Inline is the userData the CreateInstances request carries.
	Inline []byte
	// Bucket and Key name the staged object, when there is one. Key is
	// empty for a plan, which resolves the bucket but writes nothing.
	Bucket, Key string
	// SHA256 is the staged script's digest, the one the bootstrap checks.
	SHA256 string
}

// Staged reports whether this user data fetches a staged object rather
// than carrying the script itself.
func (p preparedUserData) Staged() bool { return p.Bucket != "" }

// prepareUserData settles what goes on the wire as userData for script.
//
// Three layers, each used only when the one before it does not fit (see
// userdata.go): the script raw, the gzip+base64 wrapper, and — new in
// S522.2 rework 2, because the live script is ~43 KB and 21.7 KB
// compressed — an S3 object fetched by a presigned-GET bootstrap.
//
// stage=false renders the same answer WITHOUT writing the object, which
// is what a plan/--dry-run wants: it resolves the bucket (so the report
// names it) and presigns a representative key of the same shape, so the
// reported inline size is the size the create would really send, but
// nothing is allocated and no key is recorded.
//
// A staged object is deliberately NOT cleaned up when the create that
// follows fails: the instance may exist anyway (a lost answer reconciles
// by idempotency tag) and may still be fetching. The bucket's one-day
// lifecycle rule is what collects an object no instance ever read.
func (a *adapter) prepareUserData(ctx context.Context, name string, script []byte, stage bool) (preparedUserData, error) {
	if len(script) == 0 {
		return preparedUserData{}, nil
	}
	if inline, fits := Fit(script); fits {
		return preparedUserData{Inline: inline}, nil
	}
	cl, err := a.stagingClient(ctx)
	if err != nil {
		return preparedUserData{}, err
	}
	key, err := stagingKey(name)
	if err != nil {
		return preparedUserData{}, err
	}
	sum := sha256Hex(script)
	if stage {
		if err := cl.putObject(ctx, key, script, userDataContentType); err != nil {
			// The normalized code is kept (a denied write is a
			// credential problem, a 5xx is retryable) and the message is
			// rebuilt around it, because "HTTP 403 AccessDenied" on its
			// own does not say which call was denied or why the adapter
			// was making it.
			pe := normalize(err)
			return preparedUserData{}, &provider.Error{Code: pe.Code, Retryable: pe.Retryable,
				Message: fmt.Sprintf("aws: staging the %d byte first-boot script in s3://%s/%s: %s",
					len(script), cl.bucket, key, pe.Message)}
		}
	}
	signed, err := cl.presignGet(key, a.cfg.PresignTTL)
	if err != nil {
		return preparedUserData{}, err
	}
	boot, err := StagedBootstrap(signed, sum)
	if err != nil {
		return preparedUserData{}, err
	}
	if len(boot) > UserDataLimit {
		// Unreachable with a presigned URL of the documented shape, and
		// checked anyway: an oversize bootstrap would launch a host that
		// silently never initializes.
		return preparedUserData{}, &provider.Error{Code: provider.ErrInvalidRequest, Message: fmt.Sprintf(
			"aws: the s3 fetch bootstrap is %d bytes, over the %d byte user-data limit", len(boot), UserDataLimit)}
	}
	out := preparedUserData{Inline: []byte(boot), Bucket: cl.bucket, SHA256: sum}
	if stage {
		out.Key = key
	}
	return out, nil
}

// deleteStagedUserData removes the staged script recorded on a
// destroyed instance. It returns a note for the cost report when the
// delete did not happen, and "" when there was nothing to do or it
// succeeded.
//
// A failure here costs nothing ongoing — the bucket expires objects after
// a day — so it is reported rather than raised: the instance is already
// gone, and failing the destroy over a leftover object would be worse
// than saying so.
func (a *adapter) deleteStagedUserData(ctx context.Context, tags []string) string {
	key := UserDataKey(tags)
	if key == "" {
		return ""
	}
	cl, err := a.stagingClient(ctx)
	if err != nil {
		return fmt.Sprintf("the staged first-boot script %q could not be deleted (%v); the bucket's one-day lifecycle rule will expire it", key, err)
	}
	if err := cl.deleteObject(ctx, key); err != nil {
		return fmt.Sprintf("the staged first-boot script s3://%s/%s could not be deleted (%v); the bucket's one-day lifecycle rule will expire it",
			cl.bucket, key, normalize(err))
	}
	return ""
}
