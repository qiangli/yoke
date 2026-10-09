package weave

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

// A reassign entry is the durable attempt chain. Run numbers are local to a
// queue, so the sprint's run links remain the authority for resolving them.
type sprintReassignEvent struct {
	Story               string `json:"story"`
	Repo                string `json:"repo,omitempty"`
	FromRun             int64  `json:"from_run"`
	FromAgent           string `json:"from_agent"`
	FromInstanceUUID    string `json:"from_instance_uuid,omitempty"`
	FromFamilyID        string `json:"from_family_id,omitempty"`
	FromSelectedBinding string `json:"from_selected_binding,omitempty"`
	FromSeedBand        int    `json:"from_seed_band,omitempty"`
	ToRun               int64  `json:"to_run"`
	ToAgent             string `json:"to_agent"`
	ToInstanceUUID      string `json:"to_instance_uuid,omitempty"`
	ToFamilyID          string `json:"to_family_id,omitempty"`
	ToSelectedBinding   string `json:"to_selected_binding,omitempty"`
	ToSeedBand          int    `json:"to_seed_band,omitempty"`
	Cap                 string `json:"cap"`
	Points              int    `json:"points"`
	WantedBand          int    `json:"wanted_band,omitempty"`
	Band                int    `json:"band,omitempty"`
	Environment         bool   `json:"environment,omitempty"`
	Evidence            string `json:"evidence,omitempty"`
}

type sprintReassignDeps struct {
	kill   func() error
	seed   func() (int64, error)
	launch func(string, int64, time.Duration) error
	record func(sprintReassignEvent) error
}

var sprintReassignLaunchRun = sprintReassignLaunch
var sprintReassignKillRun = sprintReassignKill
var sprintReassignSeatPool = seatPool

func sprintReassignDispatch(cmd *cobra.Command, run *weaveItem, from string, attempted map[string]bool, pool []ladder.Entrant, dry, environment bool, now time.Time, deps sprintReassignDeps) error {
	cap, ok := ladder.CapFor(ladder.Points(run.Points))
	if !ok {
		return fmt.Errorf("run #%d has invalid points %d", run.ID, run.Points)
	}
	if !sprintReassignTimedOut(run, cap.Wall, now) {
		return fmt.Errorf("run #%d has not reached its runtime cap", run.ID)
	}
	band := run.Band
	if band <= 0 {
		band = 3
	}
	remaining := make([]ladder.Entrant, 0, len(pool))
	for _, e := range pool {
		prior := attempted[e.Agent]
		if binding, _, _, err := fleetCatalog().Binding(e.Agent); err == nil {
			prior = prior || attempted[binding.MatrixKey()]
		}
		if !prior && e.Band <= band {
			remaining = append(remaining, e)
		}
	}
	chosen := seatPanel(remaining, 1, "")
	if len(chosen) == 0 {
		return fmt.Errorf("capacity wait: no untried agent available for story %s", run.Register)
	}
	to := chosen[0].Agent
	fmt.Fprintf(cmd.OutOrStdout(), "reassign story=%s from=%d/%s to=%s cap=%s environment=%t\n", run.Register, run.ID, from, to, cap.Wall, environment)
	if dry {
		return nil
	}
	if run.State == "working" {
		if err := deps.kill(); err != nil {
			return err
		}
	}
	id, err := deps.seed()
	if err != nil {
		return err
	}
	e := sprintReassignEvent{Story: run.Register, FromRun: run.ID, FromAgent: from, ToRun: id, ToAgent: to, Cap: cap.Wall.String(), Points: run.Points, WantedBand: band, Band: chosen[0].Band, Environment: environment}
	if run.Instance != "" {
		e.FromInstanceUUID, e.FromFamilyID, e.FromSelectedBinding, e.FromSeedBand = run.Instance, run.InstanceFamily, from, run.Band
	}
	if err := deps.record(e); err != nil {
		return err
	}
	return deps.launch(to, id, cap.Wall)
}

