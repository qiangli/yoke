package kubectl

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSpec(t *testing.T) {
	s := Spec("v1.31.0")
	if s.Name != "kubectl" || s.Version != "v1.31.0" {
		t.Fatalf("unexpected spec: %+v", s)
	}
	if !strings.Contains(s.URLTemplate, "dl.k8s.io/release/{version}/bin/{goos}/{goarch}/kubectl{ext}") {
		t.Errorf("URL template wrong: %s", s.URLTemplate)
	}
	// kubectl ships a bare-digest .sha256 sidecar → binmgr's default (empty
	// ChecksumURLTemplate → "<url>.sha256"). Member empty (raw binary).
	if s.ChecksumURLTemplate != "" {
		t.Errorf("ChecksumURLTemplate should be empty (default .sha256), got %q", s.ChecksumURLTemplate)
	}
	if s.Member != "" {
		t.Errorf("Member should be empty (raw binary), got %q", s.Member)
	}
}

// TestKubectlRunEReturnsProfileErrorBeforeExec pins that an unknown
// $DKS_PROFILE surfaces as an error from RunE before any network fetch or
// binary exec is attempted — the whole point of moving kubeconfig
// resolution to a function that can return an error.
func TestKubectlRunEReturnsProfileErrorBeforeExec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "bogus")

	cmd := NewKubectlCmd()
	cmd.SetArgs([]string{"get", "nodes"})
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

// TestKubectlRunEFailsClosedOnMissingPeerKubeconfig pins the same
// before-exec error surfacing for the peer-profile fail-closed case.
func TestKubectlRunEFailsClosedOnMissingPeerKubeconfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("DKS_PROFILE", "peer")
	t.Setenv("DKS_PEER_KUBECONFIG", filepath.Join(home, "missing.yaml"))

	cmd := NewKubectlCmd()
	cmd.SetArgs([]string{"get", "nodes"})
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
