// Package meet implements `bashy meet` — a multi-participant deliberation
// session where agentic CLIs and a human take turns.
//
// A meeting has three roles and the separation between them is the design:
// PARTICIPANTS decide content, the CHAIR decides process, and the SECRETARY
// decides nothing and records. See dhnt/docs/bashy-meet.md.
package meet

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

const schemaVersion = "bashy-meet-v1"

// nowFn is indirected so tests get deterministic timestamps.
var nowFn = time.Now

// Turn outcome statuses. Only ok and abstain are successes: an ABSTAIN is a
// deliberate "no comment" on an optional open question, which is a valid
// contribution, not a tool failure. Everything else nets down the participant's
// operability score and is reported per-participant in the minutes.
const (
	statusOK      = "ok"
	statusEmpty   = "empty"   // agent ran, produced nothing
	statusTimeout = "timeout" // exceeded --turn-timeout
	statusError   = "error"   // non-zero exit / launch failure
	statusShort   = "short"   // below --min-turn-chars
	statusAbstain = "abstain" // optional question, explicitly no comment
	statusInvalid = "invalid" // poll answer outside the choice set

	// statusUnrecorded appears ONLY on the live channel, never in the transcript —
	// by definition, since it means the transcript append is what failed. It is
	// how `spoke` stays truthful: a reader of the live channel takes `spoke` to
	// mean the whole turn is now durable, and when it is not, saying so is the
	// only honest option. Publishing the agent's own status here instead would
	// turn a storage failure into a silent transcript gap.
	statusUnrecorded = "unrecorded"
)

