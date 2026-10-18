package rclone

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
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

func TestQualificationExplicitVersion(t *testing.T) {
	s := Spec("v1.70.0")
	if s.Version != "v1.70.0" || s.Repo != "rclone/rclone" || s.Member != "rclone" {
		t.Fatalf("unexpected spec: %+v", s)
	}
}

// Local reversible fixtures only. A selected client is required; this test never
// calls Path/Ensure and therefore cannot provision a binary or service.
func TestQualificationExplicitDryRun(t *testing.T) {
	binary := os.Getenv("S457_RCLONE_TEST_BINARY")
	if binary == "" {
		t.Skip("rclone artifact unavailable; set S457_RCLONE_TEST_BINARY explicitly")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("test executable must be absolute")
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
	run := func(args ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		argv := append([]string{"--config", config, "--retries", "1", "--low-level-retries", "1"}, args...)
		cmd := binmgr.Command(ctx, binary, argv...)
		cmd.Env = ChildEnv([]string{"HOME=" + root, "SystemRoot=" + os.Getenv("SystemRoot")})
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("explicit fixture command: %v: %s", err, out)
		}
		return out
	}
	var entries []struct{ Name string }
	if err := json.Unmarshal(run("lsjson", src), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "fixture.txt" {
		t.Fatalf("listing: %+v", entries)
	}
	t.Logf("dry-run: %s", run("copy", src, dst, "--dry-run"))
	files, err := os.ReadDir(dst)
	if err != nil || len(files) != 0 {
		t.Fatalf("dry-run changed destination: %v %v", files, err)
	}
	t.Log("local read and dry-run passed; no transfer/write or remote backend support claimed")
}
