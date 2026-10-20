package watchcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/watch"
)

// The CLI half of the hooks: they are reachable, the ring precedence is
// visible, and a real Bash# probe's values reach the rules. These run the
// INSTALLED bashy, so they skip rather than fake when there is none — a green
// suite on a host without bashy must not claim this path works.

func hasBashy(t *testing.T) {
	t.Helper()
	if _, err := watch.LookBashy(); err != nil {
		t.Skip("no installed bashy on PATH")
	}
}

func localHook(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, name+".bsh"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_WATCH_DIR", dir)
	return dir
}

func TestListHooksShowsTheRingAndTheOverride(t *testing.T) {
	isolatePresets(t)
	out, _, code := runTool(t, "--list-hooks")
	if code != 0 {
		t.Fatalf("code = %d (%q)", code, out)
	}
	for _, want := range []string{"gpu", "reclaim-go-build", "embedded", "local ring:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--list-hooks = %q, want %q", out, want)
		}
	}

	localHook(t, "gpu", "echo mine\n")
	out, _, code = runTool(t, "--list-hooks")
	if code != 0 {
		t.Fatalf("code = %d (%q)", code, out)
	}
	if !strings.Contains(out, "local") || !strings.Contains(out, "shadows the embedded example") {
		t.Fatalf("--list-hooks = %q, want the local gpu to shadow the shipped one", out)
	}
}

func TestHookSourcePrintsAShippedExampleForCopying(t *testing.T) {
	isolatePresets(t)
	out, _, code := runTool(t, "--hook-source", "reclaim-go-build")
	if code != 0 {
		t.Fatalf("code = %d (%q)", code, out)
	}
	if !strings.Contains(out, "go-build") || !strings.Contains(out, "--apply") {
		t.Fatalf("--hook-source = %q", out)
	}
	_, errOut, code := runTool(t, "--hook-source", "nope")
	if code == 0 || !strings.Contains(errOut, "no hook") {
		t.Fatalf("(%q, %d), want a clear refusal", errOut, code)
	}
}

func TestUnknownHookIsAUsageError(t *testing.T) {
	isolatePresets(t)
	_, errOut, code := runTool(t, "--hook", "no-such-hook", "--match", "kind=added", "--", "echo x")
	if code != 2 || !strings.Contains(errOut, "no action hook") {
		t.Fatalf("(%q, %d), want a usage error naming the missing hook", errOut, code)
	}
	_, errOut, code = runTool(t, "--probe", "no-such-probe", "--match", "kind=added")
	if code != 2 || !strings.Contains(errOut, "no probe hook") {
		t.Fatalf("(%q, %d), want a usage error naming the missing probe", errOut, code)
	}
}

func TestProbeHookValuesReachTheRules(t *testing.T) {
	hasBashy(t)
	// A real Bash# probe, a real change rule: the engine treats a user's
	// sampler exactly as a built-in one.
	localHook(t, "counter", `
n=$(cat "$STATE" 2>/dev/null || echo 0)
n=$((n + 1))
printf '%s' "$n" > "$STATE"
printf '{"resource":"q:jobs","field":"depth","value":%s,"unit":"jobs"}\n' "$n"
`)
	state := filepath.Join(t.TempDir(), "n")
	t.Setenv("STATE", state)

	out, errOut, code := runTool(t, "-n", "0.01", "--probe", "counter",
		"--change", "depth", "--until", "--timeout", "5s", "--json")
	if code != 0 {
		t.Fatalf("code = %d (%q / %q)", code, out, errOut)
	}
	if !strings.Contains(out, `"field":"depth"`) || !strings.Contains(out, `"reason":"change"`) {
		t.Fatalf("stdout = %q, want the probe's value change alarmed", out)
	}
}

func TestActionHookRunsOnATransitionAndItsFailureAlarms(t *testing.T) {
	hasBashy(t)
	dir := localHook(t, "note", `
cat > "$SINK"
exit 0
`)
	sink := filepath.Join(dir, "event.json")
	t.Setenv("SINK", sink)

	out, errOut, code := runTool(t, "-n", "0.01", "--hook", "note", "--match", "kind=added",
		"--until", "--timeout", "5s", "--locator", "@test-host", "--", "echo $$")
	if code != 0 {
		t.Fatalf("code = %d (%q / %q)", code, out, errOut)
	}
	body, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("the action hook did not receive the event: %v", err)
	}
	if !strings.Contains(string(body), `"schema_version":"bashy-watch-v1"`) ||
		!strings.Contains(string(body), "@test-host") {
		t.Fatalf("hook stdin = %q", body)
	}
}

func TestActionHookNonZeroExitSurfacesInTheAlertStream(t *testing.T) {
	hasBashy(t)
	localHook(t, "broken", "cat >/dev/null\nexit 7\n")

	out, errOut, code := runTool(t, "-n", "0.01", "--hook", "broken", "--match", "kind=added",
		"--timeout", "3s", "--locator", "@test-host", "--", "echo $$")
	_ = code
	stream := out + errOut
	if !strings.Contains(stream, "hook:broken") || !strings.Contains(stream, "exited 7") {
		t.Fatalf("alert stream = %q, want the hook failure raised as an alarm", stream)
	}
}
