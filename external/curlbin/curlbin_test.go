package curlbin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQualificationMissingHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows fallback downloads; native artifact gate required")
	}
	t.Setenv("PATH", t.TempDir())
	path, err := Ensure(context.Background())
	if path != "" || err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("path=%q err=%v", path, err)
	}
}

// Explicit opt-in prevents PATH discovery from silently admitting a host binary.
// This tests transport only; curl supplies no service cursor or native envelope.
func TestQualificationExplicitClient(t *testing.T) {
	binary := os.Getenv("S457_CURL_TEST_BINARY")
	if binary == "" {
		t.Skip("set S457_CURL_TEST_BINARY to an explicitly selected curl")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("test executable must be absolute")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("platform=%s/%s executable_sha256=%x", runtime.GOOS, runtime.GOARCH, sha256.Sum256(data))
	dir := t.TempDir()
	name := "curl"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.Symlink(binary, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", dir)
	t.Setenv("CURL_HOME", dir)
	const canary = "s457-canary-secret"
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected effect: %s", r.Method)
		}
		switch r.URL.Path {
		case "/page/1":
			fmt.Fprint(w, `{"items":[1],"next":"/page/2"}`)
		case "/page/2":
			fmt.Fprint(w, `{"items":[2],"next":null}`)
		case "/denied":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, canary)
		case "/wait":
			started <- struct{}{}
			<-r.Context().Done()
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	run := func(ctx context.Context, path string) (string, string, error) {
		cmd := NewCurlCmd()
		var out, diag bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&diag)
		cmd.SetIn(strings.NewReader(""))
		cmd.SetArgs([]string{"-q", "--noproxy", "*", "--silent", "--show-error", "--fail", "--max-time", "3", server.URL + path})
		err := cmd.ExecuteContext(ctx)
		return out.String(), diag.String(), err
	}
	for _, page := range []string{"/page/1", "/page/2"} {
		out, diag, err := run(context.Background(), page)
		if err != nil || !strings.Contains(out, `"items"`) || diag != "" {
			t.Fatalf("read %s: out=%q diag=%q err=%v", page, out, diag, err)
		}
		t.Logf("read %s: %s", page, out)
	}
	out, diag, err := run(context.Background(), "/denied")
	var foreign *exec.ExitError
	if !errors.As(err, &foreign) || foreign.ExitCode() != 22 {
		t.Fatalf("foreign status: %v", err)
	}
	if strings.Contains(out+diag+err.Error(), canary) {
		t.Fatal("--fail exposed error-body canary")
	}
	t.Log("denied: foreign exit=22; --fail suppresses provider error-body canary (not a general redaction guarantee)")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := run(ctx, "/wait"); done <- err }()
	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("client did not reach fixture")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
		t.Log("cancellation: child returned failure and was reaped")
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reap client")
	}
}
