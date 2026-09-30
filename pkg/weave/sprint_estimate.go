package weave

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

type sprintEstimateEvent struct {
	Story      string   `json:"story"`
	Size       int      `json:"size,omitempty"`
	Estimators []string `json:"estimators,omitempty"`
	Points     int      `json:"points,omitempty"`
	Split      bool     `json:"split,omitempty"`
	Escalate   bool     `json:"escalate,omitempty"`
}

type sprintEstimateEntry struct {
	Agent      string        `json:"agent"`
	First      ladder.Points `json:"first"`
	Confidence float64       `json:"confidence"`
	Revote     ladder.Points `json:"revote,omitempty"`
	Shadow     bool          `json:"shadow,omitempty"`
}

type sprintEstimateRecord struct {
	Story      string                `json:"story"`
	Estimators []string              `json:"estimators"`
	Entries    []sprintEstimateEntry `json:"entries"`
}

func sprintEstimatePath(sprintID int64, story string) (string, error) {
	story = strings.TrimSpace(story)
	if story == "" || filepath.Base(story) != story || story == "." {
		return "", fmt.Errorf("--story must be a simple story id")
	}
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".bashy")
	}
	return filepath.Join(home, "sprint", "estimates", strconv.FormatInt(sprintID, 10), story+".json"), nil
}

func readSprintEstimate(path string) (sprintEstimateRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sprintEstimateRecord{}, err
	}
	var record sprintEstimateRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return sprintEstimateRecord{}, err
	}
	return record, nil
}

func saveSprintEstimate(path string, record sprintEstimateRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func estimateDraw(cmd *cobra.Command, sprintID int64, s *weaveStory, story string, input ladder.EstimateInput) ([]string, error) {
	storePath := ladder.DefaultStorePath()
	var events []ladder.Event
	if _, err := os.Stat(storePath); err == nil {
		store, err := ladder.OpenStore(storePath)
		if err != nil {
			return nil, err
		}
		events, err = store.Read()
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	pool, _, err := seatPool("", events, now)
	if err != nil {
		return nil, err
	}
	authorVendor := ""
	if s.Owner != "" {
		if _, tool, _, err := fleetCatalog().Binding(s.Owner); err == nil {
			authorVendor = tool.Name
		}
	}
	wanted := ladder.EstimatePanelSize(input)
	members := seatPanel(pool, wanted, authorVendor)
	if len(members) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "capacity wait: no agent available")
		return nil, nil
	}
	if len(members) < wanted {
		seatRecordFallback(s, seatFallbackEvent{Seat: "estimate", Story: story, WantedBand: 5, ChosenAgent: members[0].Agent, Band: members[0].Band, WantedSize: wanted, Size: len(members)})
	}
	for _, m := range members {
		if m.Band < 5 {
			seatRecordFallback(s, seatFallbackEvent{Seat: "estimate", Story: story, WantedBand: 5, ChosenAgent: m.Agent, Band: m.Band})
		}
	}
	names := make([]string, len(members))
	for i, member := range members {
		names[i] = member.Agent
	}
	return names, nil
}

