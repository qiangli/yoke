package loom

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEnsureConfig_SeedsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ensureConfig(dir, "127.0.0.1", 3000, "https://ai.dhnt.io/matrix/h/dragon/app/loom/", true)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if cfg != filepath.Join(dir, "app.ini") {
		t.Fatalf("cfg path = %s", cfg)
	}
	b, _ := os.ReadFile(cfg)
	s := string(b)
	for _, want := range []string{
		"INSTALL_LOCK = true", // boots ready, not /install
		"DB_TYPE = sqlite3",   // no external DB
		"HTTP_ADDR = 127.0.0.1",
		"HTTP_PORT = 3000",
		"ROOT_URL = https://ai.dhnt.io/matrix/h/dragon/app/loom/",
		"SECRET_KEY = ",
		"DISABLE_REGISTRATION = true",
		"ENABLE_REVERSE_PROXY_AUTHENTICATION = true",
		"ENABLE_REVERSE_PROXY_AUTO_REGISTRATION = true",
		"REVERSE_PROXY_AUTHENTICATION_USER = X-WEBAUTH-USER",
		"REVERSE_PROXY_AUTHENTICATION_EMAIL = X-WEBAUTH-EMAIL",
		"[actions]",      // local CI control plane
		"ENABLED = true", // act_runner registers against it
	} {
		if !strings.Contains(s, want) {
			t.Errorf("seeded config missing %q", want)
		}
	}
	header, err := os.ReadFile(filepath.Join(dir, "custom", "templates", "custom", "header.tmpl"))
	if err != nil {
		t.Fatalf("custom header: %v", err)
	}
	for _, want := range []string{"https://docs.gitea.com", ".page-footer", "p.large", "navbar-logo", "/app/loom/"} {
		if !strings.Contains(string(header), want) {
			t.Errorf("custom header missing %q", want)
		}
	}
	if strings.Contains(string(header), "/user/login") {
		t.Error("custom header should not hide the sign-in link; browser issue filing needs login")
	}
	// Second call must not overwrite (stable secret across restarts), with the
	// same actions toggle.
	if _, err := ensureConfig(dir, "127.0.0.1", 3000, "https://ai.dhnt.io/matrix/h/dragon/app/loom/", true); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(cfg); string(b2) != s {
		t.Fatal("ensureConfig overwrote an existing config")
	}
}

func TestEnsureConfig_ReconcilesServerAndActions(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfig(dir, "127.0.0.1", 3001, "https://ai.dhnt.io/matrix/h/dragon/app/loom/", false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	s := string(data)
	for _, want := range []string{
		"HTTP_PORT = 3001",
		"ROOT_URL = https://ai.dhnt.io/matrix/h/dragon/app/loom/",
		"ENABLED = false",
		"ENABLE_REVERSE_PROXY_AUTHENTICATION = true",
		"REVERSE_PROXY_AUTHENTICATION_USER = X-WEBAUTH-USER",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("reconciled config missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "SECRET_KEY = \n") {
		t.Fatalf("secret was lost:\n%s", s)
	}
}

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Repo != "go-gitea/gitea" || s.Name != "loom" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if Spec("v1.24.0").Version != "v1.24.0" {
		t.Fatal("version override not honored")
	}
}

func TestCommandSurfaceIncludesLifecycleManagement(t *testing.T) {
	cmd := NewLoomCmd()
	have := map[string]bool{}
	for _, c := range cmd.Commands() {
		have[c.Name()] = true
	}
	for _, name := range []string{"serve", "start", "status", "stop", "logs", "expose", "path", "proxy"} {
		if !have[name] {
			t.Fatalf("missing command %q", name)
		}
	}
}

func TestProxyTranslatesRemoteIdentityToWebauth(t *testing.T) {
	var gotUser, gotEmail, gotName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-WEBAUTH-USER")
		gotEmail = r.Header.Get("X-WEBAUTH-EMAIL")
		gotName = r.Header.Get("X-WEBAUTH-FULLNAME")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, "", false) // auto_provision OFF
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Header.Set("Remote-User", "alice@example.com")
	req.Header.Set("Remote-Email", "alice@example.com")
	req.Header.Set("Remote-Name", "Alice")
	req.Header.Set("X-WEBAUTH-USER", "attacker@example.com") // client-supplied, must be stripped
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	// EMAIL is the identity: Gitea matches an existing account by X-WEBAUTH-EMAIL.
	// With auto_provision OFF, X-WEBAUTH-USER is NOT stamped (no username-collision
	// surface), and a client-supplied one is stripped.
	if gotEmail != "alice@example.com" || gotName != "Alice" {
		t.Fatalf("email-login headers = (email=%q,name=%q), want alice@example.com/Alice", gotEmail, gotName)
	}
	if gotUser != "" {
		t.Fatalf("auto_provision OFF must not stamp X-WEBAUTH-USER, got %q", gotUser)
	}
}

