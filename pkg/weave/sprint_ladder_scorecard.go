package weave

// The manager scorecard at `sprint end` (Sprint 331, story #1205; band-ladder
// design section 10).
//
// A sprint that ends cleanly is one Glicko match for the manager's `manage`
// duty. The manager is judged against what the team it had was EXPECTED to
// deliver, so the inputs are read from the ladder ledger as it stood before
// the sprint: the sprint's delivery events are the assignments, and every
// other event rates the agents who made them. The score lands as ONE manage
// event; the same summary goes to stdout and the sprint thread.
//
// Scoring is a report, not a gate: a ledger that cannot be read or written, or
// a manager the fleet registry cannot name, records nothing and says why —
// it never fails the end that already succeeded.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
)

const (
	// sprintScorecardKind is the thread event carrying the scorecard summary.
	sprintScorecardKind = "scorecard"
	// sprintInstructKind is the thread event a supervisor instruction leaves
	// (`sprint instruct`); counted beside the score, never in it.
	sprintInstructKind = "instruct"
	// sprintBypassKind is what the lease-token should-phase records when a
	// managed verb runs without the manager's token.
	sprintBypassKind = "bypass"
)

// sprintManagerIdentity names the manager as its canonical tool:model rating
// identity: the lease holder (the owner when no lease is held), resolved
// read-only through the fleet registry. A clone resolves to its parent's
// binding, since a clone carries the parent's tool and model.
func sprintManagerIdentity(s *weaveStory) (string, error) {
	name := sprintManagerName(s)
	if name == "" {
		return "", fmt.Errorf("sprint #%d has no lease holder or owner", s.ID)
	}
	a, ok := fleetCatalog().Agent(name)
	if !ok || strings.TrimSpace(a.Tool) == "" || strings.TrimSpace(a.Model) == "" {
		return "", fmt.Errorf("manager %q does not resolve to a tool:model in the fleet registry", name)
	}
	return a.MatrixKey(), nil
}

// sprintManagerName is the manager's fleet name: the lease holder, else the
// owner.
func sprintManagerName(s *weaveStory) string {
	name := ""
	if s.Lease != nil {
		name = strings.TrimSpace(s.Lease.Holder)
	}
	if name == "" {
		name = strings.TrimSpace(s.Owner)
	}
	return name
}

// sprintScorecardAtEnd scores the ending sprint and records it. It runs inside
// end's mutation, after every refusal has passed and before the lease is
// released (the holder is the manager being scored). It appends the thread
// event itself and returns the report for the caller to print once the
// mutation has committed.
func sprintScorecardAtEnd(s *weaveStory, skip bool, hygienePassed, hygieneTotal int, now time.Time) string {
	author := weaveStoryConductorName(s, "")
	if skip {
		weaveStoryAppend(s, author, sprintScorecardKind, "scorecard skipped (--no-scorecard)")
		return "scorecard skipped (--no-scorecard); no manage event recorded"
	}
	identity, err := sprintManagerIdentity(s)
	if err != nil {
		return "scorecard not recorded: " + err.Error()
	}
	store, err := ladder.OpenStore("")
	if err != nil {
		return "scorecard not recorded: ladder store: " + err.Error()
	}
	events, err := store.Read()
	if err != nil {
		return "scorecard not recorded: ladder store: " + err.Error()
	}

	season := ladder.SeasonOf(now)
	in, notes := sprintScorecardEvidence(events, s, season, identity, sprintManagerName(s))
	in.HygieneChecksPassed, in.HygieneChecksTotal = hygienePassed, hygieneTotal
	for _, c := range s.Thread {
		switch c.Kind {
		case sprintInstructKind:
			in.SupervisorInstructions++
		case sprintBypassKind:
			if len(in.AutoFails) == 0 {
				in.AutoFails = append(in.AutoFails, ladder.AutoFailDetectedBypass)
			}
		}
	}
	card, err := ladder.ComputeScorecard(in, ladder.DefaultScorecardWeights())
	if err != nil {
		return "scorecard not recorded: " + err.Error()
	}

	sprintScorecardApplyEvidence(&card, in, notes)
	summary := sprintScorecardSummary(s.ID, identity, season, card)
	e := ladder.Event{
		ID:       fmt.Sprintf("manage-sprint-%d-%d", s.ID, now.UnixNano()),
		At:       now,
		Season:   season,
		Kind:     ladder.EventKindManage,
		Agent:    identity,
		Duty:     ladder.DutyManage,
		Sprint:   int(s.ID),
		Score:    card.Score,
		Opponent: ladder.SprintOpponent(in.Assignments),
		Note:     sprintScorecardNote(card),
	}
	if err := store.Append(e); err != nil {
		summary += "\n  not recorded: ladder store: " + err.Error()
	} else {
		summary += "\n  recorded: manage event " + e.ID
	}
	weaveStoryAppend(s, author, sprintScorecardKind, summary)
	return summary
}