func sprintReassignTimedOut(run *weaveItem, wall time.Duration, now time.Time) bool {
	if run == nil {
		return false
	}
	if run.State == "working" {
		return !run.StartedAt.IsZero() && now.Sub(run.StartedAt) >= wall
	}
	reason := strings.ToLower(run.KilledBy + " " + run.Completion)
	return (run.State == "killed" || run.State == "failed") && (strings.Contains(reason, "max-runtime") || strings.Contains(reason, "wall-clock") || strings.Contains(reason, "wall clock"))
}

func sprintReassignEnvironment(run *weaveItem) (bool, string) {
	evidence := strings.ToLower(sprintRunFailureEvidence(run))
	for _, marker := range []string{"quota", "rate limit", "too many requests", "authentication", "unauthorized", "permission denied", "network unreachable", "connection refused", "connection reset", "sandbox", "enospc", "no space left on device"} {
		if strings.Contains(evidence, marker) {
			return true, marker
		}
	}
	return false, ""
}

func sprintReassignChain(s *weaveStory, story string) []sprintReassignEvent {
	var chain []sprintReassignEvent
	for _, row := range s.Thread {
		if row.Kind != "reassign" {
			continue
		}
		var e sprintReassignEvent
		if json.Unmarshal([]byte(row.Body), &e) == nil && e.Story == story {
			chain = append(chain, e)
		}
	}
	return chain
}

func sprintReassignStorySplit(s *weaveStory, story *issue.Issue) bool {
	for _, label := range story.Labels {
		if label == "split-needed" {
			return true
		}
	}
	for _, row := range s.Thread {
		if row.Kind == "split-needed" && row.Body == story.ID {
			return true
		}
	}
	return false
}

func sprintReassignAttempted(s *weaveStory, repo, story string) (map[string]bool, error) {
	attempted := map[string]bool{}
	for _, link := range s.Runs {
		if link.Repo != repo {
			continue
		}
		dir, err := weaveQueueDirForSprintRun(link)
		if err != nil {
			return nil, err
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return nil, err
		}
		run := findWeaveItem(q, link.ID)
		if run == nil || (!link.Born.IsZero() && !link.Born.Equal(run.Created)) {
			return nil, fmt.Errorf("linked run %s#%d changed generation", link.Repo, link.ID)
		}
		if run.Register != story {
			continue
		}
		attempted[run.Owner] = true
		if agent, ok := weaveCapabilityAgent(run); ok {
			attempted[agent] = true
		}
		if binding, _, _, err := fleetCatalog().Binding(run.Owner); err == nil {
			attempted[binding.MatrixKey()] = true
		}
	}
	for _, e := range sprintReassignChain(s, story) {
		attempted[e.FromAgent], attempted[e.ToAgent] = true, true
	}
	return attempted, nil
}

func sprintReassignOutcomes(chain []sprintReassignEvent, accepted bool, lastBlame blame.Attribution, now time.Time) []ladder.Event {
	if len(chain) == 0 {
		return nil
	}
	first := chain[0]
	makeEvent := func(id, agent, instance, family, selected string, seed, points int, outcome float64, attribution blame.Attribution) ladder.Event {
		return ladder.Event{ID: id, Kind: ladder.EventKindDelivery, Agent: agent, InstanceUUID: instance, FamilyID: family, SelectedBinding: selected, SeedBand: seed, Duty: ladder.DutyCode, Story: first.Story, Points: ladder.Points(points), Outcome: outcome, Blame: attribution, At: now, Season: ladder.SeasonOf(now), Note: "reassign:" + first.Story}
	}
	var events []ladder.Event
	for _, e := range chain {
		a := blame.Attribution{Class: blame.ClassAgent, By: weaveConductorName(""), At: now, Evidence: []blame.Evidence{{Kind: blame.EvidenceGate, Ref: fmt.Sprintf("timeout:%d", e.FromRun)}}}
		if e.Environment {
			a = blame.Attribution{Class: blame.ClassEnvironment, By: weaveConductorName(""), At: now, Evidence: []blame.Evidence{{Kind: blame.EvidenceHost, Ref: fmt.Sprintf("timeout:%d", e.FromRun), Note: e.Evidence}}}
		}
		events = append(events, makeEvent(fmt.Sprintf("reassign:%s:run:%d", first.Story, e.FromRun), e.FromAgent, e.FromInstanceUUID, e.FromFamilyID, e.FromSelectedBinding, e.FromSeedBand, e.Points, 0, a))
	}
	last := chain[len(chain)-1]
	outcome := 0.0
	if accepted {
		outcome = 1
		lastBlame = blame.Attribution{}
	}
	events = append(events, makeEvent(fmt.Sprintf("reassign:%s:run:%d", first.Story, last.ToRun), last.ToAgent, last.ToInstanceUUID, last.ToFamilyID, last.ToSelectedBinding, last.ToSeedBand, last.Points, outcome, lastBlame))
	return events
}

