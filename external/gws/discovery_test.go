package gws

import (
	"strings"
	"testing"
)

// Minimal Discovery document fixtures shaped like the real Google schema
// (name/version/revision plus unrelated payload). No network, no
// credentials — inspection runs on bytes already in hand.
const (
	driveDiscoveryV3R1 = `{"name":"drive","version":"v3","revision":"20260901","schemas":{"File":{"id":"File"}},"resources":{"files":{}}}`
	driveDiscoveryV3R2 = `{"name":"drive","version":"v3","revision":"20261015","schemas":{"File":{"id":"File"},"Change":{"id":"Change"}},"resources":{"files":{},"changes":{}}}`
)

func TestInspectDiscoveryIdentity(t *testing.T) {
	d, err := InspectDiscovery([]byte(driveDiscoveryV3R1))
	if err != nil {
		t.Fatalf("InspectDiscovery: %v", err)
	}
	if d.Name != "drive" || d.Version != "v3" || d.Revision != "20260901" {
		t.Errorf("identity = %+v, want drive v3 rev 20260901", d)
	}
	if len(d.Fingerprint) != 64 {
		t.Errorf("fingerprint = %q, want 64 hex chars", d.Fingerprint)
	}
	// Any byte change — however small — changes the fingerprint.
	d2, err := InspectDiscovery([]byte(driveDiscoveryV3R1 + " "))
	if err != nil {
		t.Fatalf("InspectDiscovery: %v", err)
	}
	if d2.Fingerprint == d.Fingerprint {
		t.Error("trailing-space variant kept the fingerprint — inspection must be byte-exact")
	}
}

func TestInspectDiscoveryRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":       ``,
		"malformed":   `{"name":`,
		"no identity": `{"kind":"discovery#restDescription"}`,
		"no version":  `{"name":"drive"}`,
		"wrong shape": `[1,2,3]`,
	} {
		if _, err := InspectDiscovery([]byte(raw)); err == nil {
			t.Errorf("%s document inspected without error", name)
		} else if err.Code != CodeInvalidInput {
			t.Errorf("%s: code = %q, want invalid-input", name, err.Code)
		}
	}
}

func TestCheckDrift(t *testing.T) {
	cached, err := InspectDiscovery([]byte(driveDiscoveryV3R1))
	if err != nil {
		t.Fatalf("InspectDiscovery: %v", err)
	}
	same, err := InspectDiscovery([]byte(driveDiscoveryV3R1))
	if err != nil {
		t.Fatalf("InspectDiscovery: %v", err)
	}
	if derr := CheckDrift(cached, same); derr != nil {
		t.Errorf("identical observation drifted: %v", derr)
	}
	// A new upstream revision drifts: callers re-inspect, never mix.
	rev2, err := InspectDiscovery([]byte(driveDiscoveryV3R2))
	if err != nil {
		t.Fatalf("InspectDiscovery: %v", err)
	}
	derr := CheckDrift(cached, rev2)
	if derr == nil {
		t.Fatal("revision change reported no drift")
	}
	if derr.Retryable {
		t.Error("drift must not be retryable under the stale revision")
	}
	if !strings.Contains(derr.Message, "20260901") || !strings.Contains(derr.Message, "20261015") {
		t.Errorf("drift message = %q, must name both revisions", derr.Message)
	}
	// A different service/version is an identity change, not a silent match.
	gmail, err := InspectDiscovery([]byte(`{"name":"gmail","version":"v1","revision":"20261015"}`))
	if err != nil {
		t.Fatalf("InspectDiscovery: %v", err)
	}
	if derr := CheckDrift(cached, gmail); derr == nil {
		t.Error("cross-service observation reported no drift")
	}
}
