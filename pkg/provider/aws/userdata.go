// User-data passthrough and lossless wrapping (S522.2 item 6).
//
// The real caller is `bashy infra provision`, which hands the adapter the
// master's COMPLETE first-boot script as Spec.UserData (see
// pkg/provider/enroll). That script is ~25-35 KB. Lightsail's
// CreateInstances reference documents no length limit on userData, but
// Lightsail instances are EC2 instances underneath and EC2 enforces a hard
// 16 KB limit on raw user data at launch; a larger script does not fail
// loudly, it launches an instance that never initializes.
//
// So: a script that fits goes through BYTE-IDENTICAL (the thing the
// enrollment contract requires), and a script that does not is wrapped
// losslessly — gzip, then base64, inside a tiny POSIX sh bootstrap that
// decodes it and execs it with the original interpreter. Unwrap is the
// inverse and is what the round-trip test checks: whatever the master
// rendered is what runs on the host.
//
// What the wrapper can and cannot carry: base64 costs 4/3, so the gzip
// output has to fit in ~11.6 KiB. Shell text compresses 10x or better, so
// a 25-35 KB script of mostly shell wraps to 2-3 KB — plenty of room. The
// binding constraint is INCOMPRESSIBLE content: roughly 8 KiB of raw
// high-entropy bytes (keys, certificate chains, embedded blobs) is the
// ceiling, and past it the wrapper does not fit either.
//
// S3 STAGING (S522.2 rework 2). The live script turned out to be ~43 KB
// and still 21.7 KB after gzip+base64 — over the limit, and growing. So
// compression is no longer the last resort: when neither the raw script
// nor the wrapper fits, the adapter does what AWS documents for a large
// launch script and STAGES it in S3 (s3.go), launching instead with the
// bootstrap StagedBootstrap renders here: a few hundred bytes that fetch
// one presigned URL, verify the script's sha256, and exec it. Three
// layers, each used only when the one before it cannot fit:
//
//	raw  ->  gzip+base64 wrapper  ->  S3 object + fetch bootstrap
//
// Fit decides between the first two and reports whether either worked;
// the adapter takes the third when it did not. Nothing is ever truncated,
// and the script that runs on the host is byte-identical to what the
// master rendered in all three cases.
package aws

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// UserDataLimit is the raw user-data budget enforced by EC2 at launch and
// inherited by Lightsail: 16 KiB (16384 bytes). Nothing this adapter sends
// as userData may exceed it.
const UserDataLimit = 16 * 1024

// bootstrapMarker identifies a wrapper this package produced, so Unwrap
// never tries to decode a script the master wrote.
const bootstrapMarker = "# bashy-userdata-gzip-v1"

// payloadBegin / payloadEnd delimit the base64 payload in the wrapper.
const (
	payloadBegin = "__BASHY_USERDATA__"
	payloadEnd   = "__BASHY_USERDATA_END__"
)

// Fit returns the user-data to send INLINE for script, and whether it
// fits within UserDataLimit at all.
//
// A script within the limit is returned unchanged, so the common case is
// a byte-identical passthrough with no wrapper to go wrong. A larger
// script is gzip+base64'd into a POSIX sh bootstrap that writes it back to
// disk and execs it. When even that does not fit, Fit returns the wrapper
// it produced with fits=false: the caller stages the script in S3 instead
// (s3.go), and the returned bytes are useful only for saying how close it
// came.
//
// Fit never truncates and never errors: "does not fit" is an answer, not
// a failure, because there is a correct thing to do about it.
func Fit(script []byte) (data []byte, fits bool) {
	if len(script) == 0 {
		return nil, true
	}
	if len(script) <= UserDataLimit {
		return script, true
	}
	wrapped := []byte(bootstrap(encodePayload(script)))
	return wrapped, len(wrapped) <= UserDataLimit
}

// encodePayload is the wrapper's gzip+base64 payload. gzip.Writer only
// fails on a write error, and a bytes.Buffer has none, so there is no
// error to return.
func encodePayload(script []byte) string {
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	_, _ = zw.Write(script)
	_ = zw.Close()
	return base64.StdEncoding.EncodeToString(gz.Bytes())
}

// WrapUserData is Fit for a caller that must stay inline: it returns an
// error, naming both sizes, for a script that does not fit even
// compressed, rather than a silent truncation.
//
// The adapter does NOT use it on the create path any more (a script that
// does not fit is staged in S3, not refused); it stays because "inline or
// an explicit error" is the contract everything that is not the create
// path wants, and because the limit it enforces is the one thing about
// EC2 user data that no amount of compression changes.
func WrapUserData(script []byte) ([]byte, error) {
	data, fits := Fit(script)
	if !fits {
		return nil, fmt.Errorf("aws: user-data script is %d bytes and still %d bytes after gzip+base64, over the %d byte limit",
			len(script), len(data), UserDataLimit)
	}
	return data, nil
}