func newSprintReassignCmd() *cobra.Command {
	var runRef, class string
	var dry bool
	cmd := &cobra.Command{Use: "reassign <sprint>", Short: "Reassign a story whose run reached its runtime cap", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&runRef, "run", "", "linked run REPO#ID (default: all eligible runs)")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print the plan without stopping or launching runs")
	cmd.Flags().StringVar(&class, "blame", "", "override timeout blame: environment")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := sprintArg(cmd, (&weaveOutputFlags{}).mode(), "sprint reassign", args[0])
		if err != nil {
			return err
		}
		if class != "" && class != "environment" {
			return fmt.Errorf("--blame must be environment")
		}
		board, err := sprintStoreDir()
		if err != nil {
			return err
		}
		q, err := loadWeaveQueue(board)
		if err != nil {
			return err
		}
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		if !dry {
			if err := authorizeSprintLeaseToken(cmd, s, "reassign"); err != nil {
				return err
			}
		}
		selected := 0
		for _, link := range s.Runs {
			if runRef != "" && runRef != fmt.Sprintf("%s#%d", link.Repo, link.ID) {
				continue
			}
			queueDir, err := weaveQueueDirForSprintRun(link)
			if err != nil {
				return err
			}
			runs, err := loadWeaveQueue(queueDir)
			if err != nil {
				return err
			}
			run := findWeaveItem(runs, link.ID)
			if run == nil || (!link.Born.IsZero() && !link.Born.Equal(run.Created)) {
				return fmt.Errorf("linked run %s#%d changed generation", link.Repo, link.ID)
			}
			cap, ok := ladder.CapFor(ladder.Points(run.Points))
			if !ok || !sprintReassignTimedOut(run, cap.Wall, time.Now()) {
				continue
			}
			chain := sprintReassignChain(s, run.Register)
			if len(chain) > 0 && chain[len(chain)-1].ToRun != run.ID {
				continue
			}
			root := runs.Root
			story, err := todopkg.RepoStore(root).Resolve(run.Register)
			if err != nil {
				return err
			}
			if sprintReassignStorySplit(s, story) {
				continue
			}
			if len(chain) >= 1 {
				if dry {
					fmt.Fprintf(cmd.OutOrStdout(), "split-needed story=%s after two timed-out attempts\n", run.Register)
					selected++
					continue
				}
				if run.State == "working" {
					if err := sprintReassignKillRun(cmd, root, run.ID); err != nil {
						return err
					}
				}
				if err := sprintReassignSplit(cmd, id, root, story); err != nil {
					return err
				}
				lastBlame := sprintReassignTimeoutBlame(run.ID, weaveConductorName(""), time.Now())
				if env, marker := sprintReassignEnvironment(run); env || class == "environment" {
					if class == "environment" {
						marker = "manager override"
					}
					lastBlame = blame.Attribution{Class: blame.ClassEnvironment, By: weaveConductorName(""), At: time.Now(), Evidence: []blame.Evidence{{Kind: blame.EvidenceHost, Ref: fmt.Sprintf("timeout:%d", run.ID), Note: marker}}}
				}
				for _, ev := range sprintReassignOutcomes(chain, false, lastBlame, time.Now()) {
					if ev.Blame.Class == blame.ClassEnvironment && ev.Agent != chain[len(chain)-1].ToAgent {
						continue
					}
					ev.Sprint = int(id)
					ev.Reviewer = weaveConductorName("")
					sprintLadderAppend(cmd, &ev)
				}
				selected++
				continue
			}
			from, ok := weaveCapabilityAgent(run)
			if !ok {
				if binding, _, _, err := fleetCatalog().Binding(run.Owner); err == nil {
					from = binding.MatrixKey()
				}
			}
			if strings.TrimSpace(from) == "" {
				return fmt.Errorf("run %s#%d has no agent identity", link.Repo, run.ID)
			}
			attempted, err := sprintReassignAttempted(s, link.Repo, run.Register)
			if err != nil {
				return err
			}
			attempted[run.Owner], attempted[from] = true, true
			events, err := sprintAssignReadEvents()
			if err != nil {
				return err
			}
			pool, _, err := sprintReassignSeatPool(root, events, time.Now())
			if err != nil {
				return err
			}
			environment, marker := sprintReassignEnvironment(run)
			if class == "environment" {
				environment, marker = true, "manager override"
			}
			err = sprintReassignDispatch(cmd, run, from, attempted, pool, dry, environment, time.Now(), sprintReassignDeps{
				kill: func() error { return sprintReassignKillRun(cmd, root, run.ID) },
				seed: func() (int64, error) {
					story.Weave = 0
					newID, err := sprintAssignSeed(cmd, id, root, story, run.Points, run.Band)
					if err != nil {
						return 0, err
					}
					if err := withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
						fresh := findWeaveItem(q, newID)
						if fresh == nil {
							return fmt.Errorf("seeded run #%d not found", newID)
						}
						fresh.Body = run.Body
						return nil
					}); err != nil {
						return newID, err
					}
					return newID, nil
				},
				record: func(e sprintReassignEvent) error {
					e.Repo = link.Repo
					if binding, _, _, err := fleetCatalog().Binding(e.ToAgent); err == nil {
						e.ToAgent = binding.MatrixKey()
					}
					e.Evidence = marker
					if err := sprintReassignRecord(cmd, id, e); err != nil {
						return err
					}
					if e.Band < e.WantedBand {
						if err := sprintAssignRecord(cmd, id, "fallback", sprintAssignEvent{Story: e.Story, Agent: e.ToAgent, ChosenAgent: e.ToAgent, Seat: "worker", WantedBand: e.WantedBand, Band: e.Band}); err != nil {
							return err
						}
					}
					if environment {
						ev := sprintReassignOutcomes([]sprintReassignEvent{e}, false, blame.Attribution{}, time.Now())[0]
						ev.Sprint = int(id)
						ev.Reviewer = weaveConductorName("")
						sprintLadderAppend(cmd, &ev)
					}
					return nil
				},
				launch: func(agent string, runID int64, wall time.Duration) error {
					story.Assignee = agent
					if _, err := todopkg.RepoStore(root).Save(story); err != nil {
						return err
					}
					return sprintReassignLaunchRun(cmd, root, agent, runID, wall)
				},
			})
			if err != nil {
				return err
			}
			selected++
		}
		if runRef != "" && selected == 0 {
			return fmt.Errorf("run %s is not a linked run at its cap", runRef)
		}
		return nil
	}
	return cmd
}

