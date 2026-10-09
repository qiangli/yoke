package weave

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/external/loom"
	yokegit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	"github.com/spf13/cobra"
)

type heatFairness struct {
	Base        string        `json:"base"`
	Template    string        `json:"template"`
	PromptHash  string        `json:"prompt_hash"`
	Gate        string        `json:"gate"`
	ToolVersion string        `json:"tool_version"`
	Model       string        `json:"model"`
	Points      int           `json:"points"`
	MaxRuntime  time.Duration `json:"max_runtime"`
	Started     time.Time     `json:"started"`
}

type heatAttempt struct {
	Agent             string           `json:"agent"`
	Shadow            bool             `json:"shadow,omitempty"`
	CanonicalAgent    string           `json:"canonical_agent,omitempty"`
	InstanceUUID      string           `json:"instance_uuid,omitempty"`
	FamilyID          string           `json:"family_id,omitempty"`
	SeedBand          int              `json:"seed_band,omitempty"`
	Run               string           `json:"run"`
	Fairness          heatFairness     `json:"fairness"`
	Digest            string           `json:"digest"`
	FinishToolVersion string           `json:"finish_tool_version,omitempty"`
	FinishModel       string           `json:"finish_model,omitempty"`
	PreflightFailed   bool             `json:"preflight_failed,omitempty"`
	Verdict           string           `json:"verdict,omitempty"`
	Tamper            bool             `json:"tamper,omitempty"`
	GateExit          int              `json:"gate_exit,omitempty"`
	Grade             sprintGradeEvent `json:"grade,omitempty"`
	Turns             int              `json:"turns,omitempty"`
	Wall              time.Duration    `json:"wall,omitempty"`
	Cost              float64          `json:"cost,omitempty"`
	Error             string           `json:"error,omitempty"`
}

type heatRecord struct {
	ID       string        `json:"id"`
	Sprint   int64         `json:"sprint"`
	Story    string        `json:"story"`
	Gate     string        `json:"gate"`
	Status   string        `json:"status"`
	Reason   string        `json:"reason,omitempty"`
	Winner   string        `json:"winner,omitempty"`
	Attempts []heatAttempt `json:"attempts"`
}

func heatDigest(f heatFairness) string {
	tick := f.Started.UTC().Truncate(time.Minute).Format(time.RFC3339)
	data, _ := json.Marshal([]any{f.Base, f.Template, f.PromptHash, f.Gate, f.ToolVersion, f.Model, f.Points, f.MaxRuntime.String(), tick})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func heatMismatch(all []heatFairness) string {
	if len(all) < 2 {
		return "fewer than two attempts"
	}
	a := all[0]
	for _, b := range all[1:] {
		if a.Base != b.Base {
			return "arena base mismatch"
		}
		if a.Template != b.Template {
			return "template mismatch"
		}
		if a.PromptHash != b.PromptHash {
			return "prompt mismatch"
		}
		if a.Gate != b.Gate {
			return "gate mismatch"
		}
		if a.Points != b.Points || a.MaxRuntime != b.MaxRuntime {
			return "cap mismatch"
		}
		if d := a.Started.Sub(b.Started); d > time.Minute || d < -time.Minute {
			return "start tick mismatch"
		}
	}
	return ""
}

func heatWinner(all []heatAttempt) string {
	var real []heatAttempt
	for _, a := range all {
		if !a.Shadow {
			real = append(real, a)
		}
	}
	if len(real) == 0 {
		return ""
	}
	copy := append([]heatAttempt(nil), real...)
	sort.SliceStable(copy, func(i, j int) bool {
		a, b := copy[i], copy[j]
		if (a.Verdict == "pass") != (b.Verdict == "pass") {
			return a.Verdict == "pass"
		}
		if a.Cost != b.Cost {
			return a.Cost < b.Cost
		}
		if a.Turns != b.Turns {
			return a.Turns < b.Turns
		}
		return a.Agent < b.Agent
	})
	return copy[0].Agent
}

func heatSchedule(rec *heatRecord, launch func(int, *heatAttempt) error, grade func(int, *heatAttempt) error) {
	var wg sync.WaitGroup
	for i := range rec.Attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := &rec.Attempts[i]
			if err := launch(i, a); err != nil {
				a.Error = err.Error()
				if strings.Contains(strings.ToLower(a.Error), "preflight") {
					a.PreflightFailed = true
					a.Verdict = "fail"
				}
			}
			if !a.Fairness.Started.IsZero() {
				a.Digest = heatDigest(a.Fairness)
			}
		}(i)
	}
	wg.Wait()
	var fair []heatFairness
	for _, a := range rec.Attempts {
		if a.Shadow {
			continue
		}
		fair = append(fair, a.Fairness)
		if a.Error != "" && !a.PreflightFailed {
			rec.Reason = "launch error"
		}
		if a.FinishToolVersion != "" && a.FinishToolVersion != a.Fairness.ToolVersion || a.FinishModel != "" && a.FinishModel != a.Fairness.Model {
			rec.Reason = "entrant version changed"
		}
	}
	if reason := heatMismatch(fair); reason != "" {
		rec.Reason = reason
	}
	if rec.Reason != "" {
		rec.Status = "void"
		return
	}
	for i := range rec.Attempts {
		if rec.Attempts[i].PreflightFailed {
			continue
		}
		if err := grade(i, &rec.Attempts[i]); err != nil {
			rec.Attempts[i].Error = err.Error()
			if !rec.Attempts[i].Shadow {
				rec.Reason = "grade error"
			}
		}
	}
	if rec.Reason != "" {
		rec.Status = "void"
		return
	}
	rec.Status, rec.Winner = "rated", heatWinner(rec.Attempts)
}

