// Package room is the same-host "host room": the canonical registry of live
// agentic-tool instances (membership) plus an append-only event log (timeline).
//
// It is the P0 rung of the agent room mesh (docs/agent-room-mesh-design.md):
// discovery is membership, connection is the card's control socket, and the
// timeline is the coordination stream the notification bus, coach, and task
// continuity record all converge on. The Card and Event shapes are deliberately
// projection-friendly (A2A Agent Card / Matrix event) so the same-host store can
// later be pushed to a cloudbox and, eventually, federated — without changing the
// shape a member publishes.
package room

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// Card is one live member's record — who it is, what it is bound to, and how to
// reach it on this host.
//
// Two kinds of card share the board, and the difference is the ID:
//
//   - an AGENT SESSION card is keyed by the agent's NAME, because an agent is a
//     singleton identity on a host (one conversation store, one kb attribution,
//     one bus cursor, one API key). Mode is "interactive".
//   - a TASK card is keyed by the WORK (`weave-<issue>-<pid>`), because a task is
//     not an identity — many may run at once. Mode is "weave"/"foreman"/"meet".
//
// Keeping both in one board is deliberate: `chat sessions` should show
// everything live on the host. Keeping their ID SHAPES apart is what makes Join
// able to enforce the singleton without also refusing legitimate parallel tasks.
type Card struct {
	ID        string   `json:"id"`
	Principal string   `json:"principal,omitempty"` // who launched it
	Tool      string   `json:"tool"`
	Model     string   `json:"model,omitempty"`
	Binding   string   `json:"binding"` // tool:model
	Nick      string   `json:"nick,omitempty"`
	Band      int      `json:"band,omitempty"`
	Mode      string   `json:"mode,omitempty"` // interactive | weave | foreman | meet | oneshot
	Role      string   `json:"role,omitempty"` // role alias a launch was routed under (conductor, reviewer, …), when given
	Task      string   `json:"task,omitempty"` // what it is working on, if known
	Caps      []string `json:"caps,omitempty"`
	CtlSock   string   `json:"ctl_sock,omitempty"` // same-host reach
	LogPath   string   `json:"log_path,omitempty"`
	// EventsPath is the structured-event stream this member writes, when Events.
	//
	// It is ADVERTISED rather than recomputed. A reader used to reconstruct it
	// from binding+pid, which meant two functions had to agree on a hash forever
	// and a card could not move its own stream. A card already publishes its
	// socket and its log; its event stream is the same category of fact.
	EventsPath string `json:"events_path,omitempty"`
	// PID is the process whose liveness the membership tracks — the room prunes a
	// card whose pid is gone on read, so it never asserts a dead member is live.
	PID int `json:"pid"`
	// OwnerPID is the stable agent-harness process that owns this session.
	// Short-lived Bashy commands are descendants of it even when a shell or
	// watcher process sits between them and the harness. It is an attribution
	// claim within the host OS trust boundary, not a cryptographic credential.
	OwnerPID int `json:"owner_pid,omitempty"`
	// SessionClaim is a one-way digest of the stable tool-session identifier
	// inherited by the agent and its command subprocesses. The raw vendor
	// session identifier must never enter this public room card.
	SessionClaim string `json:"session_claim,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Native       bool   `json:"native,omitempty"` // self-governing harness (ycode)
	Events       bool   `json:"events,omitempty"` // speaks a structured event channel
	Joined       string `json:"joined"`
	// Updated is the last publisher heartbeat. Joined is the assignment start
	// and must not be rewritten by a heartbeat; consumers need both elapsed
	// work time and liveness freshness.
	Updated string `json:"updated,omitempty"`
	// LeaseUntil bounds externally reported work whose worker is not a distinct
	// local process. The managing orchestrator renews it; expiry makes the card
	// stale even while the manager process remains alive.
	LeaseUntil string `json:"lease_until,omitempty"`
}

// CapInboxDelivery means the member's owning harness watches its unified inbox
// and injects unread input through the member's real control transport. It is a
// delivery capability, not merely a background process printing to a terminal.
const CapInboxDelivery = "inbox-delivery-v1"

// CapInboxStream means an external agent harness owns the member's foreground
// stdout and is responsible for retaining and reading its inbox event stream.
// Unlike CapInboxDelivery, Bashy is not claiming it can inject a model turn.
const CapInboxStream = "inbox-stream-v1"

// HasCapability reports an exact capability advertised by a room member.
func HasCapability(c Card, capability string) bool {
	for _, candidate := range c.Caps {
		if candidate == capability {
			return true
		}
	}
	return false
}

// Event is one timeline entry — a join/leave/steer/status/note the room records.
type Event struct {
	Seq       int64  `json:"seq"`
	TS        string `json:"ts"`
	Type      string `json:"type"` // join | leave | steer | interrupt | status | note | notify
	Actor     string `json:"actor,omitempty"`
	Target    string `json:"target,omitempty"`
	Body      string `json:"body,omitempty"`
	Principal string `json:"principal,omitempty"` // who sent this notification (REQUIRED for notify)
	Topic     string `json:"topic,omitempty"`     // topic broadcast key
	Room      string `json:"room,omitempty"`      // room-scoped addressing
	To        string `json:"to,omitempty"`        // 1:1 recipient (session or role)
	// Priority selects the DELIVERY TIER a subscriber's sidecar uses: "" or
	// "queued" is read at a turn boundary, "interrupt" may break into a running
	// turn. It is a REQUEST, not a guarantee — the subscriber decides whose
	// interrupts it accepts, so an unauthorized or rate-limited interrupt is
	// demoted to queued rather than honoured or dropped.
	Priority string `json:"priority,omitempty"`
	// Activity is the compact, durable fact emitted after a successful Coreutils
	// transaction.  It deliberately carries references, never operation bodies
	// or output.  Consumers use FetchRef under their own authorization to obtain
	// details.
	Activity *Activity `json:"activity,omitempty"`
	// MatchReason records why this addressed event reached its recipient (for
	// example owner, assignment, mention, membership, or subscription).
	MatchReason string `json:"match_reason,omitempty"`
}

// Activity is the versioned wire contract for a committed state change. Keep
// this shape additive: the room timeline is also consumed by the CLI, web,
// recovery and background paths.
type Activity struct {
	ID            string    `json:"id"`
	Version       int       `json:"version"`
	Actor         string    `json:"actor"`
	Verb          string    `json:"verb"`
	Noun          string    `json:"noun"`
	ObjectRef     string    `json:"object_ref"`
	Repo          string    `json:"repo,omitempty"`
	Sprint        string    `json:"sprint,omitempty"`
	Topic         string    `json:"topic,omitempty"`
	Origin        string    `json:"origin,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Priority      string    `json:"priority,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
	FetchRef      string    `json:"fetch_ref,omitempty"`
	Summary       string    `json:"summary"`
}

