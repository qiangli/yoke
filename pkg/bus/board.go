package bus

// THE BOARD STORE — public by construction, and its own.
//
// `mb` first rode the bus's subscription + pending machinery, and that was the
// wrong substrate for a public board twice over.
//
// # It needed setup it should not need
//
// A post only reached an agent that HELD A SUBSCRIPTION, so the board depended
// on a reconcile step, and a name nobody had reconciled silently received
// nothing. A public board has no membership: you read it because it is there.
//
// # It resolved identity two different ways, and they disagreed
//
// The send side addressed the FLEET NAME (`codex-gpt5.6-sol`, what
// `bashy agent list` prints). The read side resolved `$BASHY_PRINCIPAL` →
// `$USER`, which is `dhnt:agent/Omar` for a bashy-launched agent and the login
// name for everything else. So a post addressed to an agent landed in a buffer
// that agent would never read, and every reader had to be told its own name
// with --as. Two spellings of one identity is the same defect as two live
// things sharing one address; it just fails quietly instead of loudly.
//
// # So the store is one public log plus a per-reader cursor
//
//	posts.jsonl    every post, append-only, world-readable on this host
//	seen/<reader>  one integer: the last sequence that reader has been shown
//
// That is the whole model. There is no per-recipient copy to fall out of sync,
// no membership to forget, and "read someone else's messages" is not a
// privilege escalation — it is the normal case, because the board is public.
// Addressing (`To`) is a HINT about who should act, never an access control.
//
// The bus keeps its subscriptions and pending buffers: they carry interrupts
// and topic-routed notifications, which genuinely are per-subscriber and
// genuinely are governed. The board is the public half and now owns its own
// storage.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/room"
)

// BoardSchema versions the on-disk post record.
const BoardSchema = "bashy-mb-v1"

// Post is one message on the board.
type Post struct {
	// ID is the post's UNIVERSAL identity: a UUIDv7 minted when the post is
	// first written, on whichever host that was. Seq is a per-store handle
	// (`mb:<seq>` means something only on this host); ID is the same string
	// on every host a message is delivered to, so `mb:<uuid>` resolves
	// everywhere and a redelivery can be recognised (docs/uniform-ref-
	// addressing.md D6/D10: the uuid is the identity, seq is input-only).
	// A legacy record without one reads as before; nothing renumbers.
	ID string `json:"id,omitempty"`
	// IdempotencyKey is set by PostMessageOnce for durable system notices and
	// for a message DELIVERED from another host (key = its ID).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	SchemaVersion  string `json:"schema_version"`
	Seq            int64  `json:"seq"`
	At             string `json:"at"`
	From           string `json:"from"`
	// To is the single agent expected to act. Empty means the post is not
	// directed at one agent — see Audience. It is a hint about who should act,
	// never a permission: every reader can see every post.
	To string `json:"to,omitempty"`
	// Audience is a GROUP the post is for, stored as the SELECTOR rather than
	// expanded into one post per member.
	//
	// Expanding was the first implementation and it made the board grow with
	// the size of the audience: `--band 4` wrote eight identical posts, so
	// `--all` became unreadable and every reader's scan got longer even though
	// only one line concerned them. Storing the selector keeps the board's
	// length independent of how many agents a message reaches.
	//
	// It is resolved AT READ TIME, so relevance follows the role rather than
	// whoever held it when the post was written: "L4 agents should know this"
	// is a statement about the seat, and an agent promoted afterwards should
	// see it. The opposite is defensible; this is the choice.
	Audience *Audience `json:"audience,omitempty"`
	// Mode says who OF the audience is expected to act: ModeAll (everyone,
	// views counted) or ModeAny (the first taker claims it). Empty means all.
	Mode  string `json:"mode,omitempty"`
	Topic string `json:"topic,omitempty"`
	Body  string `json:"body"`
}

// Broadcast reports a post for everyone: no named recipient and no selector.
func (p Post) Broadcast() bool {
	return strings.TrimSpace(p.To) == "" && (p.Audience == nil || p.Audience.Empty())
}

// Directed reports a post naming ONE agent — the only kind that carries an
// obligation, and therefore the only kind never truncated from a default view.
func (p Post) Directed(reader string) bool {
	to := strings.TrimSpace(p.To)
	if to == "" {
		return false
	}
	if strings.EqualFold(to, strings.TrimSpace(reader)) {
		return true
	}
	// A post addressed to a ROLE on this host is directed at whoever is reading,
	// because a seat is host-and-login scoped rather than tied to an identity.
	// That is what lets a third-party TUI read the seat's mail with no --as, no
	// principal and no setup — and it matches the board's existing rule that
	// addressing says who should ACT, never who may read.
	return AddressedToRole(to)
}

