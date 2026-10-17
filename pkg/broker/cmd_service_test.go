package broker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// Sprint 455 P7 (Story #2281, Story-ID 7ad909c17d91): `llm up/down --service`
// compose the ordinary up/down action with the fleet service layer (bashy
// wires SetServiceEnabled to pkg/fleet's Catalog.SetServiceEnabled on the
// "llm" unit, pkg/fleet/baseline/services/llm.yaml). These tests fake that
// seam so they never touch a real fleet catalog.
//
// Sprint 455 R1 (Story #2303, Story-ID bf090bf24ea9): `up --service` no
// longer starts the door itself (EnsureSupervisordRunning stands in for
// bashy supervisord instead), and `down --service` disables the unit before
// stopping the process (doorStopper records order against SetServiceEnabled).

type fakeServiceCall struct {
	unit    string
	enabled bool
}

func fakeServiceManager(t *testing.T) *[]fakeServiceCall {
	t.Helper()
	var calls []fakeServiceCall
	prev := SetServiceEnabled
	SetServiceEnabled = func(_ context.Context, unit string, enabled bool) error {
		calls = append(calls, fakeServiceCall{unit, enabled})
		return nil
	}
	t.Cleanup(func() { SetServiceEnabled = prev })
	return &calls
}

// fakeSupervisordManager fakes EnsureSupervisordRunning. By default it does
// nothing but succeed (as if a monitor already answered); tests that need
// the door to actually start pass a non-nil onEnsure to bring it up, the way
// a real supervisord would once it is running.
func fakeSupervisordManager(t *testing.T, onEnsure func()) *int {
	t.Helper()
	calls := 0
	prev := EnsureSupervisordRunning
	EnsureSupervisordRunning = func(context.Context) error {
		calls++
		if onEnsure != nil {
			onEnsure()
		}
		return nil
	}
	t.Cleanup(func() { EnsureSupervisordRunning = prev })
	return &calls
}

// freeLoopbackPort claims a free port, releases it immediately and points
// BASHY_LLM_PORT at it, so door.BaseURL()/Healthy() address a port nothing
// listens on: a connection to it is refused right away rather than hanging
// on doorJSON's long client timeout, simulating "the door is not running".
func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", ln.Addr(), err)
	}
	ln.Close()
	t.Setenv("BASHY_LLM_PORT", port)
	return port
}

