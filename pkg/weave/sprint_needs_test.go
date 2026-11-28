package weave

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ask"
)

func TestRenderSprintNeedsListsOpenRequests(t *testing.T) {
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "olivia")
	if _, err := ask.RaiseNeed(ask.NeedSpec{Kind: ask.NeedDecision, Title: "Ship the hotfix?", Sprint: "741"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ask.RaiseNeed(ask.NeedSpec{Kind: ask.NeedInfo, Title: "Other sprint", Sprint: "700"}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	renderSprintNeeds(&b, &weaveStory{ID: 741})
	out := b.String()
	if !strings.Contains(out, "NEEDS YOU:  1 open request") || !strings.Contains(out, "decision: Ship the hotfix?") ||
		!strings.Contains(out, "olivia") || strings.Contains(out, "Other sprint") {
		t.Fatalf("sprint needs block:\n%s", out)
	}
	b.Reset()
	renderSprintNeeds(&b, &weaveStory{ID: 9})
	if b.Len() != 0 {
		t.Fatalf("a sprint with no requests must print nothing: %q", b.String())
	}
}