func heatStoryPrompt(body string) string {
	var lines []string
	for _, line := range strings.Split(boothProjection(body), "\n") {
		key := strings.ToLower(strings.TrimLeft(line, " \t-*#>"))
		key = strings.ReplaceAll(key, "*", "")
		if strings.HasPrefix(key, "shadow:") || strings.HasPrefix(key, "shadow agent:") || strings.HasPrefix(key, "shadow agents:") || strings.HasPrefix(key, "shadow-only-record:") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func heatResultCost(log string) float64 {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var event weaveStreamJSONEvent
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &event) == nil && event.Type == "result" {
			if event.CostUSD > 0 {
				return event.CostUSD
			}
			return event.TotalCostUSDAlt
		}
	}
	return 0
}

func heatRecordPath(sprint int64, id string) (string, error) {
	dir, err := sprintStoreDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "heats", strconv.FormatInt(sprint, 10), id+".json"), nil
}

func heatSave(rec heatRecord) error {
	path, err := heatRecordPath(rec.Sprint, rec.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func newSprintHeatCmd() *cobra.Command {
	var story, agents, gate string
	var points int
	var allow, shadows []string
	var shadowOnlyRecord bool
	cmd := &cobra.Command{Use: "heat N --story ID --agents A,B --gate CMD", Short: "Run a blind paired story heat", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&story, "story", "", "todo story id")
	cmd.Flags().StringVar(&agents, "agents", "", "comma-separated agent names")
	cmd.Flags().StringVar(&gate, "gate", "", "shared external grading command")
	cmd.Flags().IntVar(&points, "points", 0, "story point cap (1,2,3,5,8)")
	cmd.Flags().StringArrayVar(&allow, "allow-test-change", nil, "accepted test change glob")
	cmd.Flags().StringArrayVar(&shadows, "shadow", nil, "additional blind shadow agent (repeatable; excluded from winner and merge)")
	cmd.Flags().BoolVar(&shadowOnlyRecord, "shadow-only-record", false, "shadow delivery events rate only the shadow agent itself; real agents' records are unaffected")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid sprint number")
		}
		if shadowOnlyRecord && len(shadows) == 0 {
			return fmt.Errorf("--shadow-only-record requires --shadow")
		}
		return runSprintHeat(cmd, id, story, strings.Split(agents, ","), shadows, gate, points, allow)
	}
	return cmd
}

