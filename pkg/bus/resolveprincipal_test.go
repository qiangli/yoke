package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/principal"
)

// THE AGREEMENT, stated as a test (docs/agent-comms-synergy.md). The
// addresser and the resolver used to be independent name systems: `ping
// steward` posted to the seat while `whois steward` answered "names nothing".
// The role table is bridged into pkg/principal at this package's init, so
// both must now resolve the same seat to the same stable address.
func TestWhoisAndPingAgreeOnARole(t *testing.T) {
	boardInTempHome(t)
	withHostRoles(t, HostRole{Label: "steward", Topic: "steward.host-1"})

	addr, kind, ok := ResolveSendTarget("steward")
	if !ok || kind != TargetRole || addr != "steward.host-1" {
		t.Fatalf("addresser: ResolveSendTarget(steward) = %q,%q,%v", addr, kind, ok)
	}
	targets := principal.LookupSend("steward")
	if len(targets) != 1 || targets[0].Kind != principal.KindRole || targets[0].Name != addr {
		t.Fatalf("resolver: LookupSend(steward) = %+v — the resolver knows fewer names than the addresser", targets)
	}
}

// A name the board's fast checks miss but the resolver knows IS sendable —
// here the OS login (boardInTempHome sets USER=tester), a person with no
// cursor, no roster entry and no role. Before the fallback this failed, and
// `whois` and `ping` disagreed about the same name in the same second.
func TestResolveSendTarget_FallsThroughToThePersonResolver(t *testing.T) {
	boardInTempHome(t)

	addr, kind, ok := ResolveSendTarget("tester")
	if !ok || kind != TargetPerson || addr != "tester" {
		t.Fatalf("ResolveSendTarget(tester) = %q,%q,%v, want tester/person/true", addr, kind, ok)
	}
}

// An agent seat OBSERVED on this host — a meet-roster participant no catalog
// or roster names — is sendable, because the resolver answers for it. This is
// the `weave -w<issue>` worker case: 62 names in use vs ~44 cataloged.
func TestResolveSendTarget_ObservedMeetSeatResolves(t *testing.T) {
	boardInTempHome(t)
	meetDir := t.TempDir()
	t.Setenv("BASHY_MEET_DIR", meetDir)
	if err := os.MkdirAll(filepath.Join(meetDir, "m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"id":"m1","participants":["weave-w7"],"secretary":"","human":""}`
	if err := os.WriteFile(filepath.Join(meetDir, "m1", "state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	addr, kind, ok := ResolveSendTarget("weave-w7")
	if !ok || kind != TargetAgent || addr != "weave-w7" {
		t.Fatalf("ResolveSendTarget(weave-w7) = %q,%q,%v, want weave-w7/agent/true", addr, kind, ok)
	}
}

// The fallback consults the resolver, not a guess: a name nothing on the host
// vouches for still fails, writing nothing — the S80 guarantee is preserved.
func TestResolveSendTarget_StillFailsForAnUnknownName(t *testing.T) {
	boardInTempHome(t)
	if _, _, ok := ResolveSendTarget("zz-no-such-name"); ok {
		t.Fatal("a name no source vouches for must not resolve")
	}
}

// A send to a resolver-vouched person flows end to end: posted, receipt
// judged per-reader (unverified until they read — a person has a cursor like
// anyone else), and the failure message now names the resolver's part.
func TestPingSend_PersonTargetPostsAndFailureNamesTheLadder(t *testing.T) {
	boardInTempHome(t)

	cmd, _, errb := captureCmd()
	if err := pingSend(cmd, "someone", "tester", "are you there?"); err != nil {
		t.Fatalf("send to the operator failed: %v", err)
	}
	posts, _ := Posts()
	if len(posts) != 1 || posts[0].To != "tester" {
		t.Fatalf("person send stored %+v, want one post to tester", posts)
	}
	if r := errb.String(); !strings.Contains(r, "UNVERIFIED") {
		t.Errorf("receipt = %q, want unverified for a person who never read", r)
	}

	cmd2, _, _ := captureCmd()
	err := pingSend(cmd2, "someone", "zz-no-such-name", "hello?")
	if err == nil || !strings.Contains(err.Error(), "resolvable principal") {
		t.Errorf("failure = %v, want it to say the resolver was consulted", err)
	}
}
