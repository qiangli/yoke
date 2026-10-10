// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package coord stops two agents from writing the same project at the same time.
//
// # What actually went wrong
//
// Two agent sessions worked the same repos with no coordinator. One swept the
// other's STAGED submodule pins into its own commit, landing an untested engine
// regression that took the release gate from 86/86 to 85/86. The other found an
// unexplained edit in the working tree and had to guess whose it was. Neither could
// see that the other existed.
//
// # Communication is not coordination
//
// Two agents chatting politely still stomp one another's git index. What prevents
// collision is, in order of power:
//
//	isolation  →  a claim  →  a merge gate
//
// Isolation is weave (an isolated clone). The gate is `bashy gate`. This package is
// the middle one, and it is the one that was missing.
//
// # Scope is a PATH SET, not a repo
//
// The regression proves it: the bug lived in one repo, the gate that would have
// caught it in a second, and the pin that carried it in a third. A claim on any ONE
// .git root would have prevented nothing.
//
// So a claim covers a PROJECT — the repo plus the siblings it actually depends on —
// and two claims CONFLICT when their path sets INTERSECT. Single-repo projects are
// the degenerate case.
//
// # The lease is a heartbeat, not a PID
//
// An LLM session has no stable process: it invokes commands ephemerally, and a
// conductor may be a fresh `claude` every few minutes. So a claim goes stale when
// its holder stops heartbeating, exactly as the sprint lease already does. A dead
// PID is corroborating evidence, never the test.
//
// # Refuse on CONFLICT, not on absence
//
// A claim is taken silently on first write. It only ever REFUSES when someone else
// already holds one. Zero friction when you are alone; a hard stop naming the other
// holder when you are not. An agent that read no documentation learns the rule the
// first time it tries to break it — the refusal IS the documentation.
package coord

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/role"
)

// SchemaVersion is the on-disk contract.
const SchemaVersion = "bashy-claim-v2"

// TTL is how long a claim survives without a heartbeat.
//
// Thirty minutes, matching the sprint lease, and for the same reason: an LLM
// conductor works in bursts and may be idle between them. Too short and a thinking
// agent loses its claim mid-thought; too long and a crashed one blocks the project
// until someone forces it. A stale claim is RECLAIMABLE without --force, so the
// cost of erring long is a wait, not a deadlock.
const TTL = 30 * time.Minute

// ErrLockUnsupported is returned by claim mutations on platforms where the
// shared lockfile primitive cannot provide a real advisory lock.
var ErrLockUnsupported = errors.New(
	"coord: this platform has no advisory file locking, so a claim's read-decide-write cycle cannot be serialized — " +
		"refusing to mutate rather than let two agents both acquire the same project")

const (
	ModeLease    = "lease"
	ModeAttached = "attached"
	// ModeAnnounce is advisory: no TTL, never blocks Guard, but a competing
	// Acquire still conflicts with it.
	ModeAnnounce = "announce"
)

// Claim is one agent's hold on a project or named resource.
type Claim struct {
	SchemaVersion string `json:"schema_version"`

	// Kind is the class of resource (see Kind); v1 records read as repo for
	// project claims and name for resources.
	Kind string `json:"kind,omitempty"`
	// Members is what the claim covers, compared under the kind's MatchRule.
	Members []string `json:"members,omitempty"`
	// Epoch is the fencing token: monotonic per key, bumped on every new
	// acquisition and never on a refresh by the same holder.
	Epoch uint64 `json:"epoch,omitempty"`
	// Rev is the Backend's compare-and-swap token: it changes on every committed
	// mutation of the record, unlike Epoch, which a refresh leaves alone.
	Rev uint64 `json:"rev,omitempty"`

	// Roots is the PATH SET. Conflict is intersection, not equality.
	Roots []string `json:"roots"`
	// Project is a human label (the primary root's basename).
	Project string `json:"project"`
	// Resource is a host-local name. Exactly one of Resource and Roots is set.
	Resource string `json:"resource,omitempty"`
	// Mode is lease for a detached agent hold, attached for a child-scoped
	// kernel lock, or announce for an advisory one. Project claims predate this
	// field and leave it empty.
	Mode string `json:"mode,omitempty"`

	Holder principal.Ref `json:"holder"`
	Intent string        `json:"intent,omitempty"`

	AcquiredAt time.Time `json:"acquired_at"`
	Heartbeat  time.Time `json:"heartbeat"`
	PID        int       `json:"pid,omitempty"`
}

