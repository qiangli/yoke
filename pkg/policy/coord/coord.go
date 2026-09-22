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
const SchemaVersion = "bashy-claim-v1"

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
)

// Claim is one agent's hold on a project or named resource.
type Claim struct {
	SchemaVersion string `json:"schema_version"`

	// Roots is the PATH SET. Conflict is intersection, not equality.
	Roots []string `json:"roots"`
	// Project is a human label (the primary root's basename).
	Project string `json:"project"`
	// Resource is a host-local name. Exactly one of Resource and Roots is set.
	Resource string `json:"resource,omitempty"`
	// Mode is lease for a detached agent hold or attached for a child-scoped
	// kernel lock. Project claims predate this field and leave it empty.
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
		TTL:         TTL,
	}.Live(now)
}

// Live reports whether the claim still holds. The heartbeat is the test; a dead PID
// is corroboration, never the verdict — an LLM session has no stable process, and
// judging liveness by PID would evict a conductor between two of its own commands.
func (c *Claim) Live(now time.Time) bool {
	return c.Liveness(now) == role.LivenessLive
}

// Stale is the inverse, named so the call sites read the way people think.
func (c *Claim) Stale(now time.Time) bool { return !c.Live(now) }

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

func sameHolder(a, b principal.Ref) bool {
	if a.Episode != "" && a.Episode == b.Episode {
		return true
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
	id := h.Episode
	if id == "" {
		id = h.Name + "@" + h.Host
	}
	if id == "" || id == "@" {
		id = "unattributed"
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, id)
	return filepath.Join(dir, safe+".json")
}

func resourceKey(resource string) string {
	sum := sha256.Sum256([]byte(resource))
	return fmt.Sprintf("%x", sum[:])
}

func resourceClaimPath(dir, resource string) string {
	return filepath.Join(dir, "resource-"+resourceKey(resource)+".json")
}

func resourceLockPath(dir, resource string) string {
	return filepath.Join(dir, "resource-"+resourceKey(resource)+".lock")
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
		// An attached record is diagnostic; the kernel lock is authoritative.
		// A SIGKILL cannot remove JSON, so omit the record once the lock is gone.
		if c.Resource != "" && c.Mode == ModeAttached {
			if _, held := lockfile.Owner(resourceLockPath(dir, c.Resource)); !held {
				continue
			}
		}
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Heartbeat.After(out[j].Heartbeat) })
	return out, nil
}

// Conflict is a live claim held by someone else over paths we want.
type Conflict struct{ Claim *Claim }

func (c *Conflict) Error() string {
	who := c.Claim.Holder.Name
	if who == "" {
		who = string(c.Claim.Holder.Kind)
	}
	if who == "" {
		who = "another agent"
	}
	var b strings.Builder
	if c.Claim.Resource != "" {
		host := c.Claim.Holder.Host
		if host == "" {
			host = "this host"
		}
		fmt.Fprintf(&b, "%s already holds %s on %s", who, c.Claim.Resource, host)
	} else {
		fmt.Fprintf(&b, "%s is already working in %s", who, c.Claim.Project)
	}
	if c.Claim.Intent != "" {
		fmt.Fprintf(&b, " (%s)", c.Claim.Intent)
	}
	fmt.Fprintf(&b, ", since %s.\n\n", c.Claim.AcquiredAt.Format(time.Kitchen))
	if c.Claim.Resource != "" {
		fmt.Fprintf(&b, "This advisory hold coordinates agents on this host; it does not prove the remote resource is idle.\n\n")
		fmt.Fprintf(&b, "  bashy claim list                # current project and named holds\n")
		fmt.Fprintf(&b, "  bashy claim release %s          # the holder releases when finished\n", c.Claim.Resource)
		return b.String()
	}
	fmt.Fprintf(&b, "Two agents writing one project is how an untested change reaches main: one session\n")
	fmt.Fprintf(&b, "sweeps another's staged work into its commit, and nobody can tell whose edit was whose.\n\n")
	fmt.Fprintf(&b, "  bashy claim list                # who is working, where, on what\n")
	fmt.Fprintf(&b, "  bashy claim request -m <reason> # ask this owner to merge/sequence/release\n")
	fmt.Fprintf(&b, "  bashy weave add \"<task>\"        # work in an ISOLATED workspace instead\n")
	fmt.Fprintf(&b, "  BASHY_CLAIM_FORCE=1 <command>   # override (recorded in the audit log)\n")
	return b.String()
}