// Event is one append-only entry in a meeting transcript.
type Event struct {
	Round   int       `json:"round"`
	Speaker string    `json:"speaker"`
	Role    string    `json:"role,omitempty"`
	Kind    string    `json:"kind"` // agenda|human|turn|vote|poll|question|ledger|replan|note|decision|action|confirm|invite|kick
	To      string    `json:"to,omitempty"`
	Text    string    `json:"text"`
	File    string    `json:"file,omitempty"` // per-turn full-text file (context-offloading target)
	TS      time.Time `json:"ts"`
	// Origin identifies a record copied from another durable communication
	// store. It is deliberately structured: consumers may correlate only this
	// exact source identifier, never infer identity from rendered prose.
	Origin *EventOrigin `json:"origin,omitempty"`

	// Turn outcome, recorded so a reader can tell a timeout from an empty reply
	// from a crash without re-reading logs. Absent on legacy events — statusOf()
	// reconstructs it from the marker text.
	Status   string `json:"status,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Chars    int    `json:"chars,omitempty"`
	DurMS    int64  `json:"duration_ms,omitempty"`

	// Poll / open-question fields.
	Question string   `json:"question,omitempty"`
	Choice   string   `json:"choice,omitempty"`  // on a vote: the normalized answer
	Choices  []string `json:"choices,omitempty"` // on a poll: the permitted answers

	// Ledger is set on a `ledger` event: the chair's structured decision for
	// that turn.
	Ledger *Ledger `json:"ledger,omitempty"`

	// Retracts names the record this event withdraws, by that record's
	// RFC3339Nano timestamp — the same (kind, speaker, ts) key the transcript
	// and the browser already dedupe on, because events carry no id of their
	// own. Set only on a `retraction`.
	//
	// The withdrawal is an APPEND, never an edit: the transcript is append-only,
	// the minutes are a projection of it, and a reader who saw the original
	// (an agent, most of all) must be able to find out that it was withdrawn
	// rather than discover a hole where it used to be.
	Retracts string `json:"retracts,omitempty"`

	// Raw is the agent's UN-normalized output, carried to a client only when it
	// explicitly asked to debug the transport (see renderEvent). It is never
	// stored — the record holds prose — and is absent from every ordinary read,
	// so a transcript's wire size does not double for a feature that is off.
	Raw string `json:"raw,omitempty"`
}

// EventOrigin is the durable identity of the source record copied into Meet.
// Source and Seq form the correlation key; today SeedBoardFromMB emits "mb".
type EventOrigin struct {
	Source string `json:"source"`
	Seq    int64  `json:"seq"`
}

// Ledger is one chair turn's structured decision. Speaker selection is not a
// separate question from progress: the same call that picks who speaks next also
// answers whether the request is already satisfied and whether the team is going
// in circles.
//
// This is the Magentic-One progress-ledger shape. It exists because "who speaks
// next" alone cannot detect the largest measured multi-agent failure mode — step
// repetition, ~17% of failures across 1600+ traces. A round-robin scheduler
// cannot notice a loop; a chair that must answer `looping?` every turn can.
type Ledger struct {
	Satisfied   bool   `json:"request_satisfied"`
	Looping     bool   `json:"team_looping"`
	Progressing bool   `json:"making_progress"`
	NextSpeaker string `json:"next_speaker,omitempty"`
	Instruction string `json:"instruction,omitempty"`
	Reason      string `json:"reason,omitempty"`

	// Degraded records that the chair failed to name a valid speaker and the
	// orchestrator fell back. Never silent — a fallback that hides itself looks
	// like a working selector.
	Degraded bool `json:"degraded,omitempty"`
}

// stalling reports whether this turn shows the team failing to advance.
func (l *Ledger) stalling() bool { return l.Looping || !l.Progressing }

// statusOf reports an event's outcome, reconstructing it for transcripts written
// before Status existed so `meet show` works on old sessions.
func statusOf(e Event) string {
	if e.Status != "" {
		return e.Status
	}
	switch {
	case strings.Contains(e.Text, "timed out"):
		return statusTimeout
	case strings.Contains(e.Text, "returned no content"):
		return statusEmpty
	case strings.Contains(e.Text, "unavailable this turn"):
		return statusError
	}
	return statusOK
}

// contributed reports whether an event carries a real contribution (so an
// abstention counts as coverage, but a crash does not).
func contributed(e Event) bool {
	s := statusOf(e)
	return s == statusOK || s == statusAbstain
}

// redactHome rewrites the user's home directory to `~` anywhere it appears.
// Applied to every string that reaches the published minutes: agent CLIs print
// their workdir in startup banners, and the minutes are committed to a repo that
// may be public. Without this, `/Users/<name>/…` leaks on every meeting.
func redactHome(s string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	if home = strings.TrimRight(home, string(os.PathSeparator)); len(home) < 2 {
		return s
	}
	return strings.ReplaceAll(s, home, "~")
}

// writeTurnFile persists an event's full text under the session's turns/ dir and
// returns the absolute path. Context offloading (LangChain Deep Agents pattern):
// the transcript passed to attendees carries a head/tail PREVIEW + a file link,
// and the full bytes live here for read-on-demand. Best-effort; "" on failure.
func writeTurnFile(id string, e Event) string {
	dir, err := storeDir(id)
	if err != nil {
		return ""
	}
	turns := filepath.Join(dir, "turns")
	if err := os.MkdirAll(turns, 0o755); err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(e.Text))
	name := fmt.Sprintf("%03d-%s-%s-%s.txt", e.Round, e.Kind, slugify(e.Speaker), hex.EncodeToString(sum[:])[:6])
	path := filepath.Join(turns, name)
	if err := os.WriteFile(path, []byte(e.Text), 0o644); err != nil {
		return ""
	}
	return path
}

// A meeting has exactly three roles, and the separation between them is the
// feature — not a naming convention.
//
//   - PARTICIPANT decides CONTENT. It argues, proposes, and votes.
//   - CHAIR decides PROCESS. It poses the agenda, calls on speakers, and judges
//     whether the meeting is done. It never argues.
//   - SECRETARY decides NOTHING. It records, and extracts what was decided.
//
// The chair may be an agent, the human, or absent (a fixed round-robin fan-out
// where nobody directs). The secretary is at most one agent, and may also be
// absent: a room that is a conversation rather than a meeting keeps no minutes,
// and inventing a recorder for it would file a synthesis nobody asked for.
//
// Separation of powers is enforced in Validate(), not merely documented: a
// recorder that also chairs can declare the meeting over and then write the
// minutes that say so, and a chair that also participates biases every speaker
// selection toward its own thread. Those are the two ways this design fails.
type Role string

const (
	RoleParticipant Role = "participant"
	RoleChair       Role = "chair"
	RoleSecretary   Role = "secretary"
	RoleHuman       Role = "human"
)

// State is the durable meeting header, saved as state.json.
type State struct {
	Schema string `json:"schema"`
	ID     string `json:"id"`

	// Name is the stable, human-facing name of a room that belongs to another
	// durable object (for example "sprint 99"), or the host-local address of a
	// configured permanent room. Ordinary ad-hoc meetings have no name: their
	// durable identity is ID and their short Room number is only a reusable
	// pointer. A permanent room may be addressed as either "steward" or
	// "@steward" across restarts.
	Name      string `json:"name,omitempty"`
	Permanent bool   `json:"permanent,omitempty"`
	// RoleHolders maps stable room aliases (for example "steward") to the
	// current concrete agent identity. It is lifecycle-owned metadata, not a
	// second authority record: pkg/steward remains the truth about who holds the
	// seat. Meet uses the snapshot only to route @role and roster privileges.
	RoleHolders map[string]string `json:"role_holders,omitempty"`

	// Room is the short number a human types to attach — see room.go. It is a
	// POINTER, held for the life of the meeting and released (and reused) when
	// it closes. Nothing may ever be recorded against it; the id is the identity.
	Room int `json:"room,omitempty"`

	Topic  string   `json:"topic"`
	Agenda []string `json:"agenda,omitempty"`

	// The roster. Both Chair and Secretary are optional, and both encode a mode
	// rather than a knob: empty Chair means nobody directs (a fixed round-robin),
	// empty Secretary means nobody records (no minutes, no synthesis pass). One
	// room type covers a two-seat conversation and a formal meeting, and which
	// one you are in follows from who is seated — see
	// dhnt/docs/meet-web-chatroom-design.md §4.
	//
	// Participants is MUTABLE at runtime: Invite/Kick grow and shrink it while
	// the room is open (roster.go). Every reader takes it as it is at call time.
	Participants []string `json:"participants"`
	Secretary    string   `json:"secretary"`
	// SecretaryPending means bashy will choose a concrete fleet agent on the
	// room's first real activity. Transcript capture is synchronous; the model
	// itself runs only when synthesis work exists.
	SecretaryPending bool   `json:"secretary_pending,omitempty"`
	SecretaryBand    int    `json:"secretary_band,omitempty"`
	Chair            string `json:"chair,omitempty"`
	Human            string `json:"human"`
	// Observers are additional human attendees. They are deliberately separate
	// from Participants: a person reads and posts on their own behalf, never a
	// seat this host routes or schedules for a turn.
	Observers []string `json:"observers,omitempty"`

	// Board marks a room where participants read and post on their OWN turns:
	// no chair runs the floor and no secretary is spawned. It is a room TYPE, not
	// a turn-model knob — a two-valued Mode string where "" and "meeting" mean the
	// same state would need a paper-over accessor, exactly the smell DecisionMode
	// already is. The turn model still follows from the roster (chaired vs
	// round-robin); Board only says the orchestrator never drives a turn itself.
	Board bool `json:"board,omitempty"`

	// DefaultTo is where UNADDRESSED mail in this room lands, held as a
	// LATE-BOUND role label ("conductor:99") and never as a copied name.
	//
	// A sprint's room is the place its conductor advertises for questions, so
	// "no --to" there does not mean "nobody" — it means the seat that is
	// accountable for the sprint. Storing the label rather than the holder is
	// the whole point: a lease changes hands, and mail addressed to the person
	// who held it yesterday follows the agent instead of the responsibility.
	// Resolution happens when the message is READ. See
	// dhnt/docs/agent-inbox-unified-delivery.md sections 3-4.
	DefaultTo string `json:"default_to,omitempty"`

	// OpenTo, when set, DELEGATES SEATING to a declared audience: an agent that
	// matches it may self-seat on its first board post, recorded as a `join`
	// event so the roster still shows who came. Empty (the default) keeps seating
	// organizer-push-only — the default does not change. It is the organizer
	// delegating to a declared audience, never an unconditional self-join
	// (agent-reachability-im-layer §4a). Only a board carries one; a chaired or
	// round-robin meeting spawns turns and cannot admit a seat it will not drive.
	OpenTo *OpenInvite `json:"open_to,omitempty"`

	Status      string    `json:"status"`
	Cwd         string    `json:"cwd"`
	Out         string    `json:"out,omitempty"`
	TurnTimeout string    `json:"turn_timeout,omitempty"` // per-turn agent timeout, e.g. "20m"
	Created     time.Time `json:"created"`
	Round       int       `json:"round"`

	// Initiator is who convened the meeting and therefore who must confirm it may
	// conclude. It is an ATTRIBUTE of an attendee, not a fourth role: it names the
	// human, or an agent already seated at the table. Empty means an unnamed
	// caller (an agent invoking `meet consult`), which never confirms because it
	// receives the verdict synchronously.
	Initiator string `json:"initiator,omitempty"`

	// DecisionMode is "infer" (default — the secretary may record a decision the
	// meeting converged on, tagged as inferred) or "explicit" (only decisions a
	// participant stated outright).
	DecisionMode string `json:"decision_mode,omitempty"`

	// MinTurnChars, when > 0, marks a reply shorter than this as `short` — a
	// participant that answers "ok" did not really attend.
	MinTurnChars int `json:"min_turn_chars,omitempty"`

	// Context is the shared source set every participant reads before its first
	// turn, so the panel reviews the same files rather than guessing.
	Context []string `json:"context,omitempty"`

	// Steerable runs each turn as a LIVE agent session rather than a headless
	// one-shot, so `meet say` can actually reach the agent that is speaking.
	//
	// It is opt-in, and the reason is an honest trade rather than caution. A
	// headless turn ends when the process exits — a real boundary, cheap and exact.
	// A live turn has no boundary at all: the agent simply stops typing, so the
	// turn ends on a silence timeout, and each one also pays a TUI's startup. On a
	// four-seat, three-round meeting that is minutes of pure waiting.
	//
	// So a chair who wants to be able to interrupt asks for it and pays for it.
	// Until this change `meet say` wrote into a socket that a one-shot turn never
	// listened on: it reported success, and nothing arrived. A steer that silently
	// goes nowhere is worse than one that refuses.
	Steerable bool `json:"steerable,omitempty"`

	// MaxTurns and MaxStalls are the orchestrator-owned backstops for a chaired
	// meeting. Termination is never left to a token an agent emits: the
	// literature measures both never-stopping and premature-stopping as common.
	MaxTurns  int `json:"max_turns,omitempty"`
	MaxStalls int `json:"max_stalls,omitempty"`
}

// seated reports whether a canonical name holds any seat — participant, chair,
// or secretary. Used to refuse a filter on somebody who is not at the table.
func (s *State) seated(name string) bool {
	for _, a := range s.attendees() {
		if a == name {
			return true
		}
	}
	return false
}

// chair returns the agent chairing the meeting, or "" when nobody does.
func (s *State) chair() string { return strings.TrimSpace(s.Chair) }

// chaired reports whether an agent directs the discussion. When true the meeting
// runs the chair's progress-ledger loop; when false it runs a fixed round-robin.
// The turn model is a CONSEQUENCE of who chairs, never a separate flag that can
// contradict the roster.
func (s *State) chaired() bool { return s.chair() != "" }

// board reports whether the room is a board: participants read and post on their
// own turns, and the orchestrator never spawns a turn (no chair loop, no
// secretary, no automated round/poll/ask). It sits beside chaired()/recorded()
// because it is the same kind of thing — a fact read off the header, not a mode
// that can contradict the roster.
func (s *State) board() bool { return s.Board }

// roomRef is how a human addresses this room on the CLI: the short room number
// when it holds one, else the full id. Used in refusals that tell the reader the
// exact command to run.
func (s *State) roomRef() string {
	if s.Room > 0 {
		return fmt.Sprintf("%d", s.Room)
	}
	return s.ID
}

// durableRef is how a room is addressed in anything that OUTLIVES the terminal
// it was printed in — an mb pointer, a group invite, any message another agent
// reads on a later turn.
//
// It is deliberately not roomRef. Room numbers are shell-job-number semantics:
// the lowest free number among OPEN meetings, reused the moment one closes. A
// pointer saying "join room 2" is therefore correct when written and can name a
// different room by the time it is read — the reader has no way to tell. The
// durable id never rots, so it leads; the number rides along as a labelled,
// explicitly volatile convenience for whoever is at a prompt right now.
func (s *State) durableRef() string {
	if s.Room > 0 {
		return fmt.Sprintf("%s (room %d right now)", s.ID, s.Room)
	}
	return s.ID
}

// boardRefusal is the error a facilitator-driven verb returns when the room is a board.
// It names the mode and the alternative: there is no facilitator to run turns, so the
// caller posts on its own turn with `meet tell`.
func (s *State) boardRefusal(what string) error {
	ref := s.roomRef()
	return fmt.Errorf("meet: room %s is a board — participants read and post on their own turns. "+
		"There is no facilitator to %s. Post with: bashy meet tell %s --as <you> %q",
		ref, what, ref, "...")
}

// secretary returns the agent recording the room, or "" when nobody does.
func (s *State) secretary() string { return strings.TrimSpace(s.Secretary) }

// recorded reports whether the room has a secretary. When false there is no
// synthesis pass and the minutes say so — the same shape as chaired(): the
// behaviour follows from the roster rather than from a separate mode flag.
//
// A room without one is not a broken meeting, it is a conversation. Refusing to
// open one would mean a human who wants to talk to a single assistant must first
// nominate a second agent to take notes on it.
func (s *State) recorded() bool { return s.secretary() != "" }

// turnModel describes the turn model for humans.
func (s *State) turnModel() string {
	if s.chaired() {
		return fmt.Sprintf("facilitated by %s (max %d turns, re-plan after %d stalls)",
			s.chair(), s.maxTurns(), s.maxStalls())
	}
	return "round-robin (no facilitator — every participant speaks each round)"
}

// attendees lists every seat, so the initiator can be validated against it.
func (s *State) attendees() []string {
	out := make([]string, 0, len(s.Participants)+len(s.Observers)+3)
	out = append(out, s.Participants...)
	if s.recorded() {
		out = append(out, s.secretary())
	}
	if s.chaired() {
		out = append(out, s.chair())
	}
	if s.Human != "" {
		out = append(out, s.Human)
	}
	out = append(out, s.Observers...)
	return out
}

func (s *State) maxTurns() int {
	if s.MaxTurns > 0 {
		return s.MaxTurns
	}
	return defaultMaxTurns
}

func (s *State) maxStalls() int {
	if s.MaxStalls > 0 {
		return s.MaxStalls
	}
	return defaultMaxStalls
}

// Validate enforces the role invariants. Each one exists because violating it
// silently produces a meeting that lies about itself.
func (s *State) Validate() error {
	if strings.TrimSpace(s.Topic) == "" {
		return fmt.Errorf("meet: a meeting needs a --topic")
	}
	if s.Permanent {
		name, err := permanentRoomName(s.Name)
		if err != nil || name != s.Name {
			return fmt.Errorf("meet: permanent room has invalid name %q", s.Name)
		}
	} else if strings.TrimSpace(s.Name) != "" {
		return fmt.Errorf("meet: ordinary meeting cannot claim permanent name %q", s.Name)
	} else if len(s.RoleHolders) != 0 {
		return fmt.Errorf("meet: ordinary meeting cannot claim permanent role aliases")
	}
	for roleName, holder := range s.RoleHolders {
		name, err := permanentRoomName(roleName)
		if err != nil || name != roleName || strings.TrimSpace(holder) == "" {
			return fmt.Errorf("meet: invalid permanent role holder %q=%q", roleName, holder)
		}
	}
	// No secretary check: an empty secretary is a room that keeps no minutes,
	// not an invalid one. Every invariant below that mentions the secretary is
	// therefore conditional on there being one.
	if s.SecretaryPending && s.recorded() {
		return fmt.Errorf("meet: secretary cannot be both pending and assigned")
	}
	if s.SecretaryBand < 0 || s.SecretaryBand > 4 {
		return fmt.Errorf("meet: secretary band must be 1-4")
	}

	seen := map[string]bool{}
	for _, p := range s.Participants {
		if p = strings.TrimSpace(p); p == "" {
			return fmt.Errorf("meet: empty --participant")
		}
		if seen[strings.ToLower(p)] {
			return fmt.Errorf("meet: %s is seated twice; one seat per participant "+
				"(duplicate seats dilute a vote and add no diversity)", p)
		}
		seen[strings.ToLower(p)] = true
	}

	// The secretary records what was decided. A secretary that also argues has an
	// interest in the record; a secretary that also chairs can declare the meeting
	// over and then write the minutes saying so.
	if s.recorded() && seen[strings.ToLower(s.secretary())] {
		return fmt.Errorf("meet: %s cannot be both secretary and participant — "+
			"the secretary records the decisions and must not have a stake in them", s.Secretary)
	}
	if s.recorded() && s.chaired() && strings.EqualFold(s.chair(), s.secretary()) {
		return fmt.Errorf("meet: %s cannot be both facilitator and secretary — "+
			"the facilitator decides when the meeting is done and the secretary writes down what it decided; "+
			"one agent doing both can conclude a meeting and then author the record of it.\n"+
			"      Use a different --owner, or drop --owner to let the human direct the discussion", s.Secretary)
	}
	// A chair that also argues biases every speaker selection toward its own thread.
	if s.chaired() && seen[strings.ToLower(s.chair())] {
		return fmt.Errorf("meet: %s cannot be both facilitator and participant — "+
			"the facilitator picks who speaks next and would be picking itself", s.chair())
	}
	if s.chaired() && len(s.Participants) == 0 {
		return fmt.Errorf("meet: a facilitator needs at least one --participant to call on")
	}

	// A board has no floor to run: participants read and post on their own turns.
	// A chair calls on speakers and nobody in a board is callable, so the two are
	// contradictory rather than merely redundant.
	if s.Board && s.chaired() {
		return fmt.Errorf("meet: a board has no facilitator-driven floor — participants read and post on their own turns, "+
			"so there is nobody for %s to call on. Drop --owner, or drop --board", s.chair())
	}

	// The initiator must be someone at the table, so `close` knows who to ask.
	//
	// The message is written for whoever is READING it, which is no longer only
	// somebody at a terminal: this same check answers a browser opening a room
	// through the API. It therefore names no flag — a web user has no `--initiator`
	// to correct — and lists who IS in the room, which is the one fact that makes
	// the refusal actionable from either surface.
	if n := strings.TrimSpace(s.Initiator); n != "" && !strings.EqualFold(n, s.Human) {
		for _, a := range s.attendees() {
			if strings.EqualFold(a, n) {
				return nil
			}
		}
		where := "this room has nobody in it yet"
		if at := s.attendees(); len(at) > 0 {
			where = "in this room: " + strings.Join(at, ", ")
		}
		return fmt.Errorf("meet: %s is not in this room, so cannot be the one who convened it — "+
			"whoever opens a room is in it, and only someone in it may close it (%s)", n, where)
	}
	return nil
}

func (s *State) initiatorName() string { return strings.TrimSpace(s.Initiator) }

// initiatorKind derives human-vs-agent from the roster rather than storing it, so
// the two can never disagree. An EMPTY initiator is an unnamed agent caller —
// only `meet consult` produces one, and it never confirms, because the caller
// receives the verdict synchronously. `start` always names its initiator.
func (s *State) initiatorKind() string {
	n := s.initiatorName()
	if n == "" {
		return "agent"
	}
	if s.humanAttendee(n) {
		return "human"
	}
	return "agent"
}

// humanAttendee reports whether name is one of the room's people rather than
// an agent seat. Observers are attendees for attribution and organizer checks,
// but are never put in Participants where the turn runner would try to launch
// them.
func (s *State) humanAttendee(name string) bool {
	if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(s.Human)) && strings.TrimSpace(s.Human) != "" {
		return true
	}
	return containsFold(s.Observers, name)
}

// procedural names the speaker of a procedural act — posing an agenda item, a
// poll, or an open question. That is the chair's act, whether an agent holds the
// chair or the human does.
func procedural(s *State) string {
	if s.chaired() {
		return s.chair()
	}
	if s.Human != "" {
		return s.Human
	}
	return string(RoleChair)
}

// initiatorLabel renders the initiator for humans.
func (s *State) initiatorLabel() string {
	name := s.initiatorName()
	if name == "" {
		return "an unnamed calling agent (pass --initiator to name it)"
	}
	return fmt.Sprintf("%s (%s)", name, s.initiatorKind())
}

func (s *State) decisionMode() string {
	if strings.EqualFold(strings.TrimSpace(s.DecisionMode), "explicit") {
		return "explicit"
	}
	return "infer"
}

// Decision is one recorded decision. Inferred marks a decision the secretary
// read out of the meeting's consensus rather than one a participant stated
// outright — the reader must be able to tell them apart.
//
// Support names the participants the secretary says agreed. It is the grounding
// contract: dialogue summarizers invent decisions at a measured ~23% rate, and
// the dominant error class is "circumstantial inference" — a decision that was
// implied but never made. An INFERRED decision therefore requires a proposal AND
// an acceptance (>= 2 named supporters); one that cannot name them is demoted to
// an open question by demoteUnsupported, in code, not by asking the LLM nicely.
type Decision struct {
	Text     string   `json:"text"`
	Inferred bool     `json:"inferred,omitempty"`
	Support  []string `json:"support,omitempty"`
}

// minInferredSupport is the acceptance threshold: a proposer plus at least one
// participant who agreed.
const minInferredSupport = 2

// demoteUnsupported moves every inferred decision that cannot name enough
// supporters into the open-questions list. Discussion of an option is not a
// decision, and the secretary does not get to blur that.
func (s *Synthesis) demoteUnsupported() {
	kept := s.Decisions[:0]
	for _, d := range s.Decisions {
		if d.Inferred && len(d.Support) < minInferredSupport {
			s.OpenQuestions = append(s.OpenQuestions,
				fmt.Sprintf("%s (raised, but no recorded agreement — not a decision)", d.Text))
			continue
		}
		kept = append(kept, d)
	}
	s.Decisions = kept
}

// Synthesis is the secretary's derived view of a meeting: decisions, actions,
// risks, open questions, corrections, and a summary.
//
// It lives in its own file, NOT in the append-only transcript, and the latest
// pass wins. That is what makes `meet amend` idempotent: re-running the
// secretary rewrites this file instead of appending a second set of markers.
// Human `/decision` and `/action` markers stay in the transcript and remain
// authoritative — the secretary's job is to extract, never to overrule.
type Synthesis struct {
	Schema        string     `json:"schema"`
	By            string     `json:"by"`
	At            time.Time  `json:"at"`
	Mode          string     `json:"mode"` // infer|explicit
	Decisions     []Decision `json:"decisions,omitempty"`
	Actions       []string   `json:"actions,omitempty"`
	Risks         []string   `json:"risks,omitempty"`
	OpenQuestions []string   `json:"open_questions,omitempty"`
	Corrections   []string   `json:"corrections,omitempty"`
	Summary       string     `json:"summary,omitempty"`
}

func (s *Synthesis) save(id string) error {
	dir, err := storeDir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.Schema = schemaVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "synthesis.json"), b)
}

// loadSynthesis returns the last secretary pass, or nil when none has run.
func loadSynthesis(id string) *Synthesis {
	dir, err := storeDir(id)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "synthesis.json"))
	if err != nil {
		return nil
	}
	var s Synthesis
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}

// baseDir is the root of the local session store. Overridable via
// BASHY_MEET_DIR (used by tests and by operators who want a custom location).
// BaseDir is the meet store root (BASHY_MEET_DIR, else ~/.bashy/meet) — the
// one place this path is computed, exported so a resource map can name it.
func BaseDir() (string, error) { return baseDir() }

func baseDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BASHY_MEET_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bashy", "meet"), nil
}

func storeDir(id string) (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, id), nil
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	if out == "" {
		out = "meeting"
	}
	return out
}

// newID derives a stable session id from the topic + timestamp.
func newID(topic string, now time.Time) string {
	sum := sha256.Sum256([]byte(topic + now.Format(time.RFC3339Nano)))
	short := hex.EncodeToString(sum[:])[:4]
	return fmt.Sprintf("%s-%s-%s", now.Format("2006-01-02"), slugify(topic), short)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) save() error {
	dir, err := storeDir(s.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.Schema = schemaVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "state.json"), b)
}

func loadState(id string) (*State, error) {
	dir, err := storeDir(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// appendLockWait bounds how long a writer waits for the transcript append lock.
// The critical section is one small write, so contention clears in milliseconds;
// the bound exists only so a wedged holder surfaces as an error instead of a
// hang. Generous because the kernel already releases the lock on holder death —
// a long queue of live writers is the only way to get anywhere near it.
const appendLockWait = 30 * time.Second

func appendEvent(id string, e Event) error {
	dir, err := storeDir(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// O_APPEND makes the kernel's seek+write atomic with respect to the OFFSET,
	// but it does not make the write all-or-nothing: os.File.Write can return a
	// short count with the partial bytes already in the file, the next append
	// then concatenates onto a line with no newline, and readTranscript skips
	// the merged line — two events vanish with every reader's view consistent.
	// So appends serialize on a lock, and the write loop finishes the line.
	//
	// The lock is a DISTINCT inode from run.lock: the run lease is held for a
	// whole round, so serializing on it would make every message typed during a
	// round fail ErrMeetingBusy — exactly the lease-free promise Post makes.
	host, _ := os.Hostname()
	l, err := lockfile.AcquireWithin(filepath.Join(dir, "append.lock"), appendLockWait, lockfile.Holder{
		Name: host, PID: os.Getpid(), Intent: "append meeting transcript", Since: nowFn(),
	})
	if err != nil {
		return fmt.Errorf("meet: locking append transcript: %w", err)
	}
	defer l.Release()
	f, err := os.OpenFile(filepath.Join(dir, "transcript.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for len(b) > 0 {
		n, err := f.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func readTranscript(id string) ([]Event, error) {
	dir, err := storeDir(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func listSessions() ([]*State, error) {
	base, err := baseDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if s, err := loadState(e.Name()); err == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}
