// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// serviceEnv isolates the unit store and console state from the developer's.
func serviceEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SERVICES_DIR", t.TempDir())
	t.Setenv("BASHY_SERVICES_PATH", "")
}

func appsUnit(t *testing.T) fleet.Service {
	t.Helper()
	u, err := loadServiceUnit()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func runAppService(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewAppsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"service"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// The seeded unit's exec must expand to flags `apps serve` really accepts, and
// its default launch shape must be what a bare console has always been:
// loopback, stock launcher, pairing off.
func TestAppsUnitExecMatchesServeFlags(t *testing.T) {
	serviceEnv(t)
	u := appsUnit(t)
	if u.Enabled || u.ProviderName() != fleet.ServiceProviderProcess {
		t.Fatalf("seed = enabled %v provider %s, want disabled process", u.Enabled, u.ProviderName())
	}
	argv, err := u.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(argv, " "), "bashy apps serve --port 22749 --bind 127.0.0.1 --pair=false --launcher="; got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	serve := newServeCmd()
	if err := serve.ParseFlags(argv[3:]); err != nil {
		t.Fatalf("serve rejects the unit's argv: %v", err)
	}
	if p, _ := serve.Flags().GetBool("pair"); p {
		t.Fatal("seed arms pairing")
	}
	if a := u.Expose.App; a == nil || !a.RequireLogin || !a.Elevation || !a.TrustCloudIdentity {
		t.Fatalf("expose.app = %+v, want login+elevation+cloud identity", a)
	}
	if h := do(newTestHandler(t, Options{}), "GET", "/healthz", "127.0.0.1:5555", nil); !strings.Contains(h.Body.String(), `"service":"`+otelServiceName+`"`) {
		t.Fatalf("/healthz = %s, want service %q", h.Body.String(), otelServiceName)
	}
}

func TestServiceStartWritesTheUnitAndStopDisablesIt(t *testing.T) {
	serviceEnv(t)
	port := freePort(t)
	out, err := runAppService(t, "start", "--pair", "--bind", "192.0.2.55", "--port", strconv.Itoa(port), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st service.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if st.Unit != "apps" || !st.Enabled || st.State != service.Inactive || st.Provider != "process" {
		t.Fatalf("status = %+v", st)
	}
	u := appsUnit(t)
	argv, err := u.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	want := "bashy apps serve --port " + strconv.Itoa(port) + " --bind 192.0.2.55 --pair=true --launcher="
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if !u.Enabled {
		t.Fatal("start did not enable the unit")
	}

	out, err = runAppService(t, "stop", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if u := appsUnit(t); u.Enabled {
		t.Fatal("stop left the unit enabled")
	}
	if plan := planOf(appsUnit(t)); !plan.Pair || plan.Port != port {
		t.Fatalf("stop dropped the launch shape: %+v", plan)
	}
}

// Status reports the monitor's states from the unit's own /healthz probe, and
// a port held by something else is Failed, never mistaken for the console.
func TestServiceStatusFollowsTheHealthProbe(t *testing.T) {
	serviceEnv(t)
	if _, err := runAppService(t, "start", "--port", strconv.Itoa(freePort(t))); err != nil {
		t.Fatal(err)
	}
	if _, err := runAppService(t, "stop"); err != nil {
		t.Fatal(err)
	}
	_, err := runAppService(t, "status")
	if !errors.Is(err, errInactive) {
		t.Fatalf("status of a disabled idle unit = %v, want errInactive", err)
	}

	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "service": otelServiceName})
	}))
	defer console.Close()
	port := console.Listener.Addr().(*net.TCPAddr).Port
	if _, err := runAppService(t, "start", "--port", strconv.Itoa(port), "--bind", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	out, err := runAppService(t, "status")
	if err != nil {
		t.Fatalf("status of a serving console = %v", err)
	}
	if !strings.Contains(out, "active") {
		t.Fatalf("status = %q", out)
	}

	foreign := httptest.NewServer(http.NotFoundHandler())
	defer foreign.Close()
	fport := foreign.Listener.Addr().(*net.TCPAddr).Port
	if _, err := runAppService(t, "start", "--port", strconv.Itoa(fport), "--bind", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	out, err = runAppService(t, "status", "--json")
	if !errors.Is(err, errInactive) {
		t.Fatalf("status of a foreign listener = %v", err)
	}
	var st service.Status
	if jerr := json.Unmarshal([]byte(out), &st); jerr != nil || st.State != service.Failed {
		t.Fatalf("foreign listener status = %+v (%v)", st, jerr)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestBareServiceStartReusesThePairingShapeOnTheUnit(t *testing.T) {
	serviceEnv(t)
	port := freePort(t)
	if _, err := runAppService(t, "start", "--pair", "--bind", "192.0.2.55", "--port", strconv.Itoa(port)); err != nil {
		t.Fatal(err)
	}
	if _, err := runAppService(t, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := runAppService(t, "start"); err != nil {
		t.Fatal(err)
	}
	if got := planOf(appsUnit(t)); got != (servicePlan{Port: port, Bind: "192.0.2.55", Pair: true}) {
		t.Fatalf("bare start changed the shape: %+v", got)
	}
}

func TestBareServiceStartRefusesToDisarmLivePairingState(t *testing.T) {
	serviceEnv(t)
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

	_, err = runAppService(t, "start")
	if err == nil {
		t.Fatal("bare service start with live paired devices succeeded")
	}
	if msg := err.Error(); !strings.Contains(msg, "refusing a bare start") ||
		!strings.Contains(msg, "live device") ||
		!strings.Contains(msg, "bashy app service start --pair --bind") {
		t.Fatalf("error did not name the recurring pairing repair: %v", err)
	}
	if appsUnit(t).Enabled {
		t.Fatal("a refused start still enabled the unit")
	}
}

func TestExplicitServiceStartMayChooseANewShapeEvenWithLiveDevices(t *testing.T) {
	serviceEnv(t)
	want, err := serviceStartPlan(servicePlan{Port: 1, Bind: "x", Pair: true}, servicePlan{Bind: "127.0.0.1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want != (servicePlan{Port: DefaultPort, Bind: "127.0.0.1"}) {
		t.Fatalf("explicit start plan = %+v", want)
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