func TestProxyAutoProvisionStampsUsername(t *testing.T) {
	// auto_provision ON: additionally stamp X-WEBAUTH-USER (the readable handle) so
	// Gitea auto-registers a brand-new account for a first-time visitor.
	var gotUser string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-WEBAUTH-USER")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	handler, err := loomProxyHandler(upstream.URL, "", true) // auto_provision ON
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Header.Set("Remote-User", "alice@example.com")
	req.Header.Set("Remote-Email", "alice@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotUser != "alice-example.com" {
		t.Fatalf("auto_provision ON: X-WEBAUTH-USER = %q, want alice-example.com", gotUser)
	}
}

func TestProxyUsesSharedLoopbackAdminIdentity(t *testing.T) {
	var gotUser, gotEmail, gotName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-WEBAUTH-USER")
		gotEmail = r.Header.Get("X-WEBAUTH-EMAIL")
		gotName = r.Header.Get("X-WEBAUTH-FULLNAME")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, "", false) // auto_provision OFF
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	// The on-host owner logs in by the loopback EMAIL (matches the pre-created
	// admin account); no username is stamped with auto_provision off.
	if gotEmail != LoopbackEmail || gotName != LoopbackName {
		t.Fatalf("loopback email-login = (email=%q,name=%q), want %s/%s", gotEmail, gotName, LoopbackEmail, LoopbackName)
	}
	if gotUser != "" {
		t.Fatalf("loopback (auto_provision off) must not stamp X-WEBAUTH-USER, got %q", gotUser)
	}
}

func TestProxyStripsPublicPrefix(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("body{}"))
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, "/matrix/h/dragon/app/loom", false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	resp, err := http.Get(proxy.URL + "/matrix/h/dragon/app/loom/assets/css/index.css")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotPath != "/assets/css/index.css" {
		t.Fatalf("upstream path = %q, want /assets/css/index.css", gotPath)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("content-type = %q, want text/css", ct)
	}
}

func TestLoomProxyRewritesToRootWithNoForwardedPrefix(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Location", "https://ai.dhnt.io"+prefix+"/user/login?redirect_to=%2F")
		_, _ = w.Write([]byte(`<meta content="http://127.0.0.1:31880` + prefix + `/avatars/a"><link href="` + prefix + `/assets/css/index.css"><script src="` + prefix + `/assets/js/index.js"></script>`))
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/loom/repo/issues/new", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), prefix) {
		t.Fatalf("direct local HTML still contains public prefix:\n%s", body)
	}
	for _, want := range []string{`href="/assets/css/index.css"`, `src="/assets/js/index.js"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("direct local HTML missing %q:\n%s", want, body)
		}
	}
	if got := resp.Header.Get("Location"); got != "/user/login?redirect_to=%2F" {
		t.Fatalf("Location = %q, want local path", got)
	}
}

func TestLoomProxyRewritesGiteaPathsIntoTheMountPrefix(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Location", "https://ai.dhnt.io"+prefix+"/user/login")
		_, _ = w.Write([]byte(`<link href="` + prefix + `/assets/css/x.css"><script src="` + prefix + `/assets/js/iife.js"></script>`))
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/loom/repo/issues/new", nil)
	req.Header.Set("X-Forwarded-Prefix", "/loom")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{`href="/loom/assets/css/x.css"`, `src="/loom/assets/js/iife.js"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("rewritten HTML missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "/matrix/") {
		t.Fatalf("rewritten HTML still contains cloud path:\n%s", s)
	}
	if got := resp.Header.Get("Location"); got != "/loom/user/login" {
		t.Fatalf("Location = %q, want /loom/user/login", got)
	}
}