// Validate rejects incomplete envelopes before they can become durable facts.
func (a Activity) Validate() error {
	if strings.TrimSpace(a.ID) == "" || a.Version <= 0 || strings.TrimSpace(a.Actor) == "" ||
		strings.TrimSpace(a.Verb) == "" || strings.TrimSpace(a.Noun) == "" ||
		strings.TrimSpace(a.ObjectRef) == "" || a.Timestamp.IsZero() || strings.TrimSpace(a.Summary) == "" {
		return fmt.Errorf("activity: id, version, actor, verb, noun, object_ref, timestamp, and summary are required")
	}
	if len(a.Summary) > 160 {
		return fmt.Errorf("activity: summary exceeds 160 bytes")
	}
	return nil
}

const (
	EventJoin      = "join"
	EventLeave     = "leave"
	EventSteer     = "steer"
	EventInterrupt = "interrupt"
	EventGrant     = "grant"
	EventStatus    = "status"
	EventNote      = "note"
	EventNotify    = "notify"
	EventAck       = "ack"
)

var appendMu sync.Mutex

var agentClaimUnsafe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// AgentClaimID maps a registered fleet agent name to the room key used by
// every singleton session surface. It deliberately preserves chat's original
// host identity spelling so existing conversation stores, sockets, and command
// targets do not move when the shared claim contract is adopted.
//
// Keep the public fleet name in Card.Nick. This value is the collision-safe
// room/storage key, not a replacement for the human-facing identity.
func AgentClaimID(name string) string {
	claim := strings.TrimSpace(name)
	claim = strings.Trim(agentClaimUnsafe.ReplaceAllString(claim, "-"), "-")
	if claim == "" {
		return "agent"
	}
	return claim
}

