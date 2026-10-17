package broker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/spf13/cobra"
)

// Sprint 455 P7 (Story #2281, Story-ID 7ad909c17d91): `llm up/down --service`
// compose the ordinary up/down action with the fleet service layer (bashy
// wires SetServiceEnabled to pkg/fleet's Catalog.SetServiceEnabled on the
// "llm" unit, pkg/fleet/baseline/services/llm.yaml). These tests fake that
// seam so they never touch a real fleet catalog.

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
	startHealthyDoor(t, 4242) // already answering: up is a no-op start

	out, err := runLLMCmd(t, newUpCmd(), "--service")
	if err != nil {
		t.Fatalf("up --service: %v\n%s", err, out)
	}
	if len(*calls) != 1 || (*calls)[0] != (fakeServiceCall{ServiceUnit, true}) {
		t.Fatalf("service calls = %+v, want one enable(%q)", *calls, ServiceUnit)
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

func TestLLMDownServiceRefusedWithoutAServiceLayer(t *testing.T) {
	prev := SetServiceEnabled
	SetServiceEnabled = nil
	t.Cleanup(func() { SetServiceEnabled = prev })
	freeLoopbackPort(t)

	if _, err := runLLMCmd(t, newDownCmd(), "--service"); err == nil {
		t.Fatal("down --service with no service layer: want an error")
	}
}