// Audiences describes a post's intended audience for display.
func (p Post) Audiences() string {
	if p.To != "" {
		// Render a seat address by the name people use for it. `steward` is what
		// a reader can act on; `steward.dragon-u501-b683b300b1` is what the
		// machine routes on, and showing the latter makes the one post that
		// carries an obligation look like machine noise.
		return RoleLabelFor(p.To)
	}
	if p.Audience == nil || p.Audience.Empty() {
		return "all"
	}
	var parts []string
	if p.Audience.Band != 0 {
		parts = append(parts, "band "+strconv.Itoa(p.Audience.Band))
	}
	for _, kv := range [][2]string{
		{"tool", p.Audience.Tool}, {"provider", p.Audience.Provider},
		{"family", p.Audience.Family}, {"version", p.Audience.Version},
		{"role", p.Audience.Role},
	} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, " · ")
}

// ForReader reports whether a post concerns this reader: addressed to it,
// matching a selector it satisfies, or broadcast.
//
// An ModeAny post already CLAIMED by somebody else concerns nobody else — that
// is the point of offering work to a pool rather than announcing it.
func (p Post) ForReader(reader string) bool {
	return p.forReader(reader, newAudienceSnapshot())
}

// FilterPostsForReader selects the posts that concern reader, preserving their
// order and leaving posts unchanged. Like Post.ForReader, it includes directed
// posts and broadcasts and excludes another reader's claimed work offers.
//
// Each call resolves each distinct audience at most once, including unavailable
// or empty rosters. Call it once per read operation: membership is stable across
// posts with the same selector and refreshes on the next call. It does not
// advance cursors, claim offers, or record views.
func FilterPostsForReader(posts []Post, reader string) []Post {
	audiences := newAudienceSnapshot()
	var out []Post
	for _, p := range posts {
		if p.forReader(reader, audiences) {
			out = append(out, p)
		}
	}
	return out
}

func (p Post) forReader(reader string, audiences audienceSnapshot) bool {
	if p.Directed(reader) || p.Broadcast() {
		return true
	}
	if p.Audience == nil || p.Audience.Empty() {
		return false
	}
	if !audiences.inAudience(*p.Audience, reader) {
		return false
	}
	if p.Mode == ModeAny {
		if h := ClaimHolder(p.Seq); h != "" && !strings.EqualFold(h, reader) {
			return false
		}
	}
	return true
}

// --- concerns: the subject as a ROUTE, not an address ------------------------
//
// A post's Topic names what it is ABOUT; a reader's bus subscription topics
// (Subscription.Topics) name what it CARES ABOUT. When the two match, the post
// is routed into that reader's uncapped tier: shown in full by Unseen, like a
// directed post, instead of being trimmed to the newest -n with a count.
//
// This is deliberately a ROUTE in Yoke's sense — a policy-controlled mapping
// from ingress to a principal — and NOT a fifth Address kind. Yoke's address
// kinds (principal, agent, tool, role seat, person, host, group, external) are
// all IDENTITIES: a who that resolves at send time, holds a cursor, and can be
// reported on in a receipt. A concern is a PREDICATE OVER MESSAGES — a what —
// with no cursor, no seat and no inbox, so making it addressable would leave
// every receipt state below `accepted` unreachable and fork the model this
// family was just aligned to. Hence: the declaration lives on the reader's own
// SUBSCRIPTION, the tag lives on the POST, resolution happens at READ time,
// and the send path is untouched — `mb send shared-baseline` still fails with
// choices, because a subject is not somebody you can send to.
// See docs/mb-addressing-model.md.
//
// The mechanism is general — any topic can be declared — but a vocabulary
// nobody shares routes nothing, so this small set is the documented
// convention:
//
//	shared-baseline  changes to state every agent builds on: a frozen
//	                 directory, a rebuilt shared toolchain, a moved gate
//	posix-cert       the certification campaign
//	harness          agent harness and tooling changes (a rebuilt bashy,
//	                 new or renamed verbs)
//	announce         this board's `wall`: the one concern EVERY reader is
//	                 subscribed to by default
//
// Two rules keep the route honest, and they are conventions enforced by review
// rather than code: DECLARING A CONCERN IS AN OBLIGATION TO READ IT — the
// declaration is what lifts the cap, so declaring and then skimming defeats
// the cap for nothing — and tagging `announce` to skip everyone's cap is the
// same defection as marking every email urgent, visible to everyone on a
// public board.
const ConcernAnnounce = "announce"