func TestLoomProxyLeavesTheCloudPathUnchanged(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	var gotAcceptEncoding string
	const body = `<link href="/matrix/h/dragon/app/loom/assets/css/index.css">`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+prefix+"/loom/repo/issues/new", nil)
	req.Header.Set("X-Forwarded-Prefix", prefix)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	gotBody, _ := io.ReadAll(resp.Body)
	if string(gotBody) != body {
		t.Fatalf("body changed:\n got %q\nwant %q", gotBody, body)
	}
	if gotAcceptEncoding != "gzip" {
		t.Fatalf("Accept-Encoding = %q, want preserved for forwarded traffic", gotAcceptEncoding)
	}
}

func TestLoomProxyRewritesSetCookiePathIntoTheMount(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	for _, tt := range []struct {
		name            string
		forwardedPrefix string
		wantPath        string
	}{
		{name: "mount", forwardedPrefix: "/loom", wantPath: "/loom"},
		{name: "root", wantPath: "/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Set-Cookie", "i_like_gitea=abc; Path="+prefix+"; HttpOnly; SameSite=Lax")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)

			handler, err := loomProxyHandler(upstream.URL, prefix, false)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(handler)
			t.Cleanup(proxy.Close)

			req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
			if tt.forwardedPrefix != "" {
				req.Header.Set("X-Forwarded-Prefix", tt.forwardedPrefix)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			got := resp.Header.Values("Set-Cookie")
			want := "i_like_gitea=abc; Path=" + tt.wantPath + "; HttpOnly; SameSite=Lax"
			if len(got) != 1 || got[0] != want {
				t.Fatalf("Set-Cookie = %q, want %q", got, want)
			}
		})
	}
}

func TestLoomProxyStripsTheMountPrefixFromRequests(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, "/matrix/h/dragon/app/loom", false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/loom/assets/js/iife.js", nil)
	req.Header.Set("X-Forwarded-Prefix", "/loom")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotPath != "/assets/js/iife.js" {
		t.Fatalf("upstream path = %q, want /assets/js/iife.js", gotPath)
	}
}

func TestLoomProxyRewritesTheEscapedAppSubUrl(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	const config = `<script>window.config = {appSubUrl: '\/matrix\/h\/dragon\/app\/loom', assetUrlPrefix: '\/matrix\/h\/dragon\/app\/loom\/assets'};</script>`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(config))
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/loom/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/loom")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{`appSubUrl: '\/loom'`, `assetUrlPrefix: '\/loom\/assets'`} {
		if !strings.Contains(s, want) {
			t.Fatalf("rewritten HTML missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `\/matrix\/`) {
		t.Fatalf("rewritten HTML still contains the escaped cloud prefix:\n%s", s)
	}
}

