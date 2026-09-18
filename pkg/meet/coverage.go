package meet

import (
	"fmt"
	"io"
	"strings"
)

// Coverage is one participant's attendance record: how many turns it took, how
// they came out, and how much it actually said.
//
// This exists because "opencode returned no content" in the minutes tells an
// operator nothing actionable. A coverage row tells them whether the seat was
// occupied, whether the tool is broken, and whether re-running would help.
type Coverage struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Turns    int    `json:"turns"`
	OK       int    `json:"ok"`
	Abstain  int    `json:"abstain,omitempty"`
	Empty    int    `json:"empty,omitempty"`
	Timeout  int    `json:"timeout,omitempty"`
	Errors   int    `json:"errors,omitempty"`
	Short    int    `json:"short,omitempty"`
	Invalid  int    `json:"invalid,omitempty"`
	Chars    int    `json:"chars"`
	Votes    int    `json:"votes,omitempty"`
	Messages int    `json:"messages,omitempty"` // board posts — presence, never turn quality
	Last     string `json:"last_status,omitempty"`
	ExitCode int    `json:"last_exit_code,omitempty"`
}

// Contributed reports whether the seat produced anything at all — a completed
// turn, an abstention, or a board message.
func (c Coverage) Contributed() bool { return c.OK+c.Abstain+c.Messages > 0 }

// RetryRecommended reports whether re-running this participant is likely to
// help — a timeout or crash is usually transient, an empty reply is not.
func (c Coverage) RetryRecommended() bool { return c.Timeout+c.Errors > 0 }

// coverage folds the transcript into one row per participant, in roster order so
// the table is stable across runs.
func coverage(st *State, events []Event) []Coverage {
	idx := map[string]int{}
	rows := make([]Coverage, 0, len(st.Participants))
	for _, p := range st.Participants {
		idx[p] = len(rows)
		rows = append(rows, Coverage{Name: p, Role: string(RoleParticipant)})
	}
	for _, e := range events {
		if e.Kind != "turn" && e.Kind != "vote" && e.Kind != "message" {
			continue
		}
		i, ok := idx[e.Speaker]
		if !ok {
			continue
		}
		r := &rows[i]
		if e.Kind == "message" {
			// A board post is presence, not turn quality: it marks the seat
			// occupied (Contributed) but never feeds OK — OK flows into
			// Verdict.decide()'s coverage ratio, and chat traffic must not
			// inflate a consult's confidence. It carries no turn status either,
			// so Last keeps whatever the seat's last real turn reported.
			r.Messages++
			r.Chars += e.Chars
			continue
		}
		r.Turns++
		if e.Kind == "vote" {
			r.Votes++
		}
		r.Chars += e.Chars
		status := statusOf(e)
		r.Last = status
		r.ExitCode = e.ExitCode
		switch status {
		case statusOK:
			r.OK++
		case statusAbstain:
			r.Abstain++
		case statusEmpty:
			r.Empty++
		case statusTimeout:
			r.Timeout++
		case statusError:
			r.Errors++
		case statusShort:
			r.Short++
		case statusInvalid:
			r.Invalid++
		}
	}
	return rows
}

// writeCoverageTable renders the coverage rows as a markdown table plus, for any
// seat that failed, a diagnosis line naming the failure mode, the exit code, the
// per-turn log, and whether a retry is worth it.
func writeCoverageTable(b *strings.Builder, rows []Coverage) {
	b.WriteString("| Participant | Turns | Msgs | OK | Abstain | Empty | Timeout | Error | Short | Invalid | Chars |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d |\n",
			r.Name, r.Turns, r.Messages, r.OK, r.Abstain, r.Empty, r.Timeout, r.Errors, r.Short, r.Invalid, r.Chars)
	}
	var notes []string
	for _, r := range rows {
		switch {
		case r.Turns+r.Messages == 0:
			notes = append(notes, fmt.Sprintf("**%s** never took a turn or posted a message — the seat was empty.", r.Name))
		case !r.Contributed():
			notes = append(notes, fmt.Sprintf("**%s** contributed nothing across %d turns (last: %s%s). %s",
				r.Name, r.Turns, r.Last, exitSuffix(r.ExitCode), retryAdvice(r)))
		case r.Timeout+r.Errors+r.Empty+r.Invalid > 0:
			notes = append(notes, fmt.Sprintf("**%s** had %d failed turn(s) (last: %s%s). %s",
				r.Name, r.Timeout+r.Errors+r.Empty+r.Invalid, r.Last, exitSuffix(r.ExitCode), retryAdvice(r)))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n")
		for _, n := range notes {
			fmt.Fprintf(b, "- %s\n", n)
		}
	}
}

func exitSuffix(code int) string {
	if code == 0 {
		return ""
	}
	return fmt.Sprintf(", exit %d", code)
}