// Liveness reports the honest four-state verdict shared by seats and claims.
func (c *Claim) Liveness(now time.Time) role.Liveness {
	// List only returns an attached record while its kernel lock is held. It has
	// no TTL: the process and fd are the liveness signal.
	if c.Mode == ModeAttached && c.Holder.Name != "" {
		return role.LivenessLive
	}
	return role.Seat{
		Holder:      c.Holder.Name,
		AcquiredAt:  c.AcquiredAt,
		HeartbeatAt: c.Heartbeat,
		TTL:         c.ttl(),
	}.Live(now)
}

// ttl is how long this claim survives without a heartbeat: none for an
// announcement, the kind's override if it has one, else the package TTL.
func (c *Claim) ttl() time.Duration {
	if c.Mode == ModeAnnounce {
		return 0
	}
	if k, ok := LookupKind(c.Kind); ok && k.TTL > 0 {
		return k.TTL
	}
	return TTL
}

// Live reports whether the claim still holds. The heartbeat is the test; a dead PID
// is corroboration, never the verdict — an LLM session has no stable process, and
// judging liveness by PID would evict a conductor between two of its own commands.
func (c *Claim) Live(now time.Time) bool {
	return c.Liveness(now) == role.LivenessLive
}

// Stale reports a claim a successor may take without the holder's consent:
// lapsed or vacant. An UNKNOWN claim is not stale — nothing says its holder is
// gone — so it is taken only with Force.
func (c *Claim) Stale(now time.Time) bool { return c.Liveness(now).Takeable() }

// Conflicts reports whether this claim blocks `other`. Two claims conflict when
// their path sets INTERSECT and the holders differ.
//
// Holder identity is compared by episode-and-name, not by PID: the same logical
// agent may run many processes (a shell, a subagent, a hook), and none of them
// should be told it is colliding with itself.
func (c *Claim) Conflicts(roots []string, holder principal.Ref, now time.Time) bool {
	if c.Resource != "" || c.Liveness(now).Takeable() {
		return false
	}
	if sameHolder(c.Holder, holder) {
		return false
	}
	return Intersects(c.Roots, roots)
}

// ConflictsResource reports whether this claim blocks another holder from the
// same named resource. Unknown is deliberately not takeable.
func (c *Claim) ConflictsResource(resource string, holder principal.Ref, now time.Time) bool {
	if c.Resource == "" || c.Resource != resource || c.Liveness(now).Takeable() {
		return false
	}
	return !sameHolder(c.Holder, holder)
}

// sameHolder: sessions are told apart by episode. Name and host identify a
// holder only when one side carries no episode (a legacy or unattributed
// identity) — two independently launched sessions with one tool name on one
// host are different holders.
func sameHolder(a, b principal.Ref) bool {
	if a.Episode != "" && b.Episode != "" {
		return a.Episode == b.Episode
	}
	return a.Name != "" && a.Name == b.Name && a.Host == b.Host
}

// Intersects reports whether two path sets touch — same path, or one beneath the
// other. Containment, not string equality: an agent editing <repo>/internal is
// working in <repo>, and a claim on the repo must find it.
func Intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == "" || y == "" {
				continue
			}
			if under(x, y) || under(y, x) {
				return true
			}
		}
	}
	return false
}

