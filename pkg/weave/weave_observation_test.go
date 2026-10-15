package weave

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/quotaobs"
)

func TestObservationsFromLog(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct{ tool, log, signal string }{
		{"codex", "noise\nstatus\nERROR: You've hit your usage limit. Try again at 1:30 PM.\ntrailer\nend", "usage limit"},
		{"opencode", "request failed: z.ai error 1308 quota exhausted", "1308"},
		{"gemini", "429 RESOURCE_EXHAUSTED: retry later", "RESOURCE_EXHAUSTED"},
	}
	for _, tc := range cases {
		got := ObservationsFromLog(tc.tool, "fixture-model", tc.log, now)
		if len(got) != 1 || got[0].Tool != tc.tool || !strings.Contains(got[0].Excerpt, tc.signal) {
			t.Fatalf("%s: %+v", tc.tool, got)
		}
	}
}

func TestWeaveExitObservationFeeder(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	log := "before\nERROR: You've hit your usage limit. Try again at 1:30 PM.\nafter"
	if err := weaveRecordThrottleObservation("codex", "fixture-model", "codex-agent", log, now); err != nil {
		t.Fatal(err)
	}
	got, err := quotaobs.Read(time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Agent != "codex-agent" || got[0].Source != "weave" {
		t.Fatalf("observations: %+v", got)
	}
}
