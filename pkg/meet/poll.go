package meet

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/chat"
)

// Two turn styles beyond the free-form round:
//
//   - POLL — a request for comment with a fixed answer set. Every participant
//     MUST answer with one of the choices; an unparseable reply is `invalid`,
//     never silently counted. This is the mechanism for "does everyone agree?"
//   - ASK — an open question. Answering is optional; silence means "no comment"
//     and is recorded as an abstention, NOT as a tool failure. This is the
//     mechanism for "anyone see a problem with this?"
//
// The distinction matters for operability scoring: an agent that abstains from
// an optional question attended the meeting; one that times out did not.

// defaultChoices is the yes/no ballot used when a poll names no choices.
var defaultChoices = []string{"yes", "no"}

// PollResult is one poll's outcome.
type PollResult struct {
	Question string         `json:"question"`
	Choices  []string       `json:"choices"`
	Votes    []Event        `json:"votes"`
	Tally    map[string]int `json:"tally"`
}

// Winner returns the choice with the most votes and whether that result is
// decisive. A poll that did not decide must say so rather than hand a calling
// agent a confident-looking answer.
//
// Three ways to fail to decide:
//   - nobody voted for any choice;
//   - two choices tie;
//   - the leading choice does not outnumber the non-answers (timeouts, crashes,
//     unparseable ballots). One "yes" out of a two-seat panel where the other
//     seat crashed is not a consensus, it is a sample of one.
func (p *PollResult) Winner() (string, bool) {
	best, bestN, tied := "", 0, false
	valid := map[string]bool{}
	for _, c := range p.Choices {
		valid[c] = true
		switch n := p.Tally[c]; {
		case n > bestN:
			best, bestN, tied = c, n, false
		case n == bestN && n > 0:
			tied = true
		}
	}
	nonAnswers := 0
	for k, n := range p.Tally {
		if !valid[k] {
			nonAnswers += n
		}
	}
	return best, bestN > 0 && !tied && bestN > nonAnswers
}

func pollPrompt(st *State, question string, choices []string) string {
	return fmt.Sprintf(
		"%s\nTopic: %s\n\nPOLL — you must answer.\nQuestion: %s\n\n"+
			"Reply on the FIRST line with EXACTLY one of these words and nothing else: %s\n"+
			"On the following lines give one or two sentences of rationale.",
		turnGuard, st.Topic, question, strings.Join(choices, " | "))
}

func askPrompt(st *State, question string, optional bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nTopic: %s\n\nOPEN QUESTION: %s\n", turnGuard, st.Topic, question)
	if optional {
		b.WriteString("\nAnswering is optional. If you have nothing to add, reply with exactly: (no comment)")
	}
	return b.String()
}

// normalizeChoice maps a free-text reply onto one of the permitted choices, or
// "" when the reply does not name exactly one. It reads the first few lines
// (agents put the verdict up top, as instructed), tolerating markdown emphasis,
// punctuation, and an "Answer:"/"Vote:" prefix. If the head is unreadable it
// falls back to a whole-reply scan, which succeeds only when exactly ONE choice
// appears — a reply mentioning both "yes" and "no" is invalid, not a coin flip.
func normalizeChoice(reply string, choices []string) string {
	clean := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.Trim(s, "*_`#>-. \t\"'!?:;,()[]")
		for _, p := range []string{"answer:", "vote:", "verdict:", "answer", "vote"} {
			if strings.HasPrefix(s, p) {
				s = strings.TrimSpace(strings.TrimPrefix(s, p))
				s = strings.Trim(s, "*_`\"' \t:")
			}
		}
		return s
	}

	lines := strings.Split(strings.TrimSpace(reply), "\n")
	for i, l := range lines {
		if i >= 5 {
			break
		}
		head := clean(l)
		if head == "" {
			continue
		}
		for _, c := range choices {
			lc := strings.ToLower(c)
			if head == lc || strings.HasPrefix(head, lc+" ") {
				return c
			}
		}
	}

	// Whole-reply fallback: unambiguous mention only.
	fields := strings.FieldsFunc(strings.ToLower(reply), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_'
	})
	seen := map[string]bool{}
	for _, f := range fields {
		for _, c := range choices {
			if f == strings.ToLower(c) {
				seen[c] = true
			}
		}
	}
	if len(seen) == 1 {
		for c := range seen {
			return c
		}
	}
	return ""
}

