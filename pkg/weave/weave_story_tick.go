package weave

// THE TICK — one turn's WORKSHEET for whoever is managing a sprint.
//
// `sprint --help` already describes the manager's loop in seven steps, and the
// conductor skill repeats it. Both are prose. Executing that prose meant running
// seven commands and diffing the results by hand, so every harness — claude,
// codex, agy — reimplemented the loop differently, and none of them could be
// told which step it had skipped. This verb is that loop's INPUTS, gathered once.
//
// # It gathers. It does not decide.
//
// Prioritising, assigning, merging and reassigning are judgement plus money. The
// conductor skill already applies exactly this rule to its own bindings: PLAN,
// RESEARCH and RETRO stay unbound because "a command claiming to do them would be
// a lie". The same rule governs here. A tick that assigned work would spend
// provider capacity on the manager's behalf and hide the one choice that actually
// matters — and it would do it every time somebody merely LOOKED at the sprint.
//
// So this file contains no mutation of any kind. Three prohibitions follow from
// that, and each of them is a bug that would otherwise be easy to introduce:
//
//  1. IT MUST NOT REFRESH THE LEASE. Reading mail refreshes the seat by design —
//     a manager reading its inbox is a manager working. A worksheet must not,
//     because a loop that polls it would forge liveness for a manager that has
//     stopped working, and the ghost seat is exactly what the AttachedPID field
//     exists to catch.
//  2. IT MUST NOT CONSUME A CURSOR. Counting unread mail is a PEEK, the same
//     third-person rule the console's inbox panel follows. `bashy inbox` marks
//     read; this counts. bus.UnreadNotifications and bus.UnreadPending are both
//     pure reads — deliberately NOT bus.SnapshotInbox, which materialises pending
//     records and opens subscriptions.
//  3. IT MUST NOT PROBE THE FLEET. `weave fleet --probe/--auth` runs a real
//     headless turn per row. Doing that every tick would burn provider capacity
//     on bookkeeping. The tick reports PATH + cooldown evidence only, and says so
//     in as many words, because "installed" is not "signed in" and a worksheet
//     that blurred the two would be worse than one that stayed quiet.
//
// # "Since my last tick" is measured from the last ACTION, not the last look
//
// There is no stored tick marker, and adding one would have meant writing to the
// sprint on every read. The baseline is instead the manager's own most recent
// entry in the sprint thread — checkpoint, comment, decision, review.
//
// That is not a workaround; it answers a better question. A marker keyed on
// looking would let a manager poll in a loop, see an empty delta every time, and
// conclude the sprint was healthy — the delta would be empty precisely BECAUSE it
// kept looking. Keyed on acting, an empty delta means "nothing has changed since
// you last did something", which is the sentence a manager actually needs.
//
// # The one signal nothing else reports
//
// Every other line here can be reconstructed from an existing command. `silent`
// cannot. A run is silent when it is still running, has been running a while, and
// has produced no commits: alive, and stuck. `weave status` shows liveness and
// `weave list` shows state, but neither crosses liveness with PRODUCTION, so a
// worker that started an hour ago and has written nothing looks exactly like one
// that started a minute ago. That is the failure the manager most needs to catch
// and the one it currently catches last.

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// sprintSilentAfter is how long a running, commit-less run must have been going
// before the worksheet calls it silent.
//
// It is not a timeout and nothing acts on it. The number only decides when a run
// is worth a manager's ATTENTION, so it is set from the same place `weave add
// --points` gets its cap: 8 points is documented as roughly a thirty-minute unit
// of work. A run past that with nothing committed has missed its own estimate,
// which is the moment to look — not the moment to kill.
const sprintSilentAfter = 30 * time.Minute

// sprintContinuityStale is when the resume brief stops being trustworthy.
//
// A continuity record is the ONE thing that survives the manager: it is what a
// successor reads cold. An hour is long enough that a working manager is not
// nagged mid-task, and short enough that a brief written before the last three
// merges does not get handed to somebody as the truth.
const sprintContinuityStale = time.Hour

