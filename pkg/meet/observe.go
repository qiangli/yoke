package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// A meeting runs unattended for a long time — several agents, several rounds,
// each turn a full model call. `observe` is how a human (or another agent) looks
// in on one WITHOUT joining it: attach, read the whole history, then watch the
// discussion happen, line by line, as the agents write it.
//
// It is strictly read-only. It takes no seat, casts no vote, and writes nothing
// — so any number of observers can attach to the same meeting at once, and
// attaching can never change what the meeting decides. Watching a discussion
// must not perturb it.
//
// It reads two channels the meeting already writes (see live.go): the
// transcript, which is the RECORD (one whole turn per event), and the live
// channel, which is the VIEW (each line as it is written). The history comes
// from the record — it is complete and authoritative — and everything after you
// attach comes from the view, so you are watching rather than reading minutes.

// observePoll is how often the tailer looks for new output. Fast enough that a
// line appears as it is written; slow enough not to spin a core doing it.
const observePoll = 200 * time.Millisecond

// observeIdleNotice is how long the stream may go quiet before the observer is
// told it is still alive. An agent can think for a long time before it writes
// its first line, and silence that looks abnormal gets a working meeting killed.
const observeIdleNotice = 90 * time.Second

func newObserveCmd() *cobra.Command {
	var (
		follow      bool
		fromNow     bool
		tailN       int
		jsonOut     bool
		participant []string
		kinds       []string
	)
	cmd := &cobra.Command{
		Use:     "observe [<room>|<id>]",
		Aliases: []string{"attach", "watch"},
		Short:   "attach to a meeting and watch it happen, live (read-only)",
		Long: "Attach to a meeting and watch it happen.\n\n" +
			"Name the meeting by its ROOM — the short number in `bashy meet list` —\n" +
			"or by its id, or by any unambiguous prefix of one. With no argument, the\n" +
			"most recent open meeting is attached.\n\n" +
			"The whole history is replayed first, in full, so you can see everything\n" +
			"said before you attached. After that you are watching it live: each\n" +
			"agent's answer streams in LINE BY LINE as it writes it, rather than\n" +
			"appearing all at once, minutes later, when the turn completes.\n\n" +
			"Observing is read-only. It takes no seat, casts no vote, and writes\n" +
			"nothing. Any number of observers may attach to one meeting, and\n" +
			"attaching can never change what the meeting decides. It is a transcript\n" +
			"tail, not an actionable inbox; use 'bashy inbox --watch' to follow unread\n" +
			"MB, Meet-board, Bus, and authorized role input across channels.",
		Example: "  bashy meet observe 2\n" +
			"  bashy meet observe\n" +
			"  bashy meet observe --participant Sable\n" +
			"  bashy meet observe 2 --json | jq -r 'select(.kind==\"decision\") | .text'",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var id string
			var err error
			if len(args) == 1 {
				if id, err = resolveMeeting(args[0]); err != nil {
					return err
				}
			} else if id, err = latestOpenMeeting(); err != nil {
				return err
			}
			st, err := loadState(id)
			if err != nil {
				return fmt.Errorf("meet: %s: %w", id, err)
			}
			only, err := observeFilter(st, participant, kinds)
			if err != nil {
				return err
			}

			// ^C detaches the observer; it must never take the meeting down with
			// it. The meeting is another process — this only stops watching.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			return observeMeeting(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), st, observeOpts{
				follow: follow, fromNow: fromNow, tail: tailN, json: jsonOut,
				only: only, kinded: len(kinds) > 0,
			})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", true, "keep watching (--follow=false prints the history and exits)")
	f.BoolVar(&fromNow, "from-now", false, "skip the history and show only what is said from here on")
	f.IntVar(&tailN, "tail", 0, "replay only the last N events of the history (0 = the whole history)")
	f.BoolVar(&jsonOut, "json", false, "emit the canonical transcript Events, one per line, for piping")
	f.StringArrayVar(&participant, "participant", nil, "only this seat — accepts a nickname or alias (repeatable)")
	f.StringArrayVar(&kinds, "kind", nil, "only these event kinds, e.g. turn, decision, action (repeatable)")
	return cmd
}