// sprintScorecardInput builds the delivery half of the scorecard from the
// ledger. Assignments are sprint N's delivery events; ratings are Replay over
// every event EXCEPT sprint N's — the standing at assignment time, as nearly
// as the ledger can say.
//
// BestAvailable approximates "the best agent available at the time" as the
// highest-rated agent with at least one rated code event before the sprint,
// or the chosen agent if it rates higher: availability snapshots are not
// recorded yet, and the chosen agent was available by definition, so regret
// is never negative.
//
// SpecFailures counts sprint N's failed deliveries whose blame is a VALID
// spec-class attribution (the story could not be built as written) and whose
// Author is the manager, named by any of managers (its tool:model identity or
// fleet name). A delivery with no Author counts too: a sprint's stories are
// its manager's unless recorded otherwise. With no managers given, every
// author counts.
func sprintScorecardInput(events []ladder.Event, sprint int64, season int, managers ...string) ladder.ScorecardInput {
	dropped := map[string]bool{}
	for _, e := range events {
		if e.Kind == ladder.EventKindCorrection && e.Supersedes != "" && e.Season <= season {
			dropped[e.Supersedes] = true
		}
	}
	var prior, current []ladder.Event
	for _, e := range events {
		switch {
		case e.Sprint != int(sprint):
			prior = append(prior, e)
		case e.Kind == ladder.EventKindDelivery && !dropped[e.ID] && e.Season >= 1 && e.Season <= season:
			current = append(current, e)
		}
	}
	pre := ladder.Replay(prior, season)
	rating := func(agent string) ladder.Rating {
		if a := pre.Agents[agent]; a != nil {
			if st := a.Standings[ladder.DutyCode]; st.R != 0 {
				return ladder.Rating{R: st.R, RD: st.RD, Vol: ladder.InitialVol}
			}
		}
		return ladder.NewRating()
	}
	var pool []string
	for name, a := range pre.Agents {
		if a.Standings[ladder.DutyCode].Events >= 1 {
			pool = append(pool, name)
		}
	}
	sort.Strings(pool)

	var in ladder.ScorecardInput
	for _, e := range current {
		if e.Outcome == 0 && blame.Consequence(e.Blame) == blame.ActionChargeEstimatorAndAuthor && sprintScorecardAuthoredBy(e.Author, managers) {
			in.SpecFailures++
		}
		storyR, _ := ladder.StoryInitialRating(e.Points)
		story := ladder.Rating{R: storyR, RD: ladder.InitialRD, Vol: ladder.InitialVol}
		chosen := rating(e.Agent)
		best := chosen
		for _, name := range pool {
			if r := rating(name); ladder.Expected(r, story) > ladder.Expected(best, story) {
				best = r
			}
		}
		in.Assignments = append(in.Assignments, ladder.Assignment{
			Story:         e.Story,
			Points:        int(e.Points),
			StoryRating:   storyR,
			Agent:         e.Agent,
			AgentRating:   chosen,
			BestAvailable: best,
			Outcome:       e.Outcome,
			Rated:         ladder.DeliveryRates(e),
		})
	}
	sprintScorecardMeters(&in, events, current, dropped, int(sprint), season)
	return in
}