// sprintTickPoll is how often --wait re-reads. A var so a test can drive the
// wait deterministically rather than sleeping through a real interval.
var sprintTickPoll = 5 * time.Second

// sprintTick is one worksheet. Every field answers a question the manager's loop
// asks; nothing here is a recommendation.
type sprintTick struct {
	Sprint int64  `json:"sprint"`
	As     string `json:"as"`
	// Since is the baseline the deltas are measured from — the manager's last
	// recorded ACTION on this sprint (see the file header), or the current
	// time-box's start when it has taken none.
	Since       time.Time `json:"since"`
	SinceReason string    `json:"since_reason"`

	Mail sprintTickMail `json:"mail"`
	// Board is the story census plus what demonstrably moved since Since.
	Board     sprintTickBoard       `json:"board"`
	Fleet     sprintTickFleet       `json:"fleet"`
	Silent    []sprintTickRun       `json:"silent,omitempty"`
	Review    []sprintTickRun       `json:"review,omitempty"`
	Brief     sprintTickBrief       `json:"brief"`
	Gate      sprintTickGate        `json:"gate"`
	Seat      sprintTickSeat        `json:"seat"`
	Resources SprintResourceSummary `json:"resources"`
}

type sprintTickMail struct {
	Unread int `json:"unread"`
	// Directed is the subset addressed to this manager by name rather than
	// reaching it through a topic or a room. It is broken out because a
	// directed post is somebody waiting on an answer, and a broadcast is not.
	Directed int    `json:"directed"`
	Read     string `json:"read"`
}

type sprintTickBoard struct {
	Open    int `json:"open"`
	Blocked int `json:"blocked"`
	Unowned int `json:"unowned"`
	// Opened and Closed are the delta, and they are TWO COUNTS rather than one
	// "changed" because those are the only two changes a story record can
	// prove. An issue carries Created and Closed and no modified-at field, so a
	// re-title or a re-priority is genuinely invisible here — and a single
	// "changed" number would have implied it was not. Reporting the delta this
	// tool can actually establish, and no more, is the point.
	Opened    int    `json:"opened"`
	Closed    int    `json:"closed"`
	Next      string `json:"next,omitempty"`
	NextTitle string `json:"next_title,omitempty"`
}

type sprintTickFleet struct {
	Ready    int      `json:"ready"`
	Total    int      `json:"total"`
	Cooling  []string `json:"cooling,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	Evidence string   `json:"evidence"`
}

type sprintTickRun struct {
	Repo    string `json:"repo"`
	ID      int64  `json:"id"`
	Owner   string `json:"owner,omitempty"`
	State   string `json:"state"`
	Age     string `json:"age,omitempty"`
	Commits int    `json:"commits"`
}

type sprintTickBrief struct {
	Present bool   `json:"present"`
	Age     string `json:"age,omitempty"`
	Stale   bool   `json:"stale"`
}

type sprintTickGate struct {
	Unchecked []string `json:"unchecked,omitempty"`
	Uncovered []string `json:"uncovered,omitempty"`
	Verdict   string   `json:"verdict"`
}

type sprintTickSeat struct {
	Holder string `json:"holder,omitempty"`
	State  string `json:"state"`
}

func newSprintTickCmd() *cobra.Command {
	var flags weaveOutputFlags
	var as string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "tick <sprint>",
		Short: "One turn's worksheet for the sprint manager — what changed, what needs a decision",
		Long: `tick is the manager's turn loop, gathered into one command.

It answers the seven questions ` + "`sprint --help`" + `'s THE TICK asks, in that
order:

  mail      unread, and how many are addressed to you by name
  board     open / blocked / unowned stories, what changed, what is next
  fleet     who is assignable right now (PATH + cooldown evidence only)
  silent    runs that are RUNNING and have produced NOTHING — alive and stuck
  review    work submitted and waiting on you to merge it
  brief     how old the continuity record is
  gate      unchecked goal items and stories no goal covers

IT GATHERS; IT DOES NOT DECIDE. Prioritising, assigning, merging and
reassigning are judgement plus provider spend, and they stay yours. tick
performs no mutation at all: it does not refresh your lease (so polling it
cannot forge liveness for a manager who has stopped working), does not mark
any mail read, and does not probe the fleet (a probe is a real headless turn
per row — the tick reports installed-and-not-cooling, which is not the same
as signed in).

"WHAT CHANGED" IS MEASURED FROM YOUR LAST ACTION, not your last look. The
baseline is your most recent entry in the sprint thread — a checkpoint, a
comment, a decision. There is no stored tick marker, deliberately: one would
mean writing to the sprint every time somebody read it, and it would let a
manager poll in a loop, see an empty delta every time, and conclude the sprint
was healthy when the delta was empty BECAUSE it kept looking. Measured from
acting, an empty delta means "nothing has moved since you last did something".

With --wait, block until the mail count or the board changes, then print the
worksheet. It is bounded like ` + "`bashy inbox --wait`" + `: the duration is a
CEILING, not a poll interval, and it returns early on the first change. Without
it, tick returns immediately.

Run it at the top of every turn. Act on what it shows, record what you did
(` + "`sprint checkpoint`" + ` or ` + "`sprint comment`" + `), then tick again — the record
you write is what moves the baseline forward.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runSprintTick(cmd, id, as, wait, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&as, "as", "", "manager identity to report for (default: the sprint's owner)")
	cmd.Flags().DurationVar(&wait, "wait", 0, "block up to this long for mail or the board to change, then report")
	return cmd
}