func retryAdvice(r Coverage) string {
	if r.RetryRecommended() {
		return "Retry recommended — the failure was a timeout or a crash, not a considered silence."
	}
	if r.Invalid > 0 {
		return "The agent replied but did not answer the ballot; re-ask with a narrower prompt."
	}
	return "Retry unlikely to help — the agent ran cleanly and produced nothing."
}

// writeShow prints the human-facing session summary: identity, roster, coverage,
// and where everything lives.
func writeShow(w io.Writer, st *State, events []Event, syn *Synthesis) {
	fmt.Fprintf(w, "meeting  %s\n", st.ID)
	fmt.Fprintf(w, "topic    %s\n", st.Topic)
	fmt.Fprintf(w, "status   %s  ·  round %d\n", st.Status, st.Round)
	fmt.Fprintf(w, "initiator %s\n", st.initiatorLabel())
	fmt.Fprintf(w, "secretary %s (records only)\n", st.Secretary)
	fmt.Fprintf(w, "facilitator %s\n", st.turnModel())
	if len(st.Context) > 0 {
		fmt.Fprintf(w, "context  %s\n", strings.Join(st.Context, ", "))
	}
	fmt.Fprintln(w)

	rows := coverage(st, events)
	fmt.Fprintf(w, "%-14s %-6s %5s %4s %4s %4s %6s %6s %6s %8s\n",
		"PARTICIPANT", "LAST", "TURNS", "MSGS", "OK", "ABST", "EMPTY", "T/OUT", "ERROR", "CHARS")
	for _, r := range rows {
		last := r.Last
		if last == "" {
			last = "-"
		}
		fmt.Fprintf(w, "%-14s %-6s %5d %4d %4d %4d %6d %6d %6d %8d\n",
			r.Name, last, r.Turns, r.Messages, r.OK, r.Abstain, r.Empty, r.Timeout, r.Errors, r.Chars)
	}

	var quiet []string
	for _, r := range rows {
		if !r.Contributed() {
			quiet = append(quiet, r.Name)
		}
	}
	if len(quiet) > 0 {
		fmt.Fprintf(w, "\n⚠ no contribution from: %s\n", strings.Join(quiet, ", "))
	}

	if syn != nil {
		fmt.Fprintf(w, "\nsynthesis by %s (%s mode): %d decisions, %d actions, %d risks, %d open questions\n",
			syn.By, syn.Mode, len(syn.Decisions), len(syn.Actions), len(syn.Risks), len(syn.OpenQuestions))
	} else {
		fmt.Fprintf(w, "\nsynthesis: none yet — run `bashy meet converge %s`\n", st.ID)
	}
	dir, _ := storeDir(st.ID)
	fmt.Fprintf(w, "\nstore    %s\n", redactHome(dir))
	fmt.Fprintf(w, "minutes  %s\n", redactHome(minutesPath(st)))
	fmt.Fprintf(w, "\ncontributions: bashy meet contributions %s [participant]\n", st.ID)
}

// writeContributions prints every contribution, in full, optionally filtered to
// one participant. This is the answer to "what exactly did codex say?" — the
// minutes summarize, this does not.
func writeContributions(w io.Writer, st *State, events []Event, who string) {
	want := strings.TrimSpace(who)
	n := 0
	for _, e := range events {
		if e.Kind != "turn" && e.Kind != "vote" && e.Kind != "human" && e.Kind != "message" {
			continue
		}
		if want != "" && !strings.EqualFold(e.Speaker, want) {
			continue
		}
		n++
		status := statusOf(e)
		who := e.Speaker
		if e.Kind == "message" {
			if to := messageTo(e); to != "" {
				who += " → " + to
			} else {
				who += " → room"
			}
		}
		fmt.Fprintf(w, "── round %d · %s · %s", e.Round, who, status)
		if e.Choice != "" {
			fmt.Fprintf(w, " · vote=%s", e.Choice)
		}
		if e.DurMS > 0 {
			fmt.Fprintf(w, " · %dms", e.DurMS)
		}
		if e.Chars > 0 {
			fmt.Fprintf(w, " · %d chars", e.Chars)
		}
		fmt.Fprintln(w)
		if e.Question != "" {
			fmt.Fprintf(w, "   q: %s\n", oneLine(e.Question))
		}
		fmt.Fprintf(w, "\n%s\n", redactHome(strings.TrimSpace(e.Text)))
		if e.File != "" {
			fmt.Fprintf(w, "\n   full: %s\n", redactHome(e.File))
		}
		fmt.Fprintln(w)
	}
	if n == 0 {
		if want != "" {
			fmt.Fprintf(w, "no contributions from %q in %s\n", want, st.ID)
			fmt.Fprintf(w, "participants: %s\n", strings.Join(st.Participants, ", "))
			return
		}
		fmt.Fprintf(w, "no contributions recorded in %s\n", st.ID)
	}
}