// latestOpenMeeting picks the meeting `observe` attaches to when told no id.
//
// It refuses rather than guessing when nothing is open: attaching to a meeting
// that closed last week, and watching it not move, looks exactly like attaching
// to a live meeting where nobody is saying anything.
func latestOpenMeeting() (string, error) {
	sessions, err := listSessions()
	if err != nil {
		return "", err
	}
	for _, s := range sessions { // newest-first
		if s.Status == "open" {
			return s.ID, nil
		}
	}
	if len(sessions) > 0 {
		return "", fmt.Errorf("meet: no meeting is open; `bashy meet observe %s` attaches to the most recent closed one", sessions[0].ID)
	}
	return "", fmt.Errorf("meet: no meetings on this host — `bashy meet list`")
}

// observeFilter builds the seat/kind predicate, and REFUSES a seat nobody holds.
//
// A typo'd --participant that silently matched nothing would leave the observer
// staring at a blank screen, concluding the meeting was quiet when it was in
// fact talking to somebody else. An empty view must never be reachable by a
// mistake — only by a meeting that has genuinely said nothing.
func observeFilter(st *State, participants, kinds []string) (func(kind, speaker string) bool, error) {
	seats := map[string]bool{}
	var humans []string
	for _, p := range participants {
		canon := canonAgent(p)
		// Human observer names are not fleet aliases. Check the stored observer
		// spelling too, so an observer remains filterable even if a later fleet
		// entry happens to use a matching nickname.
		if !st.seated(canon) && !st.humanAttendee(p) {
			return nil, fmt.Errorf("meet: %q is not seated at this meeting (roster: %s)",
				p, strings.Join(st.attendees(), ", "))
		}
		if st.humanAttendee(p) {
			humans = append(humans, strings.TrimSpace(p))
		} else {
			seats[canon] = true
		}
	}
	kindSet := map[string]bool{}
	for _, k := range kinds {
		kindSet[strings.TrimSpace(k)] = true
	}
	return func(kind, speaker string) bool {
		if len(seats)+len(humans) > 0 && !seats[speaker] {
			found := false
			for _, human := range humans {
				if strings.EqualFold(human, speaker) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		if len(kindSet) > 0 && !kindSet[kind] {
			return false
		}
		return true
	}, nil
}

type observeOpts struct {
	follow  bool
	fromNow bool
	tail    int
	json    bool
	kinded  bool // --kind was given, so the caller wants the record, not the live view
	only    func(kind, speaker string) bool
}

func observeMeeting(ctx context.Context, out, errW io.Writer, st *State, opt observeOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	dir, err := storeDir(st.ID)
	if err != nil {
		return err
	}
	rec := &lineTail{path: filepath.Join(dir, "transcript.jsonl")}
	live := &lineTail{path: filepath.Join(dir, "live.jsonl")}

	// The live channel is followed forward only, never replayed: its history is
	// already in the transcript as whole turns, and replaying both would print
	// the meeting twice.
	live.skipToEnd()

	// --json emits the CANONICAL transcript events, and only those. The live
	// channel is a human view with its own record shape; mixing the two into one
	// stream would hand a parser two schemas and no way to know which it has.
	// --kind likewise says "show me the record, filtered", not "let me watch".
	watchLive := opt.follow && !opt.json && !opt.kinded

	if opt.fromNow {
		rec.skipToEnd()
	}
	if !opt.json {
		observeHeader(out, st, opt, watchLive)
	}

	backlog, err := readEvents(rec)
	if err != nil {
		return err
	}
	backlog = filterEvents(backlog, opt.only)
	if opt.tail > 0 && len(backlog) > opt.tail {
		backlog = backlog[len(backlog)-opt.tail:]
	}
	for _, e := range backlog {
		writeEvent(out, e, opt.json)
	}
	if !opt.json && !opt.fromNow {
		fmt.Fprintf(errW, "─── %d event(s) of history · %s ───\n\n",
			len(backlog), followNote(st, opt.follow))
	}
	if !opt.follow {
		return nil
	}

	// streamed remembers turns the observer watched arrive line by line, so the
	// whole-turn event that lands in the transcript afterwards is not printed a
	// second time. Only turns we saw START are marked: an observer that attached
	// mid-turn saw part of it, and must get the complete text from the record
	// rather than a truncated live view.
	streamed := map[string]bool{}
	idle := time.Now()

	for {
		select {
		case <-ctx.Done():
			// Detaching is not an outcome. Say so, or a reader of the scrollback
			// cannot tell "I stopped watching" from "the meeting ended".
			if !opt.json {
				fmt.Fprintf(errW, "\ndetached — the meeting keeps running (`bashy meet observe %s` to re-attach)\n", st.ID)
			}
			return nil
		case <-time.After(observePoll):
		}

		spoke := false
		if watchLive {
			lines, err := readLive(live)
			if err != nil {
				return err
			}
			for _, l := range lines {
				if opt.only != nil && !opt.only("turn", l.Speaker) {
					continue
				}
				if writeLive(out, l, streamed) {
					spoke = true
				}
			}
		}

		events, err := readEvents(rec)
		if err != nil {
			return err
		}
		for _, e := range filterEvents(events, opt.only) {
			if streamed[turnKey(e.Round, e.Speaker)] && !opt.json {
				// Already watched, live. Print the epilogue, not the text again.
				writeTurnFooter(out, e)
				continue
			}
			writeEvent(out, e, opt.json)
			spoke = true
		}

		// An agent can think for minutes before writing its first line. Without a
		// sign of life that is indistinguishable from a wedged meeting, and the
		// observer kills a run that was working. Stderr, so --json stays clean.
		if spoke {
			idle = time.Now()
		} else if !opt.json && time.Since(idle) >= observeIdleNotice {
			fmt.Fprintf(errW, "  … still watching — nothing said for %s\n", time.Since(idle).Round(time.Second))
			idle = time.Now()
		}

		// Re-read the header, not a cached copy: the meeting is another process,
		// and its status is the only thing that says the stream has ended. Drain
		// first, then stop — the closing turns are written before the status
		// flips, and exiting on the flag alone would truncate the last word.
		if cur, err := loadState(st.ID); err == nil && cur.Status != "open" {
			rest, err := readEvents(rec)
			if err != nil {
				return err
			}
			for _, e := range filterEvents(rest, opt.only) {
				if streamed[turnKey(e.Round, e.Speaker)] && !opt.json {
					writeTurnFooter(out, e)
					continue
				}
				writeEvent(out, e, opt.json)
			}
			if !opt.json {
				fmt.Fprintf(errW, "\n─── meeting %s ───\n", cur.Status)
			}
			return nil
		}
	}
}

func turnKey(round int, speaker string) string { return fmt.Sprintf("%d|%s", round, speaker) }

func filterEvents(in []Event, only func(kind, speaker string) bool) []Event {
	if only == nil {
		return in
	}
	out := make([]Event, 0, len(in))
	for _, e := range in {
		if only(e.Kind, e.Speaker) {
			out = append(out, e)
		}
	}
	return out
}

func followNote(st *State, follow bool) string {
	if !follow {
		return "not following"
	}
	if st.Status != "open" {
		return "meeting already " + st.Status
	}
	return "watching live · ^C to detach"
}

func observeHeader(w io.Writer, st *State, opt observeOpts, live bool) {
	fmt.Fprintf(w, "meet: observing %s  (read-only)\n", st.ID)
	fmt.Fprintf(w, "  topic    %s\n", st.Topic)
	fmt.Fprintf(w, "  status   %s · round %d\n", st.Status, st.Round)
	fmt.Fprintf(w, "  roster   %s\n", strings.Join(st.attendees(), ", "))
	if opt.tail > 0 {
		fmt.Fprintf(w, "  history  last %d event(s)\n", opt.tail)
	}
	if live {
		fmt.Fprintln(w, "  live     turns stream line by line as agents write them")
	}
	fmt.Fprintln(w)
}

// writeLive renders one record of the live channel. It reports whether anything
// was shown, so the idle notice can tell a quiet meeting from a busy one.
func writeLive(w io.Writer, l LiveEvent, streamed map[string]bool) bool {
	key := turnKey(l.Round, l.Speaker)
	switch l.Kind {
	case liveSpeaking:
		// Mark the turn only when we saw it BEGIN. A turn we joined halfway
		// through must still be printed in full from the record.
		streamed[key] = true
		fmt.Fprintf(w, "[r%d %s] %s\n", l.Round, l.TS.Local().Format("15:04:05"), seatLabel(l.Speaker))
		return true
	case liveLine:
		if !streamed[key] {
			return false // mid-turn attach: the record will carry the whole thing
		}
		// A live line recorded by an older build may still be a raw transport
		// envelope; extract the assistant text and drop machine events. Clean text
		// passes through unchanged.
		text, keep := normalizeLiveLine(l.Text)
		if !keep || strings.TrimSpace(text) == "" {
			return false
		}
		for ln := range strings.SplitSeq(text, "\n") {
			fmt.Fprintf(w, "  %s\n", ln)
		}
		return true
	case liveSpoke:
		if !streamed[key] {
			return false
		}
		fmt.Fprintln(w)
		return true
	}
	return false
}

// writeTurnFooter closes out a turn the observer already watched arrive: the
// text is on screen, so only what the live channel could not know is added.
func writeTurnFooter(w io.Writer, e Event) {
	if s := statusOf(e); s != "" && s != statusOK {
		fmt.Fprintf(w, "  (%s)\n\n", s)
	}
}

// writeEvent renders one turn IN FULL. The collapsing that `meet show` does is
// right for a summary and wrong here: an observer attached to read the log wants
// the log, not a preview of it.
func writeEvent(w io.Writer, e Event, asJSON bool) {
	// Normalize at render too, not only at capture: a turn recorded by an older
	// build (or a meeting already running when this landed) still holds raw
	// transport in its Text. Extracting the assistant prose here means both the
	// human view AND `--json` emit the canonical meeting event with readable text,
	// never the raw nested provider envelope. On already-clean text this is a
	// no-op. e is a value copy, so the stored record is untouched.
	e.Text = normalizeTurnText(e.Text)
	if asJSON {
		if b, err := json.Marshal(e); err == nil {
			fmt.Fprintln(w, string(b))
		}
		return
	}

	who := seatLabel(e.Speaker)
	switch e.Kind {
	case "message":
		// A board post is directed: name its audience, not just its author.
		if to := messageTo(e); to != "" {
			who += " → " + seatLabel(to)
		} else {
			who += " → room"
		}
	case "decision":
		who = "DECISION"
	case "action":
		who = "ACTION"
	case "ledger":
		who = "FACILITATOR"
	case "replan":
		who = "FACILITATOR (new approach)"
	}

	head := fmt.Sprintf("[r%d %s] %s", e.Round, e.TS.Local().Format("15:04:05"), who)
	if s := statusOf(e); s != "" && s != statusOK {
		head += "  (" + s + ")"
	}
	if e.DurMS > 0 {
		head += fmt.Sprintf("  %.1fs", float64(e.DurMS)/1000)
	}
	fmt.Fprintln(w, head)

	text := strings.TrimRight(e.Text, "\n")
	if text == "" {
		text = "(nothing said)"
	}
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}
	fmt.Fprintln(w)
}

// messageTo names the recipient of a board message. The recipient rides the
// event's "to" wire field, which belongs to the board spine's Event definition
// (a different workstream owns session.go); reading it from the event's wire
// form keeps this renderer correct whether or not that field is compiled in
// yet. Empty means the post addressed the whole room.
func messageTo(e Event) string {
	b, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	s, _ := m["to"].(string)
	return s
}

func readEvents(t *lineTail) ([]Event, error) {
	lines, err := t.next()
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(lines))
	for _, ln := range lines {
		var e Event
		if err := json.Unmarshal(ln, &e); err != nil {
			continue // a torn line is skipped, never guessed at
		}
		out = append(out, e)
	}
	return out, nil
}

