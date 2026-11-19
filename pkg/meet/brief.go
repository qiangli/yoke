package meet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

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
// hidden counts, never dropped silently. Every cap keeps the NEWEST items in
// stable transcript order: a long absence reads as "N oldest hidden", never
// as a newest decision quietly missing.
const BriefLimit = 20

// BriefItemTextBytes caps the text of one brief item. A single huge turn
// (dumps, transcripts pasted into chat) must not make one reader's brief
// grow without bound; the full text stays one `meet read` away. Truncated
// items carry a "[truncated]" marker, never a silent cut.
const BriefItemTextBytes = 2048

// BriefTotalBytes caps the whole brief's item text. Together with the
// per-item cap above and the per-list count cap, the worst case is
// arithmetic, not hope: lists * limit * BriefItemTextBytes. At defaults that
// is about 6*20*2KB, comfortably under this budget; the budget is the
// backstop for inflated --limit values. Enforcement drops the oldest items
// of the longest lists first (standing state yields before the
// since-last-seen window) and folds every drop into the lists' hidden
// counts.
const BriefTotalBytes = 256 << 10

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
	// AckAll acknowledges even the tail the caps hid. When a cap hides an
	// item the reader has not seen, MarkSeen alone refuses rather than
	// silently consume it: re-run with AckAll (or a larger Limit) to
	// acknowledge the hidden tail too.
	AckAll bool
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
	// Questions are unresolved polls and open questions: no retraction named
	// them, no decisive winner closed the poll, and no later turn answered
	// the ask. Anything else is history, not pending, and is excluded.
	Questions []BriefPoll `json:"questions"`
	// QuestionsHidden counts questions beyond the limit.
	QuestionsHidden int `json:"questions_hidden,omitempty"`
	// SynthesisOmitted reports that the secretary's whole-transcript
	// synthesis was withheld: the transcript holds directed mail this reader
	// may not see, and a global summary written from the whole transcript
	// cannot be proven clean for them. The brief then shows only events the
	// reader may see. Never silent: the flag says what is missing and why.
	SynthesisOmitted bool `json:"synthesis_omitted,omitempty"`
}

// briefActionID is the stable identity of an action within a room: the S1
// adoption reference. It intentionally mirrors S1's actionID (adopt.go) so a
// brief and an adoption name the same candidate; collapse the two when S1's
// helper is importable from this package's tree.
func briefActionID(room, text string) string {
	norm := normBriefText(text)
	sum := sha256.Sum256([]byte(room + "\x00" + norm))
	return "act-" + hex.EncodeToString(sum[:])[:10]
}

// normBriefText lowercases and collapses whitespace so two renderings of the
// same words compare equal.
func normBriefText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// briefPrivateTexts collects the normalized texts of directed mail this reader
// must not see through a second channel: addressed to somebody else and not
// written by the reader either. The secretary reads the whole transcript, so a
// synthesis item can quote that mail back; the brief must not launder it into
// another reader's sections.
func briefPrivateTexts(events []Event, reader string) []string {
	var out []string
	for _, e := range events {
		if !addressedEvent(e) || directedEvent(e, reader) || authoredBy(e, reader) {
			continue
		}
		if t := normBriefText(e.Text); len(t) >= 12 {
			out = append(out, t)
		}
	}
	return out
}

// briefWindows returns the contiguous n-word phrases of s. Quotation is a
// shared distinctive phrase, not just a shared whole: a synthesis item that
// repeats five running words of somebody else's mail quotes it even when the
// mail's greeting or sign-off is left behind.
func briefWindows(s string, n int) []string {
	w := strings.Fields(s)
	if len(w) < n {
		return nil
	}
	out := make([]string, 0, len(w)-n+1)
	for i := 0; i+n <= len(w); i++ {
		out = append(out, strings.Join(w[i:i+n], " "))
	}
	return out
}