func runSprintTick(cmd *cobra.Command, id int64, as string, wait time.Duration, flags *weaveOutputFlags) error {
	mode := flags.mode()
	dir, err := weaveStoryDir(cmd, mode, "sprint tick")
	if err != nil {
		return err
	}

	tick, err := collectSprintTick(dir, id, as)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "sprint tick", weavecli.ExitGenericFail, err))
	}

	if wait > 0 {
		// A ceiling, not an interval: the first observed change returns. The
		// baseline is the worksheet already in hand, so a change that landed
		// between collect and wait is not missed.
		tick = waitForSprintChange(dir, id, as, tick, wait)
	}

	tick.Resources = sprintResources(cmd, id)
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "sprint tick", tick))
	}
	renderSprintTick(cmd.OutOrStdout(), tick)
	renderSprintResources(cmd.OutOrStdout(), tick.Resources)
	return nil
}

// waitForSprintChange polls the two cheap signals — mail depth and board state —
// until one moves or the ceiling expires.
//
// Only these two are watched, and that is a deliberate narrowing. A run going
// silent is defined by the PASSAGE of time, so waiting on it would return the
// instant the clock crossed the threshold and tell the manager nothing it could
// not have computed. Mail and the board are the signals that arrive from
// somewhere else, which is the only kind worth blocking on.
func waitForSprintChange(dir string, id int64, as string, base sprintTick, ceiling time.Duration) sprintTick {
	deadline := time.Now().Add(ceiling)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > sprintTickPoll {
			remaining = sprintTickPoll
		}
		time.Sleep(remaining)
		next, err := collectSprintTick(dir, id, as)
		if err != nil {
			// A transient read failure is not a change, and it is not a reason
			// to abandon the wait: the sprint file is rewritten in place by
			// other verbs, so a torn read is expected under concurrency.
			continue
		}
		if next.Mail.Unread != base.Mail.Unread ||
			next.Mail.Directed != base.Mail.Directed ||
			next.Board.Open != base.Board.Open ||
			next.Board.Blocked != base.Board.Blocked ||
			next.Board.Opened != base.Board.Opened ||
			next.Board.Closed != base.Board.Closed ||
			next.Board.Next != base.Board.Next {
			return next
		}
		base = next
	}
	return base
}

