package gate

import (
	"strings"
	"testing"
)

// A gate's exit code is a verdict. If an ambient control can reach it, the
// verdict means something different depending on where it ran — so these two
// tests are the executable form of that invariant, not coverage.

func TestEnvDropsAmbientControls(t *testing.T) {
	t.Setenv("BASHY_AGENTIC", "1")
	t.Setenv("AGENTIC", "3")
	t.Setenv("BASHY_HINTS", "1")
	t.Setenv("PATH", "/usr/bin")

	got := Env(t.TempDir())

	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "BASHY_AGENTIC", "AGENTIC", "BASHY_HINTS":
			t.Fatalf("gate env leaked control %q: a gate must not inherit it", kv)
		}
	}
	if !hasName(got, "PATH") {
		t.Fatal("gate env dropped PATH: a gate is usually a build or test command")
	}
}

// The allowlist must be fail-closed against names that do not exist yet, which
// is the whole reason it is an allowlist and not a denylist.
func TestEnvDropsUnknownFutureVariable(t *testing.T) {
	t.Setenv("SOME_CONTROL_INVENTED_LATER", "on")
	for _, kv := range Env(t.TempDir()) {
		if strings.HasPrefix(kv, "SOME_CONTROL_INVENTED_LATER=") {
			t.Fatal("gate env is fail-open: an unknown variable reached the gate")
		}
	}
}

func TestScrubControlsKeepsToolchainAndDropsControls(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"GOFLAGS=-mod=mod",
		"CGO_ENABLED=0",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"BASHY_AGENTIC=1",
		"AGENTIC_FORMAT=json",
		"BASHY_ADVISOR=0",
	}
	got := ScrubControls(in)

	for _, want := range []string{"PATH", "GOFLAGS", "CGO_ENABLED", "SSH_AUTH_SOCK"} {
		if !hasName(got, want) {
			t.Errorf("ScrubControls dropped %s: verify runs a real build command", want)
		}
	}
	for _, bad := range []string{"BASHY_AGENTIC", "AGENTIC_FORMAT", "BASHY_ADVISOR"} {
		if hasName(got, bad) {
			t.Errorf("ScrubControls kept %s: a verify verdict must not depend on it", bad)
		}
	}
}

// A control invented later in a namespace bashy owns is caught by prefix. This
// is the narrower guarantee ScrubControls documents, and the test states its
// limit as well as its promise.
func TestScrubControlsCatchesFutureControlsByPrefix(t *testing.T) {
	got := ScrubControls([]string{"BASHY_SOMETHING_NEW=1", "AGENTICNESS=9", "THIRD_PARTY_KNOB=1"})
	if hasName(got, "BASHY_SOMETHING_NEW") || hasName(got, "AGENTICNESS") {
		t.Fatal("ScrubControls missed a future control in a namespace bashy owns")
	}
	if !hasName(got, "THIRD_PARTY_KNOB") {
		t.Fatal("ScrubControls is documented as NOT covering third-party variables; " +
			"if that changed, update its doc comment and gate.Env's contrast")
	}
}

func hasName(env []string, name string) bool {
	for _, kv := range env {
		if n, _, _ := strings.Cut(kv, "="); n == name {
			return true
		}
	}
	return false
}