func TestLoomProxyRewritesTheEscapedAppSubUrlToEmptyAtRoot(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	const config = `<script>window.config = {appSubUrl: '\/matrix\/h\/dragon\/app\/loom', assetUrlPrefix: '\/matrix\/h\/dragon\/app\/loom\/assets'};</script>`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(config))
	}))
	t.Cleanup(upstream.Close)

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{`appSubUrl: ''`, `assetUrlPrefix: '\/assets'`} {
		if !strings.Contains(s, want) {
			t.Fatalf("rewritten HTML missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `\/matrix\/`) {
		t.Fatalf("rewritten HTML still contains the escaped cloud prefix:\n%s", s)
	}
}

func TestLoomProxyRewritesTheAbsoluteAppUrlToTheForwardedOrigin(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	var origin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		esc := strings.ReplaceAll(origin+prefix, "/", `\/`)
		_, _ = fmt.Fprintf(w, `<meta property="og:url" content="%s"><script>window.config = {appUrl: '%s\/'};</script>`, origin+prefix, esc)
	}))
	t.Cleanup(upstream.Close)
	origin = upstream.URL

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/loom/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/loom")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "git.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{`content="https://git.example.com/loom"`, `appUrl: 'https:\/\/git.example.com\/loom\/'`} {
		if !strings.Contains(s, want) {
			t.Fatalf("rewritten HTML missing %q:\n%s", want, s)
		}
	}
	if host := strings.TrimPrefix(origin, "http://"); strings.Contains(s, host) {
		t.Fatalf("rewritten HTML still leaks the upstream origin %q:\n%s", host, s)
	}
}

func TestLoomProxyKeepsTheAbsoluteAppUrlWithoutForwardedHeaders(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	var origin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		esc := strings.ReplaceAll(origin+prefix, "/", `\/`)
		_, _ = fmt.Fprintf(w, `<meta property="og:url" content="%s"><script>window.config = {appUrl: '%s\/'};</script>`, origin+prefix, esc)
	}))
	t.Cleanup(upstream.Close)
	origin = upstream.URL

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/loom/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/loom")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	esc := strings.ReplaceAll(origin+prefix, "/", `\/`)
	for _, want := range []string{`content="` + origin + prefix + `"`, `appUrl: '` + esc + `\/'`} {
		if !strings.Contains(s, want) {
			t.Fatalf("absolute upstream URL was rewritten without a forwarded origin; missing %q:\n%s", want, s)
		}
	}
}

func TestLoomProxyCloudPathStaysByteIdentical(t *testing.T) {
	const prefix = "/matrix/h/dragon/app/loom"
	var origin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		esc := strings.ReplaceAll(origin+prefix, "/", `\/`)
		_, _ = fmt.Fprintf(w, `<meta property="og:url" content="%s"><link href="%s/assets/css/index.css"><script>window.config = {appUrl: '%s\/', appSubUrl: '%s'};</script>`,
			origin+prefix, prefix, esc, strings.ReplaceAll(prefix, "/", `\/`))
	}))
	t.Cleanup(upstream.Close)
	origin = upstream.URL

	handler, err := loomProxyHandler(upstream.URL, prefix, false)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+prefix+"/", nil)
	req.Header.Set("X-Forwarded-Prefix", prefix)
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-Host", strings.TrimPrefix(origin, "http://"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	esc := strings.ReplaceAll(origin+prefix, "/", `\/`)
	want := fmt.Sprintf(`<meta property="og:url" content="%s"><link href="%s/assets/css/index.css"><script>window.config = {appUrl: '%s\/', appSubUrl: '%s'};</script>`,
		origin+prefix, prefix, esc, strings.ReplaceAll(prefix, "/", `\/`))
	if string(got) != want {
		t.Fatalf("cloud-path body changed:\n got %q\nwant %q", got, want)
	}
}

func TestLoomHeaderTemplateScriptCarriesTheCspNonce(t *testing.T) {
	if !strings.Contains(loomHeaderTemplate, `<script nonce="{{ctx.CspScriptNonce}}">`) {
		t.Fatalf("loomHeaderTemplate script tag lacks CSP nonce:\n%s", loomHeaderTemplate)
	}
}