func collectSprintTick(dir string, id int64, as string) (sprintTick, error) {
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return sprintTick{}, err
	}
	s := findWeaveStory(q, id)
	if s == nil {
		return sprintTick{}, fmt.Errorf("sprint #%d not found", id)
	}
	who := weaveStoryConductorName(s, as)

	t := sprintTick{Sprint: id, As: who}
	t.Since, t.SinceReason = sprintTickBaseline(s, who)
	t.Mail = sprintTickReadMail(who)
	t.Board = sprintTickReadBoard(s, t.Since)
	t.Fleet = sprintTickReadFleet(dir)
	t.Silent, t.Review = sprintTickReadRuns(s)
	t.Brief = sprintTickReadBrief(s)
	t.Gate = sprintTickReadGate(s)
	t.Seat = sprintTickReadSeat(s)
	return t, nil
}

// sprintTickBaseline finds when this manager last ACTED on the sprint.
//
// System entries are skipped: "created in backlog" and the like are written BY
// the sprint about itself, not by a manager doing something, and treating one as
// an action would silently reset the baseline for a manager that has done
// nothing at all.
func sprintTickBaseline(s *weaveStory, who string) (time.Time, string) {
	for i := len(s.Thread) - 1; i >= 0; i-- {
		c := s.Thread[i]
		if c.Kind == "system" {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(c.Author), strings.TrimSpace(who)) {
			continue
		}
		return c.At, "your last " + firstNonEmpty(c.Kind, "entry") + " on the thread"
	}
	if box := currentSprintBox(s); box != nil && !box.StartedAt.IsZero() {
		return box.StartedAt, "this time-box's start (you have recorded nothing yet)"
	}
	return s.Created, "the sprint's creation (you have recorded nothing yet)"
}

func currentSprintBox(s *weaveStory) *weaveStoryBox {
	if len(s.Boxes) == 0 {
		return nil
	}
	return &s.Boxes[len(s.Boxes)-1]
}

// sprintTickReadMail counts without consuming. See prohibition 2 in the header.
func sprintTickReadMail(who string) sprintTickMail {
	m := sprintTickMail{Read: "bashy inbox --as " + who}
	direct, _, err := bus.UnreadNotifications(who)
	if err == nil {
		m.Directed = len(direct)
	}
	pending, err := bus.UnreadPending(who)
	if err == nil {
		m.Unread = len(pending)
	}
	// A record can be represented in both views — the pending buffer
	// materialises addressed backlog — so the unread total is the wider of the
	// two rather than their sum. Overcounting mail is not a harmless error: it
	// is what makes a manager stop trusting the number.
	if m.Directed > m.Unread {
		m.Unread = m.Directed
	}
	return m
}

func sprintTickReadBoard(s *weaveStory, since time.Time) sprintTickBoard {
	b := sprintTickBoard{}
	seen := map[string]bool{}
	var stories []sprintStoryState
	for _, root := range sprintStoryRoots(s) {
		items, err := todopkg.List(todopkg.RepoStore(root), "")
		if err != nil {
			continue
		}
		for _, it := range items {
			if it.Sprint != s.ID {
				continue
			}
			key := root + "\x00" + it.ID
			if seen[key] {
				continue
			}
			seen[key] = true

			if it.Created.After(since) {
				b.Opened++
			}
			if it.Closed != nil && it.Closed.After(since) {
				b.Closed++
			}
			if it.Status == todopkg.StatusDone || it.Status == issue.StatusClosed {
				continue
			}
			b.Open++
			if it.Status == todopkg.StatusBlocked {
				b.Blocked++
			}
			if strings.TrimSpace(it.Assignee) == "" {
				b.Unowned++
			}
			stories = append(stories, sprintStoryState{
				Ref:      sprintStoryRef{Repo: root, ID: it.ID},
				Title:    it.Title,
				Status:   it.Status,
				Priority: it.Priority,
				Seq:      it.Seq,
			})
		}
	}
	// Next must come from the same scan as the counts above. A story is written
	// in two steps by the todo helper, so separately re-reading it could pair a
	// newly visible Next with an older Open count and wake --wait on that torn
	// worksheet.
	sort.SliceStable(stories, func(i, j int) bool {
		if a, b := todopkg.PriorityRank(stories[i].Priority), todopkg.PriorityRank(stories[j].Priority); a != b {
			return a < b
		}
		if stories[i].Seq != stories[j].Seq {
			return stories[i].Seq < stories[j].Seq
		}
		return stories[i].Ref.ID < stories[j].Ref.ID
	})
	for _, story := range stories {
		if story.Status == todopkg.StatusDone || story.Status == issue.StatusClosed || story.Status == todopkg.StatusBlocked {
			continue
		}
		b.Next = story.Ref.ID
		b.NextTitle = story.Title
		break
	}
	return b
}

