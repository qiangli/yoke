package gomod

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	proxySHA    = "999999999999abcdef0123456789abcdef012345"
	proxyPseudo = "v3.0.0-20260103000000-999999999999"
	proxyGoMod  = "module example.com/f/v3\n\ngo 1.21\n"
)

// goproxyFork serves one /v3 module, example.com/f/v3, over the GOPROXY
// protocol. A SHA query answers with the canonical pseudo-version; only that
// version has a .mod and a .zip. It records the paths it was asked for.
func goproxyFork(t *testing.T) (url string, requests func() []string) {
	t.Helper()
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, body := range map[string]string{
		"go.mod": proxyGoMod,
		"f.go":   "package f\n\nconst Name = \"fork\"\n",
	} {
		w, err := zw.Create("example.com/f/v3@" + proxyPseudo + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		const base = "/example.com/f/v3/@v/"
		rest, ok := strings.CutPrefix(r.URL.Path, base)
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch rest {
		case proxySHA + ".info", proxySHA[:12] + ".info", proxyPseudo + ".info":
			w.Write([]byte(`{"Version":"` + proxyPseudo + `","Time":"2026-01-03T00:00:00Z"}`))
		case proxyPseudo + ".mod":
			w.Write([]byte(proxyGoMod))
		case proxyPseudo + ".zip":
			w.Write(zbuf.Bytes())
		default:
			http.Error(w, "not found: unknown revision", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// hermeticGo points the real go command at proxyURL with an empty module cache
// and no checksum database, so nothing leaves the machine.
func hermeticGo(t *testing.T, proxyURL string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	cache := t.TempDir()
	t.Cleanup(func() { // the module cache is read-only
		filepath.Walk(cache, func(p string, _ os.FileInfo, _ error) error { return os.Chmod(p, 0o777) })
	})
	t.Setenv("GOPROXY", proxyURL)
	t.Setenv("GOMODCACHE", filepath.Join(cache, "mod"))
	t.Setenv("GOPATH", filepath.Join(cache, "path"))
	t.Setenv("GOCACHE", filepath.Join(cache, "build"))
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "")
}

// forkConsumer lays out a workspace whose module a imports the /v3 fork
// upstream.example/f/v3 through a versioned replace at a stale pseudo-version.
func forkConsumer(t *testing.T) (a string, ws *Workspace) {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.work"), "go 1.24\n\nuse (\n\t./a\n\t./f\n)\n")
	write(t, filepath.Join(root, "a/go.mod"), `module example.com/a

go 1.21

require upstream.example/f/v3 v3.1.0

replace upstream.example/f/v3 => example.com/f/v3 v3.0.0-20260102000000-aaaaaaaaaaaa
`)
	write(t, filepath.Join(root, "a/a.go"), "package a\n\nimport \"upstream.example/f/v3\"\n\nvar Name = f.Name\n")
	write(t, filepath.Join(root, "f/go.mod"), "module upstream.example/f/v3\n\ngo 1.21\n")
	var err error
	if ws, err = Load(filepath.Join(root, "a")); err != nil || ws == nil {
		t.Fatalf("Load: %v %v", ws, err)
	}
	return filepath.Join(root, "a"), ws
}

func forkHead(sha string) ResolveFunc {
	return func(dir, rev string) (string, error) {
		if filepath.Base(dir) == "f" && rev == "HEAD" {
			return sha, nil
		}
		return "", os.ErrNotExist
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func goIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := RunGo(context.Background(), dir, pinnedEnv, args...)
	if err != nil {
		t.Fatalf("go %v: %v", args, err)
	}
	return string(out)
}

// TestSyncForkReplaceRealGoViaLocalProxy drives the default RunGo against a
// local GOPROXY: the raw SHA must be resolved to the proxy's canonical /v3
// pseudo-version before `go mod edit`, the consumer must build, and a second
// Sync must leave go.mod and go.sum untouched.
func TestSyncForkReplaceRealGoViaLocalProxy(t *testing.T) {
	proxyURL, requests := goproxyFork(t)
	hermeticGo(t, proxyURL)
	a, ws := forkConsumer(t)

	// The bug this guards: `go mod edit` writes a raw SHA verbatim, and a go
	// command that cannot look the revision up then cannot read go.mod at all.
	scratch := t.TempDir()
	write(t, filepath.Join(scratch, "go.mod"), readFile(t, filepath.Join(a, "go.mod")))
	write(t, filepath.Join(scratch, "a.go"), readFile(t, filepath.Join(a, "a.go")))
	if _, err := RunGo(context.Background(), scratch, pinnedEnv, "mod", "edit", "-replace=upstream.example/f/v3=example.com/f/v3@"+proxySHA); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(scratch, "go.mod")); !strings.Contains(got, "example.com/f/v3 "+proxySHA) {
		t.Fatalf("go mod edit did not keep the raw SHA:\n%s", got)
	}
	if _, err := RunGo(context.Background(), scratch, append([]string{"GOPROXY=off"}, pinnedEnv...), "mod", "tidy"); err == nil {
		t.Fatal("go read a raw-SHA replace offline; the regression no longer reproduces")
	}

	// Default RunGo underneath; the wrapper only records what reaches go.
	var calls []string
	record := func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return RunGo(ctx, dir, env, args...)
	}
	ch, err := ws.Sync(context.Background(), ws.Module(a), nil, forkHead(proxySHA), record)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"mod download -json example.com/f/v3@" + proxySHA,
		"mod edit -replace=upstream.example/f/v3=example.com/f/v3@" + proxyPseudo,
		"mod tidy",
	}
	if strings.Join(calls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("go invocations\n got %q\nwant %q", calls, wantCalls)
	}
	if len(ch) != 1 || ch[0].Sibling != "upstream.example/f/v3" || ch[0].To != proxyPseudo {
		t.Fatalf("changes = %+v, want one move to %s", ch, proxyPseudo)
	}
	gomod := readFile(t, filepath.Join(a, "go.mod"))
	if !strings.Contains(gomod, "=> example.com/f/v3 "+proxyPseudo) || strings.Contains(gomod, proxySHA) {
		t.Fatalf("go.mod replace not the canonical pseudo-version:\n%s", gomod)
	}
	var sawSHA bool
	for _, p := range requests() {
		sawSHA = sawSHA || strings.HasSuffix(p, "/@v/"+proxySHA+".info")
	}
	if !sawSHA {
		t.Fatalf("the proxy never saw the SHA query: %q", requests())
	}
	goIn(t, a, "build", "./...")
	if v := goIn(t, a, "list", "-m", "-f", "{{.Replace.Version}}", "upstream.example/f/v3"); strings.TrimSpace(v) != proxyPseudo {
		t.Fatalf("go list replace version = %q", v)
	}

	before := [2]string{gomod, readFile(t, filepath.Join(a, "go.sum"))}
	if before[1] == "" {
		t.Fatal("go.sum is empty after a successful sync")
	}
	ws2, err := Load(a) // a second `mod sync` run re-reads go.mod
	if err != nil || ws2 == nil {
		t.Fatalf("Load: %v %v", ws2, err)
	}
	calls = nil
	ch, err = ws2.Sync(context.Background(), ws2.Module(a), nil, forkHead(proxySHA), record)
	if err != nil || len(ch) != 0 || len(calls) != 0 {
		t.Fatalf("second Sync = %+v, %v; want no changes or go calls (%q)", ch, err, calls)
	}
	if after := [2]string{readFile(t, filepath.Join(a, "go.mod")), readFile(t, filepath.Join(a, "go.sum"))}; after != before {
		t.Fatalf("second Sync rewrote go.mod/go.sum:\n%v\n---\n%v", before, after)
	}
}

// TestSyncForkReplaceRealGoUnresolvableSHA: a SHA the proxy cannot resolve
// fails the Sync before any file is written.
func TestSyncForkReplaceRealGoUnresolvableSHA(t *testing.T) {
	proxyURL, _ := goproxyFork(t)
	hermeticGo(t, proxyURL)
	a, ws := forkConsumer(t)
	before := readFile(t, filepath.Join(a, "go.mod"))

	ch, err := ws.Sync(context.Background(), ws.Module(a), nil, forkHead("123456789012abcdef0123456789abcdef012345"), nil)
	if err == nil || ch != nil {
		t.Fatalf("Sync = %+v, %v; want an error and no changes", ch, err)
	}
	if after := readFile(t, filepath.Join(a, "go.mod")); after != before {
		t.Fatalf("go.mod mutated despite the error:\n%s", after)
	}
	if _, statErr := os.Stat(filepath.Join(a, "go.sum")); !os.IsNotExist(statErr) {
		t.Fatalf("go.sum written despite the error: %v", statErr)
	}
}