func runSprintHeat(cmd *cobra.Command, sprint int64, story string, agents, shadows []string, gate string, points int, allow []string) error {
	if story == "" || strings.TrimSpace(gate) == "" || len(agents) < 2 {
		return fmt.Errorf("--story, --gate and at least two --agents required")
	}
	seen := map[string]bool{}
	for i, agent := range agents {
		agent = strings.TrimSpace(agent)
		if agent == "" || seen[agent] {
			return fmt.Errorf("agents must be distinct and nonempty")
		}
		seen[agent] = true
		agents[i] = agent
	}
	for i, agent := range shadows {
		agent = strings.TrimSpace(agent)
		if agent == "" || seen[agent] {
			return fmt.Errorf("shadow agents must be distinct from all entrants and nonempty")
		}
		seen[agent] = true
		shadows[i] = agent
	}
	realCount := len(agents)
	agents = append(agents, shadows...)
	card, err := arenaCard(sprint)
	if err != nil {
		return err
	}
	if err := sprintGradeManager(card); err != nil {
		return err
	}
	if card.Arena == nil {
		return fmt.Errorf("sprint arena is down")
	}
	if _, running, err := loom.ArenaState(cmd.Context()); err != nil {
		return err
	} else if !running {
		return fmt.Errorf("sprint arena is down")
	}
	root, err := weaveRepoRoot(".")
	if err != nil {
		return err
	}
	var base string
	for _, repo := range card.Arena.Repos {
		if repo.Repo == filepath.Base(root) {
			base = repo.Base
		}
	}
	if base == "" {
		return fmt.Errorf("repository is not in arena")
	}
	sha, err := arenaDefaultSHA(root)
	if err != nil {
		return err
	}
	if sha != base {
		return fmt.Errorf("checkout HEAD differs from pinned arena base")
	}
	_, item, err := findRegisterItem(root, story)
	if err != nil {
		return err
	}
	if !storyBelongsToSprint(item, card) {
		return fmt.Errorf("story does not belong to sprint")
	}
	if points == 0 {
		for _, link := range card.Runs {
			if dir, e := weaveQueueDirForSprintRun(link); e == nil {
				if q, e := loadWeaveQueue(dir); e == nil {
					if run := findWeaveItem(q, link.ID); run != nil && run.Register == item.ID && run.Points > 0 {
						points = run.Points
						break
					}
				}
			}
		}
	}
	if _, ok := weavePointRuntimeCap(points); !ok {
		return fmt.Errorf("heat needs valid story points")
	}
	cap, _ := weavePointRuntimeCap(points)
	board, err := sprintStoreDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(board, "heats", strconv.FormatInt(sprint, 10)), 0700); err != nil {
		return err
	}
	source, err := os.MkdirTemp(filepath.Join(board, "heats", strconv.FormatInt(sprint, 10)), "source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(source)
	if err := os.Remove(source); err != nil {
		return err
	}
	// One door (sprint 252 S252.6): --no-checkout and --detach are
	// unrouted, so these stay on the host binary through the door
	// (verbatim argv, combined output preserved in the error).
	if output, err := yokegit.RunChecked(cmd.Context(), "", []string{"clone", "--local", "--no-hardlinks", "--no-checkout", root, source}); err != nil {
		return fmt.Errorf("prepare heat source: %w: %s", err, output)
	}
	if output, err := yokegit.RunChecked(cmd.Context(), source, []string{"checkout", "--detach", base}); err != nil {
		return fmt.Errorf("checkout arena base: %w: %s", err, output)
	}
	template, err := buildBoothTemplate(cmd.Context(), int(sprint), item.ID, source, nil)
	if err != nil {
		return err
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	rec := heatRecord{ID: hex.EncodeToString(random[:]), Sprint: sprint, Story: item.ID, Gate: gate, Status: "launching", Attempts: make([]heatAttempt, len(agents))}
	queue, err := ensureWeaveQueueDir(root)
	if err != nil {
		return err
	}
	body := heatStoryPrompt(item.Body)
	if len(item.Refs) > 0 {
		body += fmt.Sprintf("\n\nThis issue also touches: %v\n", item.Refs)
	}
	promptSum := sha256.Sum256([]byte(body))
	var links []sprintRun
	for i, agent := range agents {
		it := &weaveItem{Title: item.Title, Body: body, Register: item.ID, State: "todo", Priority: "p2", Points: points, Created: time.Now().UTC()}
		if err := withWeaveQueueLock(queue, func(q *weaveQueue) error { it.ID = q.NextID; q.NextID++; q.Items = append(q.Items, it); return nil }); err != nil {
			return err
		}
		link, err := resolveAndValidateSprintRunLink(filepath.Base(root), it.ID, filepath.Base(queue))
		if err != nil {
			return err
		}
		links = append(links, link)
		rec.Attempts[i] = heatAttempt{Agent: agent, Shadow: i >= realCount, Run: fmt.Sprintf("%s#%d", link.Repo, link.ID), Fairness: heatFairness{Base: base, Template: template.Digest, PromptHash: hex.EncodeToString(promptSum[:]), Gate: gate, Model: agent, Points: points, MaxRuntime: cap}}
	}
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, sprint)
		if s == nil {
			return fmt.Errorf("sprint disappeared")
		}
		s.Runs = append(s.Runs, links[:realCount]...)
		return nil
	}); err != nil {
		return err
	}
	if err := heatSave(rec); err != nil {
		return err
	}
	heatSchedule(&rec, func(i int, a *heatAttempt) error {
		launch, _, err := weaveExpandAgent([]string{a.Agent}, body, item.Title)
		if err != nil || launch == nil {
			return fmt.Errorf("resolve agent: %v", err)
		}
		a.Fairness.Model = launch.Model
		versionCtx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		version, versionErr := exec.CommandContext(versionCtx, launch.Tool, "--version").Output()
		cancel()
		if versionErr != nil {
			return fmt.Errorf("tool version: %w", versionErr)
		}
		a.Fairness.ToolVersion = strings.TrimSpace(string(version))
		a.Fairness.Started = time.Now().UTC()
		start := &cobra.Command{}
		start.SetContext(cmd.Context())
		start.SetOut(cmd.OutOrStdout())
		start.SetErr(cmd.ErrOrStderr())
		launchErr := runWeaveStart(start, links[i].ID, "", []string{a.Agent}, weaveStartOptions{arena: strconv.FormatInt(sprint, 10), blind: true, heatTemplate: template, maxRuntime: cap, pty: "never"}, &weaveOutputFlags{})
		if q, err := loadWeaveQueue(queue); err == nil {
			if it := findWeaveItem(q, links[i].ID); it != nil && !it.StartedAt.IsZero() {
				a.Fairness.Started = it.StartedAt
			}
			if it := findWeaveItem(q, links[i].ID); it != nil {
				if agent, ok := weaveCapabilityAgent(it); ok {
					a.CanonicalAgent = agent
				}
				a.InstanceUUID, a.FamilyID, a.SeedBand = it.Instance, it.InstanceFamily, it.Band
			}
		}
		if launchErr != nil {
			return launchErr
		}
		finishCtx, finishCancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		finishVersion, finishErr := exec.CommandContext(finishCtx, launch.Tool, "--version").Output()
		finishCancel()
		if finishErr != nil {
			return fmt.Errorf("finish tool version: %w", finishErr)
		}
		a.FinishToolVersion = strings.TrimSpace(string(finishVersion))
		finishLaunch, _, finishErr := weaveExpandAgent([]string{a.Agent}, body, item.Title)
		if finishErr != nil || finishLaunch == nil {
			return fmt.Errorf("finish model: %v", finishErr)
		}
		a.FinishModel = finishLaunch.Model
		return launchErr
	}, func(i int, a *heatAttempt) error {
		q, err := loadWeaveQueue(queue)
		if err != nil {
			return err
		}
		it := findWeaveItem(q, links[i].ID)
		if it == nil {
			return fmt.Errorf("run disappeared")
		}
		grade, err := sprintGradeAttempt(cmd.Context(), sprint, a.Run, it, queue, base, gate, []string{"**/*_test.go", "tests/**"}, 10*time.Minute, allow...)
		if err != nil {
			return err
		}
		a.Grade, a.Verdict, a.Tamper, a.GateExit = grade, grade.Verdict, grade.Tamper, grade.GateExit
		if it.LogPath != "" {
			if data, err := os.ReadFile(it.LogPath); err == nil {
				a.Turns = int(weaveResultTurns(string(data)))
				a.Cost = heatResultCost(string(data))
			}
		}
		if !it.StartedAt.IsZero() && !it.FinishedAt.IsZero() {
			a.Wall = it.FinishedAt.Sub(it.StartedAt)
		}
		return nil
	})
	if rec.Reason != "" {
		return heatFinish(cmd, rec, nil)
	}
	store, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		return err
	}
	return heatFinish(cmd, rec, store)
}

