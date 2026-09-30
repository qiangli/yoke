package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ladder"
)

func execEstimateCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newSprintEstimateCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func estimateThread(t *testing.T) []weaveComment {
	t.Helper()
	board, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	var card *weaveStory
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return card.Thread
}

func TestEstimatePanelSizeAndHiddenUntilClose(t *testing.T) {
	setupPanelTestEnv(t)
	out, errOut, err := execEstimateCmd(t, "1", "open", "--story", "story-low", "--expected", "1")
	if err != nil {
		t.Fatalf("open: %v; stderr=%s", err, errOut)
	}
	if !strings.Contains(out, "size=1") {
		t.Fatalf("open output = %q, want size 1", out)
	}
	var open struct {
		Story      string   `json:"story"`
		Size       int      `json:"size"`
		Estimators []string `json:"estimators"`
	}
	for _, c := range estimateThread(t) {
		if c.Kind == "estimate-open" {
			if err := json.Unmarshal([]byte(c.Body), &open); err != nil {
				t.Fatal(err)
			}
		}
	}
	if open.Story != "story-low" || open.Size != 1 || len(open.Estimators) != 1 {
		t.Fatalf("open event = %+v", open)
	}
	agent := open.Estimators[0]
	out, errOut, err = execEstimateCmd(t, "1", "submit", "--story", "story-low", "--agent", agent, "--points", "3", "--confidence", "0.9")
	if err != nil {
		t.Fatalf("submit: %v; stderr=%s", err, errOut)
	}
	if strings.Contains(strings.ToLower(out), "3") {
		t.Fatalf("blind submit leaked estimate: %q", out)
	}
	for _, c := range estimateThread(t) {
		if strings.Contains(c.Body, `"points"`) {
			t.Fatalf("estimate leaked to thread before close: %+v", c)
		}
	}
	if _, errOut, err = execEstimateCmd(t, "1", "close", "--story", "story-low"); err != nil {
		t.Fatalf("close: %v; stderr=%s", err, errOut)
	}
	if !strings.Contains(strings.Join(func() []string {
		var b []string
		for _, c := range estimateThread(t) {
			b = append(b, c.Body)
		}
		return b
	}(), " "), `"points":3`) {
		t.Fatal("closed estimate missing settled points")
	}
}

func TestEstimateRevoteUsesFirstAndSplitEscalationAndShadow(t *testing.T) {
	setupPanelTestEnv(t)
	_, _, err := execEstimateCmd(t, "1", "open", "--story", "story-three", "--expected", "3")
	if err != nil {
		t.Fatal(err)
	}
	var open struct {
		Estimators []string `json:"estimators"`
	}
	for _, c := range estimateThread(t) {
		if c.Kind == "estimate-open" {
			_ = json.Unmarshal([]byte(c.Body), &open)
		}
	}
	if len(open.Estimators) != 3 {
		t.Fatalf("panel = %v, want 3", open.Estimators)
	}
	for i, agent := range open.Estimators {
		points := []string{"13", "5", "8"}[i]
		if _, _, err := execEstimateCmd(t, "1", "submit", "--story", "story-three", "--agent", agent, "--points", points, "--confidence", "0.9"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := execEstimateCmd(t, "1", "submit", "--story", "story-three", "--agent", open.Estimators[1], "--points", "8", "--confidence", "0.9"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execEstimateCmd(t, "1", "submit", "--story", "story-three", "--agent", "agent-shadow", "--points", "1", "--confidence", "1", "--shadow", "agent-shadow"); err != nil {
		t.Fatal(err)
	}
	out, _, err := execEstimateCmd(t, "1", "close", "--story", "story-three")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "must be split") || !strings.Contains(out, "points=8") {
		t.Fatalf("close = %q", out)
	}
	store, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ladder.Event{}
	for _, event := range events {
		if event.Kind == ladder.EventKindEstimate && event.Story == "story-three" {
			got[event.Agent] = event
		}
	}
	if got[open.Estimators[1]].Estimate != 5 {
		t.Fatalf("revote event = %+v, want first estimate 5", got[open.Estimators[1]])
	}
	if got["agent-shadow"].Note != "shadow" || got["agent-shadow"].Estimate != 1 {
		t.Fatalf("shadow event = %+v", got["agent-shadow"])
	}
}

func TestEstimateEscalatesSingleLowConfidence(t *testing.T) {
	setupPanelTestEnv(t)
	if _, _, err := execEstimateCmd(t, "1", "open", "--story", "story-escalate", "--expected", "1"); err != nil {
		t.Fatal(err)
	}
	var open struct {
		Estimators []string `json:"estimators"`
	}
	for _, c := range estimateThread(t) {
		if c.Kind == "estimate-open" {
			_ = json.Unmarshal([]byte(c.Body), &open)
		}
	}
	if _, _, err := execEstimateCmd(t, "1", "submit", "--story", "story-escalate", "--agent", open.Estimators[0], "--points", "3", "--confidence", "0.4"); err != nil {
		t.Fatal(err)
	}
	out, _, err := execEstimateCmd(t, "1", "close", "--story", "story-escalate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "escalate to 3") {
		t.Fatalf("close = %q", out)
	}
}