// AcquireResource takes or refreshes a detached lease on a host-local name.
func AcquireResource(dir, resource string, holder principal.Ref, intent string, force bool) (*Claim, error) {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return nil, fmt.Errorf("claim: resource name is required")
	}
	now := time.Now().UTC()
	return withLock(dir, func() (*Claim, error) {
		claims, err := List(dir)
		if err != nil {
			return nil, err
		}
		if !force {
			for _, other := range claims {
				if other.ConflictsResource(resource, holder, now) {
					return nil, &Conflict{Claim: other}
				}
			}
		}
		// A process-scoped kernel hold cannot be forced or refreshed into a
		// detached lease. The process holding the fd is the authority.
		for _, other := range claims {
			if other.Resource == resource && other.Mode == ModeAttached {
				return nil, &Conflict{Claim: other}
			}
		}
		c := &Claim{SchemaVersion: SchemaVersion, Resource: resource, Mode: ModeLease,
			Holder: holder, Intent: intent, AcquiredAt: now, Heartbeat: now, PID: os.Getpid()}
		p := resourceClaimPath(dir, resource)
		if b, err := os.ReadFile(p); err == nil {
			var prev Claim
			if json.Unmarshal(b, &prev) == nil && sameHolder(prev.Holder, holder) && !prev.AcquiredAt.IsZero() {
				c.AcquiredAt = prev.AcquiredAt
				if c.Intent == "" {
					c.Intent = prev.Intent
				}
			}
		}
		return c, writeClaim(p, c)
	})
}

