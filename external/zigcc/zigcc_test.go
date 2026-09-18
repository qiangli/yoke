package zigcc

import "testing"

// The pin table is the security boundary: an entry without a full digest, or a
// silent fallback on an unknown platform, would let an unverified toolchain
// build a provider whose bytes nobody can attribute.
func TestEveryPinnedAssetIsFullyVerified(t *testing.T) {
	if len(zigAsset) == 0 {
		t.Fatal("no pinned toolchains")
	}
	for plat, a := range zigAsset {
		if len(a.SHA256) != 64 {
			t.Errorf("%s: sha256 must be 64 hex chars, got %d", plat, len(a.SHA256))
		}
		for _, c := range a.SHA256 {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				t.Errorf("%s: sha256 must be lowercase hex: %q", plat, a.SHA256)
				break
			}
		}
		if a.URL == "" || a.Entry == "" {
			t.Errorf("%s: URL and Entry are required", plat)
		}
		// The pinned version must appear in the URL, or a version bump could
		// leave a digest pointing at the previous release.
		if !contains(a.URL, zigVersion) {
			t.Errorf("%s: URL %q does not carry pinned version %s", plat, a.URL, zigVersion)
		}
	}
}

func TestUnsupportedPlatformIsRefusedNotFallenBack(t *testing.T) {
	if Supported("plan9/mips") {
		t.Fatal("an unpinned platform must not report as supported")
	}
}

func TestCertificationTargetIsPinned(t *testing.T) {
	// linux/amd64 is the certification platform; losing its pin would silently
	// push provider builds onto whatever compiler the host happens to have.
	if !Supported("linux/amd64") {
		t.Fatal("linux/amd64 must remain pinned")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
