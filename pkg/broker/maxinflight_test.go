package broker

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/cligw"
)

// TestDoorMaxInFlightReachesServerAdmissionLimit pins the rework: the door's
// --max-in-flight option lands in the embedded cligw server's AdmissionLimit
// for the single "owner" principal the door authorizes every request as.
func TestDoorMaxInFlightReachesServerAdmissionLimit(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("CLIGW_MAX_IN_FLIGHT", "")
	policy := cligw.DefaultPolicy()
	policy.MaxInFlight = 7

	opts, err := cliServerOptions("test-token", DoorOptions{MaxInFlight: 9})
	if err != nil {
		t.Fatal(err)
	}
	opts.Policy = &policy
	server, err := cligw.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if got := server.AdmissionLimit(cligw.Principal); got != 9 {
		t.Fatalf("owner cap = %d, want the door flag 9", got)
	}
}

// TestDoorMaxInFlightEnvFallback: with no flag the door reads
// CLIGW_MAX_IN_FLIGHT through cligw's parser, and a bad value fails loudly
// instead of silently serving the policy default.
func TestDoorMaxInFlightEnvFallback(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	policy := cligw.DefaultPolicy()
	policy.MaxInFlight = 7

	t.Setenv("CLIGW_MAX_IN_FLIGHT", "8")
	opts, err := cliServerOptions("test-token", DoorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	opts.Policy = &policy
	server, err := cligw.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if got := server.AdmissionLimit(cligw.Principal); got != 8 {
		t.Fatalf("owner cap = %d, want the env 8", got)
	}

	t.Setenv("CLIGW_MAX_IN_FLIGHT", "many")
	if _, err := cliServerOptions("test-token", DoorOptions{}); err == nil {
		t.Fatal("bad CLIGW_MAX_IN_FLIGHT parsed without error")
	}
}

// TestDoorServeHelpNamesCapAndOwnerKey pins the docs half: the door's serve
// help names the flag, the env var, and the one principal key
// max_in_flight_by_principal must use (sessions and sticky keys share it).
func TestDoorServeHelpNamesCapAndOwnerKey(t *testing.T) {
	cmd := NewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"serve", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--max-in-flight", "CLIGW_MAX_IN_FLIGHT", "max_in_flight_by_principal", `"owner"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("door serve help missing %q:\n%s", want, out.String())
		}
	}
}
