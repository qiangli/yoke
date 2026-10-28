package browsercmd

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/browser/guard"
)

// The CLI contract shared with site skills: a guard outcome prints only
// guard:<outcome> and exits 0 / 4 / 5; --json carries outcome_class and the
// reason code. The refused value appears nowhere in the output.
func TestGuardOutcomeExitCodes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_ASK_DIR", filepath.Join(root, "ask"))
	t.Setenv("BASHY_HOME", filepath.Join(root, "home"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(root, "home", "room"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()

	const ssn = "212-48-7731"
	for _, c := range []struct {
		fake, content, class string
		code                 int
	}{
		{"fake:type-myself", guard.ContentHumanTyped, guard.ClassHumanTyped, 0},
		{"fake:skip", guard.ContentSkipped, guard.ClassSkipped, 4},
		{"fake:stop", guard.ContentStopped, guard.ClassStopped, 5},
		{"fake:timeout", guard.ContentStopped, guard.ClassStopped, 5},
	} {
		t.Setenv(guard.FakeAskEnv, c.fake)
		out, errb, code := runTool(t, "--mode", "live", "--live-port", port, "eval", `"`+ssn+`"`)
		if code != c.code || strings.TrimSpace(out) != c.content {
			t.Errorf("%s: code=%d out=%q, want %d %q (stderr %q)", c.fake, code, out, c.code, c.content, errb)
		}
		out, _, code = runTool(t, "--json", "--mode", "live", "--live-port", port, "eval", `"`+ssn+`"`)
		var env struct {
			OutcomeClass string `json:"outcome_class"`
			Reason       string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil || env.OutcomeClass != c.class || env.Reason != "value:ssn" || code != c.code {
			t.Errorf("%s --json: code=%d %s", c.fake, code, out)
		}
		if strings.Contains(out+errb, ssn) {
			t.Errorf("%s: the refused value was printed", c.fake)
		}
	}
}
