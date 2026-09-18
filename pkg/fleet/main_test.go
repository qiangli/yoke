package fleet

import (
	"os"
	"testing"
)

// TestMain strips the operator's shared-ring overlay from the test process.
//
// BASHY_TOOLS_PATH / BASHY_MODELS_PATH / BASHY_AGENTS_PATH are the ring-2
// mounts an operator exports in a login shell (the umbrella's fleet/ overlay
// since sprint 161). A catalog built under `go test` on that shell inherited
// them, so TestFamilyAliasPicksHighestVersion and
// TestResolveLaunchModelLongestCanonicalMatch saw the operator's real models
// beside their fixtures and failed on the dev host while passing in CI. A
// test that needs a shared ring sets it with t.Setenv; none may inherit one.
// The same goes for BASHY_FLEET_SEEDS: the seeds are under test here.
func TestMain(m *testing.M) {
	for _, k := range []string{"BASHY_TOOLS_PATH", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH", SeedsEnv} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