func sprintReassignRecord(cmd *cobra.Command, sprint int64, e sprintReassignEvent) error {
	board, err := sprintStoreDir()
	if err != nil {
		return err
	}
	return withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, sprint)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", sprint)
		}
		if err := authorizeSprintLeaseToken(cmd, s, "reassign"); err != nil {
			return err
		}
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		weaveStoryAppend(s, weaveConductorName(""), "reassign", string(b))
		return nil
	})
}

func sprintReassignKill(cmd *cobra.Command, root string, id int64) error {
	sprintAssignLaunchMu.Lock()
	defer sprintAssignLaunchMu.Unlock()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	defer os.Chdir(cwd)
	return runWeaveKill(cmd, id, "max-runtime", true, &weaveOutputFlags{})
}

func sprintReassignLaunch(cmd *cobra.Command, root, agent string, id int64, cap time.Duration) error {
	sprintAssignLaunchMu.Lock()
	defer sprintAssignLaunchMu.Unlock()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	defer os.Chdir(cwd)
	old, had := os.LookupEnv("BASHY_AGENT")
	if err := os.Setenv("BASHY_AGENT", agent); err != nil {
		return err
	}
	defer func() {
		if had {
			_ = os.Setenv("BASHY_AGENT", old)
		} else {
			_ = os.Unsetenv("BASHY_AGENT")
		}
	}()
	return runWeaveStart(cmd, id, "", []string{agent}, weaveStartOptions{pty: "never", maxRuntime: cap}, &weaveOutputFlags{})
}