func readLive(t *lineTail) ([]LiveEvent, error) {
	lines, err := t.next()
	if err != nil {
		return nil, err
	}
	out := make([]LiveEvent, 0, len(lines))
	for _, ln := range lines {
		var e LiveEvent
		if err := json.Unmarshal(ln, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// lineTail reads an append-only file forward, returning whatever COMPLETE lines
// have appeared since the last call.
//
// It tracks a byte offset rather than re-reading the file, so a long meeting does
// not get quadratically slower to watch. A trailing partial line is held back and
// completed on the next pass: the writer appends whole records, but a reader can
// still catch one mid-flight, and half a record parsed as a whole one would be a
// silently corrupt turn in the observer's log.
type lineTail struct {
	path string
	off  int64
	rem  []byte
}

func (t *lineTail) skipToEnd() {
	if fi, err := os.Stat(t.path); err == nil {
		t.off = fi.Size()
	}
}

func (t *lineTail) next() ([][]byte, error) {
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing written yet
		}
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	t.off += int64(len(chunk))

	data := append(t.rem, chunk...)
	cut := bytes.LastIndexByte(data, '\n')
	if cut < 0 {
		t.rem = data
		return nil, nil
	}
	t.rem = append([]byte{}, data[cut+1:]...)

	var out [][]byte
	for _, line := range bytes.Split(data[:cut], []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out = append(out, append([]byte{}, line...))
	}
	return out, nil
}
