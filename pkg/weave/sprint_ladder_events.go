package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

func sprintLadderFlags(cmd *cobra.Command) {
	cmd.Flags().String("agent", "", "delivery agent (tool:model); overrides the linked run")
	cmd.Flags().Int("points", 0, "delivery points (1, 2, 3, 5 or 8); overrides the linked run")
}

// sprintLadderDelivery joins only a recorded story/run relation, never an
// instance nickname. Use the run ledger's canonical identity resolver so
// registered clones share their base agent's tool:model binding.
func sprintLadderDelivery(cmd *cobra.Command, s *weaveStory, story *issue.Issue, actor, root string, now time.Time) (*ladder.Event, error) {
	agent, _ := cmd.Flags().GetString("agent")
	points, _ := cmd.Flags().GetInt("points")
	var wall time.Duration
	var turns int
	var matched *weaveItem
	for _, link := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(link)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		run := findWeaveItem(q, link.ID)
		if run == nil || (!link.Born.IsZero() && !link.Born.Equal(run.Created)) {
			continue
		}
		if run.Register != story.ID && !(run.Register == "" && story.Weave == run.ID && filepath.Clean(q.Root) == filepath.Clean(root)) {
			continue
		}
		if matched != nil {
			return nil, fmt.Errorf("multiple linked attempts for story %s; resolve the sprint run links before rating", story.ID)
		}
		matched = run
	}
	if matched != nil {
		if matched.LogPath != "" {
			if data, err := os.ReadFile(matched.LogPath); err == nil {
				turns = int(weaveResultTurns(string(data)))
				if turns < 0 {
					turns = 0
				}
			}
		}
		if agent == "" {
			var ok bool
			agent, ok = weaveCapabilityAgent(matched)
			if !ok {
				return nil, fmt.Errorf("no canonical agent identity for the linked run; pass --agent tool:model")
			}
		}
		if !cmd.Flags().Changed("points") {
			points = matched.Points
		}
		if !matched.StartedAt.IsZero() {
			end := matched.FinishedAt
			if end.IsZero() {
				end = now
			}
			wall = end.Sub(matched.StartedAt)
			if wall < 0 {
				return nil, fmt.Errorf("linked run has a negative duration")
			}
		}
	}
	if agent == "" {
		if story.Weave != 0 {
			return nil, fmt.Errorf("no canonical agent identity for the linked run; pass --agent tool:model")
		}
		return nil, fmt.Errorf("story has no linked run; pass --agent and --points to rate")
	}
	if !ladder.ValidPoints(ladder.Points(points)) {
		return nil, fmt.Errorf("delivery needs valid --points (1, 2, 3, 5 or 8)")
	}
	ev := &ladder.Event{Kind: ladder.EventKindDelivery, Agent: strings.TrimSpace(agent), Duty: ladder.DutyCode, Points: ladder.Points(points), At: now, Season: ladder.SeasonOf(now), Sprint: int(s.ID), Story: story.ID, Reviewer: actor}
	if matched != nil {
		ev.ID = fmt.Sprintf("sprint:%d:story:%s:run:%d:%d", s.ID, story.ID, matched.ID, matched.Created.UnixNano())
	}
	ev.CapsUsed.WallSeconds = int(wall.Seconds())
	ev.CapsUsed.Turns = turns
	rework, _ := cmd.Flags().GetInt("rework")
	ev.Outcome = ladder.OutcomeScore(ladder.ClassifyDelivery(ev.Points, turns, wall, true, rework, false))
	return ev, nil
}