func sprintReassignTimeoutBlame(run int64, actor string, now time.Time) blame.Attribution {
	return blame.Attribution{Class: blame.ClassAgent, By: actor, At: now, Evidence: []blame.Evidence{{Kind: blame.EvidenceGate, Ref: "timeout:" + strconv.FormatInt(run, 10)}}}
}

func sprintReassignSplit(cmd *cobra.Command, sprint int64, root string, story *issue.Issue) error {
	board, err := sprintStoreDir()
	if err != nil {
		return err
	}
	q, err := loadWeaveQueue(board)
	if err != nil {
		return err
	}
	current := findWeaveStory(q, sprint)
	if current == nil {
		return fmt.Errorf("sprint #%d not found", sprint)
	}
	if err := authorizeSprintLeaseToken(cmd, current, "reassign"); err != nil {
		return err
	}
	issue.AddLabels(story, []string{"split-needed"})
	story.Assignee, story.Status, story.Closed, story.ClosedBy = "", todopkg.StatusTodo, nil, ""
	if _, err := todopkg.RepoStore(root).Save(story); err != nil {
		return err
	}
	return withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, sprint)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", sprint)
		}
		if err := authorizeSprintLeaseToken(cmd, s, "reassign"); err != nil {
			return err
		}
		weaveStoryAppend(s, weaveConductorName(""), "split-needed", story.ID)
		return nil
	})
}

// sprintReassignLatestRun resolves the final linked attempt without allowing a
// recycled queue id to inherit an older run's identity.
func sprintReassignLatestRun(s *weaveStory, story string) (*weaveItem, string, error) {
	chain := sprintReassignChain(s, story)
	if len(chain) == 0 {
		return nil, "", nil
	}
	last := chain[len(chain)-1]
	for _, link := range s.Runs {
		if link.ID != last.ToRun || (last.Repo != "" && link.Repo != last.Repo) {
			continue
		}
		dir, err := weaveQueueDirForSprintRun(link)
		if err != nil {
			return nil, "", err
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return nil, "", err
		}
		run := findWeaveItem(q, link.ID)
		if run != nil && run.Register == story && (link.Born.IsZero() || link.Born.Equal(run.Created)) {
			return run, q.Root, nil
		}
	}
	return nil, "", fmt.Errorf("latest reassign run #%d not found", last.ToRun)
}

