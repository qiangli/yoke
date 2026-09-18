// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/svcd"
)

// outpost restarts any service whose `status` output contains "stopped" or
// "not running". Those two strings are therefore a wire format with a
// supervisor on the other end, and a RUNNING console printing either would be
// restarted on a 30s timer forever.
func TestServiceStatusWordsAreTheSupervisorContract(t *testing.T) {
	var b strings.Builder
	printServiceStatus(&b, svcd.Status{Running: true, Addr: "127.0.0.1:8639", PID: 42}, false, "status")
	got := strings.ToLower(b.String())
	for _, forbidden := range []string{"stopped", "not running"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("a RUNNING console printed %q (%q) — outpost greps for it and would restart on every tick",
				forbidden, b.String())
		}
	}
	if !strings.Contains(got, "running") {
		t.Fatalf("status of a running console = %q, want it to say so", b.String())
	}

	b.Reset()
	printServiceStatus(&b, svcd.Status{State: svcd.StateNotRunning}, false, "status")
	if !strings.Contains(strings.ToLower(b.String()), "stopped") {
		t.Fatalf("status of a stopped console = %q, want the word outpost restarts on", b.String())
	}
}

// The console's spec must name its own /healthz identity, or stop cannot tell
// "our console holds this port" from "an unrelated process does" — and would
// either refuse to stop a console it owns or signal something it does not.
func TestServiceSpecMatchesTheHealthEndpoint(t *testing.T) {
	if spec.Health != otelServiceName {
		t.Fatalf("spec.Health = %q, want the value /healthz reports (%q)", spec.Health, otelServiceName)
	}
	if spec.DefaultPort != DefaultPort {
		t.Fatalf("spec.DefaultPort = %d, want the console's own default %d", spec.DefaultPort, DefaultPort)
	}
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/healthz", "127.0.0.1:5555", nil)
	if !strings.Contains(w.Body.String(), `"service":"`+spec.Health+`"`) {
		t.Fatalf("/healthz = %s, want it to report service %q", strings.TrimSpace(w.Body.String()), spec.Health)
	}
}

func TestServicePairingIsExplicitAndDoesNotMutateTheBaseSpec(t *testing.T) {
	plain := serviceSpec(false)
	paired := serviceSpec(true)
	if got := strings.Join(plain.Argv, " "); got != "apps serve" {
		t.Fatalf("plain service argv = %q", got)
	}
	if got := strings.Join(paired.Argv, " "); got != "apps serve --pair" {
		t.Fatalf("paired service argv = %q", got)
	}
	if got := strings.Join(spec.Argv, " "); got != "apps serve" {
		t.Fatalf("base service spec mutated to %q", got)
	}
}

func TestBareServiceStartReusesTheSavedPairingProfile(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	want := svcd.Options{Bind: "192.0.2.55", Port: 23456}
	if err := saveServiceProfile(want, true); err != nil {
		t.Fatal(err)
	}

	gotOpt, gotPair, err := serviceStartPlan(svcd.Options{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !gotPair {
		t.Fatal("bare service start did not reuse the saved --pair profile")
	}
	if gotOpt.Bind != want.Bind || gotOpt.Port != want.Port {
		t.Fatalf("start options = %+v, want %+v", gotOpt, want)
	}
}

func TestBareServiceStartRefusesToDisarmLivePairingState(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	store, err := openPairStore()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := store.issueTicket(nil, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.redeem(secret, "phone", ""); err != nil {
		t.Fatal(err)
	}

	_, _, err = serviceStartPlan(svcd.Options{}, false, false)
	if err == nil {
		t.Fatal("bare service start with live paired devices succeeded")
	}
	if msg := err.Error(); !strings.Contains(msg, "refusing a bare start") ||
		!strings.Contains(msg, "live device") ||
		!strings.Contains(msg, "bashy app service start --pair --bind") {
		t.Fatalf("error did not name the recurring pairing repair: %v", err)
	}
}

func TestExplicitServiceStartMayChooseANewProfileEvenWithLiveDevices(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	store, err := openPairStore()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := store.issueTicket(nil, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.redeem(secret, "phone", ""); err != nil {
		t.Fatal(err)
	}

	want := svcd.Options{Bind: "127.0.0.1", Port: DefaultPort}
	gotOpt, gotPair, err := serviceStartPlan(want, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if gotPair || gotOpt != want {
		t.Fatalf("explicit start plan = %+v pair=%v, want %+v pair=false", gotOpt, gotPair, want)
	}
}

// A disabled panel must be UNROUTED, not merely unlisted. outpost publishes the
// console under one app name and HostShare grants are per app name, so if
// disabling the Terminal only hid the tile, sharing the console would still
// hand out a shell to anyone who typed /term/.
func TestDisabledPanelIsUnroutedNotJustHidden(t *testing.T) {
	h := newTestHandler(t, Options{Disable: []string{"terminal", "sprint"}})

	// The SPA catch-all answers unrouted paths, so the check is that the PANEL
	// did not answer — a 404 is not what this console does. Match on the page's
	// own <title>, not on a script name: index.html loads vendor/xterm.js, and
	// "xterm.js" contains "term.js".
	for _, tc := range []struct{ path, mustNot string }{
		{"/term/", "<title>Terminal"},
		{"/term", "<title>Terminal"},
		{"/shell", "<title>Terminal"},
		{"/sprint/", "<title>Sprint"},
		{"/api/sprint", boardSchemaVersion},
	} {
		if body := do(h, "GET", tc.path, "127.0.0.1:5555", nil).Body.String(); strings.Contains(body, tc.mustNot) {
			t.Errorf("%s was still served after the panel was disabled", tc.path)
		}
	}

	// It is absent from the tile list too, so nothing advertises a dead link.
	d := getJSON(t, h, "/api/apps")
	for _, a := range d["apps"].([]any) {
		if n := a.(map[string]any)["name"]; n == "terminal" || n == "sprint" {
			t.Errorf("disabled panel %v is still advertised in /api/apps", n)
		}
	}

	// And the panels that were NOT disabled still work, so the test is not
	// passing because everything is off.
	if got := do(h, "GET", "/mb/", "127.0.0.1:5555", nil); !strings.Contains(got.Body.String(), "<title>Messages") {
		t.Fatal("the message board stopped working when other panels were disabled")
	}
}

func TestDisableIsCaseAndSpaceInsensitive(t *testing.T) {
	h := newTestHandler(t, Options{Disable: []string{" Terminal "}})
	if w := do(h, "GET", "/term/", "127.0.0.1:5555", nil); strings.Contains(w.Body.String(), "<title>Terminal") {
		t.Fatal(`--disable " Terminal " did not disable the terminal`)
	}
	if got := do(h, "GET", "/", "127.0.0.1:5555", nil).Code; got != http.StatusOK {
		t.Fatalf("launcher = %d after a disable, want 200", got)
	}
}