func newSprintEstimateCmd() *cobra.Command {
	var story, agent, shadow string
	var expected, points int
	var confidence float64
	var crossRepo, design bool
	cmd := &cobra.Command{Use: "estimate <sprint> <open|submit|close>", Short: "Record blind L5 story estimates", Args: cobra.ExactArgs(2)}
	cmd.Flags().StringVar(&story, "story", "", "story id")
	cmd.Flags().StringVar(&agent, "agent", "", "estimating agent")
	cmd.Flags().StringVar(&shadow, "shadow", "", "shadow agent (excluded from settlement)")
	cmd.Flags().IntVar(&expected, "expected", 0, "expected points")
	cmd.Flags().IntVar(&points, "points", 0, "blind estimate points")
	cmd.Flags().Float64Var(&confidence, "confidence", 0, "estimate confidence (0..1)")
	cmd.Flags().BoolVar(&crossRepo, "cross-repo", false, "story spans repositories")
	cmd.Flags().BoolVar(&design, "design", false, "story requires design work")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := sprintArg(cmd, (&weaveOutputFlags{}).mode(), "sprint estimate", args[0])
		if err != nil {
			return err
		}
		story = strings.TrimSpace(story)
		path, err := sprintEstimatePath(id, story)
		if err != nil {
			return err
		}
		board, err := sprintStoreDir()
		if err != nil {
			return err
		}
		switch args[1] {
		case "open":
			if expected != 0 && !ladder.ValidPoints(ladder.Points(expected)) {
				return fmt.Errorf("--expected must be 1, 2, 3, 5 or 8")
			}
			return withWeaveQueueLock(board, func(q *weaveQueue) error {
				s := findWeaveStory(q, id)
				if s == nil {
					return fmt.Errorf("sprint #%d not found", id)
				}
				if _, err := os.Stat(path); err == nil {
					return fmt.Errorf("estimate for story %s is already open", story)
				}
				input := ladder.EstimateInput{Story: story, Expected: ladder.Points(expected), CrossRepo: crossRepo, Design: design}
				estimators, err := estimateDraw(cmd, id, s, story, input)
				if err != nil {
					return err
				}
				if len(estimators) == 0 {
					return nil
				}
				record := sprintEstimateRecord{Story: story, Estimators: estimators}
				if err := saveSprintEstimate(path, record); err != nil {
					return err
				}
				raw, _ := json.Marshal(sprintEstimateEvent{Story: story, Size: len(estimators), Estimators: estimators})
				weaveStoryAppend(s, weaveConductorName(""), "estimate-open", string(raw))
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "estimate %s opened: size=%d estimators=%s\n", story, len(estimators), strings.Join(estimators, ","))
				return err
			})
		case "submit":
			agent = strings.TrimSpace(agent)
			if agent == "" {
				return fmt.Errorf("--agent is required")
			}
			if !ladder.ValidPoints(ladder.Points(points)) && points != 13 {
				return fmt.Errorf("--points must be 1, 2, 3, 5, 8 or 13")
			}
			if confidence < 0 || confidence > 1 {
				return fmt.Errorf("--confidence must be between 0 and 1")
			}
			isShadow := shadow != ""
			if isShadow && strings.TrimSpace(shadow) != agent {
				return fmt.Errorf("--shadow must name --agent")
			}
			record, err := readSprintEstimate(path)
			if err != nil {
				return fmt.Errorf("estimate for story %s is not open", story)
			}
			if !isShadow {
				found := false
				for _, name := range record.Estimators {
					found = found || name == agent
				}
				if !found {
					return fmt.Errorf("agent %s is not an estimator for story %s", agent, story)
				}
			}
			for i := range record.Entries {
				if record.Entries[i].Agent == agent && record.Entries[i].Shadow == isShadow {
					record.Entries[i].Revote = ladder.Points(points)
					record.Entries[i].Confidence = confidence
					if err := saveSprintEstimate(path, record); err != nil {
						return err
					}
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "estimate recorded")
					return err
				}
			}
			record.Entries = append(record.Entries, sprintEstimateEntry{Agent: agent, First: ladder.Points(points), Confidence: confidence, Shadow: isShadow})
			if err := saveSprintEstimate(path, record); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "estimate recorded")
			return err
		case "close":
			return closeSprintEstimate(cmd, board, id, story, path)
		default:
			return fmt.Errorf("unknown estimate subverb %q; want open|submit|close", args[1])
		}
	}
	return cmd
}

func closeSprintEstimate(cmd *cobra.Command, board string, id int64, story, path string) error {
	record, err := readSprintEstimate(path)
	if err != nil {
		return fmt.Errorf("estimate for story %s is not open", story)
	}
	var rounds []ladder.EstimateRound
	for _, entry := range record.Entries {
		if !entry.Shadow {
			rounds = append(rounds, ladder.EstimateRound{Agent: entry.Agent, First: entry.First, Confidence: entry.Confidence, Revote: entry.Revote})
		}
	}
	result := ladder.DelphiMedian(rounds)
	if result.Reason != "" && !result.Escalated && len(rounds) != 3 {
		return fmt.Errorf("estimate for story %s: %s", story, result.Reason)
	}
	if result.Escalated {
		return withWeaveQueueLock(board, func(q *weaveQueue) error {
			s := findWeaveStory(q, id)
			if s == nil {
				return fmt.Errorf("sprint #%d not found", id)
			}
			input := ladder.EstimateInput{Story: story, Expected: 3}
			estimators, err := estimateDraw(cmd, id, s, story, input)
			if err != nil {
				return err
			}
			// The first estimator remains part of the Delphi round; draw only
			// supplies the two additional seats needed after escalation.
			first := record.Estimators[0]
			next := []string{first}
			for _, estimator := range estimators {
				if estimator != first && len(next) < 3 {
					next = append(next, estimator)
				}
			}
			if len(next) != 3 {
				return fmt.Errorf("could not draw two additional estimators for story %s", story)
			}
			record.Estimators = next
			if err := saveSprintEstimate(path, record); err != nil {
				return err
			}
			raw, _ := json.Marshal(sprintEstimateEvent{Story: story, Size: len(next), Estimators: next, Escalate: true})
			weaveStoryAppend(s, weaveConductorName(""), "estimate-open", string(raw))
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "escalate to 3")
			return err
		})
	}
	return withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		raw, _ := json.Marshal(sprintEstimateEvent{Story: story, Points: int(result.Points), Split: result.Split})
		weaveStoryAppend(s, weaveConductorName(""), "estimate", string(raw))
		store, err := ladder.OpenStore(ladder.DefaultStorePath())
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, entry := range record.Entries {
			event := ladder.Event{Kind: ladder.EventKindEstimate, Agent: entry.Agent, Duty: ladder.DutyJudge, Story: story, Estimate: entry.First, Sprint: int(id), Season: ladder.SeasonOf(now), At: now}
			if entry.Shadow {
				event.Note = "shadow"
			}
			if err := store.Append(event); err != nil {
				return err
			}
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "estimate %s: points=%d", story, result.Points)
		if result.Split {
			_, err = fmt.Fprint(cmd.OutOrStdout(), " must be split")
		}
		if err == nil {
			_, err = fmt.Fprintln(cmd.OutOrStdout())
		}
		return err
	})
}
