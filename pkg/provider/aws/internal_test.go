package aws

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// The Cost Explorer window must start on the 1st and END AFTER TODAY: End
// is exclusive, so an End of today drops today's usage, and on the 1st of
// a month it would be an empty interval that reports $0 spend.
func TestMonthToDateWindow(t *testing.T) {
	for _, tc := range []struct {
		now        string
		start, end string
	}{
		{"2026-10-28T13:45:00Z", "2026-10-01", "2026-10-29"},
		{"2026-10-01T00:00:01Z", "2026-10-01", "2026-10-02"},
		{"2026-10-31T23:59:59Z", "2026-10-01", "2026-11-01"},
		{"2026-02-28T12:00:00Z", "2026-02-01", "2026-03-01"},
		{"2026-12-31T06:00:00Z", "2026-12-01", "2027-01-01"},
	} {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		got := monthToDateWindow(now)
		if got.Start != tc.start || got.End != tc.end {
			t.Fatalf("at %s: window = %s..%s, want %s..%s", tc.now, got.Start, got.End, tc.start, tc.end)
		}
		if got.End <= got.Start {
			t.Fatalf("at %s: End %s is not after Start %s", tc.now, got.End, got.Start)
		}
	}
}

// The filter must EXCLUDE credit records, so the reported figure is spend
// before credits and a credit balance cannot quietly fund a fleet.
func TestExcludeCreditsFilterShape(t *testing.T) {
	raw, err := json.Marshal(excludeCredits())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Not":{"Dimensions":{"Key":"RECORD_TYPE","Values":["Credit"]}}}`
	if string(raw) != want {
		t.Fatalf("filter = %s, want %s", raw, want)
	}
}

func TestBudgetDefaults(t *testing.T) {
	var b Budget
	if b.cap() != DefaultMonthlyCapUSD || b.perInstance() != DefaultPerInstanceUSD {
		t.Fatalf("defaults = %v/%v, want %v/%v", b.cap(), b.perInstance(), DefaultMonthlyCapUSD, DefaultPerInstanceUSD)
	}
	b = Budget{MonthlyCapUSD: 10, PerInstanceUSD: 5}
	if b.cap() != 10 || b.perInstance() != 5 {
		t.Fatalf("overrides = %v/%v", b.cap(), b.perInstance())
	}
}

func TestIsGPUBundle(t *testing.T) {
	for _, tc := range []struct {
		b    bundle
		want bool
	}{
		{bundle{BundleID: "gpu_nvidia_xl_1_0"}, true},
		{bundle{BundleID: "x", InstanceType: "gpu_xl"}, true},
		{bundle{BundleID: "x", Name: "GPU Large"}, true},
		{bundle{BundleID: "small_3_0", InstanceType: "small", Name: "Small"}, false},
	} {
		if got := isGPUBundle(tc.b); got != tc.want {
			t.Fatalf("isGPUBundle(%+v) = %t, want %t", tc.b, got, tc.want)
		}
	}
}

// The flat contract tag strings and Lightsail's key/value tags must round
// trip exactly, or provider.Owned stops recognizing our own instances.
func TestTagsRoundTrip(t *testing.T) {
	flat := append(provider.LeaseTags("t1", "key-1", &provider.Authorization{
		Kind: provider.AuthTemporary, ExpiresAt: time.Unix(1800000000, 0), WorkRef: "story-2816",
	}), ManagedTag, "dhnt:sprint=522", "Name", "weird=a=b")
	back := decodeTags(encodeTags(flat))
	if !slices.Equal(back, flat) {
		t.Fatalf("round trip:\n got %v\nwant %v", back, flat)
	}
	if !provider.Owned(back, "t1") {
		t.Fatal("round-tripped tags are no longer recognized as owned")
	}
	if !Managed(back) {
		t.Fatal("round-tripped tags lost the managed marker")
	}
	if _, ok := provider.DeadlineFromTags(back); !ok {
		t.Fatal("round-tripped tags lost the TTL deadline")
	}
}

func TestEncodeTagsDropsEmptiesAndDuplicateKeys(t *testing.T) {
	got := encodeTags([]string{"a=1", "", "  ", "a=2", "=orphan", "b"})
	want := []Tag{{Key: "a", Value: "1"}, {Key: "b"}}
	if len(got) != len(want) {
		t.Fatalf("encodeTags = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("encodeTags = %+v, want %+v", got, want)
		}
	}
}

