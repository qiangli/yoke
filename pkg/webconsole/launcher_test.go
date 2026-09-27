package webconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/svcd"
)

func launcherFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"index.html": "<html><head></head><body>custom launcher</body></html>", "custom.css": "custom styles", "app.css": "wrong stock styles"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func registryRequest(h http.Handler, method, path, body, peer string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = peer
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCustomLauncherPreservesStockPagesAssetsAndGate(t *testing.T) {
	dir := launcherFixture(t)
	h := newTestHandler(t, Options{Launcher: dir})
	w := do(h, "GET", "/", "127.0.0.1:1", nil)
	if !strings.Contains(w.Body.String(), "custom launcher") || !strings.Contains(w.Body.String(), `<base href="/">`) {
		t.Fatalf("custom root = %s", w.Body.String())
	}
	if w := do(h, "GET", "/custom.css", "127.0.0.1:1", nil); w.Body.String() != "custom styles" {
		t.Fatalf("static = %s", w.Body.String())
	}
	for _, path := range []string{"/app.css", "/term/", "/api/apps"} {
		custom := do(h, "GET", path, "127.0.0.1:1", nil)
		stock := do(newTestHandler(t, Options{}), "GET", path, "127.0.0.1:1", nil)
		if custom.Code != stock.Code || !bytes.Equal(custom.Body.Bytes(), stock.Body.Bytes()) {
			t.Errorf("custom launcher changed stock route %s", path)
		}
	}
	locked := newTestHandler(t, Options{Launcher: dir, RequireLogin: true})
	for _, path := range []string{"/", "/custom.css", "/api/apps"} {
		if w := do(locked, "GET", path, "127.0.0.1:1", nil); w.Code != http.StatusForbidden {
			t.Errorf("ungated %s: %d", path, w.Code)
		}
	}
	stock := newTestHandler(t, Options{})
	if w := do(stock, "GET", "/", "127.0.0.1:1", nil); strings.Contains(w.Body.String(), "custom launcher") {
		t.Fatal("default launcher replaced")
	}
	if _, _, err := Handler(Options{Launcher: t.TempDir()}); err == nil {
		t.Fatal("missing index accepted")
	}
}

func TestCustomLauncherComposesBasePrefix(t *testing.T) {
	h := newTestHandler(t, Options{Launcher: launcherFixture(t)})
	hdr := http.Header{}
	hdr.Set(coopauth.HdrForwardedPrefix, "/hosts/example/app/console")
	// An authenticated tunnel projection uses the same test identity as stock routes.
	hdr.Set(coopauth.HdrRemoteUser, "operator")
	w := do(h, "GET", "/", "127.0.0.1:1", hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<base href="/hosts/example/app/console/">`) {
		t.Fatalf("prefix = %d %s", w.Code, w.Body.String())
	}
}

func TestRegisteredAPIIsLiveAndValidated(t *testing.T) {
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "upstream "+r.URL.Path) }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	h := newTestHandler(t, Options{})
	_ = do(h, "GET", "/api/apps", "127.0.0.1:1", nil) // prime the TTL cache
	body := `{"name":"liveapp","port":` + port + `,"label":"Live app"}`
	if w := registryRequest(h, "POST", "/api/apps/registered", body, "127.0.0.1:1", nil); w.Code != 201 {
		t.Fatalf("add = %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/apps", "127.0.0.1:1", nil); !strings.Contains(w.Body.String(), `"name":"liveapp"`) {
		t.Fatal("add hidden by probe cache")
	}
	if w := do(h, "GET", "/liveapp/x", "127.0.0.1:1", nil); w.Body.String() != "upstream /x" {
		t.Fatalf("live proxy = %d %s", w.Code, w.Body.String())
	}
	if w := registryRequest(h, "DELETE", "/api/apps/registered/liveapp", "", "127.0.0.1:1", nil); w.Code != 204 {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/apps", "127.0.0.1:1", nil); strings.Contains(w.Body.String(), `"name":"liveapp"`) {
		t.Fatal("deleted app listed")
	}
	if w := do(h, "GET", "/liveapp/x", "127.0.0.1:1", nil); strings.HasPrefix(w.Body.String(), "upstream") {
		t.Fatal("deleted app still proxied")
	}
	for _, bad := range []string{`{"name":"files","port":8080}`, `{"name":"newapp","port":0}`, `{"name":"newapp","port":65536}`, `{"name":"newapp","port":8080,"unknown":true}`, `{"name":"newapp","port":8080} {}`} {
		if w := registryRequest(h, "POST", "/api/apps/registered", bad, "127.0.0.1:1", nil); w.Code != 400 {
			t.Errorf("accepted %s: %d", bad, w.Code)
		}
	}
}

func TestRegisteredAPIDoesNotBorrowPublicAuth(t *testing.T) {
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "public upstream") }))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	cat := fleet.New(fleet.WithAppValidate(ValidateRegisteredApp))
	app := fleet.App{Name: "liveauth", Port: n, Auth: AuthPublic}
	if err := cat.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, Options{RequireLogin: true})
	if w := do(h, "GET", "/liveauth/", "192.0.2.1:1", nil); w.Code != 200 {
		t.Fatalf("public = %d", w.Code)
	}
	for _, method := range []string{"POST", "DELETE"} {
		path := "/api/apps/registered"
		if method == "DELETE" {
			path += "/liveauth"
		}
		if w := registryRequest(h, method, path, `{"name":"other","port":8080}`, "192.0.2.1:1", nil); w.Code != 403 {
			t.Errorf("public caller borrowed registry auth: %d", w.Code)
		}
	}
	app.Auth = AuthSystem
	if err := cat.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	if w := do(h, "GET", "/liveauth/", "192.0.2.1:1", nil); w.Code != 403 {
		t.Fatalf("stale public auth = %d", w.Code)
	}
	device, store := pairEnv(t)
	cookie := redeemScan(t, device, store, []string{"apps"}, time.Hour)
	if w := registryRequest(device, "POST", "/api/apps/registered", `{"name":"other","port":8080}`, "192.0.2.1:1", cookie); w.Code != 403 || !strings.Contains(w.Body.String(), "operator session") {
		t.Fatalf("live scoped device reached registry writer: %d %s", w.Code, w.Body.String())
	}

}

func TestRegisteredAPIReservesDisabledStockAndExplicitApps(t *testing.T) {
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	h := newTestHandler(t, Options{Disable: []string{"files"}, Apps: []string{"fixture"}, ProbeApp: func(_ context.Context, _ string) (AppMeta, error) {
		return AppMeta{SchemaVersion: MetaSchema, Name: "fixture", Mount: "fixture", Mode: atlas.WebProxy, Port: 8080, Auth: AuthSystem}, nil
	}})
	for _, name := range []string{"files", "fixture"} {
		if w := registryRequest(h, "POST", "/api/apps/registered", `{"name":"`+name+`","port":8081}`, "127.0.0.1:1", nil); w.Code != 400 {
			t.Errorf("reclaimed %s: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestCustomLauncherServicePersistsAbsoluteDirectory(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	dir := launcherFixture(t)
	cwd, _ := os.Getwd()
	relative, _ := filepath.Rel(cwd, dir)
	launcher, err := serviceLauncherPlan(relative, true)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(launcher) {
		t.Fatalf("launcher = %s", launcher)
	}
	if err := saveServiceProfile(svcd.Options{Port: 23456}, false, launcher); err != nil {
		t.Fatal(err)
	}
	reused, err := serviceLauncherPlan("", false)
	if err != nil || reused != launcher {
		t.Fatalf("saved launcher = %q %v", reused, err)
	}
	if got := strings.Join(serviceSpec(false, reused).Argv, " "); !strings.Contains(got, "--launcher "+launcher) {
		t.Fatalf("daemon argv = %s", got)
	}
	cleared, err := serviceLauncherPlan("", true)
	if err != nil || cleared != "" {
		t.Fatalf("explicit stock = %q %v", cleared, err)
	}
}

func TestAppListJSONMatchesAPIRows(t *testing.T) {
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	if err := fleet.New(fleet.WithAppValidate(ValidateRegisteredApp)).SaveApp(fleet.App{Name: "parityapp", Port: 54321, Label: "Parity app"}); err != nil {
		t.Fatal(err)
	}
	cmd := NewAppsCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"list", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var cli, api struct {
		Schema string   `json:"schema_version"`
		Base   string   `json:"base"`
		Apps   []Status `json:"apps"`
	}
	if err := json.Unmarshal(output.Bytes(), &cli); err != nil {
		t.Fatal(err)
	}
	w := do(newTestHandler(t, Options{}), "GET", "/api/apps", "127.0.0.1:1", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	sort.Slice(cli.Apps, func(i, j int) bool { return cli.Apps[i].Name < cli.Apps[j].Name })
	sort.Slice(api.Apps, func(i, j int) bool { return api.Apps[i].Name < api.Apps[j].Name })
	registered := false
	for _, app := range cli.Apps {
		if app.Name == "parityapp" && app.Source == "registered" {
			registered = true
		}
	}
	if !registered {
		t.Fatal("CLI projection omitted registered app")
	}
	cliBytes, _ := json.Marshal(cli)
	apiBytes, _ := json.Marshal(api)
	if !bytes.Equal(cliBytes, apiBytes) {
		t.Fatalf("CLI differs from API:\n%s\n%s", cliBytes, apiBytes)
	}
}

// Exercise the command's flag wiring: --pair/--port must still consult the
// saved launcher. A removed directory then fails before any daemon is started.
func TestServiceExplicitPairPortRetainsSavedLauncher(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	dir := launcherFixture(t)
	if err := saveServiceProfile(svcd.Options{}, false, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"service": otelServiceName})
	}))
	defer up.Close()
	port := up.Listener.Addr().(*net.TCPAddr).Port
	cmd := NewAppsCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"service", "start", "--pair", "--port", strconv.Itoa(port)})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "launcher index.html") {
		t.Fatalf("explicit pair/port dropped saved launcher: %v", err)
	}
}

func TestServiceLauncherOnlyRetainsPairingListenerProfile(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	old := svcd.Options{Bind: "192.0.2.55", Port: 23456}
	if err := saveServiceProfile(old, true); err != nil {
		t.Fatal(err)
	}
	dir, err := serviceDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := NewAppsCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"service", "start", "--launcher", launcherFixture(t), "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var status svcd.Status
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Addr != "192.0.2.55:23456" {
		t.Fatalf("launcher-only command reset listener profile: %+v", status)
	}
	_, pair, err := serviceStartPlan(svcd.Options{}, false, false)
	if err != nil || !pair {
		t.Fatalf("launcher-only command disarmed pairing: %v %v", pair, err)
	}
}

func TestRegisteredAPIRejectsSimpleFormContentType(t *testing.T) {
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	h := newTestHandler(t, Options{})
	for _, contentType := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		r := httptest.NewRequest("POST", "/api/apps/registered", strings.NewReader(`{"name":"formapp","port":8080}`))
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("accepted %q: %d", contentType, w.Code)
		}
	}
	if _, exists := fleet.New().App("formapp"); exists {
		t.Fatal("form content saved app")
	}
}