// Gitea renders custom/header with a map as dot, so an unknown key such as
// {{.CspNonce}} silently renders empty and the CSP refuses the script. Render
// the template the way Gitea does (nonce published only via ctx.CspScriptNonce)
// and assert the served script carries it.
func TestLoomHeaderRendersANonEmptyCspNonce(t *testing.T) {
	const nonce = "0123456789abcdef"
	tmpl, err := template.New("custom/header").Funcs(template.FuncMap{
		"ctx": func() map[string]string { return map[string]string{"CspScriptNonce": nonce} },
	}).Parse(loomHeaderTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	if strings.Contains(rendered, `nonce=""`) {
		t.Fatalf("rendered header has an empty script nonce:\n%s", rendered)
	}
	tags := regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(rendered, -1)
	if len(tags) == 0 {
		t.Fatal("rendered header contains no inline script")
	}
	for _, tag := range tags {
		if !strings.Contains(tag, `nonce="`+nonce+`"`) {
			t.Errorf("rendered script tag lacks the CSP nonce: %s", tag)
		}
	}
}

func TestLoomHeaderUsesTheLoomIconAndNotTheTeapot(t *testing.T) {
	// The launcher tile's loom path (pkg/webconsole/artifact/app.js): the
	// header and the tile must be the same identity.
	const loomPath = "M4 5h16M4 19h16M9 5v14M15 5v14M4 12h16"
	if !strings.Contains(loomHeaderTemplate, loomPath) {
		t.Errorf("header template missing the loom icon path %q", loomPath)
	}
	for _, bad := range []string{"logo.svg", "gitea-logo", "teapot"} {
		if strings.Contains(strings.ToLower(loomHeaderTemplate), bad) {
			t.Errorf("header template still references the Gitea logo (%q)", bad)
		}
	}
}

func TestLoomHeaderScriptsCarryTheCspNonce(t *testing.T) {
	// Gitea v1.27.3 publishes script-src with a per-request nonce, so EVERY
	// inline script must carry nonce="{{ctx.CspScriptNonce}}" or it is refused.
	tags := regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(loomHeaderTemplate, -1)
	if len(tags) == 0 {
		t.Fatal("header template contains no inline script")
	}
	for _, tag := range tags {
		if !strings.Contains(tag, `nonce="{{ctx.CspScriptNonce}}"`) {
			t.Errorf("inline script without the CSP nonce: %s", tag)
		}
	}
}

func TestLoomHeaderHomeLinkIsMountRelative(t *testing.T) {
	// The header runs under both the local mount and the cloud path
	// /matrix/h/dragon/app/loom/, so the home link must be derived from
	// window.config.appSubUrl (like the logo href), never a hardcoded "/".
	if strings.Contains(loomHeaderTemplate, `href="/"`) {
		t.Error("header template hardcodes the home link to \"/\"; the cloud path would break")
	}
	for _, want := range []string{"loom-home", "appSubUrl"} {
		if !strings.Contains(loomHeaderTemplate, want) {
			t.Errorf("header template missing derived home link marker %q", want)
		}
	}
}

func TestAdminListContainsUser(t *testing.T) {
	out := `ID   Username Email           IsAdmin
1    admin    admin@localhost true
2    alice    alice@test      true
`
	if !adminListContainsUser(out, "admin") {
		t.Fatal("admin user not detected")
	}
	if adminListContainsUser(out, "root") {
		t.Fatal("unexpected root user detected")
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := State{
		PID:       12345,
		URL:       "http://127.0.0.1:3000",
		RootURL:   "https://ai.dhnt.io/matrix/h/dragon/app/loom/",
		Addr:      "127.0.0.1:3000",
		Version:   "v1.2.3",
		DataDir:   dir,
		LogPath:   filepath.Join(dir, "loom.log"),
		StartedAt: time.Date(2026, 7, 2, 1, 2, 3, 0, time.UTC),
	}
	if err := writeState(st); err != nil {
		t.Fatal(err)
	}
	got, err := readState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != st.PID || got.URL != st.URL || got.RootURL != st.RootURL || got.LogPath != st.LogPath || !got.StartedAt.Equal(st.StartedAt) {
		t.Fatalf("state mismatch: %+v", got)
	}
	if err := removeState(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(dir); !os.IsNotExist(err) {
		t.Fatalf("expected removed state, got %v", err)
	}
}

func TestStartRefusesWhenTheProxyPortIsHeldByAForeignProcess(t *testing.T) {
	ln, host, port := listenOnEphemeralLoopback(t)
	defer ln.Close()

	_, err := StartDaemon(context.Background(), Options{
		DataDir:   t.TempDir(),
		Addr:      host,
		Port:      freeTCPPort(t),
		ProxyPort: port,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err == nil {
		t.Fatal("StartDaemon succeeded with a foreign listener on the proxy port")
	}
	if !strings.Contains(err.Error(), "proxy port") || !strings.Contains(err.Error(), "did not launch") {
		t.Fatalf("error = %v, want clear foreign proxy port refusal", err)
	}
}

func TestStopReclaimsTheProxyPort(t *testing.T) {
	dir := t.TempDir()
	ln, host, port := listenOnEphemeralLoopback(t)
	_ = ln.Close()
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	proxy := startLoomHelper(t, dir, "loom", "proxy", "--target", upstream.URL, "--addr", host, "--port", fmt.Sprintf("%d", port))
	t.Cleanup(func() {
		if proxy.ProcessState == nil {
			_ = proxy.Process.Kill()
			_ = proxy.Wait()
		}
	})
	if err := waitHTTP(context.Background(), "http://"+addr, 5*time.Second); err != nil {
		t.Fatalf("proxy helper did not start: %v", err)
	}
	if err := recordOwnedProxy(dir, addr, proxy.Process.Pid); err != nil {
		t.Fatal(err)
	}
	sleeper := startLoomHelper(t, dir, "sleep")
	t.Cleanup(func() {
		if sleeper.ProcessState == nil {
			_ = sleeper.Process.Kill()
			_ = sleeper.Wait()
		}
	})

	if err := writeState(State{
		PID:       sleeper.Process.Pid,
		URL:       "http://" + net.JoinHostPort(host, fmt.Sprintf("%d", freeTCPPort(t))),
		ProxyPID:  0,
		ProxyURL:  "http://" + addr,
		ProxyAddr: addr,
		RootURL:   "https://ai.dhnt.io/matrix/h/dragon/app/loom/",
		Addr:      net.JoinHostPort(host, fmt.Sprintf("%d", freeTCPPort(t))),
		DataDir:   dir,
		LogPath:   filepath.Join(dir, "loom.log"),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := StopDaemon(dir, 5*time.Second); err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !waitTCPPortFree(addr, 5*time.Second) {
		t.Fatalf("proxy port %s is still held after StopDaemon", addr)
	}
}

func startGiteaStandIn(t *testing.T, dir, addr string, extra ...string) *exec.Cmd {
	t.Helper()
	cmd := startLoomHelper(t, dir, append([]string{"listen", addr}, extra...)...)
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	go func() { _ = cmd.Wait() }()
	if err := waitHTTP(context.Background(), "http://"+addr, 5*time.Second); err != nil {
		t.Fatalf("gitea stand-in did not start: %v", err)
	}
	return cmd
}

func TestStopReclaimsTheGiteaPort(t *testing.T) {
	dir := t.TempDir()
	host := "127.0.0.1"
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", freeTCPPort(t)))
	// The listener is NOT State.PID (the stale-process case): only the owned
	// registry can find it.
	stale := startGiteaStandIn(t, dir, addr)
	if err := recordOwnedProxy(dir, addr, stale.Process.Pid); err != nil {
		t.Fatal(err)
	}
	sleeper := startLoomHelper(t, dir, "sleep")
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	if err := writeState(State{PID: sleeper.Process.Pid, URL: "http://" + addr, Addr: addr, DataDir: dir, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := StopDaemon(dir, 5*time.Second); err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !waitTCPPortFree(addr, 5*time.Second) {
		t.Fatalf("gitea port %s is still held after StopDaemon", addr)
	}
}

func TestStopReclaimsAnUnregisteredGiteaByItsArgv(t *testing.T) {
	dir := t.TempDir()
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", freeTCPPort(t)))
	startGiteaStandIn(t, dir, addr, "web", "--config", filepath.Join(dir, "app.ini"))
	sleeper := startLoomHelper(t, dir, "sleep")
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	if err := writeState(State{PID: sleeper.Process.Pid, URL: "http://" + addr, Addr: addr, DataDir: dir, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := StopDaemon(dir, 5*time.Second); err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !waitTCPPortFree(addr, 5*time.Second) {
		t.Fatalf("gitea port %s is still held after StopDaemon", addr)
	}
}

func TestStopLeavesAForeignGiteaPortListenerAlone(t *testing.T) {
	dir := t.TempDir()
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", freeTCPPort(t)))
	foreign := startGiteaStandIn(t, dir, addr, "web", "--config", "/somewhere/else/app.ini")
	sleeper := startLoomHelper(t, dir, "sleep")
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})
	if err := writeState(State{PID: sleeper.Process.Pid, URL: "http://" + addr, Addr: addr, DataDir: dir, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := StopDaemon(dir, 1*time.Second); err == nil || !strings.Contains(err.Error(), "did not launch") {
		t.Fatalf("StopDaemon error = %v, want it to report the foreign holder", err)
	}
	if !healthy(context.Background(), "http://"+addr, time.Second) {
		t.Fatalf("StopDaemon killed pid %d, a listener loom did not launch", foreign.Process.Pid)
	}
}

func TestStartRefusesWhenTheGiteaPortIsHeldByAForeignProcess(t *testing.T) {
	ln, host, port := listenOnEphemeralLoopback(t)
	defer ln.Close()

	_, err := StartDaemon(context.Background(), Options{
		DataDir:   t.TempDir(),
		Addr:      host,
		Port:      port,
		ProxyPort: freeTCPPort(t),
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err == nil {
		t.Fatal("StartDaemon succeeded with a foreign listener on the gitea port")
	}
	if !strings.Contains(err.Error(), "gitea port") || !strings.Contains(err.Error(), "did not launch") {
		t.Fatalf("error = %v, want clear foreign gitea port refusal", err)
	}
}

func TestStartReclaimsAStaleOwnedGiteaByItsArgv(t *testing.T) {
	dir := t.TempDir()
	host := "127.0.0.1"
	port := freeTCPPort(t)
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	startGiteaStandIn(t, dir, addr, "web", "--config", filepath.Join(dir, "app.ini"))
	if err := ensureGiteaPortFreeForStart(dir, host, port); err != nil {
		t.Fatalf("ensureGiteaPortFreeForStart: %v", err)
	}
	if !waitTCPPortFree(addr, 5*time.Second) {
		t.Fatalf("stale gitea still holds %s", addr)
	}
}

func TestIsLoomGiteaArgs(t *testing.T) {
	cfg := "/data/loom/app.ini"
	for args, want := range map[string]bool{
		"/cache/gitea web --config /data/loom/app.ini":        true,
		"/cache/gitea-1.27.3 web --config /data/loom/app.ini": true,
		"/cache/gitea web --config /other/app.ini":            false,
		"/cache/gitea web":            false,
		"nginx -c /data/loom/app.ini": false,
	} {
		if got := isLoomGiteaArgs(args, cfg); got != want {
			t.Errorf("isLoomGiteaArgs(%q) = %v, want %v", args, got, want)
		}
	}
}

func TestStartWithoutRootURLPreservesAConfiguredRootURL(t *testing.T) {
	dir := t.TempDir()
	const root = "https://ai.dhnt.io/matrix/h/dragon/app/loom/"
	cfg, err := ensureConfig(dir, "127.0.0.1", 3000, root, true)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{DataDir: dir, Addr: "127.0.0.1", Port: 3000, ProxyPort: freeTCPPort(t), Stdout: io.Discard, Stderr: io.Discard}
	rewrite, err := prepareDaemonOptions(&opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if rewrite != nil {
		t.Fatalf("bare start reported a ROOT_URL rewrite: %+v", rewrite)
	}
	if opts.RootURL != root {
		t.Fatalf("RootURL = %q, want configured %q", opts.RootURL, root)
	}
	if p := publicPrefix(opts.RootURL); p == "" {
		t.Fatalf("publicPrefix(%q) is empty; proxy would omit --public-prefix", opts.RootURL)
	}
	if _, err := ensureConfig(opts.DataDir, opts.Addr, opts.Port, opts.RootURL, opts.actionsOn()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("bare start changed app.ini:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestStartWithExplicitRootURLRewritesAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	const oldRoot = "https://ai.dhnt.io/matrix/h/dragon/app/loom/"
	const newRoot = "https://git.example.test/loom/"
	cfg, err := ensureConfig(dir, "127.0.0.1", 3000, oldRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	_, err = StartDaemon(ctx, Options{
		DataDir:         dir,
		Addr:            "127.0.0.1",
		Port:            3000,
		ProxyPort:       freeTCPPort(t),
		RootURL:         newRoot,
		RootURLExplicit: true,
		Stdout:          io.Discard,
		Stderr:          &stderr,
	})
	if err == nil {
		t.Fatal("StartDaemon unexpectedly succeeded with a canceled context")
	}
	data, readErr := os.ReadFile(cfg)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "ROOT_URL = "+newRoot) {
		t.Fatalf("explicit ROOT_URL was not written:\n%s", data)
	}
	msg := stderr.String()
	if !strings.Contains(msg, "rewriting ROOT_URL") || !strings.Contains(msg, oldRoot) || !strings.Contains(msg, newRoot) {
		t.Fatalf("stderr = %q, want explicit ROOT_URL rewrite notice", msg)
	}
}

func listenOnEphemeralLoopback(t *testing.T) (net.Listener, string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	return ln, host, port
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, _, port := listenOnEphemeralLoopback(t)
	_ = ln.Close()
	return port
}

func startLoomHelper(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmdArgs := append([]string{"-test.run=TestLoomHelperProcess", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), "LOOM_HELPER_PROCESS=1")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestLoomHelperProcess(t *testing.T) {
	if os.Getenv("LOOM_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	args = args[1:]
	if len(args) == 1 && args[0] == "sleep" {
		select {}
	}
	if len(args) >= 2 && args[0] == "listen" {
		ln, err := net.Listen("tcp", args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go func() {
			_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		}()
		select {}
	}
	if len(args) >= 1 && args[0] == "loom" {
		cmd := NewLoomCmd()
		cmd.SetArgs(args[1:])
		if err := cmd.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func TestOwnerIdentities(t *testing.T) {
	// explicit Owner + OS user, deduped; empties dropped.
	t.Setenv("LOOM_OWNER", "")
	t.Setenv("USER", "qiangli")
	t.Setenv("LOGNAME", "")
	got := ownerIdentities(Options{Owner: "liqiang@gmail.com"})
	if len(got) != 2 || got[0] != "liqiang@gmail.com" || got[1] != "qiangli" {
		t.Fatalf("ownerIdentities = %v, want [liqiang@gmail.com qiangli]", got)
	}
	// no explicit owner → OS user only
	got = ownerIdentities(Options{})
	if len(got) != 1 || got[0] != "qiangli" {
		t.Fatalf("ownerIdentities(no owner) = %v, want [qiangli]", got)
	}
	// dedup when Owner == OS user
	got = ownerIdentities(Options{Owner: "qiangli"})
	if len(got) != 1 {
		t.Fatalf("ownerIdentities(dup) = %v, want 1 entry", got)
	}
	// $LOOM_OWNER honored
	t.Setenv("LOOM_OWNER", "boss@corp.com")
	got = ownerIdentities(Options{})
	if len(got) < 1 || got[0] != "boss@corp.com" {
		t.Fatalf("ownerIdentities(LOOM_OWNER) = %v, want boss@corp.com first", got)
	}
}