// AcquireResourceWithin retries the complete detached acquisition cycle until
// the bounded wait elapses. claims.lock only serializes each attempt; it is not
// the resource hold.
func AcquireResourceWithin(dir, resource string, holder principal.Ref, intent string, force bool, wait time.Duration) (*Claim, error) {
	deadline := time.Now().Add(wait)
	backoff := 20 * time.Millisecond
	for {
		c, err := AcquireResource(dir, resource, holder, intent, force)
		var conflict *Conflict
		if err == nil || wait <= 0 || !errors.As(err, &conflict) || !time.Now().Before(deadline) {
			return c, err
		}
		d := time.Until(deadline)
		if d > backoff {
			d = backoff
		}
		if d > 0 {
			time.Sleep(d)
		}
		if backoff < 2*time.Second {
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}
}

// AcquireAttached takes a kernel-backed hold for one child process.
func AcquireAttached(dir, resource string, holder principal.Ref, intent string, wait time.Duration) (*Claim, *lockfile.Lock, error) {
	resource = strings.TrimSpace(resource)
	if resource == "" {
		return nil, nil, fmt.Errorf("claim: resource name is required")
	}
	h := lockfile.Holder{Name: holder.Name, PID: os.Getpid(), Intent: intent, Since: time.Now()}
	var l *lockfile.Lock
	var err error
	if wait > 0 {
		l, err = lockfile.AcquireWithin(resourceLockPath(dir, resource), wait, h)
	} else {
		l, err = lockfile.TryAcquire(resourceLockPath(dir, resource), h)
	}
	if err != nil {
		if b, readErr := os.ReadFile(resourceClaimPath(dir, resource)); readErr == nil {
			var c Claim
			if json.Unmarshal(b, &c) == nil && c.Resource == resource {
				return nil, nil, &Conflict{Claim: &c}
			}
		}
		if owner, ok := lockfile.HeldBy(err); ok {
			return nil, nil, &Conflict{Claim: &Claim{
				SchemaVersion: SchemaVersion, Resource: resource, Mode: ModeAttached,
				Holder: principal.Ref{Name: owner.Name}, Intent: owner.Intent,
				AcquiredAt: owner.Since, Heartbeat: owner.Since, PID: owner.PID,
			}}
		}
		return nil, nil, err
	}
	now := time.Now().UTC()
	c, err := withLock(dir, func() (*Claim, error) {
		claims, listErr := List(dir)
		if listErr != nil {
			return nil, listErr
		}
		for _, other := range claims {
			if other.Mode != ModeAttached && other.ConflictsResource(resource, holder, now) {
				return nil, &Conflict{Claim: other}
			}
		}
		claim := &Claim{SchemaVersion: SchemaVersion, Resource: resource, Mode: ModeAttached,
			Holder: holder, Intent: intent, AcquiredAt: now, Heartbeat: now, PID: os.Getpid()}
		return claim, writeClaim(resourceClaimPath(dir, resource), claim)
	})
	if err != nil {
		_ = l.Release()
		return nil, nil, err
	}
	return c, l, nil
}

// ReleaseAttached removes the diagnostic record before releasing the kernel
// lock, so a new holder never inherits the old row.
func ReleaseAttached(dir string, c *Claim, l *lockfile.Lock) error {
	if c != nil {
		_, err := withLock(dir, func() (*Claim, error) {
			return nil, os.Remove(resourceClaimPath(dir, c.Resource))
		})
		if err != nil && !os.IsNotExist(err) {
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
	_, err := withLock(dir, func() (*Claim, error) {
		p := resourceClaimPath(dir, resource)
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil, readErr
		}
		var c Claim
		if json.Unmarshal(b, &c) != nil || c.Resource != resource {
			return nil, fmt.Errorf("claim: invalid record for %s", resource)
		}
		if c.Mode == ModeAttached {
			return nil, fmt.Errorf("claim: %s is attached to a live child and cannot be released separately", resource)
		}
		if !sameHolder(c.Holder, holder) {
			return nil, &Conflict{Claim: &c}
		}
		return nil, os.Remove(p)
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Acquire takes or refreshes a claim over the given path set.
//
// It is IDEMPOTENT and silent when uncontested: the first write in a project takes
// the claim without anyone being asked. It returns a *Conflict only when someone
// else holds a LIVE claim over intersecting paths. Refuse on conflict, never on
// absence — friction that fires when you are alone is friction nobody accepts, and
// a rule nobody accepts is a rule nobody follows.
func Acquire(dir string, roots []string, holder principal.Ref, intent string, force bool) (*Claim, error) {
	now := time.Now().UTC()

	return withLock(dir, func() (*Claim, error) {
		claims, err := List(dir)
		if err != nil {
			return nil, err
		}
		if !force {
			for _, other := range claims {
				if other.Conflicts(roots, holder, now) {
					return nil, &Conflict{Claim: other}
				}
			}
		}
		project := ""
		if len(roots) > 0 {
			project = filepath.Base(roots[0])
		}
		c := &Claim{
			SchemaVersion: SchemaVersion,
			Roots:         roots,
			Project:       project,
			Holder:        holder,
			Intent:        intent,
			AcquiredAt:    now,
			Heartbeat:     now,
			PID:           os.Getpid(),
		}
		// Preserve the original acquisition time across a refresh, so "since 3pm"
		// means when the work started, not when the last command ran.
		p := claimPath(dir, holder)
		if b, err := os.ReadFile(p); err == nil {
			var prev Claim
			if json.Unmarshal(b, &prev) == nil && !prev.AcquiredAt.IsZero() && Intersects(prev.Roots, roots) {
				c.AcquiredAt = prev.AcquiredAt
				if c.Intent == "" {
					c.Intent = prev.Intent
				}
			}
		}
		return c, writeClaim(p, c)
	})
}

// Release drops this holder's claim. A claim that is never released still expires;
// releasing is a courtesy to whoever is waiting, not a correctness requirement.
func Release(dir string, holder principal.Ref) error {
	_, err := withLock(dir, func() (*Claim, error) {
		return nil, os.Remove(claimPath(dir, holder))
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
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
