package aws

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// script renders a plausible master-rendered first-boot script of at least
// n bytes, with the kinds of bytes a real one carries (quotes,
// backslashes, non-ASCII, embedded base64, a trailing newline).
func script(n int) []byte {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\numask 077\n")
	b.WriteString("MASTER_KEY='ed25519:AAAAC3NzaC1lZDI1NTE5AAAAIB+/=='\n")
	b.WriteString("echo \"enrolling naïve satellite \\$HOME \\\" ' \\\\\"\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "printf '%%s\\n' 'first-boot step %d: install, verify, enroll'\n", i)
	}
	return []byte(b.String())
}

// A script that fits goes through byte-identical: the enrollment contract
// requires exactly what the master rendered, and a wrapper that is not
// needed is a wrapper that can go wrong.
func TestWrapUserDataPassesSmallScriptsThrough(t *testing.T) {
	for _, size := range []int{1, 100, 4096, UserDataLimit - 1, UserDataLimit} {
		s := script(size)[:size]
		got, err := WrapUserData(s)
		if err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		if !bytes.Equal(got, s) {
			t.Fatalf("%d bytes was altered despite fitting", size)
		}
		if Wrapped(got) {
			t.Fatalf("%d bytes was wrapped despite fitting", size)
		}
	}
	if got, err := WrapUserData(nil); err != nil || got != nil {
		t.Fatalf("empty user data: %v %v", got, err)
	}
}

// A script over EC2's launch limit is wrapped, fits, and round-trips
// exactly. The sizes bracket the real enrollment script (~25-35 KB).
func TestWrapUserDataRoundTripsOversizeScripts(t *testing.T) {
	for _, size := range []int{UserDataLimit + 1, 25 * 1024, 30 * 1024, 35 * 1024, 200 * 1024} {
		s := script(size)
		wrapped, err := WrapUserData(s)
		if err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		if len(wrapped) > UserDataLimit {
			t.Fatalf("%d bytes wrapped to %d, over the %d byte limit", len(s), len(wrapped), UserDataLimit)
		}
		if !Wrapped(wrapped) {
			t.Fatalf("%d bytes was not wrapped", len(s))
		}
		if !bytes.HasPrefix(wrapped, []byte("#!/bin/sh\n")) {
			t.Fatalf("the wrapper must start with a shebang cloud-init will run, got %q", wrapped[:20])
		}
		back, err := Unwrap(wrapped)
		if err != nil {
			t.Fatalf("%d bytes: unwrap: %v", len(s), err)
		}
		if !bytes.Equal(back, s) {
			t.Fatalf("%d bytes did not round trip (%d bytes back)", len(s), len(back))
		}
	}
}

// Unwrap is the identity on a script the master wrote, so an adapter that
// passed a script through can still be asked to decode it.
func TestUnwrapIsIdentityOnPlainScripts(t *testing.T) {
	s := script(500)
	got, err := Unwrap(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, s) {
		t.Fatal("Unwrap altered a plain script")
	}
}

// The wrapper decodes the payload with sed/base64/gzip and execs it: those
// are the tools every Lightsail Linux blueprint has, and the payload is
// never echoed.
func TestWrapperShapeIsSelfExtracting(t *testing.T) {
	wrapped, err := WrapUserData(script(30 * 1024))
	if err != nil {
		t.Fatal(err)
	}
	s := string(wrapped)
	for _, want := range []string{"set -eu", "base64 -d", "gzip -dc", "chmod 0700", "exec "} {
		if !strings.Contains(s, want) {
			t.Fatalf("wrapper is missing %q:\n%s", want, firstLines(s, 20))
		}
	}
	if strings.Contains(s, "echo \"$0\"") {
		t.Fatal("the wrapper must not echo its own payload")
	}
	// Exactly one payload block, delimited both ends.
	if n := strings.Count(s, payloadBegin); n != 2 {
		// Once in the sed program, once as the marker line.
		t.Fatalf("payload begin marker appears %d times, want 2", n)
	}
}

// The real ceiling is incompressible content, not script length: ~6 KiB
// of high-entropy bytes still fits (the enrollment bundle's key and token
// are far smaller), while 12 KiB does not and is refused rather than
// truncated. This pins where the boundary actually is, so a future change
// to the wrapper cannot quietly move it.
func TestWrapUserDataIncompressibleCeiling(t *testing.T) {
	// A 30 KB script, mostly shell, with a block of high-entropy base64
	// standing in for keys or an embedded blob.
	build := func(entropy int) []byte {
		raw := make([]byte, entropy)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString("#!/bin/bash\nset -euo pipefail\n")
		enc := base64.StdEncoding.EncodeToString(raw)
		for i := 0; i < len(enc); i += 76 {
			b.WriteString(enc[i:min(i+76, len(enc))] + "\n")
		}
		for i := 0; b.Len() < 30*1024; i++ {
			fmt.Fprintf(&b, "printf '%%s\\n' 'first-boot step %d: install, verify, enroll'\n", i)
		}
		return []byte(b.String())
	}
	fits := build(6 * 1024)
	wrapped, err := WrapUserData(fits)
	if err != nil {
		t.Fatalf("6 KiB of incompressible content must still fit: %v", err)
	}
	back, err := Unwrap(wrapped)
	if err != nil || !bytes.Equal(back, fits) {
		t.Fatalf("round trip failed: %v", err)
	}

	tooMuch := build(12 * 1024)
	if _, err := WrapUserData(tooMuch); err == nil {
		t.Fatal("12 KiB of incompressible content cannot fit and must be refused, not truncated")
	} else if !strings.Contains(err.Error(), "after gzip+base64") {
		t.Fatalf("error should name both sizes and the limit: %v", err)
	}
}

// A payload that cannot fit even compressed is an error, never a silent
// truncation: a truncated first-boot script launches a host that never
// enrolls.
func TestWrapUserDataRefusesWhatCannotFit(t *testing.T) {
	// Incompressible data: 2 MiB of a repeating-free byte pattern.
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = byte(i*i*31 + i*7)
	}
	// Make it less compressible still by interleaving a counter.
	for i := 0; i+4 < len(big); i += 4 {
		big[i] ^= byte(i >> 8)
		big[i+1] ^= byte(i >> 3)
	}
	_, err := WrapUserData(big)
	if err == nil {
		t.Fatal("2 MiB of incompressible user data must be refused, not truncated")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("error should say what limit was exceeded: %v", err)
	}
}

func TestUnwrapRejectsACorruptedWrapper(t *testing.T) {
	wrapped, err := WrapUserData(script(30 * 1024))
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the payload's gzip stream while keeping the markers.
	bad := bytes.Replace(wrapped, []byte(payloadBegin+"\n"), []byte(payloadBegin+"\nZZZZ\n"), 1)
	if _, err := Unwrap(bad); err == nil {
		t.Fatal("a corrupted payload must not decode")
	}
	// Remove the end marker.
	noEnd := bytes.Replace(wrapped, []byte(payloadEnd), []byte("nope"), -1)
	if _, err := Unwrap(noEnd); err == nil {
		t.Fatal("a wrapper with no end marker must not decode")
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
