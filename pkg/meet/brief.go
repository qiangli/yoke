package meet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Participant catch-up brief (Sprint 666 S2).
//
// A brief tells a returning human or a replacement agent what changed in a
// room and what needs them, without replaying the transcript. It has two
// halves: the since-last-seen window (what is new for this reader) and the
// standing room state (confirmed decisions, open proposals, unresolved
// objections, owned work, pending questions), each item carrying the room,
// sequence number and timestamp that locate it in the transcript.
//
// Three rules shape everything here:
//
//   - Permission-filtered. Content addressed to somebody else is excluded on
//     the same terms as the board read path: a directed message is visible
//     only to its addressee, and the brief never leaks it into another
//     reader's sections or counts.
//   - Authority is labeled, never inferred. A human-marked decision or action
//     is authoritative; a secretary synthesis is a proposal no matter how
//     confident it sounds. The brief preserves disagreement: dissenting votes,
//     retractions and flagged risks stay visible beside what they oppose.
//   - Bounded. Every list is capped and reports how many items it hid, so a
//     long absence reads as a count plus a pointer to `meet read` / `meet
//     observe`, never as a silent truncation.
//
// Decision references coordinate with S1 action adoption (Sprint 666 S1):
// briefActionID is the same stable identity S1 adopts by, so an action in a
// brief and a story filed from it name the same act-... id. If S1's helper
// becomes importable, this copy collapses into it.

// BriefLimit caps every list in a brief. Truncated items are reported as
// hidden counts, never dropped silently.
const BriefLimit = 20

// Brief authority labels. They describe who stands behind an item, so a
// reader can tell a confirmation from a proposal at a glance.
const (
	briefAuthorityHuman     = "human"               // a person marked it: authoritative
	briefAuthorityChair     = "chair"               // procedural: a poll or question, not a ruling
	briefAuthorityVoice     = "participant"         // a seat's voice: dissent, never authority
	briefAuthoritySynthesis = "secretary-synthesis" // extracted summary: a proposal, never authority
)

// BriefOptions selects the reader and the window. A zero value is usable:
// the window starts at the reader's seen cursor.
type BriefOptions struct {
	// Reader is who the brief is for, named like --as. Required: filtering
	// and the cursor are meaningless without it.
	Reader string
	// Limit caps every list. Zero or negative means BriefLimit.
	Limit int
	// SinceSeq starts the new-for-you window at this transcript sequence
	// instead of the reader's seen cursor. Zero means the cursor.
	SinceSeq int64
	// All renders the whole visible history as the window, ignoring the cursor.
	All bool
	// MarkSeen advances the reader's seen cursor through the rendered
	// snapshot, like `meet read`. Default false: summarizing must not consume
	// the detailed inbox the summary points at.
	MarkSeen bool
}

// BriefItem is one entry in a brief: the text plus where it lives.
type BriefItem struct {
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	Speaker   string    `json:"speaker"`
	To        string    `json:"to,omitempty"`
	Authority string    `json:"authority"`
	Source    string    `json:"source,omitempty"` // agreed|proposed on actions
	ID        string    `json:"id,omitempty"`     // act-... on actions: the S1 decision reference
	Text      string    `json:"text"`
	TS        time.Time `json:"ts"`
	Directed  bool      `json:"directed,omitempty"` // addressed to this reader
	New       bool      `json:"new,omitempty"`      // inside the since-last-seen window
	Owner     string    `json:"owner,omitempty"`    // on owned work
	Mine      bool      `json:"mine,omitempty"`     // owned by this reader
}

// BriefSection is one bounded list plus how many items it hid.
type BriefSection struct {
	Items  []BriefItem `json:"items"`
	Hidden int         `json:"hidden,omitempty"`
}

// BriefPoll is a pending poll with the votes the transcript holds for it.
type BriefPoll struct {
	BriefItem
	Question string         `json:"question"`
	Choices  []string       `json:"choices,omitempty"`
	Tally    map[string]int `json:"tally,omitempty"`
	Votes    int            `json:"votes"`
}