// WellKnownConcerns is the documented convention set — see the block above.
var WellKnownConcerns = []string{"shared-baseline", "posix-cert", "harness", ConcernAnnounce}

// OnConcern reports whether this post rides one of the given declared
// concerns. Patterns match the way subscription topics always have —
// segment-anchored, so a declared `cert.*` covers a post tagged `cert.posix`.
//
// A work offer (ModeAny) is never concern-routed: it belongs to its pool and
// its claim rule, and a declarer outside the pool must not be handed — much
// less claim — work that was not offered to it.
func (p Post) OnConcern(concerns []string) bool {
	if p.Mode == ModeAny || strings.TrimSpace(p.Topic) == "" {
		return false
	}
	for _, c := range concerns {
		if topicMatch(c, p.Topic) {
			return true
		}
	}
	return false
}

// audienceSnapshot memoizes each distinct selector for one board read or
// archive scan. It belongs to that operation, never to the process: embedded
// readers must observe lease handoffs and expiry on their next read. A snapshot
// is used by one goroutine; concurrent reads each own a separate map.
// Resolution errors and empty rosters are also memoized for this operation.
type audienceSnapshot map[Audience]audienceMembers

type audienceMembers struct {
	names map[string]bool
	size  int
}

func newAudienceSnapshot() audienceSnapshot {
	return make(audienceSnapshot)
}

func (s audienceSnapshot) resolve(aud Audience) audienceMembers {
	if members, ok := s[aud]; ok {
		return members
	}
	var members audienceMembers
	if FleetSelect != nil {
		if names, err := FleetSelect(aud); err == nil {
			members.names = make(map[string]bool, len(names))
			members.size = len(names)
			for _, n := range names {
				members.names[strings.ToLower(strings.TrimSpace(n))] = true
			}
		}
	}
	s[aud] = members
	return members
}

func (s audienceSnapshot) inAudience(aud Audience, reader string) bool {
	return s.resolve(aud).names[strings.ToLower(strings.TrimSpace(reader))]
}

func (s audienceSnapshot) audienceSize(aud Audience) int {
	return s.resolve(aud).size
}

// InAudience reports whether reader is in the set a selector currently names.
// Each direct call resolves afresh; board reads reuse one operation's snapshot.
//
// An unresolvable selector matches NOBODY rather than everybody. Erring toward
// silence costs one reader a message they can still find with --all; erring the
// other way turns every group post into a broadcast, which is precisely the
// clutter this design exists to prevent.
func InAudience(aud Audience, reader string) bool {
	return newAudienceSnapshot().inAudience(aud, reader)
}