func sprintLadderAppend(cmd *cobra.Command, ev *ladder.Event) {
	if ev == nil {
		return
	}
	seed := 0
	if _, modelName, ok := strings.Cut(ev.Agent, ":"); ok {
		if model, found := fleetCatalog().Model(modelName); found {
			seed = model.Band
			if ev.ModelVersion == "" {
				ev.ModelVersion = model.Version
			}
		}
	}
	store, err := ladder.OpenStore(ladder.DefaultStorePath())
	var events []ladder.Event
	if err == nil {
		events, err = store.Read()
	}
	key := ev.RatingAgent()
	if ev.FamilyID != "" {
		seed = ev.SeedBand
	}
	before := ladder.CurrentBand(seed, events, key)
	if err == nil {
		err = store.Append(*ev)
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "ladder: delivery not recorded: %v\n", err)
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ladder: delivery recorded (%s, %d, %g)\n", ev.Agent, ev.Points, ev.Outcome)
	// Re-read after append: an identical retry is successful but must not be
	// locally appended a second time for band/audit calculation.
	afterEvents, readErr := store.Read()
	if readErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "ladder: delivery recorded but band unavailable: %v\n", readErr)
		return
	}
	after := ladder.CurrentBand(seed, afterEvents, key)
	if after.Band == before.Band || len(after.Moves) == 0 {
		return
	}
	move := after.Moves[len(after.Moves)-1]
	audit := ladder.Event{Kind: ladder.EventKindBand, Agent: ev.Agent, InstanceUUID: ev.InstanceUUID, FamilyID: ev.FamilyID, SelectedBinding: ev.SelectedBinding, SeedBand: ev.SeedBand, At: ev.At, Season: ev.Season, FromBand: move.From, ToBand: move.To, Note: move.Reason}
	if ev.ID != "" {
		audit.ID = ev.ID + ":band"
	}
	if err := store.Append(audit); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "ladder: band move not recorded: %v\n", err)
		return
	}
	verb, run := "promoted", "successes"
	if move.Reason == "relegate" {
		verb, run = "relegated", "failures"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ladder: %s %s L%d -> L%d (5 consecutive %s)\n", ev.Agent, verb, move.From, move.To, run)
}

