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
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

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
	in := sprintScorecardInput(events, s.ID, season, identity, sprintManagerName(s))
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
		card.Unrated, card.SupervisorInstructions, sprintScorecardAutoFails(card))
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
