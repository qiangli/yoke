package aws

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The staging bucket is named per account and region, and the object root
// is VIRTUAL-HOSTED in production — the form AWS documents for a
// presigned URL and the form the known-answer vector pins.
func TestUserDataBucketNameAndObjectRoot(t *testing.T) {
	if got, want := UserDataBucketName("123456789012", "us-west-2"), "dhnt-userdata-123456789012-us-west-2"; got != want {
		t.Fatalf("bucket = %q, want %q", got, want)
	}
	got := s3ObjectRoot("dhnt-userdata-123456789012-us-west-2", "us-west-2", "")
	want := "https://dhnt-userdata-123456789012-us-west-2.s3.us-west-2.amazonaws.com"
	if got != want {
		t.Fatalf("object root = %q, want %q", got, want)
	}
	// A test endpoint moves the bucket into the path (S3's path-style
	// addressing), so the fake sees which bucket it is being written to.
	if got := s3ObjectRoot("b", "us-west-2", "http://127.0.0.1:9/"); got != "http://127.0.0.1:9/b" {
		t.Fatalf("override root = %q", got)
	}
}

// A key is "<instance>/<16 hex>.sh", and two creates never collide: a
// replay must not overwrite the object an earlier, still-booting instance
// is fetching.
func TestStagingKeyShapeIsUniquePerCall(t *testing.T) {
	shape := regexp.MustCompile(`^zoo-aws/[0-9a-f]{16}\.sh$`)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		k, err := stagingKey("zoo-aws")
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(k) {
			t.Fatalf("key %q does not match %s", k, shape)
		}
		if seen[k] {
			t.Fatalf("key %q repeated", k)
		}
		seen[k] = true
	}
}

// presignedFixture is a presigned URL of the real shape, for the bootstrap
// tests. It is signed with AWS's documentation key pair, so nothing here
// resembles a real credential.
func presignedFixture(t *testing.T) string {
	t.Helper()
	c := &s3Client{
		root:   s3ObjectRoot("dhnt-userdata-123456789012-us-west-2", "us-west-2", ""),
		bucket: "dhnt-userdata-123456789012-us-west-2",
		region: "us-west-2",
		creds:  Credentials{AccessKeyID: vecPresignKeyID, SecretAccessKey: vecPresignSecret},
		now:    func() time.Time { return vecPresignTime() },
	}
	u, err := c.presignGet("zoo-aws/0123456789abcdef.sh", UserDataPresignTTL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The bootstrap is small, fetches, verifies and execs — and carries
// NOTHING credential-derived except the presigned URL itself.
func TestStagedBootstrapShape(t *testing.T) {
	url := presignedFixture(t)
	sum := sha256Hex([]byte("the complete first-boot script"))
	b, err := StagedBootstrap(url, sum)
	if err != nil {
		t.Fatal(err)
	}
	// "well under 16 KB" is the requirement; 4 KB is the budget this
	// pins, so a future edit cannot quietly grow it back toward the
	// limit the staging path exists to escape.
	if len(b) > 4096 {
		t.Fatalf("bootstrap is %d bytes, over the 4 KB budget", len(b))
	}
	if len(b) > UserDataLimit {
		t.Fatalf("bootstrap is %d bytes, over the %d byte user-data limit", len(b), UserDataLimit)
	}
	if !strings.HasPrefix(b, "#!/bin/sh\n") {
		t.Fatal("the bootstrap must start with a shebang cloud-init will run")
	}
	for _, want := range []string{
		s3BootstrapMarker, sum, url, StagedLogPath, stagedScriptPath,
		"curl", "wget", "sha256sum", "shasum", "exec ", "sleep ",
	} {
		if !strings.Contains(b, want) {
			t.Fatalf("bootstrap is missing %q:\n%s", want, b)
		}
	}
	if !StagedUserData([]byte(b)) {
		t.Fatal("the bootstrap must be recognizable as one")
	}
	if Wrapped([]byte(b)) {
		t.Fatal("a fetch bootstrap must not read as a gzip wrapper")
	}
	// The ONE credential-derived thing in it is the presigned URL. The
	// secret key never appears, and neither does anything that looks
	// like an inline key or token.
	if strings.Contains(b, vecPresignSecret) {
		t.Fatal("the bootstrap must never contain a secret access key")
	}
	// The URL is quoted exactly once, as a single-quoted literal.
	if !strings.Contains(b, "url='"+url+"'\n") {
		t.Fatal("the presigned URL must be a single-quoted shell literal")
	}
}

// A URL that cannot be safely quoted, or a digest that is not a hex
// sha256, is refused: emitting either would produce a script that runs
// something unverified (or does not parse at all).
func TestStagedBootstrapRefusesUnsafeInput(t *testing.T) {
	good := presignedFixture(t)
	sum := sha256Hex([]byte("x"))
	for _, bad := range []string{"", "https://h/k?a='b", "https://h/k b", "https://h/k\nrm -rf /"} {
		if _, err := StagedBootstrap(bad, sum); err == nil {
			t.Fatalf("URL %q must be refused", bad)
		}
	}
	for _, bad := range []string{"", "nothex", strings.ToUpper(sum), sum + "00"} {
		if _, err := StagedBootstrap(good, bad); err == nil {
			t.Fatalf("digest %q must be refused", bad)
		}
	}
}

// The bootstrap is POSIX sh, so it has to parse as POSIX sh. A script
// cloud-init cannot run is a host that silently never enrolls, and no
// byte-level assertion above can see a syntax error.
func TestStagedBootstrapIsValidPOSIXShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell to check against")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	b, err := StagedBootstrap(presignedFixture(t), sha256Hex([]byte("script")))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bootstrap.sh")
	if err := os.WriteFile(path, []byte(b), 0o700); err != nil {
		t.Fatal(err)
	}
	// -n parses without executing: nothing here runs curl or touches a
	// filesystem path the script names.
	out, err := exec.Command(sh, "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("the bootstrap is not valid POSIX sh: %v\n%s\n%s", err, out, b)
	}
}