func sprintReassignDelivery(s *weaveStory, story string, accepted bool, lastBlame blame.Attribution, now time.Time) ([]ladder.Event, error) {
	chain := sprintReassignChain(s, story)
	if len(chain) == 0 {
		return nil, nil
	}
	last, _, err := sprintReassignLatestRun(s, story)
	if err != nil {
		return nil, err
	}
	// Re-resolve every attempt from the immutable run links. The reassign row
	// is written before the replacement launches, so its ToRun cannot carry
	// instance evidence until delivery time.
	for i := range chain {
		if run := sprintReassignLinkedRun(s, chain[i].Repo, story, chain[i].FromRun); run != nil && run.Instance != "" {
			selected := chain[i].FromAgent
			if actual, ok := weaveCapabilityAgent(run); ok {
				selected = actual
			}
			chain[i].FromInstanceUUID, chain[i].FromFamilyID, chain[i].FromSelectedBinding, chain[i].FromSeedBand = run.Instance, run.InstanceFamily, selected, run.Band
		}
		if run := sprintReassignLinkedRun(s, chain[i].Repo, story, chain[i].ToRun); run != nil && run.Instance != "" {
			selected := chain[i].ToAgent
			if actual, ok := weaveCapabilityAgent(run); ok {
				selected = actual
			}
			chain[i].ToInstanceUUID, chain[i].ToFamilyID, chain[i].ToSelectedBinding, chain[i].ToSeedBand = run.Instance, run.InstanceFamily, selected, run.Band
		}
	}
	events := sprintReassignOutcomes(chain, accepted, lastBlame, now)
	for i := range events {
		events[i].Sprint = int(s.ID)
		events[i].Reviewer = weaveConductorName("")
		if i == len(events)-1 && accepted && !last.StartedAt.IsZero() {
			end := last.FinishedAt
			if end.IsZero() {
				end = now
			}
			events[i].CapsUsed.WallSeconds = int(end.Sub(last.StartedAt).Seconds())
			if last.LogPath != "" {
				if data, err := os.ReadFile(last.LogPath); err == nil {
					turns := int(weaveResultTurns(string(data)))
					if turns > 0 {
						events[i].CapsUsed.Turns = turns
					}
				}
			}
		}
	}
	return events, nil
}

func sprintReassignLinkedRun(s *weaveStory, repo, story string, id int64) *weaveItem {
	for _, link := range s.Runs {
		if link.ID != id || (repo != "" && link.Repo != repo) {
			continue
		}
		dir, err := weaveQueueDirForSprintRun(link)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		run := findWeaveItem(q, link.ID)
		if run != nil && run.Register == story && (link.Born.IsZero() || link.Born.Equal(run.Created)) {
			return run
		}
	}
	return nil
}

func sprintReassignResolve(cmd *cobra.Command, sprint int64, repo, ref string) (*weaveStory, string, *issue.Issue, error) {
	board, err := sprintStoreDir()
	if err != nil {
		return nil, "", nil, err
	}
	q, err := loadWeaveQueue(board)
	if err != nil {
		return nil, "", nil, err
	}
	s := findWeaveStory(q, sprint)
	if s == nil {
		return nil, "", nil, fmt.Errorf("sprint #%d not found", sprint)
	}
	root, story, err := resolveSprintStoryFor(s, repo, ref)
	return s, root, story, err
}

func sprintReassignAcceptHook(cmd *cobra.Command, original func(*cobra.Command, []string) error, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return original(cmd, args)
	}
	repo, _ := cmd.Flags().GetString("repo")
	s, _, story, err := sprintReassignResolve(cmd, id, repo, args[1])
	if err != nil || len(sprintReassignChain(s, story.ID)) == 0 {
		return original(cmd, args)
	}
	skip, _ := cmd.Flags().GetBool("no-rating")
	if skip {
		return original(cmd, args)
	}
	_ = cmd.Flags().Set("no-rating", "true")
	defer cmd.Flags().Set("no-rating", "false")
	out := cmd.OutOrStdout()
	var captured bytes.Buffer
	cmd.SetOut(&captured)
	err = original(cmd, args)
	cmd.SetOut(out)
	_, _ = fmt.Fprint(out, strings.ReplaceAll(captured.String(), "ladder: unrated by manager\n", ""))
	if err != nil {
		return err
	}
	s, _, story, err = sprintReassignResolve(cmd, id, repo, args[1])
	if err != nil {
		return err
	}
	events, err := sprintReassignDelivery(s, story.ID, true, blame.Attribution{}, time.Now().UTC())
	if err != nil {
		return err
	}
	for i := range events {
		if i < len(events)-1 && events[i].Blame.Class == blame.ClassEnvironment {
			continue
		}
		sprintLadderAppend(cmd, &events[i])
	}
	return nil
}