// bootstrap renders the decode-and-exec shim around a base64 payload. It
// uses only tools present on every Lightsail Linux blueprint (sh, base64,
// gzip) and never echoes the payload.
func bootstrap(b64 string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(bootstrapMarker + "\n")
	b.WriteString("# The complete first-boot script, gzip+base64 encoded: EC2 caps raw\n")
	b.WriteString("# user data at 16 KiB, so it is decoded here and run unchanged.\n")
	b.WriteString("set -eu\n")
	b.WriteString("out=/var/lib/cloud/bashy-firstboot.sh\n")
	b.WriteString("mkdir -p \"$(dirname \"$out\")\"\n")
	b.WriteString("sed -n '/^" + payloadBegin + "$/,/^" + payloadEnd + "$/p' \"$0\" \\\n")
	b.WriteString("  | sed '1d;$d' | base64 -d | gzip -dc > \"$out\"\n")
	b.WriteString("chmod 0700 \"$out\"\n")
	b.WriteString("exec \"$out\"\n")
	b.WriteString("exit 0\n")
	b.WriteString(payloadBegin + "\n")
	// Wrap the payload so no single line is unreasonably long.
	for i := 0; i < len(b64); i += 76 {
		end := min(i+76, len(b64))
		b.WriteString(b64[i:end])
		b.WriteByte('\n')
	}
	b.WriteString(payloadEnd + "\n")
	return b.String()
}

// Wrapped reports whether data is a wrapper this package produced.
func Wrapped(data []byte) bool {
	return bytes.Contains(data, []byte(bootstrapMarker))
}

// Unwrap is the inverse of WrapUserData: it returns the original script
// for a wrapper, and data unchanged for a passthrough.
func Unwrap(data []byte) ([]byte, error) {
	if !Wrapped(data) {
		return data, nil
	}
	s := string(data)
	i := strings.Index(s, payloadBegin+"\n")
	if i < 0 {
		return nil, fmt.Errorf("aws: wrapped user-data has no payload start marker")
	}
	rest := s[i+len(payloadBegin)+1:]
	j := strings.Index(rest, payloadEnd)
	if j < 0 {
		return nil, fmt.Errorf("aws: wrapped user-data has no payload end marker")
	}
	b64 := strings.Join(strings.Fields(rest[:j]), "")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("aws: wrapped user-data payload is not base64: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("aws: wrapped user-data payload is not gzip: %w", err)
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, fmt.Errorf("aws: wrapped user-data payload: %w", err)
	}
	return out.Bytes(), nil
}

// ---- S3 staging bootstrap (S522.2 rework 2) ----

// s3BootstrapMarker identifies a fetch bootstrap this package produced.
const s3BootstrapMarker = "# bashy-userdata-s3-v1"

// stagedScriptPath is where the fetched script lands on the host. It is
// the same path the gzip wrapper writes to, so an operator debugging a
// first boot looks in one place either way.
const stagedScriptPath = "/var/lib/cloud/bashy-firstboot.sh"

// StagedLogPath is where the bootstrap reports what it did. A first-boot
// failure has nobody watching it, so the one thing the bootstrap owes is
// a file that says why.
const StagedLogPath = "/var/log/dhnt-userdata.log"

// stagedFetchWindow is how long the bootstrap keeps retrying the fetch.
// A fresh instance's network is not always up when cloud-init runs the
// user data, so a single attempt would fail a host that is merely slow.
const stagedFetchWindow = 10 * time.Minute

// stagedRetryDelay is the wait between fetch attempts.
const stagedRetryDelay = 15 * time.Second

