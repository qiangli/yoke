package engine

import (
	"runtime"
	"strings"
	"testing"
)

func TestResolveMachineImage(t *testing.T) {
	got := resolveMachineImage()
	if got == "" {
		t.Fatal("resolveMachineImage() returned empty — libpod's NewOCIArtifactPull rejects this as 'no machine image endpoint provided'")
	}
	if !strings.HasPrefix(got, "docker://") && !strings.HasPrefix(got, "http") && !strings.HasPrefix(got, "/") {
		t.Errorf("resolveMachineImage() = %q; expected docker:// / http(s):// / absolute path (per diskpull.GetDisk dispatch)", got)
	}
}

// TestResolveMachineVolumes verifies that on macOS we mirror upstream
// podman-machine's default host shares (/Users, /private, /var/folders).
// Without these, every `-v /Users/me/proj:/src` bind mount fails with
// `statfs ...: no such file or directory` from inside libpod.
func TestResolveMachineVolumes(t *testing.T) {
	got := resolveMachineVolumes()
	if runtime.GOOS == "darwin" {
		// Upstream's default_darwin.go returns these three exactly.
		want := []string{
			"/Users:/Users",
			"/private:/private",
			"/var/folders:/var/folders",
		}
		if len(got) != len(want) {
			t.Fatalf("got %d volumes, want %d: got=%v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("volumes[%d] = %q, want %q", i, got[i], w)
			}
		}
	}

	// Every entry must be a HOST:GUEST string (vmconfigs.SplitVolume will
	// reject anything else at init time).
	for _, v := range got {
		if !strings.Contains(v, ":") {
			t.Errorf("volume %q missing ':' separator", v)
		}
	}
}