// sprintScorecardAuthoredBy reports whether author is one of managers; an
// empty author, or no managers at all, matches.
func sprintScorecardAuthoredBy(author string, managers []string) bool {
	author = strings.TrimSpace(author)
	if author == "" || len(managers) == 0 {
		return true
	}
	for _, m := range managers {
		if m != "" && m == author {
			return true
		}
	}
	return false
}

// sprintScorecardComponents renders the components in a stable order.
func sprintScorecardComponents(card ladder.Scorecard) string {
	names := make([]string, 0, len(card.Components))
	for k := range card.Components {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = fmt.Sprintf("%s=%.2f", k, card.Components[k])
	}
	return strings.Join(parts, " ")
}

func sprintScorecardAutoFails(card ladder.Scorecard) string {
	if len(card.AutoFails) == 0 {
		return "none"
	}
	parts := make([]string, len(card.AutoFails))
	for i, f := range card.AutoFails {
		parts[i] = string(f)
	}
	return strings.Join(parts, ",")
}

// sprintScorecardNote is the compact one-line form stored on the manage event.
func sprintScorecardNote(card ladder.Scorecard) string {
	return fmt.Sprintf("%s expected=%.2f actual=%.2f regret=%.2f unrated=%d instructions=%d autofails=%s",
		sprintScorecardComponents(card), card.Expected, card.Actual, sprintScorecardZero(card.Regret),
		card.Unrated, card.SupervisorInstructions, sprintScorecardAutoFails(card)) + " notes=" + strings.Join(card.Notes, "; ")
}

// sprintScorecardZero folds -0.00 into 0.00 for display.
func sprintScorecardZero(v float64) float64 {
	if math.Abs(v) < 0.005 {
		return 0
	}
	return v
}

// sprintScorecardSummary is the multi-line report for stdout and the thread.
func sprintScorecardSummary(sprint int64, identity string, season int, card ladder.Scorecard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "scorecard: sprint #%d manager %s score %.3f (season %d)\n", sprint, identity, card.Score, season)
	fmt.Fprintf(&b, "  components: %s\n", sprintScorecardComponents(card))
	fmt.Fprintf(&b, "  delivery: expected %.2f pts, actual %.2f pts, regret %.2f, unrated %d\n",
		card.Expected, card.Actual, sprintScorecardZero(card.Regret), card.Unrated)
	fmt.Fprintf(&b, "  supervisor instructions: %d\n", card.SupervisorInstructions)
	fmt.Fprintf(&b, "  auto-fails: %s", sprintScorecardAutoFails(card))
	if len(card.Notes) > 0 {
		fmt.Fprintf(&b, "\n  notes: %s", strings.Join(card.Notes, "; "))
	}
	return b.String()
}

