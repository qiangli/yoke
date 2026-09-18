package recall

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/kb"
)

// TestPreamble_OffByDefault is the experiment's control arm, pinned.
//
// If injection ever becomes the default before the leaderboard shows a positive
// lift, there is no clean B arm left to compare against — the comparison the
// whole feature is justified by becomes unmeasurable. This test is what makes
// flipping the default a deliberate act rather than a drive-by.
func TestPreamble_OffByDefault(t *testing.T) {
	isolateRecallStores(t)
	t.Setenv(EnvKnowledge, "")
	store := kbWith(t, t.TempDir(), page("widget", "widget lesson", "a lesson about widgets"))
	if got := Preamble("widget", HostRing{Store: store}); got != "" {
		t.Errorf("injection happened with %s unset:\n%s", EnvKnowledge, got)
	}
	for _, off := range []string{"off", "0", "false", "no", "garbage"} {
		t.Setenv(EnvKnowledge, off)
		if got := Preamble("widget", HostRing{Store: store}); got != "" {
			t.Errorf("%s=%q injected anyway", EnvKnowledge, off)
		}
	}
}

func TestPreamble_OnInjectsCitations(t *testing.T) {
	isolateRecallStores(t)
	t.Setenv(EnvKnowledge, "on")
	store := kbWith(t, t.TempDir(), page("widget", "widget lesson", "a lesson about widgets"))
	got := Preamble("widget", HostRing{Store: store})
	if got == "" {
		t.Fatal("BASHY_KNOWLEDGE=on produced no preamble")
	}
	// Every hit must be traceable and labelled as recalled — never as an
	// instruction, because a candidate note asserted as policy becomes a false
	// constraint the agent cannot argue with.
	if !strings.Contains(got, "kb:widget") {
		t.Error("preamble has no id to open; it is a summary, not a citation")
	}
	if !strings.Contains(got, "[host/page]") {
		t.Error("preamble does not label the ring and form a hit came from")
	}
}

// TestPreamble_EmptyWhenNothingKnown — an agent must not be handed a header with
// no content under it; that reads as "nothing is known" being an error state.
func TestPreamble_EmptyWhenNothingKnown(t *testing.T) {
	isolateRecallStores(t)
	t.Setenv(EnvKnowledge, "on")
	store := kbWith(t, t.TempDir(), page("ingress", "kubernetes ingress", "how ingress works"))
	if got := Preamble("zzzz qqqq nonexistent", HostRing{Store: store}); got != "" {
		t.Errorf("preamble on an unknown topic:\n%s", got)
	}
}

// TestPreamble_RespectsBudget — an agent whose context is half preamble has less
// room for the task it was given.
func TestPreamble_RespectsBudget(t *testing.T) {
	isolateRecallStores(t)
	t.Setenv(EnvKnowledge, "on")
	var pages []*kb.Page
	for _, n := range []string{"one", "two", "three", "four", "five", "six"} {
		pages = append(pages, page("widget-"+n,
			"widget handling "+n+" with a deliberately long title to consume budget",
			"a long description about widget handling that exists purely to spend tokens "+n))
	}
	store := kbWith(t, t.TempDir(), pages...)
	got := Preamble("widget handling", HostRing{Store: store})
	if got == "" {
		t.Fatal("no preamble")
	}
	if len(got) > PreambleBudget {
		t.Fatalf("preamble is %d bytes, exceeds budget over %d", len(got), PreambleBudget)
	}
	// K=2 per ring is the cap; with one ring that is at most 2 bullets.
	if n := strings.Count(got, "\n- "); n > 2 {
		t.Errorf("preamble carries %d hits, want <= 2 (K=2 per ring)", n)
	}
}