func sprintReassignFailHook(cmd *cobra.Command, original func(*cobra.Command, []string) error, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return original(cmd, args)
	}
	repo, _ := cmd.Flags().GetString("repo")
	s, root, story, err := sprintReassignResolve(cmd, id, repo, args[1])
	if err != nil || len(sprintReassignChain(s, story.ID)) == 0 {
		return original(cmd, args)
	}
	actor, now := weaveConductorName(""), time.Now().UTC()
	class, _ := cmd.Flags().GetString("blame")
	parsed, err := blame.ParseClass(class)
	if err != nil {
		return err
	}
	attribution := blame.Attribution{Class: parsed, By: actor, At: now}
	rawEvidence, _ := cmd.Flags().GetStringArray("evidence")
	for _, raw := range rawEvidence {
		p := strings.SplitN(raw, ":", 3)
		if len(p) < 2 {
			return fmt.Errorf("evidence must be KIND:REF[:NOTE]")
		}
		e := blame.Evidence{Kind: p[0], Ref: p[1]}
		if len(p) == 3 {
			e.Note = p[2]
		}
		attribution.Evidence = append(attribution.Evidence, e)
	}
	if parsed == blame.ClassUnclassified {
		last, _, resolveErr := sprintReassignLatestRun(s, story.ID)
		if resolveErr != nil {
			return resolveErr
		}
		if env, marker := sprintReassignEnvironment(last); env {
			attribution = blame.Attribution{Class: blame.ClassEnvironment, By: actor, At: now, Evidence: []blame.Evidence{{Kind: blame.EvidenceHost, Ref: fmt.Sprintf("failed:%d", last.ID), Note: marker}}}
		} else {
			attribution = blame.Attribution{Class: blame.ClassAgent, By: actor, At: now, Evidence: []blame.Evidence{{Kind: blame.EvidenceGate, Ref: fmt.Sprintf("failed:%d", last.ID)}}}
		}
	} else if err := attribution.Validate(); err != nil {
		return err
	}
	if s.Lease == nil || !strings.EqualFold(s.Lease.Holder, actor) {
		return fmt.Errorf("only sprint #%d's current manager may fail stories", id)
	}
	if todopkg.IsClosed(story.Status) || strings.TrimSpace(story.Assignee) == "" {
		return fmt.Errorf("story %s is closed or unclaimed", story.ID)
	}
	story.Assignee, story.Status, story.Closed, story.ClosedBy = "", todopkg.StatusTodo, nil, ""
	issue.AddLabels(story, []string{"split-needed"})
	if _, err := todopkg.RepoStore(root).Save(story); err != nil {
		return err
	}
	board, err := sprintStoreDir()
	if err != nil {
		return err
	}
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		current := findWeaveStory(q, id)
		if current == nil {
			return fmt.Errorf("sprint not found")
		}
		body := fmt.Sprintf("%s failed story %s", actor, shortSprintStoryID(story.ID))
		if note, _ := cmd.Flags().GetString("message"); strings.TrimSpace(note) != "" {
			body += ": " + strings.TrimSpace(note)
		}
		weaveStoryAppend(current, actor, "fail", body)
		weaveStoryAppend(current, actor, "split-needed", story.ID)
		return nil
	}); err != nil {
		return err
	}
	s, _, story, err = sprintReassignResolve(cmd, id, repo, args[1])
	if err != nil {
		return err
	}
	events, err := sprintReassignDelivery(s, story.ID, false, attribution, now)
	if err != nil {
		return err
	}
	for i := range events {
		if i < len(events)-1 && events[i].Blame.Class == blame.ClassEnvironment {
			continue
		}
		sprintLadderAppend(cmd, &events[i])
	}
	fmt.Fprintf(cmd.OutOrStdout(), "sprint #%d: failed story %s — split needed\n", id, shortSprintStoryID(story.ID))
	if attribution.Class == blame.ClassEnvironment {
		fmt.Fprintln(cmd.OutOrStdout(), "open a fix item: bashy todo add \"Fix the delivery environment\"")
	}
	return nil
}

func sprintReassignInstallHooks(accept, fail *cobra.Command) {
	oldAccept, oldFail := accept.RunE, fail.RunE
	accept.RunE = func(cmd *cobra.Command, args []string) error { return sprintReassignAcceptHook(cmd, oldAccept, args) }
	fail.RunE = func(cmd *cobra.Command, args []string) error { return sprintReassignFailHook(cmd, oldFail, args) }
}