// Meter sums use all rated points, but incomplete meters stay neutral rather
// than treating unrecorded attempts as free. Wall units are seconds throughout.
func sprintScorecardMeters(in *ladder.ScorecardInput, events, current []ladder.Event, dropped map[string]bool, sprint, season int) {
	totals := func(es []ladder.Event) (cost, wall, caps, points float64) {
		costOK, wallOK := true, true
		for _, e := range es {
			if !ladder.DeliveryRates(e) {
				continue
			}
			points += float64(e.Points)
			if e.Cost <= 0 || math.IsNaN(e.Cost) || math.IsInf(e.Cost, 0) {
				costOK = false
			} else {
				cost += e.Cost
			}
			if e.CapsUsed.WallSeconds <= 0 {
				wallOK = false
			} else {
				wall += float64(e.CapsUsed.WallSeconds)
			}
			cap, _ := ladder.CapFor(e.Points)
			caps += cap.Wall.Seconds()
		}
		if !costOK {
			cost = 0
		}
		if !wallOK {
			wall = 0
		}
		return
	}
	cost, wall, caps, points := totals(current)
	if points > 0 {
		in.CostPerPoint = cost / points
		in.WallPerPoint = wall / points
		in.ExpectedWallPerPoint = caps / points
	}
	// A manage event is the ledger's evidence that a sprint finished. Order by
	// completion time, not sprint number or append order; take exactly three.
	finished := map[int]time.Time{}
	for _, e := range events {
		if e.Kind == ladder.EventKindManage && e.Sprint > 0 && e.Sprint != sprint && !dropped[e.ID] && e.Season >= 1 && e.Season <= season && e.At.After(finished[e.Sprint]) {
			finished[e.Sprint] = e.At
		}
	}
	ids := make([]int, 0, len(finished))
	for id := range finished {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if finished[ids[i]].Equal(finished[ids[j]]) {
			return ids[i] < ids[j]
		}
		return finished[ids[i]].After(finished[ids[j]])
	})
	if len(ids) > 3 {
		ids = ids[:3]
	}
	var ratios []float64
	for _, id := range ids {
		var deliveries []ladder.Event
		for _, e := range events {
			if e.Sprint == id && !dropped[e.ID] && e.Season >= 1 && e.Season <= season && !e.At.After(finished[id]) {
				deliveries = append(deliveries, e)
			}
		}
		c, _, _, p := totals(deliveries)
		if p > 0 && c > 0 {
			ratios = append(ratios, c/p)
		}
	}
	if len(ratios) > 0 {
		in.ExpectedCostPerPoint = sprintScorecardMedian(ratios)
	}
}