// BriefResult is the whole brief: the window, the standing state, and the
// counts that let a replacement agent see what needs them.
type BriefResult struct {
	Room   string `json:"room"`
	Reader string `json:"reader"`
	Seen   int64  `json:"seen"`
	// Through is the transcript high-water mark this snapshot represents.
	// A reader that rendered the whole snapshot acknowledges exactly this.
	Through int64 `json:"through"`
	// New counts visible events in the window by kind.
	New map[string]int `json:"new"`
	// Changes is the bounded since-last-seen window itself.
	Changes BriefSection `json:"changes"`
	// Decisions are human-marked confirmations: authoritative.
	Decisions BriefSection `json:"decisions"`
	// Proposals are agreed actions and secretary-extracted candidates.
	// Only a human confirmation adopts one; listing is not authority.
	Proposals BriefSection `json:"proposals"`
	// Objections preserve disagreement: retractions, no-votes, flagged risks.
	Objections BriefSection `json:"objections"`
	// Owned names who holds each agreed action; Mine flags the reader's own.
	Owned BriefSection `json:"owned"`
	// Questions are open polls, asked questions and synthesis open questions.
	Questions []BriefPoll `json:"questions"`
	// QuestionsHidden counts questions beyond the limit.
	QuestionsHidden int `json:"questions_hidden,omitempty"`
}

// briefActionID is the stable identity of an action within a room: the S1
// adoption reference. It intentionally mirrors S1's actionID (adopt.go) so a
// brief and an adoption name the same candidate; collapse the two when S1's
// helper is importable from this package's tree.
func briefActionID(room, text string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
	sum := sha256.Sum256([]byte(room + "\x00" + norm))
	return "act-" + hex.EncodeToString(sum[:])[:10]
}

// briefSplitOwner reads the "owner: task" convention S1 adopts by. Only a
// single-token prefix counts as an owner, so prose with a colon is not
// misread. It mirrors S1's splitOwner (adopt.go); collapse the two with the
// ID helper above.
func briefSplitOwner(text string) (owner, task string) {
	if head, rest, ok := strings.Cut(text, ":"); ok {
		head = strings.TrimSpace(head)
		if head != "" && !strings.ContainsAny(head, " \t") && strings.TrimSpace(rest) != "" {
			return head, strings.TrimSpace(rest)
		}
	}
	return "", strings.TrimSpace(text)
}

// briefVisible reports whether an event is room state this reader may see.
// It hides only what is addressed to somebody else; a reader's own posts are
// room state and stay. The stricter inbound test (which also drops the
// reader's own posts) is briefNew, used for the since-last-seen window.
func briefVisible(e Event, reader string) bool {
	if addressedEvent(e) && !directedEvent(e, reader) {
		return false
	}
	return true
}

// briefNew reports whether an event is new inbound work for this reader, on
// the board read path's terms: the reader's own posts are not news, and mail
// addressed to somebody else is not theirs to see.
func briefNew(e Event, reader string) bool {
	if authoredBy(e, reader) {
		return false
	}
	if directedEvent(e, reader) {
		return true
	}
	if addressedEvent(e) {
		return false
	}
	return true
}

// briefItem renders one transcript event with its source link.
func briefItem(seq int64, e Event, reader, authority string) BriefItem {
	it := BriefItem{
		Seq: seq, Kind: e.Kind, Speaker: e.Speaker, To: strings.TrimSpace(e.To),
		Authority: authority, Text: redactHome(strings.TrimSpace(e.Text)),
		TS: e.TS, Directed: directedEvent(e, reader),
	}
	if it.Directed && strings.EqualFold(strings.TrimSpace(e.To), AllSeats) {
		it.Directed = false // a broadcast reaches everyone; it singles out nobody
	}
	return it
}