// sprintTickReadFleet reports assignability from cached, free evidence only.
// See prohibition 3 in the header: probing is a real turn per row.
func sprintTickReadFleet(dir string) sprintTickFleet {
	f := sprintTickFleet{Evidence: "PATH + cooldown only — installed is NOT signed in; run `weave fleet --auth` before you rely on it"}
	roster := weaveFleetRoster("", false)
	now := time.Now()
	cache := loadFleetProbeCache(dir)
	for _, name := range roster {
		row, _ := fleetRowForEntry(dir, name, now, false, cache)
		f.Total++
		switch {
		case !row.Found:
			f.Missing = append(f.Missing, row.Tool)
		case row.CoolingUnit != "":
			f.Cooling = append(f.Cooling, row.Tool)
		default:
			f.Ready++
		}
	}
	// The cache is deliberately not saved. A read that rewrote it would make
	// two concurrent ticks race over a file neither of them changed.
	sort.Strings(f.Missing)
	sort.Strings(f.Cooling)
	return f
}

// sprintTickReadRuns crosses liveness with PRODUCTION — the one thing no other
// command reports. See the header.
func sprintTickReadRuns(s *weaveStory) (silent, review []sprintTickRun) {
	now := time.Now()
	for _, link := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(link)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		it := findWeaveItem(q, link.ID)
		if it == nil {
			continue
		}
		row := sprintTickRun{Repo: link.Repo, ID: link.ID, Owner: it.Owner, State: it.State, Commits: it.CommitsAhead}
		if it.State == "submitted" {
			review = append(review, row)
			continue
		}
		if isTerminalState(it.State) || it.StartedAt.IsZero() {
			continue
		}
		if age := now.Sub(it.StartedAt); age >= sprintSilentAfter && it.CommitsAhead == 0 {
			row.Age = age.Round(time.Minute).String()
			silent = append(silent, row)
		}
	}
	return silent, review
}

func sprintTickReadBrief(s *weaveStory) sprintTickBrief {
	b := sprintTickBrief{Present: strings.TrimSpace(s.Continuity) != ""}
	if !b.Present {
		b.Stale = true
		return b
	}
	for i := len(s.Thread) - 1; i >= 0; i-- {
		if s.Thread[i].Kind == "progress" && strings.Contains(s.Thread[i].Body, "checkpoint") {
			age := time.Since(s.Thread[i].At)
			b.Age = age.Round(time.Minute).String()
			b.Stale = age > sprintContinuityStale
			return b
		}
	}
	// A brief with no checkpoint entry behind it cannot be dated. Unknown age
	// reads as stale rather than fresh: absence of evidence is never success.
	b.Stale = true
	return b
}

func sprintTickReadGate(s *weaveStory) sprintTickGate {
	g := sprintTickGate{Unchecked: sprintUncheckedGoals(s), Uncovered: sprintCoverageProblems(s)}
	switch {
	case len(g.Unchecked) == 0 && len(g.Uncovered) == 0:
		g.Verdict = "every goal item is checked and every story is covered — this sprint can be stopped"
	case len(g.Unchecked) == 0:
		g.Verdict = "goals are checked but stories sit outside the plan — cover them or move them off"
	default:
		g.Verdict = "keep going"
	}
	return g
}

func sprintTickReadSeat(s *weaveStory) sprintTickSeat {
	holder, stale, free := weaveStoryLeaseState(s)
	switch {
	case free:
		return sprintTickSeat{State: "free"}
	case stale:
		return sprintTickSeat{Holder: holder, State: "stale"}
	default:
		return sprintTickSeat{Holder: holder, State: "held"}
	}
}