// Dir resolves the room root:
//
//	$BASHY_ROOM_DIR     the specific override, most precise
//	$BASHY_HOME/room    the whole bashy home relocated (tests, sandboxed runs)
//	~/.bashy/room       the default, unchanged
//
// The MIDDLE rung is not cosmetic. $BASHY_HOME is the documented way to
// relocate a whole bashy home, and sprint, foreman and webconsole all honour
// it for exactly that reason. Room did not, so a run sandboxed with
// $BASHY_HOME got an isolated sprint store whose stage announcements, mb posts
// and bus events still landed on the SHARED host board. Two smoke tests
// announced a throwaway sprint onto the operator's real board that way, and
// mb is append-only, so neither could be taken back.
//
// A sandbox that silently leaks is worse than no sandbox: the caller believes
// they are isolated, so nothing prompts them to check.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv("BASHY_ROOM_DIR")); d != "" {
		return d
	}
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "room")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bashy-room")
	}
	return filepath.Join(home, ".bashy", "room")
}

func membersDir() (string, error) {
	d := filepath.Join(Dir(), "members")
	return d, os.MkdirAll(d, 0o700)
}

// memberClaimsLockPath stays outside members/: that directory is the public
// membership set, and consumers are entitled to treat every regular entry in
// it as a card. Synchronization metadata beside the set preserves that
// invariant while still coordinating every Join process on the host.
func memberClaimsLockPath() string { return filepath.Join(Dir(), "member-claims.lock") }

func timelinePath() (string, error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return "", err
	}
	return filepath.Join(Dir(), "timeline.jsonl"), nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// memberPath keeps opaque member IDs out of host path syntax. In particular,
// ':' is invalid in a Windows filename, while '/', '\\', and '..' must never
// let an ID escape the members directory on any host.
func memberPath(dir, id string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(id))
	return filepath.Join(dir, "id-"+name+".json")
}

func legacyMemberPath(dir, id string) (string, bool) {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return "", false
	}
	return filepath.Join(dir, id+".json"), true
}

// ErrLive reports that the id being joined is already held by a LIVE member.
// Callers unwrap it to tell "someone else is already this" apart from an I/O
// failure, and to say something useful about it.
type ErrLive struct {
	ID  string
	PID int
}

func (e *ErrLive) Error() string {
	return fmt.Sprintf("room: %q is already live (pid %d)", e.ID, e.PID)
}

// Join CLAIMS a membership id and records a join event.
//
// It is a claim, not a write. It used to be an unconditional WriteFile, so a
// second member taking an id already in use silently overwrote the first's card
// — and the first's Leave then deleted the survivor's. Nothing reported it,
// which is how two processes came to share one identity, one control socket and
// one bus cursor while looking like two healthy members.
//
// Re-joining an id this process already holds is an UPDATE and still succeeds:
// a member revises its own card (task, caps) as it works.
//
// A stale card left by a crash is not a conflict — its pid is dead, so it is
// reclaimed here exactly as Members would prune it on read. Reading is the
// reconciliation; there is still no sweeper.
func Join(c Card) error {
	dir, err := membersDir()
	if err != nil {
		return err
	}
	claimLock, err := lockfile.Acquire(memberClaimsLockPath(), lockfile.Holder{
		Name: c.ID, PID: c.PID, Intent: "claim room member identity",
	})
	if err != nil {
		return fmt.Errorf("room: serialize member claim: %w", err)
	}
	defer claimLock.Release()
	path := memberPath(dir, c.ID)
	prior, ok := readCard(path)
	legacy := ""
	if !ok {
		if candidate, safe := legacyMemberPath(dir, c.ID); safe {
			legacy = candidate
			prior, ok = readCard(candidate)
		}
	}
	if ok {
		if prior.PID != c.PID && PidAlive(prior.PID) {
			return &ErrLive{ID: c.ID, PID: prior.PID}
		}
		if prior.PID == c.PID && c.Joined == "" {
			c.Joined = prior.Joined
		}
	}
	if c.Joined == "" {
		c.Joined = now()
	}
	c.Updated = now()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	if legacy != "" && legacy != path {
		_ = os.Remove(legacy)
	}
	return Emit(Event{Type: EventJoin, Actor: c.Principal, Target: c.ID, Body: c.Binding})
}

// readCard loads one card file. A missing or unreadable card is "no card" —
// membership is advisory and a garbled file must never wedge a claim.
func readCard(path string) (Card, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Card{}, false
	}
	var c Card
	if json.Unmarshal(b, &c) != nil || c.ID == "" {
		return Card{}, false
	}
	return c, true
}

// Leave removes THIS PROCESS'S membership card and records a leave event.
//
// The pid check is load-bearing, not defensive dressing. Every caller pairs
// `Join` with a deferred `Leave`, and that defer runs whether the Join was
// granted or REFUSED — so without this, a process that lost a claim would
// evict the winner's card on its way out and hand the id back to nobody.
// Leaving an id you do not hold is a no-op, not an error.
func Leave(id string) {
	LeavePID(id, os.Getpid())
}

