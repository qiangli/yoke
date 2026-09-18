package kube

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeHome points $HOME (and USERPROFILE on Windows) at a fresh temp dir so
// os.UserHomeDir() is hermetic and ~/.kube/config never leaks from the real
// host running the tests.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	return home
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestResolveKubeconfigExplicitWins(t *testing.T) {
	home := fakeHome(t)
	explicit := filepath.Join(home, "explicit.yaml")
	t.Setenv("KUBECONFIG", explicit)
	t.Setenv("DKS_PROFILE", "peer") // must be ignored entirely
	t.Setenv("OUTPOST_KUBECONFIG_PATH", filepath.Join(home, "outpost.yaml"))

	res, err := ResolveKubeconfig()
	if err != nil {
		t.Fatalf("ResolveKubeconfig() error = %v", err)
	}
	if res != (Resolution{}) {
		t.Fatalf("ResolveKubeconfig() = %+v, want zero Resolution when $KUBECONFIG is explicit", res)
	}
}

func TestResolveKubeconfigPeerProfile(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "peer")
	peer := filepath.Join(home, "peer.yaml")
	t.Setenv("DKS_PEER_KUBECONFIG", peer)
	if err := os.WriteFile(peer, []byte("peer"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveKubeconfig()
	if err != nil {
		t.Fatalf("ResolveKubeconfig() error = %v", err)
	}
	if res.KUBECONFIG != peer {
		t.Errorf("KUBECONFIG = %q, want %q", res.KUBECONFIG, peer)
	}
	if res.Refresh {
		t.Error("Refresh = true, want false for the peer profile")
	}
}

func TestResolveKubeconfigPeerProfileDefaultPath(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "peer")
	t.Setenv("DKS_PEER_KUBECONFIG", "")
	want := filepath.Join(home, ".kube", "outpost-control-plane", "k3s.yaml")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("peer"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveKubeconfig()
	if err != nil {
		t.Fatalf("ResolveKubeconfig() error = %v", err)
	}
	if res.KUBECONFIG != want {
		t.Errorf("KUBECONFIG = %q, want default %q", res.KUBECONFIG, want)
	}
}

func TestResolveKubeconfigPeerProfileFailsClosedWhenMissing(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "peer")
	t.Setenv("DKS_PEER_KUBECONFIG", filepath.Join(home, "missing-peer.yaml"))
	// A cloudbox file DOES exist — must not be used as a fallback.
	cloudbox := filepath.Join(home, "outpost.yaml")
	t.Setenv("OUTPOST_KUBECONFIG_PATH", cloudbox)
	if err := os.WriteFile(cloudbox, []byte("cloudbox"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveKubeconfig()
	if err == nil {
		t.Fatalf("ResolveKubeconfig() = %+v, want an error for a missing peer kubeconfig", res)
	}
	if res != (Resolution{}) {
		t.Errorf("ResolveKubeconfig() = %+v, want zero Resolution on error", res)
	}
}

func TestResolveKubeconfigCloudboxProfile(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "cloudbox")
	cloudbox := filepath.Join(home, "outpost.yaml")
	t.Setenv("OUTPOST_KUBECONFIG_PATH", cloudbox)

	res, err := ResolveKubeconfig()
	if err != nil {
		t.Fatalf("ResolveKubeconfig() error = %v", err)
	}
	if res.KUBECONFIG != cloudbox {
		t.Errorf("KUBECONFIG = %q, want %q", res.KUBECONFIG, cloudbox)
	}
	if !res.Refresh {
		t.Error("Refresh = false, want true for the cloudbox profile")
	}
}

func TestResolveKubeconfigAutoPrefersStandardConfig(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	unsetenv(t, "DKS_PROFILE")
	standard := filepath.Join(home, ".kube", "config")
	if err := os.MkdirAll(filepath.Dir(standard), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(standard, []byte("standard"), 0o600); err != nil {
		t.Fatal(err)
	}
	cloudbox := filepath.Join(home, "outpost.yaml")
	t.Setenv("OUTPOST_KUBECONFIG_PATH", cloudbox)
	if err := os.WriteFile(cloudbox, []byte("cloudbox"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := ResolveKubeconfig()
	if err != nil {
		t.Fatalf("ResolveKubeconfig() error = %v", err)
	}
	if res != (Resolution{}) {
		t.Fatalf("ResolveKubeconfig() = %+v, want zero Resolution so native kubectl honors ~/.kube/config's current context", res)
	}
}

func TestResolveKubeconfigAutoFallsBackToCloudboxWhenNoStandardConfig(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	unsetenv(t, "DKS_PROFILE")
	// No ~/.kube/config written.
	cloudbox := filepath.Join(home, "outpost.yaml")
	t.Setenv("OUTPOST_KUBECONFIG_PATH", cloudbox)

	res, err := ResolveKubeconfig()
	if err != nil {
		t.Fatalf("ResolveKubeconfig() error = %v", err)
	}
	if res.KUBECONFIG != cloudbox {
		t.Errorf("KUBECONFIG = %q, want cloudbox fallback %q", res.KUBECONFIG, cloudbox)
	}
	if !res.Refresh {
		t.Error("Refresh = false, want true for the implicit cloudbox fallback")
	}
}

func TestResolveKubeconfigUnknownProfileFailsClosed(t *testing.T) {
	fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "bogus")

	res, err := ResolveKubeconfig()
	if err == nil {
		t.Fatalf("ResolveKubeconfig() = %+v, want an error for an unknown profile", res)
	}
	if res != (Resolution{}) {
		t.Errorf("ResolveKubeconfig() = %+v, want zero Resolution on error", res)
	}
}

func TestResolveKubeconfigEmptyProfileFailsClosed(t *testing.T) {
	fakeHome(t)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "")

	res, err := ResolveKubeconfig()
	if err == nil {
		t.Fatalf("ResolveKubeconfig() = %+v, want an error for explicitly empty DKS_PROFILE", res)
	}
	if res != (Resolution{}) {
		t.Errorf("ResolveKubeconfig() = %+v, want zero Resolution on error", res)
	}
}

func TestFileFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !fileFresh(path, 50*time.Minute) {
		t.Fatal("new kubeconfig reported stale")
	}
	old := time.Now().Add(-51 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if fileFresh(path, 50*time.Minute) {
		t.Fatal("expired kubeconfig reported fresh")
	}
}

func TestRefreshKubeconfigSkipsWhenPathEmpty(t *testing.T) {
	var stderr bytes.Buffer
	RefreshKubeconfig(t.Context(), &stderr, "")
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr output: %q", stderr.String())
	}
}

func TestRefreshKubeconfigSkipsWhenFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outpost.yaml")
	if err := os.WriteFile(path, []byte("config"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	// Fresh file: must not shell out to "outpost" even if it were on PATH.
	RefreshKubeconfig(t.Context(), &stderr, path)
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr output: %q", stderr.String())
	}
}

func TestExecEnvFor(t *testing.T) {
	env := ExecEnvFor("")
	for _, e := range env {
		if len(e) >= len("KUBECONFIG=") && e[:len("KUBECONFIG=")] == "KUBECONFIG=" {
			t.Fatalf("ExecEnvFor(\"\") injected %q, want no KUBECONFIG entry", e)
		}
	}

	env = ExecEnvFor("/tmp/kc.yaml")
	found := false
	for _, e := range env {
		if e == "KUBECONFIG=/tmp/kc.yaml" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ExecEnvFor(path) did not inject KUBECONFIG, env = %v", env)
	}
}
