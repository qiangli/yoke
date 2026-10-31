package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/nfthelper"
)

func TestSafePathPodmanRejectsBashyShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "podman")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec bashy oci \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := safePathPodman(); err == nil || !strings.Contains(err.Error(), "bashy oci") {
		t.Fatalf("got %v", err)
	}
}

func TestResolvePodmanBinaryCachedEngine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", root)
	t.Setenv("BASHY_OFFLINE", "1")
	t.Setenv("BASHY_PODMAN_SYSTEM", "")
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(root, "podman", "v6.1.2", "podman-"+runtime.GOOS+"-"+runtime.GOARCH,
		"usr", "local", "bin", "podman")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("cached engine"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePodmanBinary()
	if err != nil || got != bin {
		t.Fatalf("resolvePodmanBinary() = %q, %v; want %q", got, err, bin)
	}
}

func TestResolvePodmanBinaryIgnoresPathUnlessOptedIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable fixture uses Unix mode")
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_OFFLINE", "1")
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BASHY_PODMAN_SYSTEM", "")
	if got, err := resolvePodmanBinary(); err == nil || got != "" || !strings.Contains(err.Error(), "no cached engine") {
		t.Fatalf("without opt-in: %q, %v", got, err)
	}
	t.Setenv("BASHY_PODMAN_SYSTEM", "1")
	if got, err := resolvePodmanBinary(); err != nil || got != bin {
		t.Fatalf("with opt-in: %q, %v; want %q", got, err, bin)
	}
}

func TestManagedExecLinuxHelpers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux helper layout")
	}
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	binDir := filepath.Join(dir, "usr", "local", "bin")
	helpers := filepath.Join(dir, "usr", "local", "lib", "podman")
	for _, d := range []string{binDir, helpers} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(helpers, "conmon"), filepath.Join(binDir, "crun"), filepath.Join(binDir, "runc")} {
		if err := os.WriteFile(f, []byte("x"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	find := func(cmd *exec.Cmd) string {
		for _, e := range cmd.Env {
			if v, ok := strings.CutPrefix(e, "CONTAINERS_CONF_OVERRIDE="); ok {
				return v
			}
		}
		return ""
	}
	cmd := buildManagedExec(context.Background(), filepath.Join(binDir, "podman"), "", nil)
	conf := find(cmd)
	if conf == "" {
		t.Fatal("missing CONTAINERS_CONF_OVERRIDE")
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"conmon_path = [\"" + filepath.Join(helpers, "conmon") + "\"]",
		"helper_binaries_dir = [\"" + nfthelper.Dir() + "\", \"" + helpers + "\", \"" + filepath.Join(dir, "usr", "local", "libexec", "podman") + "\", \"" + binDir + "\"]",
		"[engine.runtimes]",
		"crun = [\"" + filepath.Join(binDir, "crun") + "\"]",
		"runc = [\"" + filepath.Join(binDir, "runc") + "\"]",
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("conf missing %q:\n%s", want, data)
		}
	}
	// Netavark execs nft by name. The private helper must be visible to that
	// child without adding an nft command to the caller's shell namespace.
	private := nfthelper.Dir()
	if !strings.Contains(string(data), `"`+private+`"`) {
		t.Fatalf("helper config does not include private nft directory: %s", data)
	}
	if _, err := os.Stat(filepath.Join(private, "nft")); err != nil {
		t.Fatalf("private nft helper: %v", err)
	}
	if again := find(buildManagedExec(context.Background(), filepath.Join(binDir, "podman"), "", nil)); again != conf {
		t.Fatalf("not idempotent: %q vs %q", again, conf)
	}
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "/operator/conf")
	for _, e := range buildManagedExec(context.Background(), filepath.Join(binDir, "podman"), "", nil).Env {
		if strings.HasPrefix(e, "CONTAINERS_CONF_OVERRIDE=") && e != "CONTAINERS_CONF_OVERRIDE=/operator/conf" {
			t.Fatalf("operator value overridden: %s", e)
		}
	}
}

func TestHelperConfContent(t *testing.T) {
	binDir := filepath.Join(t.TempDir(), "usr", "local", "bin")
	root := filepath.Join(binDir, "..")
	got := helperConfContent(binDir, filepath.Join(root, "lib", "podman"))
	want := "helper_binaries_dir = [\"" + nfthelper.Dir() + "\", \"" + filepath.Join(root, "lib", "podman") + "\", \"" +
		filepath.Join(root, "libexec", "podman") + "\", \"" + binDir + "\"]"
	if !strings.Contains(got, want) {
		t.Fatalf("conf missing %q:\n%s", want, got)
	}
}

func TestPrivateNFTHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private nft helper is Linux only")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, err := nfthelper.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "nft")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if target != self {
		t.Fatalf("nft helper points to %q, want %q", target, self)
	}
	if !strings.Contains(helperConfContent("/tmp/bin", "/tmp/lib/podman"), `"`+dir+`"`) {
		t.Fatal("helper config omits nft helper")
	}
}

func TestShouldEnsureMachine(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"default", nil, true},
		{"run", []string{"run", "alpine"}, true},
		{"info", []string{"info"}, true},
		{"help flag", []string{"--help"}, false},
		{"version flag", []string{"--version"}, false},
		{"machine", []string{"machine", "list"}, false},
		{"help command", []string{"help", "run"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldEnsureMachine(tt.args); got != tt.want {
				t.Fatalf("shouldEnsureMachine(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
