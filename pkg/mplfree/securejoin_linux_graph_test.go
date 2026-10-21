package mplfree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cypharModule is the MPL-2.0 module path the engine callers import.
const cypharModule = "github.com/cyphar/filepath-securejoin"

// TestLinuxEngineSecureJoinIsPermissive pins the S488.3d contract for
// the shipped Linux engine graph: selinux, storage, podman libpod and
// runc import the pathrs-lite and pathrs-lite/procfs subpackages there,
// so every package under the cyphar module path must resolve to the
// clean-room external/securejoin replacement. Unlike the host graph
// check this does not skip on a go list failure — a subpackage missing
// from the replacement is exactly the regression it exists to catch.
func TestLinuxEngineSecureJoinIsPermissive(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}\t{{if .Module}}{{.Module.Dir}}{{end}}", enginePkg)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	cmd.Dir = root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("GOOS=linux go list -deps %s failed: %v\n%s", enginePkg, err, stderr.String())
	}
	wantDir := filepath.Join(root, "external", "securejoin")
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], cypharModule) {
			continue
		}
		seen[fields[0]] = true
		if fields[1] != wantDir {
			t.Errorf("%s resolves to module dir %s, want the clean-room replacement at %s", fields[0], fields[1], wantDir)
		}
	}
	for _, pkg := range []string{cypharModule + "/pathrs-lite", cypharModule + "/pathrs-lite/procfs"} {
		if !seen[pkg] {
			t.Logf("%s not in the Linux engine graph: contract satisfied for it", pkg)
		}
	}
}
