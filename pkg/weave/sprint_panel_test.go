package weave

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
)

func setupPanelTestEnv(t *testing.T) (string, *fleet.Catalog, *ladder.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, "room"))
	t.Setenv("BASHY_FLEET_SEEDS", "off")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_MODELS_PATH", "")

	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	old := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { fleetCatalog = old })

	// Register tools across 3 vendors
	for _, tool := range []string{"tool-a", "tool-b", "tool-c"} {
		if err := cat.SaveTool(fleet.Tool{Name: tool}); err != nil {
			t.Fatal(err)
		}
	}
	// Register models
	for _, m := range []string{"model-a", "model-b", "model-c"} {
		if err := cat.SaveModel(fleet.Model{Name: m, Band: 5, CostMicro: 100}); err != nil {
			t.Fatal(err)
		}
	}
	// Register agents
	agents := []struct {
		name, tool, model string
	}{
		{"agent-a", "tool-a", "model-a"},
		{"agent-b", "tool-b", "model-b"},
		{"agent-c", "tool-c", "model-c"},
		{"agent-d", "tool-a", "model-a"},
		{"agent-e", "tool-b", "model-b"},
	}
	for _, a := range agents {
		if err := cat.SaveAgent(fleet.Agent{Name: a.name, Tool: a.tool, Model: a.model, Band: 5}); err != nil {
			t.Fatal(err)
		}
	}

	// Create sprint card #1
	board, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		q.Stories = append(q.Stories, &weaveStory{
			ID:     1,
			UUID:   "11111111-2222-3333-4444-555555555555",
			Title:  "panel sprint",
			Column: "doing",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Open ladder store and seat agents as provisional 5
	store, err := ladder.OpenStore(filepath.Join(home, "ladder", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, a := range agents {
		err := store.Append(ladder.Event{
			Kind:        ladder.EventKindSeat,
			Agent:       a.name,
			Provisional: 5,
			Season:      1,
			At:          now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	return home, cat, store
}

func execPanelCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newSprintPanelCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetContext(context.Background())
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestPanelDrawSizesAndVendors(t *testing.T) {
	setupPanelTestEnv(t)

	// Test 1: low-stakes draw (size = 1), author vendor = tool-a
	out, errOut, err := execPanelCmd(t, "1", "draw", "--case", "c-low", "--kind", "low-stakes", "--author-vendor", "tool-a")
	if err != nil {
		t.Fatalf("draw low-stakes: %v; errOut=%s", err, errOut)
	}
	if !strings.Contains(out, "c-low") {
		t.Errorf("stdout should mention case id: %s", out)
	}

	// Check card thread
	board, _ := sprintStoreDir()
	var card *weaveStory
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	if card == nil {
		t.Fatal("card not found")
	}

	var lowEv struct {
		Case    string   `json:"case"`
		Size    int      `json:"size"`
		Members []string `json:"members"`
	}
	found := false
	for _, c := range card.Thread {
		if c.Kind == "panel" && strings.Contains(c.Body, "c-low") {
			if err := json.Unmarshal([]byte(c.Body), &lowEv); err != nil {
				t.Fatalf("unmarshal panel thread body: %v", err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("panel thread event not found for c-low in %+v", card.Thread)
	}
	if lowEv.Size != 1 || len(lowEv.Members) != 1 {
		t.Errorf("size = %d, members = %v, want 1", lowEv.Size, lowEv.Members)
	}
	// Author vendor tool-a must be excluded for single judge
	cat := fleetCatalog()
	for _, m := range lowEv.Members {
		binding, tool, _, _ := cat.Binding(m)
		_ = binding
		if tool.Name == "tool-a" {
			t.Errorf("single judge %s shares author vendor tool-a", m)
		}
	}

	// Test 2: design draw (size = 3), author vendor = tool-a
	_, errOut, err = execPanelCmd(t, "1", "draw", "--case", "c-design", "--kind", "design", "--author-vendor", "tool-a")
	if err != nil {
		t.Fatalf("draw design: %v; errOut=%s", err, errOut)
	}
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	var designEv struct {
		Case    string   `json:"case"`
		Size    int      `json:"size"`
		Members []string `json:"members"`
	}
	found = false
	for _, c := range card.Thread {
		if c.Kind == "panel" && strings.Contains(c.Body, "c-design") {
			if err := json.Unmarshal([]byte(c.Body), &designEv); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			found = true
			break
		}
	}
	if !found || designEv.Size != 3 || len(designEv.Members) != 3 {
		t.Fatalf("design panel size = %d, members = %v, want 3", designEv.Size, designEv.Members)
	}
	// Must span at least 2 vendors and exclude tool-a when enough remain
	designVendors := map[string]bool{}
	for _, m := range designEv.Members {
		_, tool, _, _ := cat.Binding(m)
		designVendors[tool.Name] = true
		if tool.Name == "tool-a" {
			t.Errorf("design panel member %s from author vendor tool-a when enough remain", m)
		}
	}
	if len(designVendors) < 2 {
		t.Errorf("design panel vendors = %v, want >= 2", designVendors)
	}

	// Test 3: dispute draw (size = 5)
	_, errOut, err = execPanelCmd(t, "1", "draw", "--case", "c-dispute", "--kind", "dispute")
	if err != nil {
		t.Fatalf("draw dispute: %v; errOut=%s", err, errOut)
	}
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	var disputeEv struct {
		Case    string   `json:"case"`
		Size    int      `json:"size"`
		Members []string `json:"members"`
	}
	for _, c := range card.Thread {
		if c.Kind == "panel" && strings.Contains(c.Body, "c-dispute") {
			json.Unmarshal([]byte(c.Body), &disputeEv)
		}
	}
	if disputeEv.Size != 5 || len(disputeEv.Members) != 5 {
		t.Errorf("dispute panel size = %d, members = %v, want 5", disputeEv.Size, disputeEv.Members)
	}
}

func TestPanelVotesToVerdictAndEscalation(t *testing.T) {
	setupPanelTestEnv(t)

	// Draw design panel (3 members)
	_, _, err := execPanelCmd(t, "1", "draw", "--case", "c-v1", "--kind", "design")
	if err != nil {
		t.Fatal(err)
	}

	board, _ := sprintStoreDir()
	var card *weaveStory
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	var panelEv struct {
		Members []string `json:"members"`
	}
	for _, c := range card.Thread {
		if c.Kind == "panel" && strings.Contains(c.Body, "c-v1") {
			json.Unmarshal([]byte(c.Body), &panelEv)
		}
	}
	if len(panelEv.Members) != 3 {
		t.Fatalf("expected 3 members, got %v", panelEv.Members)
	}

	// Panelists vote: 2 accept, 1 reject
	m := panelEv.Members
	if _, _, err := execPanelCmd(t, "1", "vote", "--case", "c-v1", "--agent", m[0], "--verdict", "accept", "--confidence", "0.9", "--rubric", "quality=0.9"); err != nil {
		t.Fatalf("vote 1: %v", err)
	}
	if _, _, err := execPanelCmd(t, "1", "vote", "--case", "c-v1", "--agent", m[1], "--verdict", "accept", "--confidence", "0.8", "--rubric", "quality=0.8"); err != nil {
		t.Fatalf("vote 2: %v", err)
	}
	if _, _, err := execPanelCmd(t, "1", "vote", "--case", "c-v1", "--agent", m[2], "--verdict", "reject", "--confidence", "0.7", "--rubric", "quality=0.6"); err != nil {
		t.Fatalf("vote 3: %v", err)
	}

	// Compute verdict: majority accept
	out, errOut, err := execPanelCmd(t, "1", "verdict", "--case", "c-v1")
	if err != nil {
		t.Fatalf("verdict: %v; errOut=%s", err, errOut)
	}
	if strings.Contains(out, "ESCALATE TO OWNER") {
		t.Errorf("expected clean verdict, got escalation: %s", out)
	}
	if !strings.Contains(out, "accept") {
		t.Errorf("expected verdict accept, got: %s", out)
	}

	// Verify thread has kind "verdict"
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	hasVerdict := false
	for _, c := range card.Thread {
		if c.Kind == "verdict" && strings.Contains(c.Body, "c-v1") && strings.Contains(c.Body, "accept") {
			hasVerdict = true
			break
		}
	}
	if !hasVerdict {
		t.Errorf("verdict thread event not found: %+v", card.Thread)
	}

	// Now test 3-way split -> Owner escalation
	_, _, err = execPanelCmd(t, "1", "draw", "--case", "c-split", "--kind", "design")
	if err != nil {
		t.Fatal(err)
	}
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	for _, c := range card.Thread {
		if c.Kind == "panel" && strings.Contains(c.Body, "c-split") {
			json.Unmarshal([]byte(c.Body), &panelEv)
		}
	}
	m = panelEv.Members
	execPanelCmd(t, "1", "vote", "--case", "c-split", "--agent", m[0], "--verdict", "accept", "--confidence", "0.9")
	execPanelCmd(t, "1", "vote", "--case", "c-split", "--agent", m[1], "--verdict", "reject", "--confidence", "0.9")
	execPanelCmd(t, "1", "vote", "--case", "c-split", "--agent", m[2], "--verdict", "rework", "--confidence", "0.9")

	out, _, err = execPanelCmd(t, "1", "verdict", "--case", "c-split")
	if err != nil {
		t.Fatalf("split verdict: %v", err)
	}
	if !strings.Contains(out, "ESCALATE TO OWNER") {
		t.Fatalf("expected ESCALATE TO OWNER, got: %s", out)
	}

	// Check thread has kind "escalation"
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	hasEscalation := false
	for _, c := range card.Thread {
		if c.Kind == "escalation" && strings.Contains(c.Body, "c-split") {
			hasEscalation = true
			break
		}
	}
	if !hasEscalation {
		t.Errorf("escalation thread event not found: %+v", card.Thread)
	}
}

func TestPanelPlantedFileNeverOnCard(t *testing.T) {
	home, _, _ := setupPanelTestEnv(t)

	// Find a case ID that is planted for sprint UUID "11111111-2222-3333-4444-555555555555"
	sprintUUID := "11111111-2222-3333-4444-555555555555"
	var plantedCaseID string
	for i := 0; i < 500; i++ {
		cand := fmt.Sprintf("case-calib-%d", i)
		h := panelCaseSeed(sprintUUID, cand)
		if ladder.IsPlanted(cand, h) {
			plantedCaseID = cand
			break
		}
	}
	if plantedCaseID == "" {
		t.Fatal("could not find planted case id")
	}

	// Draw panel for this planted case with --answer accept
	out, errOut, err := execPanelCmd(t, "1", "draw", "--case", plantedCaseID, "--kind", "low-stakes", "--answer", "accept")
	if err != nil {
		t.Fatalf("draw planted: %v; errOut=%s", err, errOut)
	}
	_ = out

	// Check planted.json was written privately
	plantedPath := filepath.Join(home, "sprint", "panels", "1", "planted.json")
	data, err := os.ReadFile(plantedPath)
	if err != nil {
		t.Fatalf("expected planted.json at %s: %v", plantedPath, err)
	}
	if !strings.Contains(string(data), plantedCaseID) {
		t.Fatalf("planted.json missing %s: %s", plantedCaseID, string(data))
	}

	// Check that the card thread NEVER contains the word "planted"
	board, _ := sprintStoreDir()
	var card *weaveStory
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	for _, c := range card.Thread {
		if strings.Contains(strings.ToLower(c.Body), "planted") {
			t.Fatalf("planted leak on card thread: %+v", c)
		}
		if strings.Contains(c.Body, "planted.json") {
			t.Fatalf("planted.json path leak on card thread: %+v", c)
		}
		if strings.EqualFold(c.Kind, "planted") {
			t.Fatalf("planted kind leak on card thread: %+v", c)
		}
	}
}

func TestPanelJudgeEventsForPlantedCases(t *testing.T) {
	home, _, store := setupPanelTestEnv(t)

	sprintUUID := "11111111-2222-3333-4444-555555555555"
	var plantedCaseID string
	for i := 0; i < 500; i++ {
		cand := fmt.Sprintf("planted-eval-%d", i)
		h := panelCaseSeed(sprintUUID, cand)
		if ladder.IsPlanted(cand, h) {
			plantedCaseID = cand
			break
		}
	}
	if plantedCaseID == "" {
		t.Fatal("could not find planted case id")
	}

	// Draw panel for planted case
	_, _, err := execPanelCmd(t, "1", "draw", "--case", plantedCaseID, "--kind", "design", "--answer", "accept")
	if err != nil {
		t.Fatal(err)
	}

	// Check planted.json has answer accept
	plantedPath := filepath.Join(home, "sprint", "panels", "1", "planted.json")
	if _, err := os.Stat(plantedPath); err != nil {
		t.Fatalf("planted.json not created: %v", err)
	}

	board, _ := sprintStoreDir()
	var card *weaveStory
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		card = findWeaveStory(q, 1)
		return nil
	})
	var panelEv struct {
		Members []string `json:"members"`
	}
	for _, c := range card.Thread {
		if c.Kind == "panel" && strings.Contains(c.Body, plantedCaseID) {
			json.Unmarshal([]byte(c.Body), &panelEv)
		}
	}
	m := panelEv.Members
	if len(m) != 3 {
		t.Fatalf("want 3 members, got %v", m)
	}

	// Member 0 votes "accept" (match -> score 1)
	// Member 1 votes "reject" (miss -> score 0)
	// Member 2 votes "accept" (match -> score 1)
	execPanelCmd(t, "1", "vote", "--case", plantedCaseID, "--agent", m[0], "--verdict", "accept", "--confidence", "0.9")
	execPanelCmd(t, "1", "vote", "--case", plantedCaseID, "--agent", m[1], "--verdict", "reject", "--confidence", "0.8")
	execPanelCmd(t, "1", "vote", "--case", plantedCaseID, "--agent", m[2], "--verdict", "accept", "--confidence", "0.9")

	// Call verdict
	_, _, err = execPanelCmd(t, "1", "verdict", "--case", plantedCaseID)
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}

	// Read ladder events from store
	events, err := store.Read()
	if err != nil {
		t.Fatalf("read store: %v", err)
	}

	var judgeEvents []ladder.Event
	for _, e := range events {
		if e.Duty == ladder.DutyJudge || e.Kind == "judge" {
			judgeEvents = append(judgeEvents, e)
		}
	}
	if len(judgeEvents) != 3 {
		t.Fatalf("expected 3 judge events, got %d: %+v", len(judgeEvents), judgeEvents)
	}

	scoreByAgent := map[string]float64{}
	for _, je := range judgeEvents {
		scoreByAgent[je.Agent] = je.Score
		if je.Opponent.R != 1500 || je.Opponent.RD != 50 {
			t.Errorf("agent %s opponent = %+v, want R:1500, RD:50", je.Agent, je.Opponent)
		}
	}
	if scoreByAgent[m[0]] != 1.0 {
		t.Errorf("agent %s score = %g, want 1.0", m[0], scoreByAgent[m[0]])
	}
	if scoreByAgent[m[1]] != 0.0 {
		t.Errorf("agent %s score = %g, want 0.0", m[1], scoreByAgent[m[1]])
	}
	if scoreByAgent[m[2]] != 1.0 {
		t.Errorf("agent %s score = %g, want 1.0", m[2], scoreByAgent[m[2]])
	}
}

func TestPanelAuditSampleDeterministic(t *testing.T) {
	setupPanelTestEnv(t)

	board, _ := sprintStoreDir()

	// Create 20 non-planted cases and 2 planted cases on sprint #1
	var nonPlantedIDs []string
	var plantedIDs []string

	sprintUUID := "11111111-2222-3333-4444-555555555555"
	for i := 0; len(nonPlantedIDs) < 20 || len(plantedIDs) < 2; i++ {
		cid := fmt.Sprintf("case-audit-%03d", i)
		h := panelCaseSeed(sprintUUID, cid)
		if ladder.IsPlanted(cid, h) {
			if len(plantedIDs) < 2 {
				plantedIDs = append(plantedIDs, cid)
			}
		} else {
			if len(nonPlantedIDs) < 20 {
				nonPlantedIDs = append(nonPlantedIDs, cid)
			}
		}
	}

	// Record planted cases in planted.json
	plantedMap := map[string]plantedEntry{}
	for _, pid := range plantedIDs {
		plantedMap[pid] = plantedEntry{Case: pid, Planted: true, Answer: "accept"}
	}
	plantedPath, _ := sprintPlantedPath(1)
	savePlantedFile(plantedPath, plantedMap)

	// Record verdicts on sprint card
	withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, 1)
		for _, cid := range nonPlantedIDs {
			body := struct {
				Case    string         `json:"case"`
				Verdict string         `json:"verdict"`
				Tally   map[string]int `json:"tally"`
				Season  int            `json:"season"`
			}{cid, "accept", map[string]int{"accept": 3}, 1}
			raw, _ := json.Marshal(body)
			weaveStoryAppend(s, "conductor", "verdict", string(raw))
		}
		for _, cid := range plantedIDs {
			body := struct {
				Case    string         `json:"case"`
				Verdict string         `json:"verdict"`
				Tally   map[string]int `json:"tally"`
				Season  int            `json:"season"`
			}{cid, "accept", map[string]int{"accept": 3}, 1}
			raw, _ := json.Marshal(body)
			weaveStoryAppend(s, "conductor", "verdict", string(raw))
		}
		return nil
	})

	// Run audit multiple times
	var firstOut string
	for i := 0; i < 5; i++ {
		out, errOut, err := execPanelCmd(t, "1", "audit", "--season", "1", "--rate", "0.1")
		if err != nil {
			t.Fatalf("audit run %d: %v; errOut=%s", i, err, errOut)
		}
		if i == 0 {
			firstOut = out
		} else if out != firstOut {
			t.Fatalf("audit nondeterministic:\nrun 0:\n%s\nrun %d:\n%s", firstOut, i, out)
		}
	}

	// Verify none of the planted cases are in the audit output
	for _, pid := range plantedIDs {
		if strings.Contains(firstOut, pid) {
			t.Errorf("planted case %s appeared in owner audit sample!", pid)
		}
	}

	// Verify that at least one non-planted case is sampled (around 10% of 20 = ~2)
	sampledCount := 0
	for _, cid := range nonPlantedIDs {
		if strings.Contains(firstOut, cid) {
			sampledCount++
		}
	}
	if sampledCount == 0 || sampledCount > 6 {
		t.Errorf("sampled %d cases out of 20 with rate 0.1, want ~2", sampledCount)
	}
}