func heatFinish(cmd *cobra.Command, rec heatRecord, store *ladder.Store) error {
	if err := heatSave(rec); err != nil {
		return err
	}
	board, err := sprintStoreDir()
	if err != nil {
		return err
	}
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, rec.Sprint)
		if s == nil {
			return fmt.Errorf("sprint disappeared")
		}
		brief := struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Reason   string `json:"reason,omitempty"`
			Winner   string `json:"winner,omitempty"`
			Attempts []struct {
				Agent   string `json:"agent"`
				Run     string `json:"run"`
				Digest  string `json:"digest"`
				Verdict string `json:"verdict"`
				Tamper  bool   `json:"tamper"`
			} `json:"attempts"`
			Shadows []struct {
				Agent   string `json:"agent"`
				Run     string `json:"run"`
				Digest  string `json:"digest"`
				Verdict string `json:"verdict"`
				Tamper  bool   `json:"tamper"`
			} `json:"shadows,omitempty"`
		}{ID: rec.ID, Status: rec.Status, Reason: rec.Reason, Winner: rec.Winner}
		for _, a := range rec.Attempts {
			entry := struct {
				Agent   string `json:"agent"`
				Run     string `json:"run"`
				Digest  string `json:"digest"`
				Verdict string `json:"verdict"`
				Tamper  bool   `json:"tamper"`
			}{a.Agent, a.Run, a.Digest, a.Verdict, a.Tamper}
			if a.Shadow {
				brief.Shadows = append(brief.Shadows, entry)
			} else {
				brief.Attempts = append(brief.Attempts, entry)
			}
		}
		summary, err := json.Marshal(brief)
		if err != nil {
			return err
		}
		weaveStoryAppend(s, weaveConductorName(""), "heat", string(summary))
		return nil
	}); err != nil {
		return err
	}
	if store != nil {
		now := time.Now().UTC()
		for _, a := range rec.Attempts {
			ev := heatDeliveryEvent(rec, a, now)
			if err := store.Append(ev); err != nil {
				return err
			}
		}
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "heat %s %s winner=%s reason=%s\n", rec.ID, rec.Status, rec.Winner, rec.Reason)
	return err
}

