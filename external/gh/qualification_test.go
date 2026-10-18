package gh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/adapt"
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Local fake-host fixture only: gh's own http_unix_socket setting routes every
// API request over a temp unix socket served here, so no network, account or
// real data is touched. A selected client is required; this test never calls
// Ensure and therefore cannot provision a binary.
func TestQualificationFakeHost(t *testing.T) {
	binary := os.Getenv("S457_GH_TEST_BINARY")
	if binary == "" {
		t.Skip("gh artifact unavailable; set S457_GH_TEST_BINARY explicitly")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses gh's http_unix_socket transport")
	}
	if !filepath.IsAbs(binary) || filepath.Base(binary) != "gh" {
		t.Fatal("test executable must be an absolute path to a file named gh")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("platform=%s/%s executable_sha256=%x", runtime.GOOS, runtime.GOARCH, sha256.Sum256(data))

	const canary = "s457-canary-token"
	// Short path: unix socket names are length-limited (104 bytes on darwin).
	dir, err := os.MkdirTemp("", "s457gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	var mu sync.Mutex
	var auth []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/issues":
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`[{"number":2}]`))
				return
			}
			w.Header().Set("Link", `<https://api.github.com/repos/o/r/issues?page=2>; rel="next", <https://api.github.com/repos/o/r/issues?page=2>; rel="last"`)
			_, _ = w.Write([]byte(`[{"number":1}]`))
		case "/denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		case "/wait":
			waiting <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cfg := filepath.Join(dir, "cfg")
	if err := os.Mkdir(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.yml"), []byte("http_unix_socket: "+sock+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := ChildEnv([]string{"HOME=" + dir, "GH_CONFIG_DIR=" + cfg, "GH_TOKEN=" + canary, "GH_PROMPT_DISABLED=1", "NO_COLOR=1"})
	run := func(ctx context.Context, extra []string, args ...string) (string, string, error) {
		cmd := binmgr.Command(ctx, binary, args...)
		cmd.Env = append(append([]string(nil), env...), extra...)
		var out, diag bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diag
		err := cmd.Run()
		return out.String(), diag.String(), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Read + pagination: --paginate follows the Link header to page 2.
	out, diag, err := run(ctx, nil, "api", "--paginate", "--slurp", "/repos/o/r/issues")
	if err != nil {
		t.Fatalf("paginated read: %v %s", err, diag)
	}
	var pages [][]struct{ Number int }
	if err := json.Unmarshal([]byte(out), &pages); err != nil || len(pages) != 2 || pages[0][0].Number != 1 || pages[1][0].Number != 2 {
		t.Fatalf("pagination: %q %v", out, err)
	}
	t.Log("read+pagination: gh api --paginate fetched both fixture pages")

	// Dry-run: gh has no preview, so the shipped adapt row refuses before exec.
	rows, errs := fleet.New(fleet.WithoutCloudOverlay()).Adapters()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if _, err := adapt.Build(rows, []string{"gh", "api", "-X", "DELETE", "/repos/o/r"}, adapt.Request{Options: map[string]string{"dry-run": "true"}}); err == nil {
		t.Fatal("adapt admitted an unsupported gh dry-run")
	}
	t.Log("dry-run: adapt row gh-api preview_level=unsupported refuses before execution")

	// Redaction: provider failure and API debug tracing must not print the token.
	out, diag, err = run(ctx, []string{"GH_DEBUG=api"}, "api", "/denied")
	if err == nil {
		t.Fatal("denied fixture reported success")
	}
	if strings.Contains(out+diag, canary) {
		t.Fatal("gh diagnostics disclosed the canary token")
	}
	if !strings.Contains(diag, "Authorization: token") {
		t.Fatalf("debug trace did not show the (redacted) auth header: %s", diag)
	}
	mu.Lock()
	seen := append([]string(nil), auth...)
	mu.Unlock()
	for _, a := range seen {
		if a != "token "+canary {
			t.Fatalf("fixture saw unexpected credential header %q", a)
		}
	}
	t.Log("redaction: 403 failure + GH_DEBUG=api trace omit canary token; header sent only to fixture")

	// Cancellation: the fixture holds /wait open; cancel must kill and reap gh.
	cctx, ccancel := context.WithCancel(context.Background())
	defer ccancel()
	done := make(chan error, 1)
	go func() { _, _, err := run(cctx, nil, "api", "/wait"); done <- err }()
	select {
	case <-waiting:
		ccancel()
	case <-time.After(10 * time.Second):
		t.Fatal("client did not reach fixture")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reap client")
	}
	t.Log("cancellation: child returned failure and was reaped; no account/host support claimed (Sprint 465)")
}
