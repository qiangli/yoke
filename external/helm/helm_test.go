package helm

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSpec(t *testing.T) {
	s := Spec("v3.16.3")
	if s.Name != "helm" || s.Version != "v3.16.3" {
		t.Fatalf("unexpected spec: %+v", s)
	}
	if !strings.Contains(s.URLTemplate, "get.helm.sh/helm-{version}-{goos}-{goarch}.tar.gz") {
		t.Errorf("URL template wrong: %s", s.URLTemplate)
	}
	if !strings.HasSuffix(s.ChecksumURLTemplate, ".tar.gz.sha256sum") {
		t.Errorf("checksum template wrong: %s", s.ChecksumURLTemplate)
	}
	// Member points at the binary inside the <goos>-<goarch>/ tree, .exe on Windows.
	wantExe := "helm"
	if runtime.GOOS == "windows" {
		wantExe = "helm.exe"
	}
	want := runtime.GOOS + "-" + runtime.GOARCH + "/" + wantExe
	if s.Member != want {
		t.Errorf("Member = %q, want %q", s.Member, want)
	}
}

// TestHelmRunEReturnsProfileErrorBeforeExec pins that an unknown $DKS_PROFILE
// surfaces as an error from RunE before any network fetch or binary exec is
// attempted — the whole point of moving kubeconfig resolution to a function
// that can return an error.
func TestHelmRunEReturnsProfileErrorBeforeExec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "bogus")

	cmd := NewHelmCmd()
	cmd.SetArgs([]string{"list"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error for an unknown DKS_PROFILE")
	}
	if !strings.Contains(err.Error(), "DKS_PROFILE") {
		t.Fatalf("Execute() error = %v, want it to mention DKS_PROFILE", err)
	}
}

// TestHelmRunEFailsClosedOnMissingPeerKubeconfig pins the same before-exec
// error surfacing for the peer-profile fail-closed case.
func TestHelmRunEFailsClosedOnMissingPeerKubeconfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "peer")
	t.Setenv("DKS_PEER_KUBECONFIG", filepath.Join(home, "missing.yaml"))

	cmd := NewHelmCmd()
	cmd.SetArgs([]string{"list"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error for a missing peer kubeconfig")
	}
	if !strings.Contains(err.Error(), "peer") {
		t.Fatalf("Execute() error = %v, want it to mention the peer profile", err)
	}
}