// capBrief bounds a list and reports what it hid.
func capBrief(items []BriefItem, limit int) BriefSection {
	if limit <= 0 {
		limit = BriefLimit
	}
	if len(items) > limit {
		return BriefSection{Items: items[:limit], Hidden: len(items) - limit}
	}
	return BriefSection{Items: items}
}

// Brief builds the catch-up brief for reader in the room named by ref.
//
// It is read-only except when MarkSeen is set, in which case it acknowledges
// exactly the snapshot it rendered; events appended concurrently stay unread.
func Brief(ref string, opt BriefOptions) (*BriefResult, error) {
	reader := canonAgent(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(opt.Reader), "@")))
	if reader == "" {
		return nil, fmt.Errorf("meet: a brief needs --as NAME: filtering and the cursor belong to somebody")
	}
	limit := opt.Limit
	if limit <= 0 {
		limit = BriefLimit
	}
	st, err := roomOf(ref)
	if err != nil {
		return nil, err
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil {
		return nil, err
	}
	through := int64(len(events))

	since := opt.SinceSeq
	if !opt.All && since <= 0 {
		since = SeenSeq(st.ID, reader)
	}

	res := &BriefResult{
		Room: st.ID, Reader: reader, Seen: SeenSeq(st.ID, reader),
		Through: through, New: map[string]int{},
	}

	// A retraction withdraws its target from the standing state. The
	// transcript key is the retracted record's timestamp, the same key the
	// transcript and the browser dedupe on.
	retracted := map[string]bool{}
	for _, e := range events {
		if e.Kind == "retraction" && strings.TrimSpace(e.Retracts) != "" {
			retracted[strings.TrimSpace(e.Retracts)] = true
		}
	}
	withdrawn := func(e Event) bool {
		return retracted[e.TS.Format(time.RFC3339Nano)]
	}

	var changes []BriefItem
	for i, e := range events {
		seq := int64(i + 1)
		if seq <= since || !briefNew(e, reader) {
			continue
		}
		res.New[e.Kind]++
		it := briefItem(seq, e, reader, briefWindowAuthority(e))
		it.New = true
		changes = append(changes, it)
	}
	res.Changes = capBrief(changes, limit)

	var decisions, proposals, objections, owned []BriefItem
	var questions []BriefPoll
	for i, e := range events {
		seq := int64(i + 1)
		if !briefVisible(e, reader) {
			continue
		}
		isNew := seq > since && briefNew(e, reader)
		switch e.Kind {
		case "decision":
			if withdrawn(e) {
				break
			}
			it := briefItem(seq, e, reader, briefAuthorityHuman)
			it.New = isNew
			decisions = append(decisions, it)
		case "action":
			if withdrawn(e) {
				break
			}
			it := briefItem(seq, e, reader, briefAuthorityHuman)
			it.Source = "agreed"
			it.ID = briefActionID(st.ID, e.Text)
			it.New = isNew
			proposals = append(proposals, it)
			owner, _ := briefSplitOwner(e.Text)
			if owner != "" {
				work := it
				work.Owner = owner
				work.Mine = briefOwns(reader, owner)
				owned = append(owned, work)
			}
		case "retraction":
			it := briefItem(seq, e, reader, briefAuthorityVoice)
			it.New = isNew
			objections = append(objections, it)
		case "vote":
			if briefDissent(e) {
				it := briefItem(seq, e, reader, briefAuthorityVoice)
				it.New = isNew
				objections = append(objections, it)
			}
		case "poll", "question", "ask":
			if e.Kind == "ask" && addressedEvent(e) && !directedEvent(e, reader) {
				break // another reader's asked question: handled above, kept explicit
			}
			q := BriefPoll{
				BriefItem: briefItem(seq, e, reader, briefAuthorityChair),
				Question:  redactHome(firstLine(questionOf(e))),
				Choices:   append([]string(nil), e.Choices...),
				Tally:     map[string]int{},
			}
			q.BriefItem.New = isNew
			questions = append(questions, q)
		}
	}

	// Votes in the transcript are tallied onto the most recent preceding poll
	// that ran the same ballot. Positional, not inferred: a vote names its
	// choices but not its question, so the ballot it answered is the last one
	// with those choices.
	for i, e := range events {
		if e.Kind != "vote" || !briefVisible(e, reader) {
			continue
		}
		for q := len(questions) - 1; q >= 0; q-- {
			if int64(questionSeq(events, questions[q])) <= int64(i+1) && sameChoices(questions[q].Choices, e.Choices) {
				choice := strings.TrimSpace(e.Choice)
				if choice == "" {
					choice = "no-choice"
				}
				questions[q].Tally[choice]++
				questions[q].Votes++
				break
			}
		}
	}

	// The secretary's synthesis is a proposal under a human's brief, never a
	// confirmation. Its decisions join proposals, its open questions join
	// questions, and its risks and corrections join objections — each labeled
	// so no reader mistakes extraction for authority.
	if syn := loadSynthesis(st.ID); syn != nil {
		for _, d := range syn.Decisions {
			text := strings.TrimSpace(d.Text)
			if text == "" {
				continue
			}
			proposals = append(proposals, BriefItem{
				Kind: "decision", Speaker: "secretary",
				Authority: briefAuthoritySynthesis, Source: "proposed",
				Text: redactHome(text), TS: syn.At,
			})
		}
		for _, a := range syn.Actions {
			text := strings.TrimSpace(a)
			if text == "" || briefWithdrawnText(events, st.ID, text) {
				continue
			}
			it := BriefItem{
				Kind: "action", Speaker: "secretary",
				Authority: briefAuthoritySynthesis, Source: "proposed",
				ID:   briefActionID(st.ID, text),
				Text: redactHome(text), TS: syn.At,
			}
			proposals = append(proposals, it)
			if owner, _ := briefSplitOwner(text); owner != "" {
				it.Owner = owner
				it.Mine = briefOwns(reader, owner)
				owned = append(owned, it)
			}
		}
		for _, o := range syn.OpenQuestions {
			if strings.TrimSpace(o) == "" {
				continue
			}
			questions = append(questions, BriefPoll{
				BriefItem: BriefItem{
					Kind: "question", Speaker: "secretary",
					Authority: briefAuthoritySynthesis,
					Text:      redactHome(strings.TrimSpace(o)), TS: syn.At,
				},
				Question: redactHome(strings.TrimSpace(o)),
			})
		}
		for _, r := range append(append([]string{}, syn.Risks...), syn.Corrections...) {
			if strings.TrimSpace(r) == "" {
				continue
			}
			objections = append(objections, BriefItem{
				Kind: "note", Speaker: "secretary",
				Authority: briefAuthoritySynthesis,
				Text:      redactHome(strings.TrimSpace(r)), TS: syn.At,
			})
		}
	}

	res.Decisions = capBrief(decisions, limit)
	res.Proposals = capBrief(proposals, limit)
	res.Objections = capBrief(objections, limit)
	res.Owned = capBrief(owned, limit)
	if len(questions) > limit {
		res.Questions = questions[:limit]
		res.QuestionsHidden = len(questions) - limit
	} else {
		res.Questions = questions
	}
	if res.Questions == nil {
		res.Questions = []BriefPoll{}
	}

	if opt.MarkSeen {
		if err := MarkSeenThrough(st.ID, reader, through); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// briefWindowAuthority labels an event inside the changes window by the same
// standing rules the sections use.
func briefWindowAuthority(e Event) string {
	switch e.Kind {
	case "decision", "action", "agenda", "confirm":
		return briefAuthorityHuman
	case "poll", "question", "ask":
		return briefAuthorityChair
	case "retraction", "vote":
		return briefAuthorityVoice
	default:
		return briefAuthorityVoice
	}
}

// briefDissent reports whether a vote is a preserved disagreement: an explicit
// no on its ballot. Any other choice, and any ballot the vote did not answer,
// is not an objection — the brief must not manufacture dissent.
func briefDissent(e Event) bool {
	return strings.EqualFold(strings.TrimSpace(e.Choice), "no")
}

// briefOwns reports whether an owner prefix names this reader.
func briefOwns(reader, owner string) bool {
	owner = strings.TrimSpace(strings.TrimPrefix(owner, "@"))
	return owner != "" && strings.EqualFold(owner, reader)
}

// briefWithdrawnText reports whether a synthesis-proposed action was withdrawn
// in the room: a retraction of the agreed record with the same S1 identity.
func briefWithdrawnText(events []Event, room, text string) bool {
	want := briefActionID(room, text)
	retracted := map[string]bool{}
	for _, e := range events {
		if e.Kind == "retraction" && strings.TrimSpace(e.Retracts) != "" {
			retracted[strings.TrimSpace(e.Retracts)] = true
		}
	}
	for _, e := range events {
		if e.Kind == "action" && retracted[e.TS.Format(time.RFC3339Nano)] &&
			briefActionID(room, e.Text) == want {
			return true
		}
	}
	return false
}

// questionOf renders the question a poll, ask or question event puts.
func questionOf(e Event) string {
	if q := strings.TrimSpace(e.Question); q != "" {
		return q
	}
	return e.Text
}

// questionSeq recovers a poll item's transcript sequence. Items built from
// synthesis carry Seq 0 and never match a transcript position.
func questionSeq(events []Event, q BriefPoll) int {
	if q.Seq > 0 {
		return int(q.Seq)
	}
	return len(events) + 1
}

// sameChoices reports whether two ballots offer the same answers.
func sameChoices(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// writeBrief renders a brief for a human: the window first, then the standing
// state, every item carrying the room and sequence that locate it.
func writeBrief(w io.Writer, res *BriefResult) {
	fmt.Fprintf(w, "brief: room %s for %s — seen through %d, snapshot at %d\n",
		res.Room, res.Reader, res.Seen, res.Through)
	if len(res.New) == 0 {
		fmt.Fprintln(w, "nothing new since you last looked")
	} else {
		parts := make([]string, 0, len(res.New))
		for kind, n := range res.New {
			parts = append(parts, fmt.Sprintf("%d %s", n, kind))
		}
		fmt.Fprintf(w, "new since last seen: %s\n", strings.Join(parts, ", "))
	}
	fmt.Fprintln(w)
	writeBriefSection(w, "changes since last seen", res.Changes)
	writeBriefSection(w, "confirmed decisions (human authority)", res.Decisions)
	writeBriefSection(w, "proposals (confirmation adopts; listing does not)", res.Proposals)
	writeBriefSection(w, "unresolved objections (disagreement preserved)", res.Objections)
	writeBriefSection(w, "owned work", res.Owned)
	fmt.Fprintf(w, "pending questions (%d", len(res.Questions))
	if res.QuestionsHidden > 0 {
		fmt.Fprintf(w, ", %d hidden by --limit", res.QuestionsHidden)
	}
	fmt.Fprintln(w, ")")
	for _, q := range res.Questions {
		writeBriefItem(w, q.BriefItem)
		if len(q.Choices) > 0 {
			fmt.Fprintf(w, "    ballot [%s]", strings.Join(q.Choices, " | "))
			if q.Votes > 0 {
				tally := make([]string, 0, len(q.Tally))
				for c, n := range q.Tally {
					tally = append(tally, fmt.Sprintf("%s=%d", c, n))
				}
				fmt.Fprintf(w, "  votes: %s", strings.Join(tally, ", "))
			} else {
				fmt.Fprintf(w, "  no votes recorded")
			}
			fmt.Fprintln(w)
		}
	}
	if len(res.Questions) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
}

func writeBriefSection(w io.Writer, title string, sec BriefSection) {
	fmt.Fprintf(w, "%s (%d", title, len(sec.Items))
	if sec.Hidden > 0 {
		fmt.Fprintf(w, ", %d hidden by --limit", sec.Hidden)
	}
	fmt.Fprintln(w, ")")
	if len(sec.Items) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, it := range sec.Items {
		writeBriefItem(w, it)
	}
}

func writeBriefItem(w io.Writer, it BriefItem) {
	where := ""
	if it.Seq > 0 {
		where = fmt.Sprintf("  [#%d %s]", it.Seq, it.TS.Local().Format("2006-01-02 15:04"))
	} else {
		where = fmt.Sprintf("  [synthesis %s]", it.TS.Local().Format("2006-01-02 15:04"))
	}
	head := it.Speaker
	if head == "" {
		head = "room"
	}
	marks := []string{it.Authority}
	if it.Source != "" {
		marks = append(marks, it.Source)
	}
	if it.ID != "" {
		marks = append(marks, it.ID)
	}
	if it.Directed {
		marks = append(marks, "for you")
	}
	if it.New {
		marks = append(marks, "new")
	}
	if it.Owner != "" {
		owner := "owner " + it.Owner
		if it.Mine {
			owner += " (you)"
		}
		marks = append(marks, owner)
	}
	to := ""
	if strings.TrimSpace(it.To) != "" {
		to = " → " + it.To
	}
	fmt.Fprintf(w, "  - [%s] %s%s%s (%s)\n", it.Kind, head, to, where, strings.Join(marks, ", "))
	text := firstLine(it.Text)
	if text == "" {
		text = "(nothing said)"
	}
	fmt.Fprintf(w, "    %s\n", text)
}

func newBriefCmd() *cobra.Command {
	var as string
	var limit int
	var sinceSeq int64
	var all, markSeen, jsonOut bool
	cmd := &cobra.Command{
		Use:   "brief <room>|<id> --as NAME [--limit N] [--since-seq N] [--all] [--mark-seen] [--json]",
		Short: "catch up on a room: what changed and what needs you (read-only)",
		Long: "Print a bounded, permission-filtered catch-up brief for one participant.\n\n" +
			"The brief shows what is new since the reader last looked, the room's\n" +
			"confirmed decisions, open proposals, unresolved objections, owned work\n" +
			"and pending questions — every item carrying the room and transcript\n" +
			"sequence that locate it. Content addressed to somebody else is\n" +
			"excluded. Human-marked decisions are authoritative; secretary synthesis\n" +
			"is labeled a proposal and never confirms anything.\n\n" +
			"The brief advances nothing by default: use --mark-seen to move the\n" +
			"reader's cursor through the rendered snapshot, like `meet read`.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return fmt.Errorf("meet: --limit cannot be negative")
			}
			if sinceSeq < 0 {
				return fmt.Errorf("meet: --since-seq cannot be negative")
			}
			res, err := Brief(args[0], BriefOptions{
				Reader: readerName(as), Limit: limit,
				SinceSeq: sinceSeq, All: all, MarkSeen: markSeen,
			})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			writeBrief(w, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "participant to brief (required)")
	cmd.Flags().IntVarP(&limit, "limit", "n", BriefLimit, "maximum items per list")
	cmd.Flags().Int64Var(&sinceSeq, "since-seq", 0, "start the new-for-you window here instead of the seen cursor")
	cmd.Flags().BoolVar(&all, "all", false, "window covers the whole visible history, ignoring the seen cursor")
	cmd.Flags().BoolVar(&markSeen, "mark-seen", false, "advance the reader's seen cursor through the rendered snapshot")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the brief as JSON")
	return cmd
}

// readerName canonicalizes a --as name the way the read path does.
func readerName(as string) string {
	return canonAgent(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(as), "@")))
}