func renderSprintTick(w io.Writer, t sprintTick) {
	fmt.Fprintf(w, "sprint #%d tick — as %s\n", t.Sprint, t.As)
	fmt.Fprintf(w, "  since:   %s (%s)\n", t.Since.Format("01-02 15:04"), t.SinceReason)

	if t.Mail.Unread == 0 {
		fmt.Fprintln(w, "  mail:    none unread")
	} else {
		fmt.Fprintf(w, "  mail:    %d unread, %d addressed to you — read with `%s`\n", t.Mail.Unread, t.Mail.Directed, t.Mail.Read)
	}

	fmt.Fprintf(w, "  board:   %d open (%d blocked, %d unowned); since then %d opened, %d closed\n",
		t.Board.Open, t.Board.Blocked, t.Board.Unowned, t.Board.Opened, t.Board.Closed)
	if t.Board.Next != "" {
		fmt.Fprintf(w, "  next:    %s — %s\n", t.Board.Next, t.Board.NextTitle)
	}

	fmt.Fprintf(w, "  fleet:   %d/%d assignable", t.Fleet.Ready, t.Fleet.Total)
	if len(t.Fleet.Cooling) > 0 {
		fmt.Fprintf(w, "; cooling: %s", strings.Join(t.Fleet.Cooling, ", "))
	}
	if len(t.Fleet.Missing) > 0 {
		fmt.Fprintf(w, "; not installed: %s", strings.Join(t.Fleet.Missing, ", "))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "           %s\n", t.Fleet.Evidence)

	if len(t.Silent) > 0 {
		fmt.Fprintln(w, "  ── SILENT: running, nothing committed — step in or reassign ──")
		for _, r := range t.Silent {
			fmt.Fprintf(w, "    %s#%d %s (%s) running %s, 0 commits\n", r.Repo, r.ID, firstNonEmpty(r.Owner, "unowned"), r.State, r.Age)
		}
	}
	if len(t.Review) > 0 {
		fmt.Fprintln(w, "  ── SUBMITTED: waiting on you to review and merge ──")
		for _, r := range t.Review {
			fmt.Fprintf(w, "    %s#%d %s — %d commits ahead (`weave pull %d`)\n", r.Repo, r.ID, firstNonEmpty(r.Owner, "unowned"), r.Commits, r.ID)
		}
	}

	switch {
	case !t.Brief.Present:
		fmt.Fprintf(w, "  brief:   NONE — write one now (`sprint checkpoint %d -m '<where it stands>'`); it is what a successor reads cold\n", t.Sprint)
	case t.Brief.Stale:
		fmt.Fprintf(w, "  brief:   %s old — refresh it (`sprint checkpoint %d -m '<where it stands>'`)\n", firstNonEmpty(t.Brief.Age, "undated"), t.Sprint)
	default:
		fmt.Fprintf(w, "  brief:   %s old\n", t.Brief.Age)
	}

	fmt.Fprintf(w, "  gate:    %s\n", t.Gate.Verdict)
	if len(t.Gate.Unchecked) > 0 {
		fmt.Fprintf(w, "           unchecked: %s\n", strings.Join(t.Gate.Unchecked, ", "))
	}
	if len(t.Gate.Uncovered) > 0 {
		fmt.Fprintf(w, "           uncovered by the plan: %s\n", strings.Join(t.Gate.Uncovered, ", "))
	}

	if t.Seat.State != "held" {
		fmt.Fprintf(w, "  seat:    %s%s\n", t.Seat.State, sprintTickSeatHint(t))
	}

	fmt.Fprintf(w, "  next tick: act, record what you did (`sprint checkpoint %d -m …`), then `sprint tick %d` again\n", t.Sprint, t.Sprint)
}

func sprintTickSeatHint(t sprintTick) string {
	switch t.Seat.State {
	case "free":
		return fmt.Sprintf(" — nobody holds this sprint; take it with `sprint start %d --owner <your own name>`", t.Sprint)
	case "stale":
		return fmt.Sprintf(" — %s stopped beating; take it with `sprint start %d --owner <your own name>`", t.Seat.Holder, t.Sprint)
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
