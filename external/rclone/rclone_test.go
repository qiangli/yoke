package rclone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/adapt"
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet"
)

func TestQualificationAssetSelection(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch string
		want           bool
	}{
		{"rclone-v1.70.0-osx-arm64.zip", "darwin", "arm64", true},
		{"rclone-v1.70.0-linux-amd64.zip", "linux", "amd64", true},
		{"rclone-v1.70.0-windows-amd64.zip", "windows", "amd64", true},
		{"rclone-v1.70.0-linux-arm64.zip", "linux", "arm", false},
		{"rclone-v1.70.0-linux-amd64.zip.sha256", "linux", "amd64", false},
		{"rclone-v1.70.0-linux-amd64.deb", "linux", "amd64", false},
		{"other-linux-amd64.zip", "linux", "amd64", false},
		{"rclone-v1.70.0-osx-arm64.zip", "linux", "arm64", false},
	} {
		if got := assetMatch(tc.name, tc.os, tc.arch); got != tc.want {
			t.Errorf("%s %s/%s: %v, want %v", tc.name, tc.os, tc.arch, got, tc.want)
		}
	}
}

// The default is a reviewed release with a committed digest on every shipped
// platform; "latest" and unpinned versions are refused before any network.
func TestDefaultVersionPinnedAndUnpinnedRefused(t *testing.T) {
	if DefaultVersion == "latest" || DefaultVersion == "" {
		t.Fatalf("DefaultVersion %q is not a reviewed pin", DefaultVersion)
	}
	for _, p := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"} {
		if sha, ok := binmgr.PinnedSHA256("rclone", DefaultVersion, p); !ok || len(sha) != 64 {
			t.Errorf("rclone %s has no committed pin for %s", DefaultVersion, p)
		}
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	for _, v := range []string{"latest", "v0.0.1-unreviewed"} {
		if _, err := Path(context.Background(), v); err == nil || !strings.Contains(err.Error(), "no committed sha256 pin") {
			t.Errorf("Path(%q) = %v, want fail-closed pin refusal", v, err)
		}
	}
}

func TestQualificationExplicitVersion(t *testing.T) {
	s := Spec("v1.70.0")
	if s.Version != "v1.70.0" || s.Repo != "rclone/rclone" || s.Member != "rclone" {
		t.Fatalf("unexpected spec: %+v", s)
	}
}

// Local reversible fixtures only. A selected client is required; this test never
// calls Path/Ensure and therefore cannot provision a binary or service. The
// dry-run goes through the 446 S6 adapt rows (pkg/fleet baseline rclone-copy),
// so the preview mapping under test is the shipped one.
func TestQualificationExplicitDryRun(t *testing.T) {
	binary := os.Getenv("S457_RCLONE_TEST_BINARY")
	if binary == "" {
		t.Skip("rclone artifact unavailable; set S457_RCLONE_TEST_BINARY explicitly")
	}
	if !filepath.IsAbs(binary) || strings.TrimSuffix(filepath.Base(binary), ".exe") != "rclone" {
		t.Fatal("test executable must be an absolute path to a file named rclone")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("platform=%s/%s executable_sha256=%x", runtime.GOOS, runtime.GOARCH, sha256.Sum256(data))
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	for _, dir := range []string{src, dst} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "fixture.txt"), []byte("s457 local fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "empty.conf")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	env := ChildEnv([]string{"HOME=" + root, "SystemRoot=" + os.Getenv("SystemRoot")})
	global := []string{"--config", config, "--retries", "1", "--low-level-retries", "1"}
	exec := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := binmgr.Command(ctx, binary, append(append([]string(nil), global...), args...)...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	run := func(args ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec(ctx, args...)
		if err != nil {
			t.Fatalf("explicit fixture command: %v: %s", err, out)
		}
		return out
	}

	// Read.
	var entries []struct{ Name string }
	if err := json.Unmarshal(run("lsjson", src), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "fixture.txt" {
		t.Fatalf("listing: %+v", entries)
	}
	t.Log("read: lsjson on local backend returned the fixture")

	// Dry-run transfer via the shipped adapt row; the child is the explicit
	// binary (adapt execs argv[0] from PATH, so PATH holds only its directory).
	rows, errs := fleet.New(fleet.WithoutCloudOverlay()).Adapters()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	plan, err := adapt.Build(rows, append([]string{"rclone", "copy", src, dst}, global...), adapt.Request{Options: map[string]string{"dry-run": "true"}})
	if err != nil || plan.Adapter.Name != "rclone-copy" || plan.Argv[len(plan.Argv)-1] != "--dry-run" {
		t.Fatalf("adapt plan: %+v %v", plan, err)
	}
	t.Setenv("PATH", filepath.Dir(binary))
	t.Setenv("HOME", root)
	var out, diag bytes.Buffer
	if code := adapt.Run(context.Background(), plan, nil, &out, &diag, nil); code != 0 {
		t.Fatalf("adapt dry-run exit=%d: %s%s", code, out.String(), diag.String())
	}
	if files, err := os.ReadDir(dst); err != nil || len(files) != 0 {
		t.Fatalf("dry-run changed destination: %v %v", files, err)
	}
	t.Logf("dry-run: adapt row %s, preview_level=%s, destination unchanged", plan.Adapter.Name, plan.Adapter.PreviewLevel)

	// Cancellation: rcat blocks on a stdin that never closes; cancelling the
	// context must kill and reap the child, and write nothing.
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	defer pr.Close()
	cmd := binmgr.Command(ctx, binary, append(append([]string(nil), global...), "rcat", filepath.Join(dst, "cancelled.txt"))...)
	cmd.Env, cmd.Stdin = env, pr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reap client")
	}
	if _, err := os.Stat(filepath.Join(dst, "cancelled.txt")); !os.IsNotExist(err) {
		t.Fatalf("cancelled rcat left a destination file: %v", err)
	}
	t.Log("cancellation: child returned failure, was reaped, wrote nothing")

	// Secret redaction: a canary credential in a loopback sftp remote (port 1,
	// refused) must not appear in rclone's own redacted config dump or in the
	// provider failure diagnostics, neither as plaintext nor obscured.
	const canary = "s457-canary-secret"
	obscured := strings.TrimSpace(string(run("obscure", canary)))
	secretConf := filepath.Join(root, "secret.conf")
	body := "[canary]\ntype = sftp\nhost = 127.0.0.1\nport = 1\nuser = u\npass = " + obscured + "\n"
	if err := os.WriteFile(secretConf, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	global[1] = secretConf
	redacted := run("config", "redacted", "canary")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	failed, err := exec(ctx2, "--contimeout", "2s", "lsjson", "canary:")
	if err == nil {
		t.Fatal("loopback sftp fixture unexpectedly succeeded")
	}
	for _, leak := range []string{canary, obscured} {
		if bytes.Contains(redacted, []byte(leak)) || bytes.Contains(failed, []byte(leak)) {
			t.Fatal("rclone diagnostics disclosed the canary credential")
		}
	}
	t.Log("redaction: config redacted and provider failure omit canary (plain and obscured)")
	t.Log("local read, adapt dry-run, cancellation and redaction passed; no remote backend support claimed (Sprint 465)")
}
