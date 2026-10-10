package cligw

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

// The door's admission preview refuses a candidate whose model another
// episode holds — under the registry name or the provider-side id — so the
// router never hands it the turn.
func TestDoorPreviewRefusesClaimedModel(t *testing.T) {
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
	t.Setenv("BASHY_EPISODE", "ep-me")
	restore := llmbudget.SetDefault(llmbudget.New(llmbudget.Config{StatePath: filepath.Join(t.TempDir(), "meter.json")}))
	t.Cleanup(restore)
	other := principal.Ref{Name: "other", Episode: "ep-other", Host: "h"}
	agent := Agent{Name: "genie-gpt-oss-20b", Model: "gpt-oss-20b", ModelID: "gpt-oss:20b", Tool: "genie", Provider: "ollama"}
	ctx := context.Background()
	for _, ref := range []string{"model:gpt-oss:20b", "model:gpt-oss-20b"} {
		if _, err := coord.AcquireRef(ctx, coord.Request{Ref: coord.ParseRef(ref), Holder: other, Intent: "eval"}); err != nil {
			t.Fatal(err)
		}
		allowed, reason := (llmBudgetQuota{}).Preview(ctx, agent)
		if allowed || !strings.Contains(reason, "already holds "+ref) {
			t.Fatalf("%s: allowed = %v, reason = %q", ref, allowed, reason)
		}
		if err := coord.ReleaseRef(ctx, coord.ParseRef(ref), other, 0); err != nil {
			t.Fatal(err)
		}
	}
	if allowed, reason := (llmBudgetQuota{}).Preview(ctx, agent); !allowed {
		t.Fatalf("unclaimed candidate refused: %s", reason)
	}
}
