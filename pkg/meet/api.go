package meet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/chat"
)

// The exported API — one truth, three callers.
//
// serve.go's rule is "Add a transport, never a second truth", and until now that
// held only because there was exactly one write surface: the cobra tree, whose
// verbs were unexported closures around the engine. A second surface (the HTTP
// layer in serve.go, driven by the browser SPA) could not reuse a closure, so the
// choice was to re-implement each verb over there — and a re-implementation is a
// second truth by construction: the day `close` grows a step, one of the two
// surfaces gets it.
//
// So every write verb is a thin exported func here, and BOTH the cobra command
// and the HTTP handler call it. Thin is the operative word: nothing in this file
// decides anything. It resolves a room reference, applies the privilege check
// that belongs to the act, and calls the same engine function the REPL calls.
// Presentation (tables, prose, JSON envelopes) stays with the caller, because
// that is the one thing a CLI and a browser genuinely do differently.
//
// Every `ref` argument accepts what a human types at `bashy meet list`: a room
// number, a full id, or an unambiguous id prefix. That is resolveMeeting's job
// (room.go) and it is reused rather than reproduced — two resolvers would drift
// on which spellings are accepted, and the failure mode is attaching to the
// wrong meeting.

// ErrNoRoom marks a reference that names no meeting on this host.
//
// It exists so a transport can classify the failure (the HTTP layer answers 404)
// without matching on prose. resolveMeeting's messages are written for a human
// and will be reworded; this sentinel is the machine-readable half. The wrapper
// keeps the human message intact — Error() is still resolveMeeting's text.
var ErrNoRoom = errors.New("meet: no such room")

type notFound struct{ err error }

func (n *notFound) Error() string { return n.err.Error() }
func (n *notFound) Unwrap() error { return n.err }
func (n *notFound) Is(target error) bool {
	return target == ErrNoRoom
}

// roomOf is the front door of every verb below: resolve a reference, load the
// session, and classify a miss as ErrNoRoom.
//
// A state.json that cannot be read is reported as a miss too. The distinction
// between "no such room" and "a room whose header is unreadable" is real, but
// there is nothing a caller can do differently about it, and calling it a server
// fault would page somebody over a directory a user deleted by hand.
func roomOf(ref string) (*State, error) {
	id, err := resolveMeeting(ref)
	if err != nil {
		return nil, &notFound{err}
	}
	st, err := loadState(id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &notFound{fmt.Errorf("meet: %s has no state.json", id)}
		}
		return nil, err
	}
	return st, nil
}

// RoomSummary is a room as a list shows it — enough to render a channel list
// without loading every transcript.
type RoomSummary struct {
	ID        string `json:"id"`
	Room      int    `json:"room,omitempty"`
	Name      string `json:"name,omitempty"`
	Permanent bool   `json:"permanent,omitempty"`
	// Board mirrors State.Board so a channel list can tell a board from a
	// meeting without loading every room — a browser uses it to not offer
	// chair-driven actions the server would refuse with ErrWrongMode.
	Board   bool      `json:"board,omitempty"`
	Topic   string    `json:"topic"`
	Status  string    `json:"status"`
	Members []string  `json:"members"`
	Updated time.Time `json:"updated"`
}

// JobRef identifies work a caller started but is not waiting for. The long verbs
// (round, poll, ask, address, converge) run for minutes under the room lease, so
// a transport hands back one of these immediately and the work's OUTPUT arrives
// on the room's existing event stream — the transcript, which `observe` and the
// /observe WebSocket already tail. There is deliberately no second progress
// channel: a job that reported itself somewhere else would be a second truth
// about what happened in the room.
type JobRef struct {
	ID   string `json:"job"`
	Room string `json:"room"`
}