func heatDeliveryEvent(rec heatRecord, a heatAttempt, now time.Time) ladder.Event {
	agent := a.CanonicalAgent
	if agent == "" {
		if binding, ok := fleetCatalog().Agent(a.Agent); ok {
			agent = binding.MatrixKey()
		} else {
			agent = a.Agent
		}
	}
	ev := ladder.Event{Kind: ladder.EventKindDelivery, Agent: agent, Duty: ladder.DutyCode, Points: ladder.Points(a.Fairness.Points), At: now, Season: ladder.SeasonOf(now), Sprint: int(rec.Sprint), Story: rec.Story, Note: "heat:" + rec.ID, Reviewer: weaveConductorName("")}
	if a.InstanceUUID != "" {
		ev.InstanceUUID, ev.FamilyID, ev.SelectedBinding, ev.SeedBand = a.InstanceUUID, a.FamilyID, agent, a.SeedBand
	}
	ev.ID = "heat:" + rec.ID + ":run:" + a.Run
	if a.Shadow {
		ev.Note = "shadow heat:" + rec.ID
	}
	if a.Verdict == "pass" {
		ev.Outcome = 1
		if a.Fairness.MaxRuntime > 0 && a.Wall > a.Fairness.MaxRuntime {
			ev.Outcome = 0.5
		}
	} else {
		class, kind, note := blame.ClassAgent, blame.EvidenceGate, a.Verdict
		if a.PreflightFailed {
			class, note = blame.ClassEnvironment, a.Error
		}
		ev.Blame = blame.Attribution{Class: class, Evidence: []blame.Evidence{{Kind: kind, Ref: "heat:" + rec.ID + "/" + a.Run, Note: note}}, By: ev.Reviewer, At: now}
	}
	ev.CapsUsed.Turns = a.Turns
	ev.CapsUsed.WallSeconds = int(a.Wall.Seconds())
	return ev
}