func newSprintFailCmd() *cobra.Command {
	var flags weaveOutputFlags
	var repo, note, class string
	var evidence []string
	var falseDone bool
	cmd := &cobra.Command{Use: "fail <sprint> <story>", Short: "Record a failed delivery and return its story to todo", Args: cobra.ExactArgs(2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := sprintArg(cmd, flags.mode(), "sprint fail", args[0])
		if err != nil {
			return err
		}
		actor := weaveConductorName("")
		now := time.Now().UTC()
		attribution := blame.Attribution{By: actor, At: now}
		attribution.Class, err = blame.ParseClass(class)
		if err != nil {
			return err
		}
		for _, raw := range evidence {
			parts := strings.SplitN(raw, ":", 3)
			if len(parts) < 2 {
				return fmt.Errorf("evidence must be KIND:REF[:NOTE]")
			}
			ev := blame.Evidence{Kind: strings.TrimSpace(parts[0]), Ref: strings.TrimSpace(parts[1])}
			if len(parts) == 3 {
				ev.Note = strings.TrimSpace(parts[2])
			}
			attribution.Evidence = append(attribution.Evidence, ev)
		}
		if err = attribution.Validate(); err != nil {
			return err
		}
		if attribution.Class == blame.ClassUnclassified {
			attribution = blame.Attribution{}
		}
		var event *ladder.Event
		err = runWeaveStoryMutate(cmd, id, "sprint fail", &flags, func(s *weaveStory) (string, error) {
			if s.Lease == nil || !strings.EqualFold(s.Lease.Holder, actor) {
				return "", fmt.Errorf("only sprint #%d's current manager may fail stories", id)
			}
			root, it, err := resolveSprintStoryFor(s, repo, args[1])
			if err != nil {
				return "", err
			}
			if todopkg.IsClosed(it.Status) {
				return "", fmt.Errorf("story %s is already closed", it.ID)
			}
			if strings.TrimSpace(it.Assignee) == "" {
				return "", fmt.Errorf("story %s is not claimed by anyone", it.ID)
			}
			event, err = sprintLadderDelivery(cmd, s, it, actor, root, now)
			if err != nil {
				return "", err
			}
			if event == nil {
				return "", fmt.Errorf("failure delivery needs a linked run or --agent and --points")
			}
			if attribution.Class == blame.ClassUnclassified {
				attribution = sprintEnvironmentBlameFromRunEvidence(s, it, root, actor, now)
			}
			event.Outcome = 0
			event.Blame = attribution
			event.Note = strings.TrimSpace(note)
			if falseDone {
				event.Note = strings.TrimSpace("false-done: " + event.Note)
			}
			it.Assignee = ""
			it.Status = todopkg.StatusTodo
			it.Closed = nil
			it.ClosedBy = ""
			if _, err = todopkg.RepoStore(root).Save(it); err != nil {
				return "", err
			}
			body := fmt.Sprintf("%s failed story %s", actor, shortSprintStoryID(it.ID))
			if event.Note != "" {
				body += ": " + event.Note
			}
			weaveStoryAppend(s, actor, "fail", body)
			return fmt.Sprintf("sprint #%d: failed story %s — it is open again", id, shortSprintStoryID(it.ID)), nil
		})
		if err != nil {
			return err
		}
		sprintLadderAppend(cmd, event)
		if attribution.Class == blame.ClassUnclassified {
			fmt.Fprintln(cmd.OutOrStdout(), "ladder: unrated (unclassified failure)")
		}
		if attribution.Class == blame.ClassEnvironment {
			fmt.Fprintln(cmd.OutOrStdout(), "open a fix item: bashy todo add \"Fix the delivery environment\"")
		}
		return nil
	}
	sprintLadderFlags(cmd)
	cmd.Flags().StringVar(&repo, "repo", "", "repo root holding the story")
	cmd.Flags().StringVar(&class, "blame", "", "failure class: agent, environment or spec")
	cmd.Flags().StringArrayVar(&evidence, "evidence", nil, "evidence KIND:REF[:NOTE] (repeatable)")
	cmd.Flags().BoolVar(&falseDone, "false-done", false, "record a false completion claim")
	cmd.Flags().StringVarP(&note, "message", "m", "", "manager continuity note")
	flags.attach(cmd)
	return cmd
}

func sprintEnvironmentBlameFromRunEvidence(s *weaveStory, story *issue.Issue, root, actor string, now time.Time) blame.Attribution {
	run := sprintLinkedRunForStory(s, story, root)
	if run == nil || !weaveOutputContainsENOSPC(sprintRunFailureEvidence(run)) {
		return blame.Attribution{}
	}
	return blame.Attribution{
		Class: blame.ClassEnvironment,
		By:    actor,
		At:    now,
		Evidence: []blame.Evidence{{
			Kind: blame.EvidenceHost,
			Ref:  fmt.Sprintf("weave-run-%d", run.ID),
			Note: "captured gate output contains ENOSPC/no space left on device",
		}},
	}
}

func sprintLinkedRunForStory(s *weaveStory, story *issue.Issue, root string) *weaveItem {
	if s == nil || story == nil {
		return nil
	}
	for _, link := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(link)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		run := findWeaveItem(q, link.ID)
		if run == nil || (!link.Born.IsZero() && !link.Born.Equal(run.Created)) {
			continue
		}
		if run.Register == story.ID || (run.Register == "" && story.Weave == run.ID && filepath.Clean(q.Root) == filepath.Clean(root)) {
			return run
		}
	}
	return nil
}

func sprintRunFailureEvidence(run *weaveItem) string {
	if run == nil {
		return ""
	}
	var parts []string
	parts = append(parts, run.VerifyOutput, run.SuiteGateOutput, run.Completion, run.KilledBy)
	if run.LogPath != "" {
		parts = append(parts, weaveReadThrottleLogTail(run.LogPath))
	}
	return strings.Join(parts, "\n")
}

func weaveOutputContainsENOSPC(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "enospc") || strings.Contains(s, "no space left on device")
}