// Fit is the three-layer decision: raw, then the gzip wrapper, then
// "does not fit" — which is the signal to stage in S3 and never a
// truncation.
func TestFitPicksTheSmallestLayerThatWorks(t *testing.T) {
	small := script(4096)[:4096]
	if got, fits := Fit(small); !fits || string(got) != string(small) {
		t.Fatal("a script within the limit must go through raw")
	}
	compressible := script(43 * 1024)
	got, fits := Fit(compressible)
	if !fits {
		t.Fatalf("a 43 KB shell script compresses easily and must still fit (got %d bytes)", len(got))
	}
	if !Wrapped(got) {
		t.Fatal("an oversize but compressible script must take the gzip wrapper")
	}
	back, err := Unwrap(got)
	if err != nil || string(back) != string(compressible) {
		t.Fatalf("the wrapper must round trip: %v", err)
	}
	// The live case: ~43 KB with enough incompressible content that the
	// wrapper is still over the limit. This is what sent S522.2 to S3.
	got, fits = Fit(realisticOversizeScript(t))
	if fits {
		t.Fatalf("a script that is still %d bytes wrapped must NOT report as fitting", len(got))
	}
	if n := len(got); n <= UserDataLimit {
		t.Fatalf("a non-fitting answer should report the oversize wrapper it produced, got %d bytes", n)
	}
	// WrapUserData, the inline-only contract, still names both sizes.
	if _, err := WrapUserData(realisticOversizeScript(t)); err == nil {
		t.Fatal("WrapUserData must refuse what cannot go inline")
	} else if !strings.Contains(err.Error(), "after gzip+base64") {
		t.Fatalf("error should name both sizes: %v", err)
	}
}

// realisticOversizeScript is a ~43 KB first-boot script with enough
// high-entropy content (keys, certificates, an embedded blob) that
// gzip+base64 cannot fit it in 16 KiB — the shape of the live master
// script that made the S3 staging path necessary.
func realisticOversizeScript(t *testing.T) []byte {
	t.Helper()
	raw := make([]byte, 16*1024)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n")
	enc := base64.StdEncoding.EncodeToString(raw)
	for i := 0; i < len(enc); i += 76 {
		b.WriteString(enc[i:min(i+76, len(enc))] + "\n")
	}
	for i := 0; b.Len() < 43*1024; i++ {
		fmt.Fprintf(&b, "printf '%%s\\n' 'first-boot step %d: install, verify, enroll'\n", i)
	}
	return []byte(b.String())
}