// CreateOptions is the room a caller wants. It is the sessionFlags set minus the
// flag plumbing, so `meet open` and a browser build the same State through the
// same validation.
//
// The zero value is meaningful and is the chat-room case: no secretary (the room
// keeps no minutes), no chair (nobody directs). See the design of record §4 —
// one room type, from a human plus one assistant up to a chaired panel.
// OutStore is the Out sentinel for "publish the minutes to the host-local
// session store, NEVER into the repo working tree".
//
// The default puts minutes in `<repo>/docs/meetings/`, which is right for a
// meeting a person convened and wrong for a ROLE ROOM. A role room is lifecycle
// plumbing: pkg/role opens one on every assume and closes it on release, so a
// repo campaign acquiring its lease a dozen times in an afternoon left a dozen
// pairs of minutes in the source tree — all of them empty, because the room
// exists before anyone needs it and usually nobody ever speaks in it.
//
// The churn is the visible half. The load-bearing half is that minutes name
// their attendees — a real hostname and a real OS user — and coreutils ships as
// public MIT source. Auto-generating those into a tracked directory means the
// next `git add` publishes them, with nothing in the flow prompting anyone to
// look. The transcript already lives in the session store; the minutes belong
// beside it.
const OutStore = "store"

type CreateOptions struct {
	// Name is an optional stable, human-facing room name. Ordinary ad-hoc
	// meetings leave it empty; rooms owned by another durable object use it so
	// every surface calls the conversation the same thing. It is deliberately
	// distinct from Topic, which may carry a longer description.
	Name          string
	Topic         string
	Participants  []string
	Secretary     string
	SecretaryBand int
	NoSecretary   bool
	Chair         string
	Agenda        []string
	Context       []string
	Initiator     string
	// Human is the identity that takes the room's human seat — and, unless
	// Initiator names somebody else at the table, convenes it.
	//
	// A transport that AUTHENTICATED its caller sets this to whoever that was. It
	// is not required to look like an OS username: through the tunnel the caller
	// is a cloudbox account, so `qiangli@example.com` is the honest answer and the
	// only one the organizer check can later be applied against. Empty means the
	// caller has no identity beyond the machine it is running on — the loopback
	// and CLI case — and the OS user is used.
	Human        string
	Out          string
	TurnTimeout  string
	DecisionMode string
	MinBand      int
	MinTurnChars int
	MaxTurns     int
	MaxStalls    int
	Steerable    bool
	// DefaultTo is the room's late-bound default addressee: a role LABEL
	// ("conductor:99"), resolved to its holder at READ time. See State.DefaultTo.
	DefaultTo string
	// Board opens a room where participants read and post on their own turns: no
	// chair, no spawned secretary. It implies NoSecretary — a board keeps no
	// minutes and must never arm a pending one.
	Board bool
	// FromMB seeds the board with these message-board posts as opening context,
	// attributed to their ORIGINAL authors, and posts a pointer BACK to mb so the
	// thread and the room are correlated — a room is the correlation id mb never
	// had. It requires Board; a meeting that spawns turns has no board to seed.
	FromMB []int64
}

// Rooms lists the active meetings a channel sidebar can attach to.
//
// It goes through openRooms (not listSessions) so an open meeting that predates
// room numbers is given one on the way past — the list is where a human reads the
// number they are about to type, and one that appeared only after some other
// command ran would be a door that is sometimes there. Closed and abandoned
// meetings remain available through the history-oriented CLI (`meet list`), but
// they have released their room numbers and must not reappear in Relay as stale
// copies of active channels with the same topic or door.
func Rooms() ([]RoomSummary, error) {
	sessions, err := openRooms()
	if err != nil {
		return nil, err
	}
	out := make([]RoomSummary, 0, len(sessions))
	for _, s := range sessions {
		if s.Status != "open" {
			continue
		}
		out = append(out, RoomSummary{
			ID: s.ID, Room: s.Room, Name: s.Name, Permanent: s.Permanent,
			Board: s.Board, Topic: s.Topic, Status: s.Status,
			Members: s.attendees(), Updated: lastActivity(s),
		})
	}
	return out, nil
}

// lastActivity is when the room last said anything, for sorting a channel list
// by recency. The transcript's mtime is the honest answer — state.json is
// rewritten by bookkeeping (a room backfill, a round counter) that nobody said
// anything during. Falls back to Created for a room that has yet to speak.
func lastActivity(s *State) time.Time {
	dir, err := storeDir(s.ID)
	if err != nil {
		return s.Created
	}
	if fi, err := os.Stat(filepath.Join(dir, "transcript.jsonl")); err == nil {
		return fi.ModTime()
	}
	return s.Created
}