func under(parent, p string) bool {
	parent = filepath.Clean(parent)
	p = filepath.Clean(p)
	if parent == p {
		return true
	}
	rel, err := filepath.Rel(parent, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// DefaultDir is the host-wide registry: ~/.bashy/coord/.
//
// HOST-WIDE, deliberately. The question "who else is working right now, and where?"
// had no answer anywhere in this codebase — weave knew about its own issues, sprint
// about its own board, and nothing knew about a plain `claude` a human launched in a
// terminal. That blind spot is exactly how two sessions became invisible to each
// other.
func DefaultDir() string {
	if v := os.Getenv("BASHY_COORD_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bashy-coord")
	}
	return filepath.Join(home, ".bashy", "coord")
}

// claimPath keys a claim by its HOLDER, not by its project.
//
// One agent holds at most one claim, and a project may be claimed by only one agent
// (enforced by Acquire). Keying by holder means a crashed agent leaves exactly one
// stale file, and a heartbeat is a rewrite of that one file rather than a scan.
func claimPath(dir string, h principal.Ref) string {
	return filepath.Join(dir, holderID(h)+".json")
}

func resourceKey(resource string) string {
	sum := sha256.Sum256([]byte(resource))
	return fmt.Sprintf("%x", sum[:])
}

func resourceClaimPath(dir, resource string) string {
	return keyedClaimPath(dir, Ref{Kind: KindName, Name: resource})
}

func resourceLockPath(dir, resource string) string {
	return keyedLockPath(dir, Ref{Kind: KindName, Name: resource})
}

// List returns every claim on this host, freshest first.
func List(dir string) ([]*Claim, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Claim
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var c Claim
		if json.Unmarshal(b, &c) != nil || c.SchemaVersion == "" {
			continue // a corrupt claim must not hide the healthy ones
		}
		c.normalize()
		// An attached record is diagnostic; the kernel lock is authoritative.
		// A SIGKILL cannot remove JSON, so omit the record once the lock is gone.
		if c.Resource != "" && c.Mode == ModeAttached {
			if _, held := lockfile.Owner(keyedLockPath(dir, c.Ref())); !held {
				continue
			}
		}
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Heartbeat.After(out[j].Heartbeat) })
	return out, nil
}

// AcquireResource takes or refreshes a detached lease on a host-local name.
func AcquireResource(dir, resource string, holder principal.Ref, intent string, force bool) (*Claim, error) {
	return AcquireResourceWithin(dir, resource, holder, intent, force, 0)
}

// AcquireResourceWithin retries the complete detached acquisition cycle until
// the bounded wait elapses. claims.lock only serializes each attempt; it is not
// the resource hold.
func AcquireResourceWithin(dir, resource string, holder principal.Ref, intent string, force bool, wait time.Duration) (*Claim, error) {
	g, err := store{dir}.acquireWait(context.Background(), acquireSpec{req: Request{
		Ref: Ref{Kind: KindName, Name: resource}, Holder: holder, Intent: intent, Mode: ModeLease, Force: force,
	}}, wait)
	return g.Claim, err
}

// AcquireAttached takes a kernel-backed hold for one child process.
func AcquireAttached(dir, resource string, holder principal.Ref, intent string, wait time.Duration) (*Claim, *lockfile.Lock, error) {
	g, l, err := store{dir}.acquireAttached(Request{
		Ref: Ref{Kind: KindName, Name: resource}, Holder: holder, Intent: intent, Mode: ModeAttached, Wait: wait,
	})
	return g.Claim, l, err
}

// ReleaseAttached removes the diagnostic record before releasing the kernel
// lock, so a new holder never inherits the old row. The record goes through the
// claim's own backend and only if it is still this claim's — same holder, same
// epoch — so a stale or repeated cleanup never deletes a successor's record.
func ReleaseAttached(dir string, c *Claim, l *lockfile.Lock) error {
	if c != nil {
		s := store{dir}
		key := c.Ref().String()
		b := s.backend(c.Ref().Kind)
		var err error
		for range 3 {
			err = s.txn(b, func(locked bool) error {
				cur, err := b.Load(key)
				if err != nil || cur == nil {
					return err
				}
				if cur.Epoch != c.Epoch || !sameHolder(cur.Holder, c.Holder) {
					return nil
				}
				return commit(b, locked, key, cur.Rev, nil)
			})
			if !errors.Is(err, ErrEpochMismatch) {
				break
			}
		}
		if err != nil {
			_ = l.Release()
			return err
		}
	}
	return l.Release()
}

