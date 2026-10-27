package loom

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

// userStub builds an *os/user.User without touching the host's account
// database — dropFromUser's input is pure data.
type userStub struct{ name, uid, gid string }

func (s userStub) user() *user.User {
	return &user.User{Username: s.name, Uid: s.uid, Gid: s.gid}
}

// newProbeCmd is a command that is never started: these tests assert what a
// constructor PUTS on it.
func newProbeCmd() *exec.Cmd { return exec.Command("/nonexistent/gitea", "web") }

func hasEnvPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// assertEnv checks that every want is present verbatim. Env entries are
// appended last by construction, which is what makes them win in exec's dedup.
func assertEnv(t testingT, env []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, e := range env {
			if e == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("env is missing %q: %v", w, env)
		}
	}
}

type testingT interface {
	Helper()
	Fatalf(string, ...any)
}

// TestMain makes the account provisioner inert for the WHOLE package. A unit
// test must never add an account to the host's /etc/passwd, and the suite does
// run as root in some CI images — where Start/StartDaemon would otherwise take
// the real privilege-drop path. Tests that exercise the drop install their own
// fake (withFakeAccount); anything else reaching here is a bug in the test, so
// it fails loudly rather than mutating the host.
func TestMain(m *testing.M) {
	ensureAccount = func(_ context.Context, name, _ string) (*privDrop, error) {
		return nil, fmt.Errorf("loom test: refusing to provision the system account %q; install a fake with withFakeAccount", name)
	}
	os.Exit(m.Run())
}

// searchableTempDir is a t.TempDir() with the others-search bit already set, so
// ensureTraversable's walk up the ancestor chain STOPS there. Without it a test
// would add +x to the host's per-user temp directories — harmless, but a test
// has no business touching anything above its own tree.
func searchableTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	return dir
}

// lastEnvValue is the value exec would actually use: os/exec keeps the LAST
// entry for a duplicated key, which is the whole reason drop.env() is appended
// rather than prepended. Returns "", false when the key is absent.
func lastEnvValue(env []string, key string) (string, bool) {
	val, ok := "", false
	for _, e := range env {
		if k, v, found := strings.Cut(e, "="); found && k == key {
			val, ok = v, true
		}
	}
	return val, ok
}
