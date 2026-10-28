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
// ceiling, and past it WrapUserData returns an error naming both sizes.
// A caller that hits it has to stage the payload out of band rather than
// inline it, which is a decision for the caller, not a truncation here.
package aws

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"strings"
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

// WrapUserData returns the userData to send to Lightsail for script.
//
// A script within UserDataLimit is returned unchanged, so the common case
// is a byte-identical passthrough with no wrapper to go wrong. A larger
// script is gzip+base64'd into a POSIX sh bootstrap that writes it back to
// disk and execs it. A script that does not fit even compressed is an
// error, never a silent truncation.
func WrapUserData(script []byte) ([]byte, error) {
	if len(script) == 0 {
		return nil, nil
	}
	if len(script) <= UserDataLimit {
		return script, nil
	}
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(script); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	b64 := base64.StdEncoding.EncodeToString(gz.Bytes())
	wrapped := []byte(bootstrap(b64))
	if len(wrapped) > UserDataLimit {
		return nil, fmt.Errorf("aws: user-data script is %d bytes and still %d bytes after gzip+base64, over the %d byte limit",
			len(script), len(wrapped), UserDataLimit)
	}
	return wrapped, nil
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