// runPoll puts a fixed-choice question to each participant and tallies the
// answers. Every vote is recorded with its normalized choice, so the tally is
// reproducible from the transcript alone.
func runPoll(ctx context.Context, st *State, question string, choices, participants []string, runner chat.Runner) (*PollResult, error) {
	// THE BOARD GUARD BELONGS HERE, not only on the Poll/Ask API wrappers.
	// Five call sites reach this function directly — the CLI verbs (which pass
	// their own --participant list), the REPL, and the two whole-room wrappers —
	// so a guard on the wrappers alone left `bashy meet poll <board>` running
	// past the mode check and INVOKING A MODEL. A board's one promise is that
	// nothing is spawned; the check has to sit in the single implementation or
	// the next caller reopens the hole.
	if st.board() {
		return nil, st.boardRefusal("run a poll")
	}
	// Argument validation comes AFTER the mode check on purpose: "poll needs a
	// --question" on a board tells the caller to supply one, which is the wrong
	// instruction — there is no chair to run a poll at all.
	if strings.TrimSpace(question) == "" {
		return nil, fmt.Errorf("meet: poll needs a --question")
	}
	if len(choices) == 0 {
		choices = defaultChoices
	}
	if len(participants) == 0 {
		participants = st.Participants
	}
	if len(participants) == 0 {
		return nil, fmt.Errorf("meet: poll needs at least one participant")
	}

	lease, err := acquireRunLease(st.ID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	st.Round++
	_ = st.save()
	if _, err := recordFull(st, Event{
		Round: st.Round, Speaker: procedural(st), Role: string(RoleChair), Kind: "poll",
		Text: question, Question: question, Choices: choices,
	}); err != nil {
		return nil, err
	}

	res := &PollResult{Question: question, Choices: choices, Tally: map[string]int{}}
	for _, name := range participants {
		// The ballot is finished and tallied INSIDE the persist callback, so the
		// vote reaches the transcript before the live channel's `spoke` claims it
		// did — and so the status published on the live channel is the ballot's
		// real one (an off-menu answer is `invalid`, not `ok`).
		var vote Event
		_, _ = invokeAgent(ctx, st, name, "", pollPrompt(st, question, choices), question, runner,
			func(e Event) (Event, error) {
				e.Kind = "vote"
				e.Choices = choices
				if statusOf(e) == statusOK {
					if c := normalizeChoice(e.Text, choices); c != "" {
						e.Choice = c
						res.Tally[c]++
					} else {
						// The agent answered, but not with a ballot. That is a failure
						// of the poll, not of the agent's availability — surface it as
						// such.
						e.Status = statusInvalid
					}
				}
				if e.Choice == "" {
					res.Tally[statusOf(e)]++
				}
				vote = e
				return e, appendEvent(st.ID, e)
			})
		res.Votes = append(res.Votes, vote)
	}
	return res, nil
}

// runAsk puts an open question to each participant. When optional, a participant
// that declines is recorded as abstaining — a real contribution, not a failure.
func runAsk(ctx context.Context, st *State, question string, optional bool, participants []string, runner chat.Runner) ([]Event, error) {
	// THE BOARD GUARD BELONGS HERE, not only on the Poll/Ask API wrappers.
	// Five call sites reach this function directly — the CLI verbs (which pass
	// their own --participant list), the REPL, and the two whole-room wrappers —
	// so a guard on the wrappers alone left `bashy meet poll <board>` running
	// past the mode check and INVOKING A MODEL. A board's one promise is that
	// nothing is spawned; the check has to sit in the single implementation or
	// the next caller reopens the hole.
	if st.board() {
		return nil, st.boardRefusal("put a question to the room")
	}
	// Argument validation comes AFTER the mode check on purpose: "poll needs a
	// --question" on a board tells the caller to supply one, which is the wrong
	// instruction — there is no chair to run a poll at all.
	if strings.TrimSpace(question) == "" {
		return nil, fmt.Errorf("meet: ask needs a --question")
	}
	if len(participants) == 0 {
		participants = st.Participants
	}
	if len(participants) == 0 {
		return nil, fmt.Errorf("meet: ask needs at least one participant")
	}

	lease, err := acquireRunLease(st.ID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	st.Round++
	_ = st.save()
	if _, err := recordFull(st, Event{
		Round: st.Round, Speaker: procedural(st), Role: string(RoleChair), Kind: "question",
		Text: question, Question: question,
	}); err != nil {
		return nil, err
	}

	var out []Event
	for _, name := range participants {
		// Same shape as a poll: the abstain reclassification happens before the
		// append, and the append before the floor is freed, so the live channel's
		// `spoke` reports `abstain` for a declined optional question rather than the
		// `empty` that silence classified as.
		var answer Event
		_, _ = invokeAgent(ctx, st, name, "", askPrompt(st, question, optional), question, runner,
			func(e Event) (Event, error) {
				if optional && isNoComment(e) {
					e.Status = statusAbstain
					e.Text = fmt.Sprintf("(%s: no comment)", name)
				}
				answer = e
				return e, appendEvent(st.ID, e)
			})
		out = append(out, answer)
	}
	return out, nil
}

// isNoComment recognizes an explicit pass, and treats an empty reply to an
// OPTIONAL question as the same thing — silence means no comment.
func isNoComment(e Event) bool {
	if statusOf(e) == statusEmpty {
		return true
	}
	if statusOf(e) != statusOK {
		return false
	}
	t := strings.ToLower(strings.TrimSpace(e.Text))
	t = strings.Trim(t, "().*_` \t\n")
	return t == "no comment" || t == "nothing to add" || t == "pass" || t == "abstain"
}

// renderPolls writes every poll and its tally into the minutes, reconstructed
// from the transcript.
func renderPolls(b *strings.Builder, events []Event) {
	type poll struct {
		q       string
		choices []string
		votes   []Event
	}
	var polls []*poll
	for _, e := range events {
		switch e.Kind {
		case "poll":
			polls = append(polls, &poll{q: e.Text, choices: e.Choices})
		case "vote":
			if len(polls) > 0 {
				p := polls[len(polls)-1]
				p.votes = append(p.votes, e)
			}
		}
	}
	if len(polls) == 0 {
		return
	}
	b.WriteString("\n## Polls\n")
	for _, p := range polls {
		fmt.Fprintf(b, "\n**%s**\n\n", redactHome(p.q))
		res := &PollResult{Question: p.q, Choices: p.choices, Votes: p.votes, Tally: map[string]int{}}
		for _, v := range p.votes {
			if v.Choice != "" {
				res.Tally[v.Choice]++
			} else {
				res.Tally[statusOf(v)]++
			}
		}
		keys := make([]string, 0, len(res.Tally))
		for k := range res.Tally {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, res.Tally[k]))
		}
		fmt.Fprintf(b, "Tally: %s", strings.Join(parts, " · "))
		if w, ok := res.Winner(); ok {
			fmt.Fprintf(b, " → **%s**\n", w)
		} else {
			b.WriteString(" → *no clear result*\n")
		}
		b.WriteString("\n")
		for _, v := range p.votes {
			answer := v.Choice
			if answer == "" {
				answer = statusOf(v)
			}
			fmt.Fprintf(b, "- **%s**: %s — %s\n", v.Speaker, answer, oneLine(redactHome(v.Text)))
		}
	}
}

// recordFull appends a fully-formed event (sanitizing its text), for the callers
// that need to set fields `record` does not expose.
func recordFull(st *State, e Event) (Event, error) {
	e.Text = sanitizeTurn(e.Text)
	if e.TS.IsZero() {
		e.TS = nowFn()
	}
	return e, appendEvent(st.ID, e)
}
