package guard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/browser"
	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/live"
	"github.com/qiangli/yoke/pkg/browser/probe"
	"github.com/qiangli/yoke/pkg/browser/solo"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// These pin the WIRING, from inside the protected package: every backend's
// Execute routes through the gate, so a change that unhooks one fails here.
// No browser is needed — a guarded value is refused before anything is
// launched or dialled.

func isolateEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_ASK_DIR", filepath.Join(root, "ask"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "home"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(root, "home", "room"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestEveryBackendRoutesThroughTheGate(t *testing.T) {
	isolateEnv(t)
	t.Setenv(guard.FakeAskEnv, "fake:skip")
	ssn := wire.Action{Type: wire.ActionEvaluate, Script: `"212-48-7731"`}
	backends := map[string]browser.Client{
		// A headless solo Chrome has no human: stopped, without launching one.
		"solo":  solo.New(solo.Config{ChromePath: "/nonexistent/chrome"}),
		"probe": probe.New("http://127.0.0.1:1"),
		"live":  live.New(freePort(t)),
	}
	for name, c := range backends {
		res, err := c.Execute(context.Background(), ssn)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := guard.ExitCode(res); !ok || res.Reason != "value:ssn" {
			t.Errorf("%s: a guarded value was not stopped by the gate: %+v", name, res)
		}
	}
}

// The hub refuses a raw /dispatch carrying a guarded value — a client that
// skipped the gate (an old binary, a curl) cannot route around it.
func TestHubDispatchRefusesGuardedValues(t *testing.T) {
	isolateEnv(t)
	port := freePort(t)
	s := live.New(port)
	if err := s.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	post := func(method string, params map[string]any) string {
		body, _ := json.Marshal(map[string]any{"method": method, "params": params})
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/dispatch", port), "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for _, c := range []struct {
		method string
		params map[string]any
	}{
		{"type", map[string]any{"selector": "#q", "text": "4111 1111 1111 1111"}},
		{"evaluate", map[string]any{"script": "x='212-48-7731'"}},
		{"clipboard_write", map[string]any{"text": "021000021"}},
		{"navigate", map[string]any{"url": "https://x.test/?iban=DE89370400440532013000"}},
	} {
		got := post(c.method, c.params)
		if !strings.Contains(got, "guard: refused "+c.method) {
			t.Errorf("hub let %s through: %s", c.method, got)
		}
		for _, v := range c.params {
			if s, _ := v.(string); s != "" && s != "#q" && strings.Contains(got, s) {
				t.Errorf("hub echoed the refused value: %s", got)
			}
		}
	}
	// An ordinary value passes the guard (and then meets "not connected").
	if got := post("type", map[string]any{"selector": "#q", "text": "Ada"}); strings.Contains(got, "guard:") {
		t.Errorf("hub refused an ordinary value: %s", got)
	}
}