// BoardDir is the board's store. It is deliberately NOT under the room
// directory: the room holds the bus's private per-subscriber state, and mixing
// a public log into it is how the two got confused in the first place.
func BoardDir() string {
	if d := strings.TrimSpace(os.Getenv("BASHY_MB_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bashy", "mb")
}

func postsPath() string { return filepath.Join(BoardDir(), "posts.jsonl") }

// PostMessage appends to the board. It discards the assigned sequence; callers
// that need it to judge delivery use PostMessageSeq.
func PostMessage(p Post) error {
	_, err := PostMessageSeq(p)
	return err
}

// PostMessageSeq appends to the board and returns the sequence it assigned.
//
// The sequence is what a receipt is measured against: `queued` means a reader's
// cursor is behind THIS seq, `read` means at or past it. A send that could not
// name the seq it wrote could not tell the two apart, so the write returns it.
//
// The append itself, and the seq it is assigned, happen under a short
// best-effort lock (see withBoardLock) rather than bare O_APPEND. That
// changed when archiving did: O_APPEND alone is safe only as long as nothing
// ever REWRITES the file, and RotateBoard now does exactly that to move old
// posts into the archive. The seq is `archivedThrough() + live count + 1`,
// not just live count + 1, so a post written after a rotation can never
// collide with a sequence rotation has already handed to an archived post.
//
// Rotation itself runs after the append, outside the lock and throttled —
// see rotateBoardOpportunistic — so a hygiene sweep never sits on the
// critical path of delivering a message.
func PostMessageSeq(p Post) (int64, error) {
	dir := BoardDir()
	if dir == "" {
		return 0, fmt.Errorf("mb: no board directory")
	}
	if strings.TrimSpace(p.From) == "" {
		// A post with no author is unattributable, and an unattributable
		// message on a shared board is worse than none: nobody can ask the
		// sender what they meant.
		return 0, fmt.Errorf("mb: a post needs a sender")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	p.SchemaVersion = BoardSchema
	if p.At == "" {
		p.At = time.Now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(p.ID) == "" {
		p.ID = NewPostID()
	}

	var writeErr error
	withBoardLock("append", func() {
		existing, _ := Posts()
		p.Seq = archivedThrough() + int64(len(existing)) + 1
		line, merr := json.Marshal(p)
		if merr != nil {
			writeErr = merr
			return
		}
		f, ferr := os.OpenFile(postsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if ferr != nil {
			writeErr = ferr
			return
		}
		_, werr := f.Write(append(line, '\n'))
		cerr := f.Close()
		if werr != nil {
			writeErr = werr
			return
		}
		writeErr = cerr
	})
	if writeErr != nil {
		return 0, writeErr
	}

	rotateBoardOpportunistic()
	return p.Seq, nil
}

// NewPostID mints the universal identity of a post (UUIDv7: time-ordered, so
// a same-minute pair still sorts, and a dashed prefix is a valid ref handle).
func NewPostID() string { return uuid.Must(uuid.NewV7()).String() }

// Posts returns the whole board, oldest first.
//
// A malformed line is SKIPPED rather than fatal: this is an append-only log
// several processes write, and one torn record must not make the board
// unreadable. An absent board is empty and not an error.
func Posts() ([]Post, error) {
	f, err := os.Open(postsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Post
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var p Post
		if json.Unmarshal([]byte(line), &p) != nil {
			continue
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

// Unseen returns what this reader has not been shown, split by obligation.
//
// directed posts NAME this reader, so somebody is waiting on it: they are
// returned in full and never truncated. Capping them is how an assignment gets
// dropped.
//
// Posts on a concern this reader has DECLARED (see OnConcern) are the same
// tier of obligation by the reader's own choice, so they are also never
// truncated — and the route works both ways: a declared concern surfaces a
// matching post even when it is addressed to somebody else, because a standing
// interest in the subject is exactly what the declaration says. They are
// returned inside `other`, in board order, because they carry no reply
// obligation — only the obligation to read.
//
// The rest — broadcasts and selector posts on nothing the reader declared —
// is trimmed to the newest limit, with older reporting how many were left
// out. A cap that does not say what it hid is a silent drop, and a reader who
// cannot tell "nothing else" from "twelve more" will act on the wrong one.
// An UNDECLARED concern is never silently promoted: the tag alone lifts no
// cap, or tagging would become the megaphone the convention forbids.
func Unseen(reader string, limit int) (directed, other []Post, older int, err error) {
	return unseen(reader, limit, newAudienceSnapshot())
}

func unseen(reader string, limit int, audiences audienceSnapshot) (directed, other []Post, older int, err error) {
	all, e := Posts()
	if e != nil {
		return nil, nil, 0, e
	}
	at := SeenSeq(reader)
	concerns := DeclaredConcerns(reader)
	var concerned, rest []Post
	for _, p := range all {
		if p.Seq <= at {
			continue
		}
		onConcern := p.OnConcern(concerns)
		if !onConcern && !p.forReader(reader, audiences) {
			continue
		}
		// Membership is an uncapped tier like a declared concern: a group post
		// says who should ACT on it, and the directed-tier rule — never
		// truncate an obligation — cannot stop at the first address kind.
		// Broadcasts deliberately stay in the capped rest below.
		inAudience := p.Audience != nil && !p.Audience.Empty() && audiences.inAudience(*p.Audience, reader)
		switch {
		case p.Directed(reader):
			directed = append(directed, p)
		case onConcern || inAudience:
			concerned = append(concerned, p)
		default:
			rest = append(rest, p)
		}
	}
	if limit > 0 && len(rest) > limit {
		older = len(rest) - limit
		rest = rest[len(rest)-limit:]
	}
	other = mergeBySeq(concerned, rest)
	return directed, other, older, nil
}

// mergeBySeq interleaves two seq-ordered slices back into board order, so the
// cap exemption changes what a reader is shown, never the order it reads in.
func mergeBySeq(a, b []Post) []Post {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]Post, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		if a[0].Seq <= b[0].Seq {
			out = append(out, a[0])
			a = a[1:]
		} else {
			out = append(out, b[0])
			b = b[1:]
		}
	}
	return append(append(out, a...), b...)
}

func seenPath(reader string) string {
	name := safeName.ReplaceAllString(strings.TrimSpace(reader), "_")
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "anonymous"
	}
	return filepath.Join(BoardDir(), "seen", name)
}

// SeenSeq is the last sequence this reader has been shown. Zero when it has
// never read — so a first-time reader sees the whole board rather than nothing,
// which is what "public" means and the opposite of the private-inbox rule that
// opens a new mailbox at the head.
func SeenSeq(reader string) int64 {
	n, _ := CursorSeq(reader)
	return n
}

// CursorSeq is SeenSeq with the one distinction a receipt cannot collapse:
// whether the reader has a cursor AT ALL. `ok` is false when this reader has
// never read the board — which is NOT the same as a cursor of zero, because a
// reader that has never looked cannot be reported as merely `queued`. That
// difference is the whole reason `unverified` is a separate state.
func CursorSeq(reader string) (seq int64, ok bool) {
	b, err := os.ReadFile(seenPath(reader))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// MarkSeen advances a reader's cursor. Never moves it backwards: re-reading an
// older view must not un-see what was already shown.
func MarkSeen(reader string, seq int64) error {
	if seq <= SeenSeq(reader) {
		return nil
	}
	p := seenPath(reader)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o644)
}

// FleetResolveName maps any spelling of an agent — a nickname, a principal like
// `dhnt:agent/Omar`, a tool:model binding — to its canonical fleet name.
// Injected by the host, for the same reason FleetNames and FleetSelect are.
var FleetResolveName func(string) string

// DetectHarness reports the agentic harness driving this process, injected by
// the host for the same reason FleetNames and FleetSelect are: the marker table
// is registry DATA owned by the catalog (`bashy tool add` extends it), and a
// transport keeping its own copy is a second opinion that can drift.
//
// A nil hook means "this host cannot tell", and BoardIdentity then behaves as it
// did before the check existed. That is the weaker of the two safe directions
// and it is chosen deliberately: pkg/bus is importable by hosts that have no
// catalog at all, and refusing every caller on a host that simply cannot answer
// the question would break the board rather than protect it. The consequence is
// that WIRING THIS IS LOAD-BEARING — an unwired host silently keeps the bug it
// exists to prevent, which is exactly the failure shape this fix is about.
var DetectHarness func() (string, bool)

// ErrUnattributed reports a caller that is demonstrably an agent but cannot be
// resolved to a fleet identity.
var ErrUnattributed = errors.New("unattributed agent session")

// BoardIdentity is WHO YOU ARE on the board, and it exists because the send and
// read sides used to disagree.
//
// Posts are addressed to the fleet name `bashy agent list` prints. But a
// caller's environment carries something else: a bashy-launched agent has
// BASHY_PRINCIPAL=dhnt:agent/<Nick>, and everything else falls back to $USER.
// Resolving those to the same name is what lets a bare `bashy mb` work, instead
// of every agent having to be told its own identity with --as.
//
// The ladder, most explicit first.
//
// # Why the login fallback can refuse
//
// $USER is the right answer for a human at a terminal, who is a legitimate
// board participant under their login name. It is the WRONG answer for an agent
// in a raw TUI, which has no BASHY_PRINCIPAL and inherits the operator's
// environment — and the two are indistinguishable by environment alone unless
// you ask whether a harness is driving the process.
//
// Left un-asked, the fallback misattributes silently, and that is worse than
// failing on both sides of the board:
//
//	SEND  a post is SIGNED with the operator's name. PostMessage refuses a post
//	      with no sender precisely because attribution is the board's one
//	      guarantee — but it cannot detect a DEFAULT sender that is wrong. The
//	      record is then corrupt in a way nothing on the board reports.
//	READ  the cursor, the claim and the viewed-by receipt all land under the
//	      operator's name. A claimed any-of-group post is the sharp edge: the
//	      claim exists, so a second agent correctly skips work that nobody
//	      actually took.
//
// Observed 2026-08-03 on this host: six of eight posts on a live board were
// attributed to the login user, spanning the operator AND two different agents.
// The board could not distinguish the three, and a reply arrived addressed from
// its own recipient.
//
// So when a harness IS detected and nothing resolved, refuse and say what to
// pass. No attribution is better than a guessed one — the whole point of a
// receipt is that it names somebody.
func BoardIdentity(as string) (string, error) {
	if s := strings.TrimSpace(as); s != "" {
		// Explicit always wins, including a human inside an agent session who
		// means to speak as themselves: `--as qiangli`.
		return resolveBoardName(s), nil
	}
	if v := strings.TrimSpace(os.Getenv("BASHY_PRINCIPAL")); v != "" {
		// `dhnt:agent/Omar` → `Omar` → the catalog's canonical name.
		if _, nick, ok := strings.Cut(v, "agent/"); ok {
			if n := resolveBoardName(nick); n != "" {
				return n, nil
			}
		}
		if n := resolveBoardName(v); n != "" {
			return n, nil
		}
	}
	if DetectHarness != nil {
		if tool, ok := DetectHarness(); ok {
			return "", fmt.Errorf("%w: running under %s, with no agent identity to sign with\n"+
				"  pass --as <agent>   (`bashy agent list` names them; `--as %s-<model>` if unsure)\n"+
				"  a human meaning to speak as themselves here passes --as %s\n"+
				"  refusing rather than signing as the login user: the board's one guarantee is that a post names who sent it",
				ErrUnattributed, tool, tool, loginName())
		}
	}
	if n := loginName(); n != "" {
		return n, nil
	}
	return "anonymous", nil
}

func loginName() string {
	for _, k := range []string{"USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func resolveBoardName(s string) string {
	if FleetResolveName != nil {
		if n := strings.TrimSpace(FleetResolveName(s)); n != "" {
			return n
		}
	}
	return s
}

// DefaultBoardLimit caps posts NOT addressed to a reader by name. Five is a
// screenful: enough that a busy board is still scannable at a turn boundary,
// small enough that it does not become the turn.
const DefaultBoardLimit = 5

// describe renders a selector for a confirmation line.
func (a Audience) describe() string {
	return Post{Audience: &a}.Audiences()
}

// --- the push tier: reach a live session immediately ------------------------

// Delivery reports how far a post got for one recipient.
//
// The distinction is the whole point of having two tiers: `steered` means the
// message is in the agent's input and it will act on it this turn; `posted`
// means it is on the board and the agent will see it when it next looks. Both
// are successes and they are NOT the same success, so the sender is told which.
type Delivery struct {
	To      string `json:"to"`
	Steered bool   `json:"steered"`
	Reason  string `json:"reason,omitempty"` // why it could not be steered
	// State is the PROVABLE delivery state — one of the six in the block below.
	// Steered is the raw signal (did SteerLive push?); State is the claim a
	// receipt is allowed to make about it, which is a narrower thing.
	State string `json:"state,omitempty"`
}

// SteerLive injects text into a recipient's live session when it has one.
//
// GRACEFUL DEGRADATION, in one direction only. A bashy-launched agent has a
// control socket, so it can be reached NOW; a raw-launched TUI has none, and no
// amount of trying changes that — writing to its tty would paint its display
// without reaching its reasoning, which is why that path is deliberately absent
// (see pkg/ctty: the tty reaches the HUMAN, never the agent).
//
// So this is best-effort by construction: the board post has already happened
// before it is called, and a failure here costs immediacy, never the message.
// That ordering is not incidental — steering first and posting second would
// lose the message entirely if the post failed, and the durable copy is the one
// that must not be optional.
func SteerLive(agent, text string) Delivery {
	d := Delivery{To: agent}
	members, err := room.Members()
	if err != nil {
		d.Reason = "no session registry"
		return d
	}
	for _, c := range members {
		if !strings.EqualFold(c.ID, agent) && !strings.EqualFold(c.Binding, agent) {
			continue
		}
		if strings.TrimSpace(c.CtlSock) == "" {
			// A shell-only presence card: running under bashy but not launched
			// by it, so there is no socket. This is the common case for a TUI
			// the operator started, and it is why the board exists.
			d.Reason = "session has no control socket (not bashy-launched)"
			return d
		}
		if serr := SteerFrame(c.CtlSock, text); serr != nil {
			d.Reason = serr.Error()
			return d
		}
		d.Steered = true
		_ = room.Emit(room.Event{Type: room.EventSteer, Target: c.ID, Body: text})
		return d
	}
	d.Reason = "not running"
	return d
}

// SteerFrame writes a text frame to a control socket. Injected so pkg/bus does
// not depend on the pty layer's transport details, and so a test can observe a
// steer without a live agent on the other end.
var SteerFrame = func(sock, text string) error {
	return agentpty.SendFrame(sock, agentpty.TextFrame(text))
}

// --- delivery modes: who, of a group, is expected to act --------------------
//
// A post to a GROUP means one of two different things, and conflating them was
// the gap. "Any L3 please take this" is work offered to a pool: once somebody
// takes it, nobody else should. "Every L4 must know this" is an announcement:
// everybody sees it, and what the sender wants back is confirmation of reach.
//
//	ModeAll  every member sees it; each view is counted, so the sender can ask
//	         "have all eight seen the quota notice?"
//	ModeAny  the FIRST member to view it CLAIMS it and the rest never see it —
//	         a work queue, not a message
//
// ModeAll is the default for a selector because the failure directions are not
// symmetric: an announcement wrongly treated as a claim is CONSUMED by the
// first reader and the other seven never learn it, while a work offer wrongly
// treated as an announcement merely gets done twice and is visible when it
// happens. Silent non-delivery is worse than visible duplication.
const (
	ModeAll = "all"
	ModeAny = "any"
)

func claimPath(seq int64) string {
	return filepath.Join(BoardDir(), "claims", strconv.FormatInt(seq, 10))
}

// ClaimPost records the first reader to take an ModeAny post. It returns the
// holder, which is the caller when the claim was granted and somebody else when
// it was not.
//
// O_EXCL is the whole mechanism: the filesystem decides the winner, so two
// agents reading the board in the same millisecond cannot both take the work.
// This is the first shared-mutable state on the board — everything else is
// per-reader — so it is worth being explicit that the race is handled by the
// create, not by a lock we would have to get right.
func ClaimPost(seq int64, reader string) (holder string, granted bool) {
	p := claimPath(seq)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", false
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		defer f.Close()
		_, _ = f.WriteString(reader + " " + time.Now().UTC().Format(time.RFC3339) + "\n")
		return reader, true
	}
	b, rerr := os.ReadFile(p)
	if rerr != nil {
		return "", false
	}
	return strings.Fields(strings.TrimSpace(string(b)))[0], false
}

// ClaimHolder reports who holds an ModeAny post, or "" when it is unclaimed.
func ClaimHolder(seq int64) string {
	b, err := os.ReadFile(claimPath(seq))
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(string(b)))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func viewsPath(seq int64) string {
	return filepath.Join(BoardDir(), "views", strconv.FormatInt(seq, 10))
}

// RecordView notes that a reader has seen an ModeAll post. Idempotent per
// reader: a second read does not inflate the count, so "5 of 8" means five
// distinct agents rather than five reads.
func RecordView(seq int64, reader string) error {
	seen := Viewers(seq)
	for _, v := range seen {
		if strings.EqualFold(v, reader) {
			return nil
		}
	}
	p := viewsPath(seq)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(reader + "\n")
	return err
}

// Viewers lists the distinct readers that have seen a post.
func Viewers(seq int64) []string {
	b, err := os.ReadFile(viewsPath(seq))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// AudienceSize is how many agents a selector currently names, for the "seen by
// N of M" line. Zero when it cannot be resolved — and the caller renders the
// count alone rather than inventing a denominator.
func AudienceSize(aud Audience) int {
	return newAudienceSnapshot().audienceSize(aud)
}

// --- provable delivery states -----------------------------------------------
//
// A receipt may claim only what the store can PROVE. These are the six states,
// canonical in docs/mb-addressing-model.md, from most contact to least:
//
//	delivered   pushed into a live session — SteerLive succeeded
//	read        the recipient's cursor is at or past the post's sequence
//	queued      appended, and the recipient's cursor is BEHIND that sequence
//	unverified  appended, but the recipient has NO cursor at all — it has never
//	            read the board, so "queued" would claim more than is known
//	accepted    well-formed and the target resolved, with no single reader
//	            cursor to judge — a role seat, a selector group, a broadcast
//	failed      the target resolved to no role, agent or reader; nothing written
//
// unverified is the one the old wording erased. `bashy ping X "..."` to a name
// that has never read the board reported "waiting on the board for X" — a
// receipt indistinguishable from a real delivery, when the evidence supports
// only "posted, and nobody by that name has ever looked". A reader that has
// never read is not merely behind.
const (
	StateAccepted   = "accepted"
	StateQueued     = "queued"
	StateDelivered  = "delivered"
	StateRead       = "read"
	StateFailed     = "failed"
	StateUnverified = "unverified"
)

// deliveryState computes the provable state of a directed post to `to` at `seq`.
//
// steered is the raw SteerLive outcome. perReader says whether `to` is a single
// reader with a cursor of its own (an agent, or an existing board reader) or a
// seat/group with no single cursor to judge (a role, a selector, a broadcast):
// the cursor-based states apply only to the former, and the latter can prove no
// more than `accepted`.
func deliveryState(to string, seq int64, steered, perReader bool) string {
	if steered {
		return StateDelivered
	}
	if !perReader {
		return StateAccepted
	}
	cur, has := CursorSeq(to)
	switch {
	case !has:
		return StateUnverified
	case cur >= seq:
		return StateRead
	default:
		return StateQueued
	}
}

// The kinds a send target can resolve to, most specific first.
const (
	TargetRole   = "role"
	TargetAgent  = "agent"
	TargetReader = "reader"
	// TargetPerson is a human the resolver vouches for — a people-catalog
	// entry or the OS login — reached through the resolver fallback. A
	// person is a per-reader recipient exactly like an agent: their cursor,
	// if any, is what the receipt judges.
	TargetPerson = "person"
)

// ResolveSendTarget resolves a target a sender typed to a routable address, AT
// SEND TIME, and reports how it resolved.
//
// This is the guard that turns a post to a name nobody answers into a `failed`
// instead of a receipt indistinguishable from a real delivery. Precedence:
//
//	ROLE    a seat (steward, conductor:22) — survives a handover
//	AGENT   a name in the roster (`bashy agent list`)
//	READER  a name with a cursor — it has read the board at least once, so it is
//	        demonstrably a participant even if the roster does not know it
//
// When all three miss, the HOST RESOLVER gets the last word — the same
// authority `whois` answers from, consulted through its cheap, probe-free
// lookup (see resolveprincipal.go). That is what keeps the addresser and the
// resolver agreeing about every name on the host: a person the people
// catalog knows, the OS login, or a seat observed in a meet roster is
// sendable here precisely because whois resolves it.
//
// A target matching nothing anywhere resolves to nothing, and the caller
// reports failed with near misses rather than posting into the void. This is
// the Yoke rule that an unresolvable identity must "fail with choices
// instead of guessing".
func ResolveSendTarget(target string) (addr, kind string, ok bool) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", false
	}
	if topic, isRole := ResolveRole(t); isRole {
		return topic, TargetRole, true
	}
	if name, isAgent := resolveAgentName(t); isAgent {
		return name, TargetAgent, true
	}
	if _, has := CursorSeq(t); has {
		return t, TargetReader, true
	}
	return resolvePrincipalTarget(t)
}

// resolveAgentName reports whether target names an agent in the roster, and the
// canonical name if so. A nickname or principal is canonicalized first, so
// `dhnt:agent/Omar` resolves the same agent `codex-gpt5.6-sol` does.
func resolveAgentName(target string) (string, bool) {
	canon := strings.TrimSpace(target)
	if FleetResolveName != nil {
		if n := strings.TrimSpace(FleetResolveName(target)); n != "" {
			canon = n
		}
	}
	if FleetNames != nil {
		for _, n := range FleetNames() {
			if strings.EqualFold(n, canon) || strings.EqualFold(n, target) {
				return n, true
			}
		}
	}
	return "", false
}

// boardReaders lists the names that have a cursor — everyone who has read the
// board at least once. Best-effort: an unreadable store yields none.
func boardReaders() []string {
	entries, err := os.ReadDir(filepath.Join(BoardDir(), "seen"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// NearMisses names the addressable identities closest to an unresolved target,
// so a failed send offers choices instead of a dead end. It draws from every
// pool a target could have meant — roles, the roster, and existing readers —
// and ranks by edit distance, keeping only genuinely close candidates.
func NearMisses(target string, max int) []string {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return nil
	}
	seen := map[string]bool{}
	var cands []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		k := strings.ToLower(s)
		if seen[k] {
			return
		}
		seen[k] = true
		cands = append(cands, s)
	}
	if HostRoles != nil {
		for _, r := range HostRoles() {
			add(r.Label)
		}
	}
	if FleetNames != nil {
		for _, n := range FleetNames() {
			add(n)
		}
	}
	for _, r := range boardReaders() {
		add(r)
	}

	type scored struct {
		name string
		d    int
	}
	var ranked []scored
	for _, c := range cands {
		lc := strings.ToLower(c)
		if lc == t {
			// An exact match would have resolved; it is not a near miss.
			continue
		}
		d := levenshtein(t, lc)
		if strings.Contains(lc, t) || strings.Contains(t, lc) {
			d = 0
		}
		if d > 3 {
			continue
		}
		ranked = append(ranked, scored{c, d})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].d < ranked[j].d })
	var out []string
	for _, s := range ranked {
		out = append(out, s.name)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

// unresolvedTargetError is the receipt for a send whose target matched nothing:
// the word `failed`, what was tried, the near misses, and the broadcast escape
// hatch. Nothing is written before this is returned.
func unresolvedTargetError(target string) error {
	msg := fmt.Sprintf("failed: %q matches no role, agent, board reader, or resolvable principal on this host — nothing was posted",
		strings.TrimSpace(target))
	if nm := NearMisses(target, 5); len(nm) > 0 {
		msg += "\n  did you mean: " + strings.Join(nm, ", ")
	}
	msg += "\n  or broadcast to everyone: bashy mb post \"...\""
	return errors.New(msg)
}

// levenshtein is the edit distance between two strings, for ranking near misses.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
