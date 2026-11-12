package webconsole

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSiteFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func siteRequest(h http.Handler, target string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if header != nil {
		r.Header = header.Clone()
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSiteHandlerServesLiveIndexRangeAndSecurityHeaders(t *testing.T) {
	root := t.TempDir()
	writeSiteFile(t, root, "index.html", `<a href="media.bin">media</a>`)
	writeSiteFile(t, root, "media.bin", "0123456789")
	h := siteHandler(root)

	index := siteRequest(h, "/", nil)
	if index.Code != http.StatusOK || index.Body.String() != `<a href="media.bin">media</a>` {
		t.Fatalf("index = %d %q", index.Code, index.Body.String())
	}
	if got := index.Header().Get("Content-Security-Policy"); got != siteContentSecurityPolicy || strings.Contains(got, "allow-same-origin") {
		t.Fatalf("CSP = %q", got)
	}
	if got := index.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if index.Header().Get("ETag") == "" || index.Header().Get("Last-Modified") == "" || !strings.HasPrefix(index.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("metadata headers = %#v", index.Header())
	}

	rangeHeader := http.Header{"Range": {"bytes=3-6"}}
	part := siteRequest(h, "/media.bin", rangeHeader)
	if part.Code != http.StatusPartialContent || part.Body.String() != "3456" || part.Header().Get("Content-Range") != "bytes 3-6/10" {
		t.Fatalf("range = %d %q %q", part.Code, part.Body.String(), part.Header().Get("Content-Range"))
	}

	writeSiteFile(t, root, "index.html", "edited live")
	live := siteRequest(h, "/", nil)
	if live.Code != http.StatusOK || live.Body.String() != "edited live" {
		t.Fatalf("live edit = %d %q", live.Code, live.Body.String())
	}
}

func TestSiteHandlerRefusesTraversalDotsGitignoreAndSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeSiteFile(t, root, "index.html", "ok")
	writeSiteFile(t, root, ".env", "secret")
	writeSiteFile(t, root, ".Git/config", "secret")
	writeSiteFile(t, root, "ignored.txt", "secret")
	writeSiteFile(t, root, "ignored/child.txt", "secret")
	writeSiteFile(t, root, "plain/child.txt", "not listed")
	writeSiteFile(t, root, ".gitignore", "ignored.txt\nignored/\n")
	writeSiteFile(t, outside, "secret.txt", "outside")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Logf("symlink unavailable: %v", err)
	}
	h := siteHandler(root)

	for _, target := range []string{
		"/../secret.txt",
		"/%2e%2e/secret.txt",
		`/..\secret.txt`,
		"/.env",
		"/.Git/config",
		"/ignored.txt",
		"/ignored/child.txt",
		"/escape.txt",
	} {
		t.Run(target, func(t *testing.T) {
			w := siteRequest(h, target, nil)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s = %d %q", target, w.Code, w.Body.String())
			}
		})
	}
	if w := siteRequest(h, "/ignored/", nil); w.Code != http.StatusNotFound {
		t.Fatalf("directory listing/ignore = %d %q", w.Code, w.Body.String())
	}
	if w := siteRequest(h, "/plain/", nil); w.Code != http.StatusNotFound {
		t.Fatalf("directory listing = %d %q", w.Code, w.Body.String())
	}
}

func TestRegisteredSiteUsesPrefixMountAndAuthPath(t *testing.T) {
	root := t.TempDir()
	writeSiteFile(t, root, "index.html", `<a href="asset.txt">asset</a>`)
	writeSiteFile(t, root, "asset.txt", "mounted")
	writeAppRecord(t, "vlog", fmt.Sprintf("name: vlog\nkind: app\ntype: site\nsource: %q\n", root))

	h := newTestHandler(t, Options{Ctx: context.Background()})
	index := do(h, http.MethodGet, "/vlog/", "127.0.0.1:5555", nil)
	if index.Code != http.StatusOK || index.Body.String() != `<a href="asset.txt">asset</a>` {
		t.Fatalf("mounted index = %d %q", index.Code, index.Body.String())
	}
	asset := do(h, http.MethodGet, "/vlog/asset.txt", "127.0.0.1:5555", nil)
	if asset.Code != http.StatusOK || asset.Body.String() != "mounted" {
		t.Fatalf("mounted asset = %d %q", asset.Code, asset.Body.String())
	}

	// Keep modtime observably different on filesystems with coarse timestamps;
	// the next request must still reopen the linked directory rather than a copy.
	now := time.Now().Add(2 * time.Second)
	writeSiteFile(t, root, "asset.txt", "changed")
	_ = os.Chtimes(filepath.Join(root, "asset.txt"), now, now)
	asset = do(h, http.MethodGet, "/vlog/asset.txt", "127.0.0.1:5555", nil)
	if asset.Code != http.StatusOK || asset.Body.String() != "changed" {
		t.Fatalf("mounted live edit = %d %q", asset.Code, asset.Body.String())
	}
}
