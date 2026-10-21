package mplfree

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The bashy engine import graph must not contain the MPL-licensed error
// aggregation chain (hashicorp/go-multierror, hashicorp/errwrap).
func TestEngineGraphHasNoMultierrorChain(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "github.com/qiangli/yoke/external/podman/engine")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if p == "github.com/hashicorp/go-multierror" || p == "github.com/hashicorp/errwrap" {
			t.Errorf("MPL package in engine import graph: %s", p)
		}
	}
}
