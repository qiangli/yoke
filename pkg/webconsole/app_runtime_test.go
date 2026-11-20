package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/coopauth"
	shape "github.com/qiangli/yoke/pkg/shape/state"
)

func TestAppRuntimeLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, base, _, _, _ := serverAppSource(t, home)
	panel := serverPanel(source)
	h := newServerAppHarness(t, home, panel)

	absent := NewAppsCmd()
	absent.SetArgs([]string{"show", panel.Name, "--runtime"})
	if err := absent.Execute(); err == nil || !strings.Contains(err.Error(), "open /serverapp/") {
		t.Fatalf("absent runtime: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://untrusted.invalid/serverapp/page", nil)
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}))
	handler := coopauth.Mount("/serverapp", h.s.shapedApp(panel).handler())
	member := httptest.NewRecorder()
	handler.ServeHTTP(member, withPrincipal(req, "device:test-member"))
	if member.Code != http.StatusOK {
		t.Fatalf("start member fixture: %d %s", member.Code, member.Body.String())
	}
	memberStore, err := shape.Open(panel.Name, "device:test-member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memberStore.ReadRuntime(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member runtime: %v", err)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, withPrincipal(req, osPrincipal(currentOSUser())))
	if w.Code != http.StatusOK {
		t.Fatalf("start fixture: %d %s", w.Code, w.Body.String())
	}

	var output bytes.Buffer
	cmd := NewAppsCmd()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"show", panel.Name, "--runtime", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var info shape.Runtime
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	st, err := shape.Open(panel.Name, osPrincipal(currentOSUser()))
	if err != nil {
		t.Fatal(err)
	}
	if info.URL != "http://127.0.0.1:8080/serverapp/" || info.Revision != base ||
		info.Data != filepath.Join(st.Dir(), "data") || info.PID != os.Getpid() {
		t.Fatalf("unexpected runtime: %+v", info)
	}
	file, err := os.Stat(filepath.Join(st.Dir(), "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && file.Mode().Perm() != 0o600 {
		t.Fatalf("runtime permissions = %04o", file.Mode().Perm())
	}
	h.s.servers.reap(time.Now().Add(time.Second))
	if _, err := st.ReadRuntime(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime after idle stop: %v", err)
	}
}

func TestAppRuntimeRejectsStalePID(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	st, err := shape.Open("viewer", osPrincipal(currentOSUser()))
	if err != nil {
		t.Fatal(err)
	}
	const mount = "http://127.0.0.1:8080/viewer/"
	if err := st.WriteRuntime(shape.Runtime{PID: 2147483647, URL: mount}); err != nil {
		t.Fatal(err)
	}
	_, err = localAppRuntime(context.Background(), "viewer")
	if err == nil || !strings.Contains(err.Error(), "runtime process has stopped") ||
		!strings.Contains(err.Error(), mount) || !strings.Contains(err.Error(), "VIEWER_ADDR and VIEWER_DATA") {
		t.Fatalf("stale runtime: %v", err)
	}
}

func TestAppRuntimeRejectsUnhealthyListener(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	st, err := shape.Open("chart", osPrincipal(currentOSUser()))
	if err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("health check path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer listener.Close()
	info := shape.Runtime{
		Addr: strings.TrimPrefix(listener.URL, "http://"), Data: filepath.Join(st.Dir(), "data"),
		URL: "http://127.0.0.1:8080/chart/", PID: os.Getpid(), Revision: "test-revision",
	}
	if err := st.WriteRuntime(info); err != nil {
		t.Fatal(err)
	}
	_, err = localAppRuntime(context.Background(), "chart")
	if err == nil || !strings.Contains(err.Error(), "runtime health check returned 503") {
		t.Fatalf("unhealthy runtime: %v", err)
	}
}

func TestRuntimeRecordOldProcessCannotRemoveReplacement(t *testing.T) {
	st, err := shape.OpenAt(t.TempDir(), "viewer", "os:operator")
	if err != nil {
		t.Fatal(err)
	}
	old, current := shape.Runtime{PID: 1}, shape.Runtime{PID: 2}
	for _, info := range []shape.Runtime{old, current} {
		if err := st.WriteRuntime(info); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RemoveRuntime(old); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ReadRuntime(); err != nil || got != current {
		t.Fatalf("replacement = %+v, %v", got, err)
	}
}
