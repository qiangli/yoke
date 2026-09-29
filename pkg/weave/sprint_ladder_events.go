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
// instance nickname. The launch recipe preserves a clone's underlying model.
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
		if agent == "" && matched.LaunchSpec != nil {
			tool := matched.LaunchSpec.Tool
			if tool == "" {
				tool = matched.Tool
			}
			if tool != "" && matched.LaunchSpec.Model != "" {
				agent = tool + ":" + matched.LaunchSpec.Model
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
		if matched != nil || story.Weave != 0 {
			return nil, fmt.Errorf("cannot resolve linked run tool:model; supply --agent")
		}
		return nil, nil
	}
	if !ladder.ValidPoints(ladder.Points(points)) {
		return nil, fmt.Errorf("delivery needs valid --points (1, 2, 3, 5 or 8)")
	}
	ev := &ladder.Event{Kind: ladder.EventKindDelivery, Agent: strings.TrimSpace(agent), Duty: ladder.DutyCode, Points: ladder.Points(points), At: now, Season: ladder.SeasonOf(now), Sprint: int(s.ID), Story: story.ID, Reviewer: actor}
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
	store, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err == nil {
		err = store.Append(*ev)
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "ladder: delivery not recorded: %v\n", err)
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ladder: delivery recorded (%s, %d, %g)\n", ev.Agent, ev.Points, ev.Outcome)
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
		attribution := blame.Attribution{By: actor, At: time.Now().UTC()}
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
			event, err = sprintLadderDelivery(cmd, s, it, actor, root, time.Now().UTC())
			if err != nil {
				return "", err
			}
			if event == nil {
				return "", fmt.Errorf("failure delivery needs a linked run or --agent and --points")
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