// startHealthyDoor claims a free port, points BASHY_LLM_PORT at it and
// serves GET /health with a decodable Health envelope: enough for both
// Healthy() (up's idempotent short-circuit) and doorJSON's decode.
func startHealthyDoor(t *testing.T, pid int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", ln.Addr(), err)
	}
	t.Setenv("BASHY_LLM_PORT", port)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"schema_version":"bashy-broker-health-v1","pid":%d}`, pid)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// runLLMCmd executes a standalone `llm up`/`llm down` leaf command (as built
// by newUpCmd/newDownCmd) with the given args.
func runLLMCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

func TestLLMUpServiceEnablesUnitBeforeStarting(t *testing.T) {
	calls := fakeServiceManager(t)
	ensures := fakeSupervisordManager(t, nil)
	startHealthyDoor(t, 4242) // already answering: up is a no-op start

	out, err := runLLMCmd(t, newUpCmd(), "--service")
	if err != nil {
		t.Fatalf("up --service: %v\n%s", err, out)
	}
	if len(*calls) != 1 || (*calls)[0] != (fakeServiceCall{ServiceUnit, true}) {
		t.Fatalf("service calls = %+v, want one enable(%q)", *calls, ServiceUnit)
	}
	if *ensures != 1 {
		t.Fatalf("EnsureSupervisordRunning calls = %d, want 1", *ensures)
	}
}

// TestLLMUpServiceLeavesStartingTheDoorToSupervisord is the regression this
// story fixes: `up --service` must not start the door as a second,
// unsupervised process. SelfArgv is nil, so EnsureUp's own start path would
// fail outright; the only way the door comes up here is through
// EnsureSupervisordRunning, standing in for bashy supervisord starting the
// now-enabled unit.
func TestLLMUpServiceLeavesStartingTheDoorToSupervisord(t *testing.T) {
	prevSelf := SelfArgv
	SelfArgv = nil
	t.Cleanup(func() { SelfArgv = prevSelf })

	calls := fakeServiceManager(t)
	freeLoopbackPort(t) // nothing answers until the fake "supervisord" starts it
	ensures := fakeSupervisordManager(t, func() { startHealthyDoor(t, 4343) })

	out, err := runLLMCmd(t, newUpCmd(), "--service")
	if err != nil {
		t.Fatalf("up --service: %v\n%s", err, out)
	}
	if len(*calls) != 1 || (*calls)[0] != (fakeServiceCall{ServiceUnit, true}) {
		t.Fatalf("service calls = %+v, want one enable(%q)", *calls, ServiceUnit)
	}
	if *ensures != 1 {
		t.Fatalf("EnsureSupervisordRunning calls = %d, want 1", *ensures)
	}
}

func TestLLMUpWithoutServiceDoesNotTouchServiceLayer(t *testing.T) {
	calls := fakeServiceManager(t)
	startHealthyDoor(t, 4242)

	out, err := runLLMCmd(t, newUpCmd())
	if err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	if len(*calls) != 0 {
		t.Fatalf("service calls = %+v, want none", *calls)
	}
}

func TestLLMUpServiceRefusedWithoutAServiceLayer(t *testing.T) {
	prev := SetServiceEnabled
	SetServiceEnabled = nil
	t.Cleanup(func() { SetServiceEnabled = prev })

	if _, err := runLLMCmd(t, newUpCmd(), "--service"); err == nil {
		t.Fatal("up --service with no service layer: want an error")
	}
}

func TestLLMUpServiceRefusedWithoutASupervisordHelper(t *testing.T) {
	fakeServiceManager(t)
	prev := EnsureSupervisordRunning
	EnsureSupervisordRunning = nil
	t.Cleanup(func() { EnsureSupervisordRunning = prev })

	if _, err := runLLMCmd(t, newUpCmd(), "--service"); err == nil {
		t.Fatal("up --service with no supervisord helper: want an error")
	}
}

func TestLLMDownServiceDisablesUnitEvenWhenNotRunning(t *testing.T) {
	calls := fakeServiceManager(t)
	freeLoopbackPort(t) // reserves BASHY_LLM_PORT but nothing is served there

	out, err := runLLMCmd(t, newDownCmd(), "--service")
	if err != nil {
		t.Fatalf("down --service: %v\n%s", err, out)
	}
	if len(*calls) != 1 || (*calls)[0] != (fakeServiceCall{ServiceUnit, false}) {
		t.Fatalf("service calls = %+v, want one disable(%q)", *calls, ServiceUnit)
	}
}

func TestLLMDownWithoutServiceStillErrorsWhenNotRunning(t *testing.T) {
	calls := fakeServiceManager(t)
	freeLoopbackPort(t)

	if _, err := runLLMCmd(t, newDownCmd()); err == nil {
		t.Fatal("down with no door running: want an error")
	}
	if len(*calls) != 0 {
		t.Fatalf("service calls = %+v, want none", *calls)
	}
}

// TestLLMDownServiceDisablesBeforeStopping is the regression this story
// fixes: once the unit is disabled, a supervisord watching it does not
// restart it when the process exits. Disabling after stopping risks the
// monitor's on-failure policy restarting it once before it reads the
// disable.
func TestLLMDownServiceDisablesBeforeStopping(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process")
	}
	child, exited := spawnStopHelper(t)
	startHealthyDoor(t, child.Process.Pid)

	var order []string
	prevSet := SetServiceEnabled
	SetServiceEnabled = func(context.Context, string, bool) error {
		order = append(order, "disable")
		return nil
	}
	t.Cleanup(func() { SetServiceEnabled = prevSet })
	prevStop := doorStopper
	doorStopper = func(p *os.Process) error {
		order = append(order, "stop")
		return prevStop(p)
	}
	t.Cleanup(func() { doorStopper = prevStop })

	out, err := runLLMCmd(t, newDownCmd(), "--service")
	if err != nil {
		t.Fatalf("down --service: %v\n%s", err, out)
	}
	if len(order) != 2 || order[0] != "disable" || order[1] != "stop" {
		t.Fatalf("call order = %v, want [disable stop]", order)
	}
	waitExit(t, "door", child.Process.Pid, exited)
}

func TestLLMDownServiceRefusedWithoutAServiceLayer(t *testing.T) {
	prev := SetServiceEnabled
	SetServiceEnabled = nil
	t.Cleanup(func() { SetServiceEnabled = prev })
	freeLoopbackPort(t)

	if _, err := runLLMCmd(t, newDownCmd(), "--service"); err == nil {
		t.Fatal("down --service with no service layer: want an error")
	}
}
