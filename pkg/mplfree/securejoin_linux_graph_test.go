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
	cmd := linuxEngineGraphCommand(root)
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

// Desktop validation supplies -tags=remote in GOFLAGS for its native client
// build. This audit deliberately examines the shipped Linux server graph;
// explicit tags override that inherited client selection without discarding
// unrelated flags (module policy, build parallelism, etc.).
func linuxEngineGraphCommand(root string) *exec.Cmd {
	cmd := exec.Command("go", "list", "-tags=exclude_graphdriver_btrfs,containers_image_openpgp", "-deps", "-f", "{{.ImportPath}}\t{{if .Module}}{{.Module.Dir}}{{end}}", enginePkg)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	cmd.Dir = root
	return cmd
}

func TestLinuxEngineGraphCommandOverridesDesktopTags(t *testing.T) {
	t.Setenv("GOFLAGS", "-p=2 -mod=readonly -tags=remote")
	cmd := linuxEngineGraphCommand(t.TempDir())
	if !strings.Contains(strings.Join(cmd.Args, " "), "-tags=exclude_graphdriver_btrfs,containers_image_openpgp") {
		t.Fatalf("Linux graph does not override desktop tags: %q", cmd.Args)
	}
	found := false
	for _, entry := range cmd.Env {
		if entry == "GOFLAGS=-p=2 -mod=readonly -tags=remote" {
			found = true
		}
	}
	if !found {
		t.Fatal("unrelated inherited Go flags were lost")
	}
}