// LeavePID removes a membership card owned by pid.
//
// Most members own their card directly and use Leave, which supplies the
// caller's pid. External orchestrators are different: a short-lived `bashy`
// command publishes work on behalf of its long-lived parent and a later
// command retires that same card. LeavePID lets that launcher prove the same
// parent still owns the card without weakening the incumbent protection.
func LeavePID(id string, pid int) {
	dir, err := membersDir()
	if err != nil {
		return
	}
	path := memberPath(dir, id)
	if _, ok := readCard(path); !ok {
		if legacy, safe := legacyMemberPath(dir, id); safe {
			path = legacy
		}
	}
	if prior, ok := readCard(path); ok && prior.PID != pid && PidAlive(prior.PID) {
		return
	}
	_ = os.Remove(path)
	_ = Emit(Event{Type: EventLeave, Target: id})
}

// Members returns the live membership, newest first, pruning any card whose pid is
// gone (a crash left the file behind). Reading IS the reconciliation — no sweeper,
// so the board never asserts a dead member is live.
func Members() ([]Card, error) {
	dir, err := membersDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Card
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		c, ok := readCard(p)
		if !ok {
			continue
		}
		if !PidAlive(c.PID) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Joined > out[j].Joined })
	return out, nil
}

// Find resolves an id to a live member. A unique id/nick prefix matches, so an
// operator can type `elif` when only one such member is up.
func Find(id string) (Card, bool, error) {
	id = strings.TrimSpace(id)
	members, err := Members()
	if err != nil {
		return Card{}, false, err
	}
	if id == "" {
		if len(members) == 1 {
			return members[0], true, nil
		}
		return Card{}, false, nil
	}
	for _, c := range members {
		if c.ID == id {
			return c, true, nil
		}
	}
	var pref []Card
	for _, c := range members {
		if strings.HasPrefix(c.ID, id) || strings.EqualFold(c.Nick, id) {
			pref = append(pref, c)
		}
	}
	if len(pref) == 1 {
		return pref[0], true, nil
	}
	return Card{}, false, nil // 0 or ambiguous — caller reports the count
}

// Emit appends an event to the timeline. Seq is never stored — Timeline
// recomputes it from line position plus the archive offset on every read
// (see archive.go) — so the append itself stays a plain, cheap write.
//
// The append is guarded by appendMu (in-process) AND a short cross-process
// lock (see timelineLockPath) — the latter is what makes it safe for
// RotateTimeline to later rewrite this same file: without it, a rewrite
// racing a concurrent process's append could silently drop that append.
// Locking is best-effort, matching pkg/bus's withBoardLock: on any failure to
// acquire (contention, or advisory locking unsupported on this platform) the
// append still proceeds, which is the pre-existing behavior this must not
// regress.
//
// Rotation runs opportunistically after the append, outside both locks and
// throttled — see rotateTimelineOpportunistic — so a hygiene sweep never
// sits on the critical path of recording an event.
func Emit(e Event) error {
	path, err := timelinePath()
	if err != nil {
		return err
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}

	appendMu.Lock()
	lock, lerr := lockfile.AcquireWithin(timelineLockPath(), 2*time.Second, lockfile.Holder{
		Name: "room-emit", PID: os.Getpid(), Intent: "append",
	})
	f, ferr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	var werr, cerr error
	if ferr == nil {
		_, werr = f.Write(append(b, '\n'))
		cerr = f.Close()
	}
	if lerr == nil {
		lock.Release()
	}
	appendMu.Unlock()
	if ferr != nil {
		return ferr
	}
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}

	rotateTimelineOpportunistic()
	return nil
}

// Timeline returns the last n events (all when n <= 0), oldest-first. Seq is
// LINE POSITION PLUS archivedThrough(): the file itself only ever holds the
// live tail once rotation has run, so the offset is what keeps a seq stable
// across a rotation that removed everything before it.
func Timeline(n int) ([]Event, error) {
	path, err := timelinePath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var all []Event
	seq := archivedThrough()
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		seq++
		e.Seq = seq
		all = append(all, e)
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// Notify publishes a notification event to the timeline after enforcing the
// REPORT/AUTHOR invariant: every notification must carry a non-empty Principal
// asserting who sent it. A notification with no principal is rejected.
func Notify(e Event) error {
	if strings.TrimSpace(e.Principal) == "" {
		return fmt.Errorf("notify: principal is required (REPORT/AUTHOR invariant)")
	}
	if e.Type == "" {
		e.Type = EventNotify
	}
	return Emit(e)
}