func sprintScorecardMedian(values []float64) float64 {
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	n := len(v)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// Evidence joins are deliberately exact. Prose story references and shortened
// IDs do not prove a merge, and a grade alone does not prove one either.
func sprintScorecardEvidence(events []ladder.Event, s *weaveStory, season int, managers ...string) (ladder.ScorecardInput, []string) {
	in := sprintScorecardInput(events, s.ID, season, managers...)
	notes := []string{}
	if private, err := sprintPlantedCounts(s.ID); err == nil {
		in.PlantedDefects = private.PlantedDefects
		in.PlantedCaught = private.PlantedCaught
		in.FalseRejections = private.FalseRejections
		if private.FalseRejections == 0 {
			notes = append(notes, "false rejections unavailable: panels required; left at 0")
		}
	} else {
		notes = append(notes, "private review outcomes unavailable: "+err.Error())
	}
	if in.ExpectedCostPerPoint == 0 {
		notes = append(notes, "no prior sprint with recorded cost in the last 3 finished sprints; cost baseline unavailable")
	}
	if in.CostPerPoint == 0 {
		notes = append(notes, "rated delivery cost evidence absent or incomplete; cost neutral")
	}
	if in.WallPerPoint == 0 {
		notes = append(notes, "rated delivery wall evidence absent or incomplete; wall neutral")
	}
	merged := map[string]bool{}
	// Explicit story references can survive a pruned run queue.
	for _, c := range s.Thread {
		if c.Kind == "merge" {
			var e struct {
				Story string `json:"story"`
			}
			if json.Unmarshal([]byte(c.Body), &e) == nil && e.Story != "" {
				merged[e.Story] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, link := range s.Runs {
		key := fmt.Sprintf("%s/%s#%d/%s", link.Queue, link.Repo, link.ID, link.Born.Format(time.RFC3339Nano))
		if seen[key] {
			continue
		}
		seen[key] = true
		name := fmt.Sprintf("%s#%d", link.Repo, link.ID)
		dir, err := weaveQueueDirForSprintRun(link)
		var run *weaveItem
		if err == nil {
			q, e := loadWeaveQueue(dir)
			if e == nil {
				run = findWeaveItem(q, link.ID)
			}
		}
		if run == nil || (!link.Born.IsZero() && !link.Born.Equal(run.Created)) {
			notes = append(notes, "run "+name+" evidence unavailable")
			continue
		}
		for _, c := range s.Thread {
			if c.Kind != "merge" {
				continue
			}
			var e sprintGradeEvent
			if json.Unmarshal([]byte(c.Body), &e) == nil && e.Run == name && e.Generation == filepath.Base(dir)+":"+run.Created.UTC().Format(time.RFC3339Nano) && run.Register != "" {
				merged[run.Register] = true
			}
		}
		if run.State != "failed" && run.State != "killed" && run.State != "looped" {
			continue
		}
		progress := run.FinishedAt
		if run.LogPath != "" {
			if info, e := os.Stat(run.LogPath); e == nil && !info.ModTime().Before(run.StartedAt) && (progress.IsZero() || !info.ModTime().After(progress)) {
				progress = info.ModTime()
			}
		}
		var next time.Time
		if !progress.IsZero() {
			for _, c := range s.Thread {
				if c.Kind != "fail" && c.Kind != "abandon" && c.Kind != "assign" && c.Kind != "relaunch" {
					continue
				}
				if c.Author != sprintManagerName(s) || c.At.Before(progress) {
					continue
				}
				matches := false
				var e struct {
					Run   json.RawMessage `json:"run"`
					Story string          `json:"story"`
				}
				if json.Unmarshal([]byte(c.Body), &e) == nil {
					var ref string
					var id int64
					if json.Unmarshal(e.Run, &ref) == nil {
						matches = ref == name
					} else if json.Unmarshal(e.Run, &id) == nil && id == link.ID && e.Story != "" && e.Story == run.Register {
						count := 0
						for _, r := range s.Runs {
							if r.ID == id {
								count++
							}
						}
						matches = count == 1
					}
				} else {
					for _, word := range strings.FieldsFunc(c.Body, func(r rune) bool {
						return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '#' && r != '-' && r != '_' && r != '.'
					}) {
						if word == name {
							matches = true
						}
					}
				}
				if matches && (next.IsZero() || c.At.Before(next)) {
					next = c.At
				}
			}
		}
		if progress.IsZero() || next.IsZero() {
			notes = append(notes, "run "+name+" stall detection unavailable: missing progress or manager action")
			continue
		}
		in.StallDetectMinutes = append(in.StallDetectMinutes, next.Sub(progress).Minutes())
	}
	dropped := map[string]bool{}
	for _, e := range events {
		if e.Kind == ladder.EventKindCorrection && e.Season >= 1 && e.Season <= season {
			dropped[e.Supersedes] = true
		}
	}
	for _, e := range events {
		if e.Kind == ladder.EventKindRegression && !dropped[e.ID] && e.Season >= 1 && e.Season <= season && merged[e.Story] {
			in.EscapedRegressions++
		}
	}
	if len(merged) == 0 {
		notes = append(notes, "merged story evidence unavailable; escaped regressions left at 0")
	}
	if len(in.StallDetectMinutes) == 0 {
		notes = append(notes, "no recorded stall detection intervals; no recovery penalty")
	}
	return in, notes
}

// A median recovery delay over 15 minutes subtracts 0.10 from efficiency
// (at most 0.015 from the default weighted score). This bounded penalty is
// applied here because the ladder calculator does not yet consume stalls.
func sprintScorecardApplyEvidence(card *ladder.Scorecard, in ladder.ScorecardInput, notes []string) {
	card.Notes = append(card.Notes, notes...)
	if len(in.StallDetectMinutes) == 0 {
		return
	}
	median := sprintScorecardMedian(in.StallDetectMinutes)
	card.Notes = append(card.Notes, fmt.Sprintf("stall detection minutes %v; median %.1f", in.StallDetectMinutes, median))
	if median <= 15 {
		return
	}
	before := card.Components["efficiency"]
	card.Components["efficiency"] = math.Max(0, before-0.10)
	if len(card.AutoFails) == 0 {
		card.Score -= ladder.DefaultScorecardWeights().Efficiency * (before - card.Components["efficiency"])
	}
	card.Notes = append(card.Notes, "median stall detection exceeds 15 minutes; efficiency penalty 0.10 (floored at 0)")
}
