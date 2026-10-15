package weave

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/quotaobs"
)

func TestObservationsFromLog(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		tool, model, log, signal string
		exit                     int
	}{
		{"codex", "fixture-model", "noise\nstatus\nERROR: You've hit your usage limit. Try again at 1:30 PM.\ntrailer\nend", "usage limit", 1},
		{"opencode", "glm-5.2", `{"error":{"code":"1308","message":"Usage limit reached for 5 hour. Your limit will reset at 2026-10-12 17:34:44"}}`, "1308", 0},
		{"gemini", "fixture-model", "429 RESOURCE_EXHAUSTED: retry later", "RESOURCE_EXHAUSTED", 1},
	}
	for _, tc := range cases {
		got := ObservationsFromLog(tc.tool, tc.model, tc.log, now, true, tc.exit)
		if len(got) != 1 || got[0].Tool != tc.tool || !strings.Contains(got[0].Excerpt, tc.signal) {
			t.Fatalf("%s: %+v", tc.tool, got)
		}
	}
}

func TestWeaveExitObservationFeeder(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	log := "before\nERROR: You've hit your usage limit. Try again at 1:30 PM.\nafter"
	if err := weaveRecordThrottleObservation("codex", "fixture-model", "codex-agent", log, now, true, 1); err != nil {
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

func TestObservationFalsePositiveExcerpts(t *testing.T) {
	f, err := os.Open("testdata/observation_false_positives.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	count := 0
	for scanner.Scan() {
		var excerpt string
		if err := json.Unmarshal(scanner.Bytes(), &excerpt); err != nil {
			t.Fatal(err)
		}
		tool, model := "codex", "codex-gpt-5.6"
		if count == 2 {
			tool, model = "claude", "claude-opus-5"
		} else if count == 4 || count == 9 || count >= 16 {
			tool, model = "opencode", "glm-5.2"
		}
		if got := ObservationsFromLog(tool, model, excerpt, now, true, 0); len(got) != 0 {
			t.Errorf("fixture %d: unexpected observation: %+v", count, got)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 18 {
		t.Fatalf("fixtures: got %d, want 18", count)
	}
}

func TestClaudeRateLimitEventObservations(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		status, kind, window, typ, utilization string
		want                                   bool
	}{
		{"rejected", "exhausted", "5h", "five_hour", "0.23", true},
		{"allowed_warning", "warning", "week", "seven_day", "0.79", true},
		{"rejected", "exhausted", "week", "seven_day_opus", "0.23", true},
		{"allowed", "warning", "5h", "five_hour", "0.80", true},
		{"allowed", "", "5h", "five_hour", "0.23", false},
	} {
		line := `{"type":"rate_limit_event","rate_limit_info":{"status":"` + tc.status + `","resetsAt":1791609000,"rateLimitType":"` + tc.typ + `","unifiedWindows":{"` + tc.typ + `":{"utilization":` + tc.utilization + `}}}}`
		got := ObservationsFromLog("claude", "claude-opus-5", "before\n"+line+"\nafter", now, true, 0)
		if (len(got) == 1) != tc.want {
			t.Fatalf("%s/%s: %+v", tc.status, tc.typ, got)
		}
		if !tc.want {
			if failed := ObservationsFromLog("claude", "claude-opus-5", line, now, true, 1); len(failed) != 0 {
				t.Fatalf("allowed event on failed run: %+v", failed)
			}
			continue
		}
		if got[0].Excerpt != line {
			t.Fatalf("excerpt = %q", got[0].Excerpt)
		}
		tool, ok := fleet.New().Tool("claude")
		if !ok {
			t.Fatal("claude tool missing")
		}
		parsed := fleet.MatchSignals(tool.UsageSignals, got[0].Excerpt, nil, now)
		if len(parsed) != 1 || parsed[0].Kind != tc.kind || parsed[0].Window != tc.window || parsed[0].ResetAt == nil || parsed[0].ResetAt.Unix() != 1791609000 {
			t.Fatalf("%s/%s: %+v", tc.status, tc.typ, parsed)
		}
	}
}

func TestStructuredUsageLimitWithoutFailedExit(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	line := `{"type":"result","usage_limit":"reset at 13:00"}`
	for _, tool := range []string{"claude", "codex"} {
		got := ObservationsFromLog(tool, "fixture-model", line, now, false, 0)
		if len(got) != 1 || got[0].Excerpt != line {
			t.Fatalf("%s: %+v", tool, got)
		}
	}
	if got := ObservationsFromLog("codex", "fixture-model", "note: quota at 13:00", now, false, 1); len(got) != 0 {
		t.Fatalf("unfinished prose: %+v", got)
	}
}