// Room returns a room's header and the secretary's latest pass. The synthesis is
// nil when no pass has run — including for a room with no secretary, which will
// never have one.
func Room(ref string) (*State, *Synthesis, error) {
	st, err := roomOf(ref)
	if err != nil {
		return nil, nil, err
	}
	return st, loadSynthesis(st.ID), nil
}

// Create opens a room and returns its saved header.
//
// It builds through sessionFlags.newState so a room created from a browser is
// held to exactly the invariants `meet open` enforces: band seating, roster
// canonicalization, and Validate's separation of powers, in that order.
//
// It does NOT call guardDepth. That guard refuses to convene a meeting from
// inside one, and it is about an AGENT MID-TURN spawning its own panel — the
// recursion that forks exponentially. An HTTP client is not a turn, and a server
// process that marked its own depth (so the agents it spawns are correctly
// inside a meeting) would otherwise be unable to open a room at all.
//
// Opts.Human is what makes the room OPENABLE by a cloud-authenticated caller.
// Validate insists the initiator be somebody at the table, and at CREATE time the
// table is whatever this call is about to build — so a caller whose identity was
// not seated could not possibly pass it. Nothing about that check was wrong; it
// was being applied to an identity the room had not been told about. Seating the
// authenticated caller as the room's human is what tells it, and it is also the
// truth: the person who opened the room is in it.
func Create(opts CreateOptions) (*State, error) {
	// A board keeps no minutes and must never arm a pending secretary, so it is
	// NoSecretary by construction whatever the caller passed.
	if opts.Board {
		opts.NoSecretary = true
	}
	if len(opts.FromMB) > 0 && !opts.Board {
		return nil, fmt.Errorf("meet: --from-mb seeds a board; pass --board")
	}
	sf := sessionFlags{
		topic: opts.Topic, participants: opts.Participants,
		secretary: opts.Secretary, chair: opts.Chair,
		agenda: opts.Agenda, context: opts.Context,
		initiator: opts.Initiator, human: opts.Human, out: opts.Out,
		turnTimeout: opts.TurnTimeout, decisionMode: opts.DecisionMode,
		minBand: opts.MinBand, minTurnChars: opts.MinTurnChars,
		maxTurns: opts.MaxTurns, maxStalls: opts.MaxStalls,
		steerable: opts.Steerable, board: opts.Board,
	}
	if strings.TrimSpace(sf.out) == "" {
		sf.out = "docs"
	}
	if strings.TrimSpace(sf.turnTimeout) == "" {
		sf.turnTimeout = "20m"
	}
	// A room opened through the API always names its organizer, for the reason
	// requireOrganizer gives: an unnamed initiator disables the privilege check
	// permanently, and a room reachable from a browser is exactly the one that
	// needs it.
	//
	// It defaults to the room's own human rather than to humanName(). Those are
	// the same string on loopback and different ones through the tunnel, and using
	// the host's OS user there was the bug: the room got an organizer nobody had
	// seated, and Validate — correctly — refused to open it.
	if strings.TrimSpace(sf.initiator) == "" {
		sf.initiator = sf.humanSeat()
	}
	st, err := sf.newState()
	if err != nil {
		return nil, err
	}
	st.Name = strings.TrimSpace(opts.Name)
	st.DefaultTo = strings.TrimSpace(opts.DefaultTo)
	if strings.TrimSpace(st.Secretary) == "" && !opts.NoSecretary && StartRoomSecretary != nil {
		st.SecretaryPending = true
		st.SecretaryBand = opts.SecretaryBand
		if st.SecretaryBand == 0 {
			st.SecretaryBand = 2
		}
	}
	if err := st.save(); err != nil {
		return nil, err
	}
	for _, a := range st.Agenda {
		_, _ = record(st, "agenda", procedural(st), string(RoleChair), a)
	}
	if len(opts.FromMB) > 0 {
		if _, err := SeedBoardFromMB(st, opts.FromMB); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// SetDefaultTo declares an existing room's late-bound default addressee.
//
// It exists because DefaultTo was added to a codebase that already had rooms:
// a field set only at Create is inert on every room that predates it, which on
// this host was ALL of them, including the one sprint room the feature was
// written for. A feature that only works for rooms nobody has opened yet has
// not shipped.
//
// Idempotent and narrowing-only in practice: the caller passes the label the
// room's own role derives, so re-running it is a no-op. It writes nothing when
// the value is unchanged, so a heal on a read path costs no disk.
func SetDefaultTo(ref, label string) error {
	st, err := roomOf(ref)
	if err != nil {
		return err
	}
	label = strings.TrimSpace(label)
	if st.DefaultTo == label {
		return nil
	}
	st.DefaultTo = label
	return st.save()
}

// SetName gives an existing object-owned room its stable human-facing name.
// It is the migration half of CreateOptions.Name: sprint rooms opened before
// names existed must become "sprint <id>" when their lifecycle next touches
// them, without replacing the room and losing its transcript.
func SetName(ref, name string) error {
	st, err := roomOf(ref)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if st.Name == name {
		return nil
	}
	st.Name = name
	return st.save()
}

// SeedBoardFromMB seeds a board with message-board posts as opening context and
// posts a pointer BACK to mb, in one step. It is the shared body of `meet open
// --board --from-mb` and CreateOptions.FromMB, so the CLI and a programmatic
// Create seed identically.
//
// The fetch rides the FetchMB seam (Meet does not import pkg/bus); a bare
// embedding with no seam wired is told so rather than silently seeding nothing.
// Each post is recorded attributed to its ORIGINAL author — the board is the
// correlation id the thread lacked, and re-attributing the context to whoever
// opened the room would erase who actually said it. The pointer back is
// best-effort: a board that cannot announce itself is still a usable board, but
// the CLI surfaces the failure so a silent non-report never reads as delivered.
func SeedBoardFromMB(st *State, seqs []int64) (pointerPosted bool, err error) {
	if !st.board() {
		return false, fmt.Errorf("meet: --from-mb seeds a board; %s is not one", st.ID)
	}
	if len(seqs) == 0 {
		return false, fmt.Errorf("meet: --from-mb needs at least one post sequence")
	}
	if FetchMB == nil {
		return false, fmt.Errorf("meet: --from-mb is unavailable: no message-board seam is wired")
	}
	posts, err := FetchMB(seqs)
	if err != nil {
		return false, fmt.Errorf("meet: fetch mb posts: %w", err)
	}
	for _, p := range posts {
		author := strings.TrimSpace(p.From)
		if author == "" {
			author = "mb"
		}
		text := strings.TrimSpace(p.Body)
		if t := strings.TrimSpace(p.Topic); t != "" {
			text = "[" + t + "] " + text
		}
		ev := Event{
			Round: st.Round, Speaker: canonAgent(author), Role: string(RoleParticipant),
			Kind: "message", Text: fmt.Sprintf("mb #%d — %s", p.Seq, sanitizeTurn(text)), TS: nowFn(),
			Origin: &EventOrigin{Source: "mb", Seq: p.Seq},
		}
		if err := AppendEvent(st.ID, ev); err != nil {
			return false, err
		}
	}
	if PostMB != nil {
		body := fmt.Sprintf("seeded board %s from mb %s — reply there; the room is the thread.\n"+
			"read: bashy meet read %s --as <you> --wait 15m",
			st.durableRef(), joinSeqs(seqs), st.ID)
		if _, err := PostMB(MBPost{From: st.initiatorName(), Topic: st.Topic, Body: body}, nil); err != nil {
			return false, fmt.Errorf("meet: post mb pointer: %w", err)
		}
		return true, nil
	}
	return false, nil
}

// joinSeqs renders a seq list the way the pointer post and receipts show it:
// "#3, #7, #12".
func joinSeqs(seqs []int64) string {
	parts := make([]string, len(seqs))
	for i, s := range seqs {
		parts[i] = fmt.Sprintf("#%d", s)
	}
	return strings.Join(parts, ", ")
}

// Post appends a human contribution. It takes NO lease.
//
// That is the whole reason a room can feel like chat rather than like a meeting:
// a message is a single append to an O_APPEND file, so any number of people may
// be typing while an agent holds the floor for its turn. The lease guards ROUND
// EXECUTION — the read-modify-write over st.Round and the turn loop — and a post
// is neither.
func Post(ref, author, text string) (Event, error) {
	return PostAs(ref, author, "", text)
}

// PostAs appends a caller-attributed message.
//
// In ordinary rooms this preserves Post's long-standing behavior: an empty
// author is credited to the room human, and the event is a human contribution.
// In board rooms the author is the participant seat, so it is mandatory and
// must already be invited.
func PostAs(ref, author, to, text string) (Event, error) {
	st, err := roomOf(ref)
	if err != nil {
		return Event{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Event{}, fmt.Errorf("meet: an empty message is not a contribution")
	}
	who := strings.TrimSpace(author)
	if st.board() {
		if err := bus.ValidateCoordinationBody(text); err != nil {
			return Event{}, fmt.Errorf("meet tell: %w", err)
		}
		if who == "" {
			return Event{}, fmt.Errorf("meet: --as NAME is required on a board")
		}
		who, err = bus.ResolveAuthoredActor(who)
		if err != nil {
			return Event{}, err
		}
		who = canonAgent(who)
		if !participantSeat(st, who) {
			// An open board delegates seating to a declared audience: a matching
			// agent self-seats on this, its first post, rather than being refused.
			// A non-match still gets the ordinary not-seated error.
			seated, err := selfSeat(st, who)
			if err != nil {
				return Event{}, err
			}
			if !seated {
				return Event{}, fmt.Errorf("meet: %s has no seat in board %s; invite it with `bashy meet invite %s %s`",
					seatLabel(who), st.ID, st.ID, who)
			}
		}
		target := ""
		if isAllSeats(to) {
			target = AllSeats
		} else if strings.TrimSpace(to) != "" {
			target = canonAgent(strings.TrimSpace(strings.TrimPrefix(to, "@")))
			if !participantSeat(st, target) {
				return Event{}, fmt.Errorf("meet: failed: %s has no seat in board %s; invite it with `bashy meet invite %s %s`",
					seatLabel(target), st.ID, st.ID, target)
			}
		}
		ev := Event{
			Round: st.Round, Speaker: who, Role: string(RoleParticipant),
			Kind: "message", To: target, Text: sanitizeTurn(text), TS: nowFn(),
		}
		if st.Shared {
			// The post's universal id, so every host files it once.
			ev.Origin = &EventOrigin{Source: sharedOriginPrefix + bus.NewPostID()}
		}
		if err := AppendEvent(st.ID, ev); err != nil {
			return ev, err
		}
		return ev, relayIfShared(st, ev)
	}
	if err := ensureRoomSecretary(context.Background(), st); err != nil {
		return Event{}, err
	}
	if who == "" {
		who = st.Human
	}
	// A chaired room carries an addressee too. It did not, and that was half of
	// why a sprint's own room was write-only: a question asked there named
	// nobody, so nobody was accountable for answering it. An explicit --to wins;
	// otherwise the room's late-bound DefaultTo applies, which for a sprint room
	// is `conductor:<n>` — the SEAT. The label is stored verbatim and resolved to
	// a holder at READ time, so a handover re-targets mail already in flight.
	target := strings.TrimSpace(strings.TrimPrefix(to, "@"))
	switch {
	case isAllSeats(target):
		// An EXPLICIT broadcast. It must not fall through to DefaultTo below:
		// in a sprint's room that would silently redirect "everyone" to the
		// conductor's seat, and every other participant would then see nothing
		// at all — directed mail is filtered out of the room-history bucket, so
		// a message meant for the whole room would reach exactly one reader
		// while its author was told it went to all of them.
		target = AllSeats
	case target == "":
		target = strings.TrimSpace(st.DefaultTo)
	default:
		if _, isRole := bus.RoleHolderFor(target); !isRole {
			target = canonAgent(target)
		}
	}
	ev := Event{
		Round: st.Round, Speaker: who, Role: string(RoleHuman),
		Kind: "human", To: target, Text: text, TS: nowFn(),
	}
	return recordFull(st, ev)
}

func participantSeat(st *State, name string) bool {
	name = canonAgent(strings.TrimSpace(strings.TrimPrefix(name, "@")))
	for _, p := range st.Participants {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	// People at the table post and are addressed on their own behalf.
	return st.humanAttendee(name)
}

// Address puts a message to ONE agent and returns its reply — the REPL's
// `@name <text>`, which is how an agent answers in a chat room.
//
// It takes the room lease for the duration, because a turn appends to the
// transcript that a concurrent round is also writing, and two speakers holding
// the floor at once is the exact incoherence lease.go was added for.
func Address(ctx context.Context, ref, agent, text string) (Event, error) {
	return addressJob(ctx, ref, agent, text, nil)
}

// addressJob is Address with the RECALL boundary made explicit.
//
// j is the in-flight dispatch this run belongs to, or nil for a CLI call that
// nobody can recall. Two things happen through it, and they are the whole of
// the cancel/retract split described in recall.go: the run refuses to write
// anything once its context is cancelled, and the instant it DOES write, it
// says so — after that point the message exists and only a retraction can
// withdraw it.
func addressJob(ctx context.Context, ref, agent, text string, j *liveJob) (Event, error) {
	st, err := roomOf(ref)
	if err != nil {
		return Event{}, err
	}
	name := strings.TrimSpace(strings.TrimPrefix(agent, "@"))
	if name == "" {
		return Event{}, fmt.Errorf("meet: no agent addressed")
	}
	if holder := strings.TrimSpace(st.RoleHolders[strings.ToLower(name)]); holder != "" {
		name = holder
	} else if st.Permanent && strings.EqualFold(name, st.Name) {
		holder, err := ensurePermanentRoleStarted(ctx, st, strings.ToLower(name))
		if err != nil {
			return Event{}, err
		}
		name = holder
	}
	if err := ensureRoomSecretary(ctx, st); err != nil {
		return Event{}, err
	}
	lease, err := acquireRunLease(st.ID)
	if err != nil {
		return Event{}, err
	}
	defer lease.Release()
	target := canonAgent(name)
	// THE BOUNDARY. Everything above this line is preparation that leaves no
	// trace a reader could see, so a recall arriving here still means "not
	// sent". Checked after the lease rather than before, because waiting for a
	// busy room is exactly when a sender changes their mind.
	if err := ctx.Err(); err != nil {
		return Event{}, errRecalled
	}
	asked, err := recordAsked(st, target, text)
	if err != nil {
		return Event{}, err
	}
	j.markCommitted(asked)
	ev, err := runTurn(ctx, st, target, text, apiRunner())
	if err != nil {
		// The question stays UNREAD deliberately: the agent never answered it, so
		// the next `meet dispatch` should still deliver it. Same rule Dispatch
		// applies to its own failed turns.
		return ev, err
	}
	markAnswered(st, target)
	return ev, nil
}

// recordAsked writes the human's question into the room BEFORE the agent runs.
//
// Without it, addressing an agent from the browser made your own message
// disappear: `Address` recorded only the reply, so the room showed answers to
// questions nobody could see, and a turn that timed out left no trace that
// anything had been asked at all. The 1:1 chat never had that problem — a DM
// records the human's message and then runs the turn (relay_dm.go), which is
// the same store — so this makes the room agree with the chat rather than
// inventing a shape.
//
// The addressee is the agent, for the same reason it is in a DM: that is what
// makes it directed mail in that agent's unified inbox, and what a later
// handover re-targets. markAnswered is the other half — mail answered on the
// spot must not be delivered a second time.
func recordAsked(st *State, agent, text string) (Event, error) {
	who := strings.TrimSpace(st.Human)
	if who == "" {
		// Every room-creating path names a human (meet.go humanSeat, serve.go
		// actorOf). A room that somehow has none gets no invented one: an event
		// attributed to "" is worse than an unrecorded question.
		return Event{}, nil
	}
	return recordFull(st, Event{
		Round: st.Round, Speaker: who, Role: string(RoleHuman),
		Kind: "human", To: agent, Text: text, TS: nowFn(),
	})
}

// markAnswered advances the addressed agent's read cursor past the exchange it
// just took part in.
//
// The question above is directed mail, and Dispatch wakes on exactly that — so
// without this, an agent that answered synchronously would be handed the same
// question again on the next dispatch pass and would answer it twice. The mark
// is honest rather than convenient: every turn is given the whole transcript as
// context (transcriptContext), so an agent that took a turn HAS been shown the
// room through that point. It is the same acknowledgement Dispatch makes after
// its own turns.
//
// A cursor that fails to advance is reported IN THE ROOM rather than returned:
// the turn is recorded and the reply is real, and the async job path turns any
// error here into a "did not run" note, which would be a false report of the
// one thing that did run.
func markAnswered(st *State, agent string) {
	_, _, _, through, err := UnreadThrough(st.ID, agent, 0)
	if err == nil {
		err = MarkSeenThrough(st.ID, agent, through)
	}
	if err != nil {
		_, _ = record(st, "note", otelServiceName, "",
			fmt.Sprintf("%s answered, but its read mark did not advance (%v); `bashy meet dispatch` may ask again", agent, err))
	}
}

// apiRunner supplies the chat.Runner the exported verbs run turns with. In
// production it returns nil, which is what makes chat.Invoke build the real
// exec runner — behaviour is unchanged.
//
// It exists because the HTTP surface had no way to be driven end to end.
// serve_test.go's header records the constraint that forced that:
// "the routes that run agents … are exercised only as far as their 202/409
// contract: actually running one would spawn a real CLI, which is the one thing
// a hermetic suite must never do." The seam removes the dilemma instead of
// living with it — a test substitutes a canned runner and the SAME transport,
// lease, transcript and live-tee code runs, so what the suite proves is the
// path the browser actually takes.
//
// Same shape as operableFn (roster.go) and nowFn (session.go): one package-level
// var, overridden and restored by the test that needs it.
var apiRunner = func() chat.Runner { return nil }

// Round runs one moderated round across the participants, on the agenda item the
// room is currently on. runRound takes the lease itself.
func Round(ctx context.Context, ref string) ([]Event, error) {
	st, err := roomOf(ref)
	if err != nil {
		return nil, err
	}
	if st.board() {
		return nil, st.boardRefusal("run a round")
	}
	return runRound(ctx, st, currentAgenda(st), apiRunner())
}

// Poll puts a fixed-choice question to every participant and tallies the answers.
// An empty choice set is the default yes/no — runPoll owns that default.
//
// Poll and Ask below are the WHOLE-ROOM form. `meet poll --participant` narrows
// the same call to a subset and therefore keeps passing its own operand list to
// runPoll: runPoll is the single implementation of a poll, and these are the
// two-line spelling of it that the frozen contract names. Nothing is duplicated
// here — widening the exported signature to carry a filter no browser sends
// would be the change that has to justify itself, not this.
func Poll(ctx context.Context, ref, q string, choices []string) (*PollResult, error) {
	st, err := roomOf(ref)
	if err != nil {
		return nil, err
	}
	if st.board() {
		return nil, st.boardRefusal("run a poll")
	}
	return runPoll(ctx, st, q, choices, nil, apiRunner())
}

// Ask puts an open question to every participant. Answering is optional here, as
// it is for the REPL's `/ask`: silence is a recorded abstention, not a failure.
func Ask(ctx context.Context, ref, q string) ([]Event, error) {
	st, err := roomOf(ref)
	if err != nil {
		return nil, err
	}
	if st.board() {
		return nil, st.boardRefusal("put a question to the room")
	}
	return runAsk(ctx, st, q, true, nil, apiRunner())
}

// Converge runs the secretary pass. A room with no secretary is refused by
// converge itself, in terms of the recovery (`--secretary`), rather than here.
func Converge(ctx context.Context, ref string) (*Synthesis, error) {
	st, err := roomOf(ref)
	if err != nil {
		return nil, err
	}
	return converge(ctx, st, apiRunner())
}

// Close converges, files the minutes, and marks the room closed. Organizer only —
// the same privilege that gates the roster: any member may post, only whoever
// convened the room may dissolve it.
//
// The confirm step is neutralized with Yes:true rather than skipped. Confirm:false
// would leave no record that the room was concluded deliberately; Yes records a
// `confirm` event naming the actor, and — the part that matters for a transport —
// returns before confirmConclusion ever reaches for stdin. An HTTP handler has no
// terminal, and the prompt path would either error out or block on a closed pipe.
func Close(ref, actor string) error {
	_, err := closeRoom(context.Background(), ref, actor, closeOptions{
		Synthesize: true, Confirm: true, Yes: true,
		In: strings.NewReader(""), Out: io.Discard,
	})
	return err
}

// Open reopens a closed room with a fresh session artifact set. The prior
// transcript, synthesis, live stream, and full turn files move under
// archive/<timestamp>; a permanent room additionally retains its stable name.
func Open(ref, actor string) (*State, error) {
	id, err := resolveMeeting(ref)
	if err != nil {
		return nil, err
	}
	lease, err := acquireRunLease(id)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	st, err := loadState(id)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizer(st, actor); err != nil {
		return nil, err
	}
	if st.Status == "open" {
		return st, nil
	}
	if err := archiveSessionArtifacts(st); err != nil {
		return nil, err
	}
	st.Round = 0
	st.Created = nowFn()
	st.Status = "open"
	st.Room = assignRoom()
	if err := st.save(); err != nil {
		return nil, err
	}
	return st, nil
}

func archiveSessionArtifacts(st *State) error {
	dir, err := storeDir(st.ID)
	if err != nil {
		return err
	}
	archive := filepath.Join(dir, "archive", nowFn().UTC().Format("20060102T150405.000000000Z"))
	// "seen" is the per-participant read cursor store. It MUST move with the
	// transcript: reopening restarts ordinals at 1, and a seen/<name> left holding
	// the prior session's high-water mark leaves every participant permanently
	// "caught up" — MarkSeen never moves a cursor backwards, so they would never
	// see another message. Archiving it resets the cursor to zero along with the log.
	for _, name := range []string{"transcript.jsonl", "synthesis.json", "live.jsonl", "turns", "seen"} {
		source := filepath.Join(dir, name)
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.MkdirAll(archive, 0o700); err != nil {
			return err
		}
		if err := os.Rename(source, filepath.Join(archive, name)); err != nil {
			return fmt.Errorf("meet: archive %s: %w", name, err)
		}
	}
	return nil
}

// closeRoom is the body `meet close` and Close share.
//
// An EMPTY actor skips the organizer check, and that is not a hole — it is the
// attended path. On a terminal the check that matters is confirmConclusion, which
// asks the room's INITIATOR whether it may end (prompting a human, or putting the
// question to an agent through its own channel); who typed the command is not the
// question. An unattended caller — the HTTP route — has no such prompt to lean
// on, so it names an actor and gets the privilege check instead.
func closeRoom(ctx context.Context, ref, actor string, opt closeOptions) (string, error) {
	st, err := roomOf(ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(actor) != "" {
		if err := requireOrganizer(st, actor); err != nil {
			return "", err
		}
	}
	probe, err := acquireRunLease(st.ID)
	if err != nil {
		return "", err
	}
	probe.Release()
	return closeMeeting(ctx, st, opt, apiRunner())
}

// markKinds are the human markers a caller may write into the transcript. The set
// is closed on purpose: `kind` is what every reader switches on (the minutes
// renderer, coverage, the observer), so an invented kind is an event nothing
// displays — recorded, and invisible.
var markKinds = map[string]bool{
	"decision": true,
	"action":   true,
	"agenda":   true,
	"note":     true,
}

// Mark records a human marker: a decision, an action item, an agenda item, or a
// note. These are the REPL's `/decision`, `/action`, and `/agenda`, and they stay
// AUTHORITATIVE over anything the secretary later extracts — the secretary's job
// is to extract, never to overrule.
func Mark(ref, kind, text string) (Event, error) {
	st, err := roomOf(ref)
	if err != nil {
		return Event{}, err
	}
	k := strings.ToLower(strings.TrimSpace(kind))
	if !markKinds[k] {
		return Event{}, fmt.Errorf("meet: %q is not a marker kind; use decision, action, agenda, or note", kind)
	}
	if strings.TrimSpace(text) == "" {
		return Event{}, fmt.Errorf("meet: an empty %s is not a marker", k)
	}
	if k == "agenda" {
		// An agenda item is a procedural act — the chair's, whoever holds it — and
		// it is added to the header as well as recorded, because currentAgenda()
		// reads the header to decide what the next round is about.
		st.Agenda = append(st.Agenda, text)
		if err := st.save(); err != nil {
			return Event{}, err
		}
		return record(st, "agenda", procedural(st), string(RoleChair), text)
	}
	return record(st, k, st.Human, "", text)
}
