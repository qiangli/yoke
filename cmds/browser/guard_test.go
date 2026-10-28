package browsercmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/wire"
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
		// eval has no field: "self" is not on offer, so it stops.
		{"fake:self", guard.ContentStopped, guard.ClassStopped, 5},
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

// filled-by-human and human-typed both exit 0 and print only the outcome.
func TestGuardSuccessOutcomesExitZero(t *testing.T) {
	for _, r := range []*wire.Result{
		{Success: true, Content: guard.ContentFilledByHuman, OutcomeClass: guard.ClassFilledByHuman, Reason: "field:label-ssn"},
		{Success: true, Content: guard.ContentHumanTyped, OutcomeClass: guard.ClassHumanTyped, Reason: "field:label-ssn"},
	} {
		var out, errb bytes.Buffer
		rc := &tool.RunContext{Ctx: context.Background(), Stdio: tool.Stdio{Out: &out, Err: &errb}}
		if code := printResult(rc, r, false); code != 0 || strings.TrimSpace(out.String()) != r.Content {
			t.Errorf("%s: code=%d out=%q", r.OutcomeClass, code, out.String())
		}
	}
}

// `type --from-ask ID <target>` takes a target and never text.
func TestTypeFromAskArgs(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	a, err := actionFromArgs([]string{"type", "#ssn"}, actionFlags{FromAsk: id})
	if err != nil || a.Type != wire.ActionType || a.Selector != "#ssn" || a.FromAsk != id || a.Text != "" {
		t.Fatalf("got %+v, %v", a, err)
	}
	if a, err := actionFromArgs([]string{"type"}, actionFlags{FromAsk: id, Index: 3}); err != nil || a.ElementID != 3 {
		t.Fatalf("--index: %+v, %v", a, err)
	}
	for _, args := range [][]string{{"type", "#ssn", "212-48-7731"}, {"type"}} {
		if _, err := actionFromArgs(args, actionFlags{FromAsk: id}); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	_, _, code := runTool(t, "--mode", "live", "--live-port", "1", "--from-ask", id, "type", "#ssn", "extra")
	if code != 2 {
		t.Errorf("text alongside --from-ask: exit %d, want 2", code)
	}
}
