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

// Claim is one agent's hold on a project.
type Claim struct {
	SchemaVersion string `json:"schema_version"`

	// Roots is the PATH SET. Conflict is intersection, not equality.
	Roots []string `json:"roots"`
	// Project is a human label (the primary root's basename).
	Project string `json:"project"`

	Holder principal.Ref `json:"holder"`
	Intent string        `json:"intent,omitempty"`

	AcquiredAt time.Time `json:"acquired_at"`
	Heartbeat  time.Time `json:"heartbeat"`
	PID        int       `json:"pid,omitempty"`
}

// Live reports whether the claim still holds. The heartbeat is the test; a dead PID
// is corroboration, never the verdict — an LLM session has no stable process, and
// judging liveness by PID would evict a conductor between two of its own commands.
func (c *Claim) Live(now time.Time) bool {
	return now.Sub(c.Heartbeat) < TTL
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
	if c.Stale(now) {
		return false
	}
	if sameHolder(c.Holder, holder) {
		return false
	}
	return Intersects(c.Roots, roots)
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
	fmt.Fprintf(&b, "%s is already working in %s", who, c.Claim.Project)
	if c.Claim.Intent != "" {
		fmt.Fprintf(&b, " (%s)", c.Claim.Intent)
	}
	fmt.Fprintf(&b, ", since %s.\n\n", c.Claim.AcquiredAt.Format(time.Kitchen))
	fmt.Fprintf(&b, "Two agents writing one project is how an untested change reaches main: one session\n")
	fmt.Fprintf(&b, "sweeps another's staged work into its commit, and nobody can tell whose edit was whose.\n\n")
	fmt.Fprintf(&b, "  bashy claim list                # who is working, where, on what\n")
	fmt.Fprintf(&b, "  bashy claim request -m <reason> # ask this owner to merge/sequence/release\n")
	fmt.Fprintf(&b, "  bashy weave add \"<task>\"        # work in an ISOLATED workspace instead\n")
	fmt.Fprintf(&b, "  BASHY_CLAIM_FORCE=1 <command>   # override (recorded in the audit log)\n")
	return b.String()
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