// ReleaseResource drops a detached named hold.
func ReleaseResource(dir, resource string, holder principal.Ref) error {
	if strings.TrimSpace(resource) == "" {
		return fmt.Errorf("claim: resource name is required")
	}
	return store{dir}.release(Ref{Kind: KindName, Name: resource}, holder, 0)
}

// Acquire takes or refreshes a claim over the given path set.
//
// It is IDEMPOTENT and silent when uncontested: the first write in a project takes
// the claim without anyone being asked. It returns a *Conflict only when someone
// else holds a LIVE claim over intersecting paths. Refuse on conflict, never on
// absence — friction that fires when you are alone is friction nobody accepts, and
// a rule nobody accepts is a rule nobody follows.
func Acquire(dir string, roots []string, holder principal.Ref, intent string, force bool) (*Claim, error) {
	g, err := store{dir}.acquire(acquireSpec{
		req:    Request{Holder: holder, Intent: intent, Force: force},
		legacy: true, roots: roots,
	})
	return g.Claim, err
}

// Prunable reports whether prune would remove this claim: a lapsed,
// non-attached hold. Attached holds are kernel-scoped — the lockfile is the
// liveness signal, not the heartbeat — so they are never pruned. Unknown
// holds are the absence of evidence either way, and like acquisition, prune
// looks rather than seizes: only LivenessLapsed goes.
func (c *Claim) Prunable(now time.Time) bool {
	if c.Mode == ModeAttached {
		return false
	}
	return c.Liveness(now) == role.LivenessLapsed
}

// Lapsed returns every claim prune would remove, freshest first.
func Lapsed(dir string, now time.Time) ([]*Claim, error) {
	claims, err := List(dir)
	if err != nil {
		return nil, err
	}
	var out []*Claim
	for _, c := range claims {
		if c.Prunable(now) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Prune removes every lapsed project and named-resource claim from dir and
// returns what it removed, freshest first. Live, unknown, and attached
// (kernel-locked) claims are untouched. With dryRun it only reports what
// would go, writing nothing. Removal holds claims.lock across the whole
// scan-and-remove pass so two operators cannot prune half-overlapping sets.
func Prune(dir string, now time.Time, dryRun bool) ([]*Claim, error) {
	if dryRun {
		return Lapsed(dir, now)
	}
	var pruned []*Claim
	b := fileBackend{dir: dir}
	err := withDirLock(dir, func() error {
		lapsed, err := Lapsed(dir, now)
		if err != nil {
			return err
		}
		for _, c := range lapsed {
			if err := b.commitLocked(c.key(), c.Rev, nil); err != nil && !errors.Is(err, ErrEpochMismatch) {
				return err
			}
			pruned = append(pruned, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pruned, nil
}

// Release drops this holder's claim. A claim that is never released still expires;
// releasing is a courtesy to whoever is waiting, not a correctness requirement.
func Release(dir string, holder principal.Ref) error {
	b := fileBackend{dir: dir}
	return withDirLock(dir, func() error {
		cur, err := b.Load(holderKey(holder))
		if err != nil || cur == nil {
			return err
		}
		return b.commitLocked(holderKey(holder), cur.Rev, nil)
	})
}

func writeClaim(path string, c *Claim) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func withLock(dir string, fn func() (*Claim, error)) (*Claim, error) {
	if !lockPlatformSupported() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("claim lock: %w", err)
		}
		return nil, ErrLockUnsupported
	}
	l, err := lockfile.Acquire(filepath.Join(dir, "claims.lock"), lockfile.Holder{
		Name: "coord-claims", PID: os.Getpid(), Intent: "update claims", Since: time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("claim lock: %w", err)
	}
	defer l.Release()
	return fn()
}

func lockPlatformSupported() bool {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "openbsd", "dragonfly", "solaris", "windows":
		return true
	default:
		return false
	}
}
