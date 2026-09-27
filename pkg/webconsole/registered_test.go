// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestMain keeps every test in this package off the developer's real
// registered-app ring: Discover reads it, and a stray record would change
// the tile list under every other test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "webconsole-apps-")
	if err != nil {
		panic(err)
	}
	os.Setenv("BASHY_APPS_DIR", dir)
	os.Setenv("BASHY_APPS_PATH", "")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func writeAppRecord(t *testing.T, name, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_APPS_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A registered record becomes a proxy tile, read as data: the exec seam is
// never touched.
func TestRegisteredAppIsTiledWithoutExec(t *testing.T) {
	writeAppRecord(t, "jup", "name: jup\nkind: app\nlabel: Jupyter\nport: 8888\n")
	var got *Panel
	for _, p := range Discover() {
		if p.Name == "jup" {
			p := p
			got = &p
		}
	}
	if got == nil {
		t.Fatal("registered app not discovered")
	}
	if got.Source != "registered" || got.Mode != "proxy" || got.Port != 8888 || got.Path != "/jup/" || got.Label != "Jupyter" || got.Auth != AuthSystem {
		t.Errorf("panel = %+v", *got)
	}
}

// A registered app never shadows what bashy ships.
func TestRegisteredAppCannotShadowStockPanel(t *testing.T) {
	writeAppRecord(t, "files", "name: files\nport: 9000\n")
	for _, p := range Discover() {
		if p.Name == "files" && p.Source != "builtin" {
			t.Fatalf("files shadowed by %+v", p)
		}
	}
	if err := ValidateRegisteredApp(appRecord("files", 9000)); err == nil {
		t.Error("add would accept the files mount")
	}
	if err := ValidateRegisteredApp(appRecord("meta", 9000)); err == nil {
		t.Error("add would accept a console-reserved mount")
	}
	if err := ValidateRegisteredApp(appRecord("jup", 9000)); err != nil {
		t.Errorf("a free mount refused: %v", err)
	}
}

// End to end through the handler: /<name>/ proxies to the recorded port.
func TestRegisteredAppIsProxied(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from "+r.URL.Path)
	}))
	defer up.Close()
	_, portStr, _ := net.SplitHostPort(up.Listener.Addr().String())
	writeAppRecord(t, "pyhttp", "name: pyhttp\nport: "+portStr+"\n")

	h := newTestHandler(t, Options{Ctx: context.Background()})
	w := do(h, "GET", "/pyhttp/x", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK || w.Body.String() != "hello from /x" {
		t.Fatalf("proxy = %d %q", w.Code, w.Body.String())
	}
}

func appRecord(name string, port int) fleet.App { return fleet.App{Name: name, Port: port} }