// Lightsail state names map onto the normalized states; an in-flight state
// must never read as a resting one (a "stopping" instance reported as
// stopped would let a caller believe a stop finished).
func TestStateMapping(t *testing.T) {
	for name, want := range map[string]provider.State{
		"pending":       provider.StatePending,
		"running":       provider.StateRunning,
		"stopping":      provider.StatePending,
		"stopped":       provider.StateStopped,
		"rebooting":     provider.StatePending,
		"shutting-down": provider.StateDeleting,
		"terminated":    provider.StateDeleted,
		"wat":           provider.StateUnknown,
	} {
		if got := State(name); got != want {
			t.Fatalf("State(%q) = %q, want %q", name, got, want)
		}
	}
}

// A region is accepted either as a region or as a full AZ, and the AZ's
// trailing letter is stripped for the endpoint.
func TestRegionOf(t *testing.T) {
	for in, want := range map[string]string{
		"us-west-2":       "us-west-2",
		"us-west-2a":      "us-west-2",
		"ap-southeast-1":  "ap-southeast-1",
		"ap-southeast-1c": "ap-southeast-1",
		"r1":              "r1",
	} {
		if got := regionOf(in); got != want {
			t.Fatalf("regionOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A port rule with a CIDR alias must carry NO cidrs: an SSH rule that
// quietly also opened 0.0.0.0/0 would be a world-facing sshd.
func TestPortRuleWithAliasIsNotWorldOpen(t *testing.T) {
	ssh := PortRule{Protocol: "tcp", FromPort: 22, ToPort: 22, Aliases: []string{cidrAliasLightsailConnect}}
	w := ssh.wire()
	if len(w.CIDRs) != 0 || len(w.IPv6CIDRs) != 0 {
		t.Fatalf("aliased rule carries cidrs: %+v", w)
	}
	if !slices.Equal(w.CIDRListAliases, []string{cidrAliasLightsailConnect}) {
		t.Fatalf("aliases = %v", w.CIDRListAliases)
	}
	web := PortRule{Protocol: "tcp", FromPort: 443, ToPort: 443}.wire()
	if !slices.Equal(web.CIDRs, []string{anyIPv4}) || !slices.Equal(web.IPv6CIDRs, []string{anyIPv6}) {
		t.Fatalf("plain rule = %+v, want world-open v4+v6", web)
	}
}

// The satellite port set is the DO dhnt-satellite firewall's mirror.
func TestDefaultSatellitePorts(t *testing.T) {
	got := DefaultSatellitePorts()
	want := []PortRule{
		{Protocol: "tcp", FromPort: 80, ToPort: 80},
		{Protocol: "tcp", FromPort: 443, ToPort: 443},
		{Protocol: "tcp", FromPort: 6443, ToPort: 6443},
		{Protocol: "tcp", FromPort: 10250, ToPort: 10250},
		{Protocol: "udp", FromPort: 8475, ToPort: 8476},
		{Protocol: "tcp", FromPort: 22, ToPort: 22, Aliases: []string{cidrAliasLightsailConnect}},
	}
	if len(got) != len(want) {
		t.Fatalf("ports = %+v", got)
	}
	for i := range want {
		if got[i].Protocol != want[i].Protocol || got[i].FromPort != want[i].FromPort ||
			got[i].ToPort != want[i].ToPort || !slices.Equal(got[i].Aliases, want[i].Aliases) {
			t.Fatalf("port %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// WebPorts is the minimal preset for a request that asks only for HTTP
// and HTTPS; console SSH rides along because it is never a public
// exposure.
func TestWebPorts(t *testing.T) {
	got := WebPorts()
	if len(got) != 3 {
		t.Fatalf("WebPorts = %+v, want 3 rules", got)
	}
	if got[0].FromPort != 80 || got[1].FromPort != 443 {
		t.Fatalf("WebPorts = %+v, want 80 and 443 first", got)
	}
	ssh := got[2]
	if ssh.FromPort != 22 || !slices.Equal(ssh.Aliases, []string{cidrAliasLightsailConnect}) {
		t.Fatalf("WebPorts ssh rule = %+v", ssh)
	}
	for _, r := range got {
		if r.Protocol != "tcp" {
			t.Fatalf("WebPorts carries a non-tcp rule: %+v", r)
		}
	}
}

// Lightsail bills monthly; the hourly figure is a 730-hour proration (AWS's
// own convention), reported alongside so a caller can compare with an
// hourly provider.
func TestHourlyProration(t *testing.T) {
	if got := hourly(730); got != 1 {
		t.Fatalf("hourly(730) = %v, want 1", got)
	}
	if got := hourly(0); got != 0 {
		t.Fatalf("hourly(0) = %v", got)
	}
}

func TestRedactKeyID(t *testing.T) {
	for in, want := range map[string]string{
		"AKIDEXAMPLE": "****MPLE",
		"AKIA":        "****",
		"":            "****",
		"AKIA1":       "****KIA1",
	} {
		if got := redactKeyID(in); got != want {
			t.Fatalf("redactKeyID(%q) = %q, want %q", in, got, want)
		}
	}
}