// briefLeaksPrivate reports whether a synthesis string reproduces private
// directed content: verbatim either way, or sharing a five-word phrase. A
// paraphrase beyond quotation is the secretary's own words and stays; a quoted
// slice of somebody else's mail does not cross readers. Short fragments stay
// out of the comparison so generic few-word phrases cannot suppress unrelated
// synthesis.
func briefLeaksPrivate(text string, private []string) bool {
	t := normBriefText(text)
	if len(t) < 12 {
		return false
	}
	for _, p := range private {
		if strings.Contains(t, p) || strings.Contains(p, t) {
			return true
		}
		for _, win := range briefWindows(p, 5) {
			if strings.Contains(t, win) {
				return true
			}
		}
		for _, win := range briefWindows(t, 5) {
			if strings.Contains(p, win) {
				return true
			}
		}
	}
	return false
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

// briefItem renders one transcript event with its source link. The text is
// truncated to the per-item byte bound: the full turn stays one `meet read`
// away, and a huge turn cannot make one reader's brief grow without bound.
func briefItem(seq int64, e Event, reader, authority string) BriefItem {
	it := BriefItem{
		Seq: seq, Kind: e.Kind, Speaker: e.Speaker, To: strings.TrimSpace(e.To),
		Authority: authority, Text: truncateBriefText(redactHome(strings.TrimSpace(e.Text))),
		TS: e.TS, Directed: directedEvent(e, reader),
	}
	if it.Directed && strings.EqualFold(strings.TrimSpace(e.To), AllSeats) {
		it.Directed = false // a broadcast reaches everyone; it singles out nobody
	}
	return it
}

// capBrief bounds a list and reports what it hid. The cap keeps the NEWEST
// items in stable transcript order and hides the oldest: a capped brief
// must never hide the newest decision while showing a stale one. It also
// reports how many of the hidden items the reader has not seen, so MarkSeen
// can refuse to silently consume them.
func capBrief(items []BriefItem, limit int) (BriefSection, int) {
	if limit <= 0 {
		limit = BriefLimit
	}
	if len(items) > limit {
		dropped := items[:len(items)-limit]
		unseen := 0
		for _, it := range dropped {
			if it.New {
				unseen++
			}
		}
		return BriefSection{Items: items[len(items)-limit:], Hidden: len(items) - limit}, unseen
	}
	if items == nil {
		items = []BriefItem{}
	}
	return BriefSection{Items: items}, 0
}

// truncateBriefText bounds one item's text on a rune boundary. The marker is
// part of the bound, and an empty result is never returned for non-empty
// input: the reader must see that text existed.
func truncateBriefText(s string) string {
	const marker = " [truncated]"
	if len(s) <= BriefItemTextBytes {
		return s
	}
	cut := BriefItemTextBytes - len(marker)
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	if cut <= 0 {
		return marker[len(" "):]
	}
	return s[:cut] + marker
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

	// A withdrawal removes its target everywhere: the retracted decision,
	// action, vote or question is no longer standing state and no longer
	// news. The retraction record itself stays — it is the evidence that
	// something was withdrawn.
	voteRetracted := func(e Event) bool {
		return e.Kind == "vote" && withdrawn(e)
	}

	var changes []BriefItem
	for i, e := range events {
		seq := int64(i + 1)
		if seq <= since || !briefNew(e, reader) {
			continue
		}
		if withdrawn(e) {
			continue
		}
		res.New[e.Kind]++
		it := briefItem(seq, e, reader, briefStandingAuthority(st, e))
		it.New = true
		changes = append(changes, it)
	}
	var hiddenUnseen int
	res.Changes, hiddenUnseen = capBrief(changes, limit)

	var decisions, proposals, objections, owned []BriefItem
	var questions []BriefPoll
	for i, e := range events {
		seq := int64(i + 1)
		if !briefVisible(e, reader) {
			continue
		}
		isNew := seq > since && briefNew(e, reader)
		switch e.Kind {
		case "decision", "confirm":
			if withdrawn(e) {
				break
			}
			if !participantSeat(st, e.Speaker) {
				// A decision is confirmed only by somebody at the table. A
				// seat outside the roster naming a decision is a proposal
				// in another reader's voice, never authority.
				it := briefItem(seq, e, reader, briefAuthorityVoice)
				it.Source = "proposed"
				it.New = isNew
				proposals = append(proposals, it)
				break
			}
			it := briefItem(seq, e, reader, briefAuthorityHuman)
			it.New = isNew
			decisions = append(decisions, it)
		case "action":
			if withdrawn(e) {
				break
			}
			if !participantSeat(st, e.Speaker) {
				it := briefItem(seq, e, reader, briefAuthorityVoice)
				it.Source = "proposed"
				it.New = isNew
				proposals = append(proposals, it)
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
		case "agenda":
			if withdrawn(e) {
				break
			}
			it := briefItem(seq, e, reader, briefAuthorityChair)
			it.New = isNew
			proposals = append(proposals, it)
		case "retraction":
			it := briefItem(seq, e, reader, briefAuthorityVoice)
			it.New = isNew
			objections = append(objections, it)
		case "vote":
			if withdrawn(e) {
				break
			}
			if briefDissent(e) {
				it := briefItem(seq, e, reader, briefAuthorityVoice)
				it.New = isNew
				objections = append(objections, it)
			}
		case "poll", "question", "ask":
			if withdrawn(e) {
				break
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
	// with those choices. Retracted votes are withdrawn assertions: they
	// tally nowhere and dissent nowhere.
	for i, e := range events {
		if e.Kind != "vote" || voteRetracted(e) || !briefVisible(e, reader) {
			continue
		}
		for q := len(questions) - 1; q >= 0; q-- {
			if int64(questionSeq(events, questions[q])) <= int64(i+1) && sameChoices(questions[q].Choices, e.Choices) {
				choice := strings.TrimSpace(e.Choice)
				if choice == "" {
					choice = statusOf(e)
					if choice == "" {
						choice = "no-choice"
					}
				}
				questions[q].Tally[choice]++
				questions[q].Votes++
				break
			}
		}
	}

	// Pending means evidenced-unresolved. A retracted question is gone
	// (filtered above); a poll the transcript decided is history; an ask a
	// later turn answered is history too. What the transcript never resolves
	// — an open question with no answering turn — stays listed under a name
	// that says exactly that.
	questions = filterResolvedQuestions(events, questions, reader)

	// The secretary's synthesis is a proposal under a human's brief, never a
	// confirmation. Its decisions join proposals, its open questions join
	// questions, and its risks and corrections join objections — each labeled
	// so no reader mistakes extraction for authority.
	//
	// The secretary reads the whole transcript, directed mail included, so a
	// global synthesis written where the transcript holds mail this reader
	// may not see cannot be proven clean for them — not only verbatim
	// quotation, but paraphrase of that mail too. When such mail exists the
	// synthesis is withheld from that reader entirely and the omission is
	// flagged on the result; the brief then shows only events the reader may
	// see. The addressee's own brief is unaffected: their mail is theirs.
	private := briefPrivateTexts(events, reader)
	if syn := loadSynthesis(st.ID); syn != nil {
		if len(private) > 0 {
			res.SynthesisOmitted = true
		} else {
			for _, d := range syn.Decisions {
				text := strings.TrimSpace(d.Text)
				if text == "" || briefLeaksPrivate(text, private) {
					continue
				}
				proposals = append(proposals, BriefItem{
					Kind: "decision", Speaker: "secretary",
					Authority: briefAuthoritySynthesis, Source: "proposed",
					Text: truncateBriefText(redactHome(text)), TS: syn.At,
				})
			}
			for _, a := range syn.Actions {
				text := strings.TrimSpace(a)
				if text == "" || briefWithdrawnText(events, st.ID, text) || briefLeaksPrivate(text, private) {
					continue
				}
				it := BriefItem{
					Kind: "action", Speaker: "secretary",
					Authority: briefAuthoritySynthesis, Source: "proposed",
					ID:   briefActionID(st.ID, text),
					Text: truncateBriefText(redactHome(text)), TS: syn.At,
				}
				proposals = append(proposals, it)
				if owner, _ := briefSplitOwner(text); owner != "" {
					it.Owner = owner
					it.Mine = briefOwns(reader, owner)
					owned = append(owned, it)
				}
			}
			for _, o := range syn.OpenQuestions {
				o = strings.TrimSpace(o)
				if o == "" || briefLeaksPrivate(o, private) {
					continue
				}
				o = truncateBriefText(redactHome(o))
				questions = append(questions, BriefPoll{
					BriefItem: BriefItem{
						Kind: "question", Speaker: "secretary",
						Authority: briefAuthoritySynthesis,
						Text:      o, TS: syn.At,
					},
					Question: o,
				})
			}
			for _, r := range append(append([]string{}, syn.Risks...), syn.Corrections...) {
				r = strings.TrimSpace(r)
				if r == "" || briefLeaksPrivate(r, private) {
					continue
				}
				objections = append(objections, BriefItem{
					Kind: "note", Speaker: "secretary",
					Authority: briefAuthoritySynthesis,
					Text:      truncateBriefText(redactHome(r)), TS: syn.At,
				})
			}
		}
	}

	var unseen int
	res.Decisions, unseen = capBrief(decisions, limit)
	hiddenUnseen += unseen
	res.Proposals, unseen = capBrief(proposals, limit)
	hiddenUnseen += unseen
	res.Objections, unseen = capBrief(objections, limit)
	hiddenUnseen += unseen
	res.Owned, unseen = capBrief(owned, limit)
	hiddenUnseen += unseen
	res.Questions, res.QuestionsHidden = capBriefPolls(questions, limit)
	if res.QuestionsHidden > 0 {
		for _, q := range questions[:len(questions)-len(res.Questions)] {
			if q.BriefItem.New {
				hiddenUnseen++
			}
		}
	}
	if res.Questions == nil {
		res.Questions = []BriefPoll{}
	}

	// The total byte budget is the backstop for inflated limits: drop the
	// oldest items of the longest lists first — standing state yields before
	// the since-last-seen window — and fold every drop into hidden counts.
	enforceBriefBudget(res)

	if opt.MarkSeen {
		if hiddenUnseen > 0 && !opt.AckAll {
			return nil, fmt.Errorf("meet: brief hid %d unseen item(s); re-run with --mark-seen --ack-all to acknowledge the hidden tail too, or raise --limit", hiddenUnseen)
		}
		if err := MarkSeenThrough(st.ID, reader, through); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// briefStandingAuthority labels who stands behind an event. Confirmed
// authority needs two things, never one: the right KIND and a real seat at
// the table. A human-marked decision or action by a roster member (a
// participant, or a person the room knows — the human, an observer) is
// authoritative; the same kind from a seat outside the roster, or from an
// agent's secretary prose, is a participant voice or a labeled proposal,
// never a confirmation. Polls and questions are chair procedure, not
// rulings; votes and retractions are voices that preserve disagreement.
// The changes window uses these same labels, so the window never upgrades
// what the sections would not confirm.
func briefStandingAuthority(st *State, e Event) string {
	member := participantSeat(st, e.Speaker)
	switch e.Kind {
	case "decision", "action", "confirm":
		if member {
			return briefAuthorityHuman
		}
		return briefAuthorityVoice
	case "agenda", "poll", "question", "ask":
		return briefAuthorityChair
	default:
		return briefAuthorityVoice
	}
}

// filterResolvedQuestions keeps only evidenced-unresolved questions. A poll
// the visible, unretracted votes decide (PollResult.Winner, the same rule
// the minutes use) is history, not pending; an open question a later turn
// answered is history too. Answering is evidenced by a later turn that names
// the question in its Question field — the link runAsk records — with a
// successful status. Silence, abstention and failure answer nothing; a
// question the transcript never resolves stays listed, honestly pending.
func filterResolvedQuestions(events []Event, questions []BriefPoll, reader string) []BriefPoll {
	kept := questions[:0]
	for _, q := range questions {
		switch q.Kind {
		case "poll":
			if _, ok := (&PollResult{Question: q.Question, Choices: q.Choices, Tally: q.Tally}).Winner(); ok {
				continue // the transcript decided this ballot: history
			}
			kept = append(kept, q)
		case "question", "ask":
			key := normBriefText(questionOf(Event{Question: q.Question, Text: q.BriefItem.Text}))
			if answeredAfter(events, key, int(q.Seq), reader) {
				continue
			}
			kept = append(kept, q)
		default:
			kept = append(kept, q) // synthesis open questions: no transcript state
		}
	}
	return kept
}

// answeredAfter reports whether a later visible turn answered the question
// key: a subsequent event carrying the same Question link with a successful
// status. Only the reader's visible transcript counts — an answer the
// reader may not see resolves nothing for them.
func answeredAfter(events []Event, key string, afterSeq int, reader string) bool {
	if key == "" {
		return false
	}
	for i, e := range events {
		if int64(i+1) <= int64(afterSeq) {
			continue
		}
		switch e.Kind {
		case "poll", "question", "ask", "vote", "retraction":
			continue
		}
		if !briefVisible(e, reader) {
			continue
		}
		if statusOf(e) != statusOK {
			continue
		}
		if q := strings.TrimSpace(e.Question); q != "" && normBriefText(q) == key {
			return true
		}
	}
	return false
}

// capBriefPolls bounds the question list newest-first like capBrief. Polls
// built from synthesis carry Seq 0 and sort after transcript polls; within
// equal recency the transcript order stands.
func capBriefPolls(questions []BriefPoll, limit int) ([]BriefPoll, int) {
	if limit <= 0 {
		limit = BriefLimit
	}
	if len(questions) > limit {
		return questions[len(questions)-limit:], len(questions) - limit
	}
	if questions == nil {
		questions = []BriefPoll{}
	}
	return questions, 0
}

// briefItemBytes is the byte weight of one rendered item for the total
// budget: its text plus a fixed per-item overhead for the envelope. Poll
// questions add their Question text at the call sites.
func briefItemBytes(it BriefItem) int {
	return len(it.Text) + 64
}

// enforceBriefBudget drops the oldest items of the longest lists until the
// brief's item text fits BriefTotalBytes. Standing state yields before the
// since-last-seen window; every drop folds into that list's hidden count,
// never a silent cut. Per-item truncation above guarantees termination with
// a non-empty result: no single item can exceed the budget alone.
func enforceBriefBudget(res *BriefResult) {
	total := 0
	for _, it := range res.Changes.Items {
		total += briefItemBytes(it)
	}
	for _, it := range res.Decisions.Items {
		total += briefItemBytes(it)
	}
	for _, it := range res.Proposals.Items {
		total += briefItemBytes(it)
	}
	for _, it := range res.Objections.Items {
		total += briefItemBytes(it)
	}
	for _, it := range res.Owned.Items {
		total += briefItemBytes(it)
	}
	for _, q := range res.Questions {
		total += briefItemBytes(q.BriefItem) + len(q.Question)
	}
	for total > BriefTotalBytes {
		dropped := dropOldestFromLongest(res)
		if !dropped {
			return
		}
		// Recompute rather than track deltas: lists are short and capped,
		// and exactness beats cleverness in a bound.
		total = 0
		for _, it := range res.Changes.Items {
			total += briefItemBytes(it)
		}
		for _, it := range res.Decisions.Items {
			total += briefItemBytes(it)
		}
		for _, it := range res.Proposals.Items {
			total += briefItemBytes(it)
		}
		for _, it := range res.Objections.Items {
			total += briefItemBytes(it)
		}
		for _, it := range res.Owned.Items {
			total += briefItemBytes(it)
		}
		for _, q := range res.Questions {
			total += briefItemBytes(q.BriefItem) + len(q.Question)
		}
	}
}

// dropOldestFromLongest removes the oldest item of the longest list,
// standing state before the window, and reports whether anything dropped.
func dropOldestFromLongest(res *BriefResult) bool {
	best := -1
	bestLen := 1 // a list of one has no older tail worth preferring; ties yield
	lists := []struct {
		name string
		n    int
	}{
		{"questions", len(res.Questions)},
		{"owned", len(res.Owned.Items)},
		{"objections", len(res.Objections.Items)},
		{"proposals", len(res.Proposals.Items)},
		{"decisions", len(res.Decisions.Items)},
		{"changes", len(res.Changes.Items)},
	}
	for i, l := range lists {
		if l.n > bestLen {
			bestLen = l.n
			best = i
		}
	}
	if best < 0 {
		return false
	}
	switch lists[best].name {
	case "questions":
		res.Questions = res.Questions[1:]
		res.QuestionsHidden++
	case "owned":
		res.Owned.Items = res.Owned.Items[1:]
		res.Owned.Hidden++
	case "objections":
		res.Objections.Items = res.Objections.Items[1:]
		res.Objections.Hidden++
	case "proposals":
		res.Proposals.Items = res.Proposals.Items[1:]
		res.Proposals.Hidden++
	case "decisions":
		res.Decisions.Items = res.Decisions.Items[1:]
		res.Decisions.Hidden++
	case "changes":
		res.Changes.Items = res.Changes.Items[1:]
		res.Changes.Hidden++
	}
	return true
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
	if res.SynthesisOmitted {
		fmt.Fprintln(w, "note: secretary synthesis withheld — the transcript holds mail addressed elsewhere, so the global summary cannot be proven clean for you; only events you may see are shown")
		fmt.Fprintln(w)
	}
	writeBriefSection(w, "changes since last seen", res.Changes)
	writeBriefSection(w, "confirmed decisions (member authority)", res.Decisions)
	writeBriefSection(w, "proposals (confirmation adopts; listing does not)", res.Proposals)
	writeBriefSection(w, "unresolved objections (disagreement preserved)", res.Objections)
	writeBriefSection(w, "owned work", res.Owned)
	fmt.Fprintf(w, "open questions — no resolution recorded (%d", len(res.Questions))
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
	var all, markSeen, ackAll, jsonOut bool
	cmd := &cobra.Command{
		Use:   "brief <room>|<id> --as NAME [--limit N] [--since-seq N] [--all] [--mark-seen [--ack-all]] [--json]",
		Short: "catch up on a room: what changed and what needs you (read-only)",
		Long: "Print a bounded, permission-filtered catch-up brief for one participant.\n\n" +
			"The brief shows what is new since the reader last looked, the room's\n" +
			"confirmed decisions, open proposals, unresolved objections, owned work\n" +
			"and open questions — every item carrying the room and transcript\n" +
			"sequence that locate it. Content addressed to somebody else is\n" +
			"excluded. A decision or action is authoritative only when a roster\n" +
			"member marked it; the same kind from outside the roster is a\n" +
			"proposal in a participant's voice. Secretary synthesis is labeled a\n" +
			"proposal and never confirms anything — and where the transcript\n" +
			"holds mail the reader may not see, the synthesis is withheld\n" +
			"entirely rather than risk laundering it.\n\n" +
			"Every list keeps the newest items and reports how many oldest it\n" +
			"hid; item texts are capped and the whole brief has a byte budget,\n" +
			"so a huge turn cannot grow it without bound.\n\n" +
			"The brief advances nothing by default: use --mark-seen to move the\n" +
			"reader's cursor through the rendered snapshot, like `meet read`.\n" +
			"When a cap hid something the reader has not seen, --mark-seen\n" +
			"refuses unless --ack-all also acknowledges the hidden tail.\n\n" +
			"--as names a seat on the local machine-owner path, the same trust as\n" +
			"the read path's local naming. Over HTTP the reader is the verified\n" +
			"caller: a vouched request for another participant's brief is refused,\n" +
			"so one browser caller can neither read another's directed mail nor\n" +
			"advance another's cursor.",
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
				SinceSeq: sinceSeq, All: all, MarkSeen: markSeen, AckAll: ackAll,
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
	cmd.Flags().BoolVar(&ackAll, "ack-all", false, "with --mark-seen, also acknowledge items hidden by caps")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the brief as JSON")
	return cmd
}

// readerName canonicalizes a --as name the way the read path does.
func readerName(as string) string {
	return canonAgent(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(as), "@")))
}