// StagedBootstrap renders the user data for a script staged in S3: fetch
// the presigned URL, verify its sha256, exec it.
//
// What it is allowed to contain is the whole security argument for this
// path, so it is worth stating. The bootstrap is visible to anyone who
// can read the instance's user data (any process on the host, the
// Lightsail console, an operator with lightsail:GetInstance). The only
// credential-derived thing in it is the presigned URL, which authorizes
// exactly one GET of one object and expires on its own; no access key,
// no secret, and no enrollment token ever appears here — the enrollment
// token lives in the STAGED script, which is what the URL fetches. The
// digest is what makes that fetch trustworthy: a wrong or tampered
// object does not run.
//
// It is POSIX sh and uses only what a Lightsail Linux blueprint has:
// curl or wget, sha256sum or shasum, date, sleep.
func StagedBootstrap(presignedURL, sha256hex string) (string, error) {
	if strings.TrimSpace(presignedURL) == "" {
		return "", fmt.Errorf("aws: staged user-data bootstrap needs a presigned URL")
	}
	// The URL goes inside single quotes, so a single quote (or any
	// whitespace) in it would break the script open. A SigV4 presigned
	// URL contains none; refuse rather than emit something unquoted.
	if strings.ContainsAny(presignedURL, "'\"\\ \t\r\n") {
		return "", fmt.Errorf("aws: presigned URL contains characters that cannot be quoted in a shell script")
	}
	if !hexDigest.MatchString(sha256hex) {
		return "", fmt.Errorf("aws: staged user-data bootstrap needs a hex sha256 digest, got %q", sha256hex)
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(s3BootstrapMarker + "\n")
	b.WriteString("# The complete first-boot script does not fit EC2's 16 KiB user-data\n")
	b.WriteString("# limit, so it is staged in S3. Fetch it, verify its digest, run it.\n")
	b.WriteString("set -u\n")
	b.WriteString("log=" + StagedLogPath + "\n")
	b.WriteString("out=" + stagedScriptPath + "\n")
	b.WriteString("sum=" + sha256hex + "\n")
	b.WriteString("url='" + presignedURL + "'\n")
	b.WriteString("say() { echo \"$(date -u +%Y-%m-%dT%H:%M:%SZ) dhnt-userdata: $*\" >>\"$log\" 2>/dev/null; }\n")
	b.WriteString("mkdir -p \"$(dirname \"$out\")\" /var/log\n")
	b.WriteString(fmt.Sprintf("deadline=$(( $(date +%%s) + %d ))\n", int(stagedFetchWindow.Seconds())))
	b.WriteString("while :; do\n")
	b.WriteString("  if command -v curl >/dev/null 2>&1; then\n")
	b.WriteString("    curl -fsS --max-time 120 -o \"$out\" \"$url\" && break\n")
	b.WriteString("  elif command -v wget >/dev/null 2>&1; then\n")
	b.WriteString("    wget -q -T 120 -O \"$out\" \"$url\" && break\n")
	b.WriteString("  else\n")
	b.WriteString("    say 'neither curl nor wget is installed; cannot fetch the first-boot script'\n")
	b.WriteString("    exit 1\n")
	b.WriteString("  fi\n")
	b.WriteString("  if [ \"$(date +%s)\" -ge \"$deadline\" ]; then\n")
	b.WriteString("    say 'could not fetch the staged first-boot script before the deadline; the presigned URL may have expired'\n")
	b.WriteString("    exit 1\n")
	b.WriteString("  fi\n")
	b.WriteString("  say 'fetch failed, retrying'\n")
	b.WriteString(fmt.Sprintf("  sleep %d\n", int(stagedRetryDelay.Seconds())))
	b.WriteString("done\n")
	// Digest check. Two tools, because a blueprint has one or the other.
	b.WriteString("got=$(sha256sum \"$out\" 2>/dev/null | cut -d' ' -f1)\n")
	b.WriteString("[ -n \"$got\" ] || got=$(shasum -a 256 \"$out\" 2>/dev/null | cut -d' ' -f1)\n")
	b.WriteString("if [ -z \"$got\" ]; then\n")
	b.WriteString("  say 'neither sha256sum nor shasum is installed; refusing to run an unverified script'\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	b.WriteString("if [ \"$got\" != \"$sum\" ]; then\n")
	b.WriteString("  say \"digest mismatch: expected $sum, got $got; refusing to run it\"\n")
	b.WriteString("  rm -f \"$out\"\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	b.WriteString("chmod 0700 \"$out\"\n")
	b.WriteString("say \"fetched and verified $out ($sum); running it\"\n")
	// The staged script is the master's, with its own shebang (bash, in
	// practice): exec it directly so it runs under the interpreter it
	// asked for. A script with no shebang gets sh, which is what exec
	// would otherwise refuse.
	b.WriteString("if head -c2 \"$out\" | grep -q '#!'; then\n")
	b.WriteString("  exec \"$out\"\n")
	b.WriteString("fi\n")
	b.WriteString("exec sh \"$out\"\n")
	return b.String(), nil
}

// hexDigest matches a hex sha256.
var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// StagedUserData reports whether data is a fetch bootstrap this package
// produced (as opposed to a passthrough or a gzip wrapper).
func StagedUserData(data []byte) bool {
	return bytes.Contains(data, []byte(s3BootstrapMarker))
}
