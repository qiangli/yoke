// Package dbstore is the shared, CGO-free storage-adapter boundary behind the
// Sprint 453 database tools (dhnt/docs/sprint-453-database-memory-contract.md).
//
// It is the adapter FOUNDATION, not the `bashy db` command: it resolves an
// explicit target to its canonical identity (§3.2), opens or creates an engine
// in one of the two storage modes (§3.3) under the contract invariants — opening
// a missing target never creates it, read-only is enforced by the engine or the
// connection, waits are bounded, and an in-memory database never touches disk —
// and reports what each engine actually supports (Caps, §7). It defines no
// locator grammar, no result envelope and no lease registry: those belong to
// 446 S11, the shared loom-v2 / bashy-db envelopes, and `bashy claim`.
//
// This package is stdlib-only. Each engine lives in its own subpackage
// (sqlitedb, bboltdb, blevedb, bonsaidb, vecdb) so an engine's dependencies
// reach only the binaries that import it. The engines are the pinned upstream
// modules, reused in place — nothing is copied here.
package dbstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mode is the storage mode, part of the provider-owned db target (§3.1).
type Mode string

const (
	Memory     Mode = "memory"
	Persistent Mode = "persistent"
)

// DefaultWait bounds a lock or admission wait when the caller gives none.
// No operation waits forever (§6).
const DefaultWait = 5 * time.Second

// Target is an explicit, already-parsed target. Persistent targets name a
// Path; memory targets name the owner Session and a Name inside it.
type Target struct {
	Engine  string
	Mode    Mode
	Path    string
	Session string
	Name    string
}

// Identity is a resolved target. ID is the canonical identity string — the
// key for claims, sessions and a result's target.id.
type Identity struct {
	Target
	ID string
}

// ClaimUse is the coupling seam to the existing resource/claim mechanism
// (§9.5): resource kind "db", match rule name, on the canonical identity, so an
// alias or a second spelling of one file cannot bypass a claim.
func (id Identity) ClaimUse() (kind, name string) { return "db", id.ID }

// Resolve computes the canonical identity. A persistent path is made absolute,
// symlinks are resolved and the on-disk case is taken from the filesystem, so
// two spellings of one file give one ID. A path that does not exist yet (for
// Create) resolves through its existing parent directory.
func Resolve(t Target) (Identity, error) {
	if t.Engine == "" || strings.ContainsAny(t.Engine, ":/") {
		return Identity{}, fmt.Errorf("%w: engine %q", ErrInvalidInput, t.Engine)
	}
	switch t.Mode {
	case Persistent:
		if t.Path == "" || t.Session != "" || t.Name != "" {
			return Identity{}, fmt.Errorf("%w: a persistent target names a path only", ErrInvalidInput)
		}
		p, err := canonicalPath(t.Path)
		if err != nil {
			return Identity{}, err
		}
		t.Path = p
		return Identity{Target: t, ID: "db:" + t.Engine + ":file:" + p}, nil
	case Memory:
		if t.Path != "" || t.Session == "" || t.Name == "" || strings.Contains(t.Session, "/") {
			return Identity{}, fmt.Errorf("%w: a memory target names an owner session and a name", ErrInvalidInput)
		}
		return Identity{Target: t, ID: "db:" + t.Engine + ":mem:" + t.Session + "/" + t.Name}, nil
	}
	return Identity{}, fmt.Errorf("%w: mode %q (want memory or persistent)", ErrInvalidInput, t.Mode)
}

func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return realCase(real), nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("%w: parent of %s: %v", ErrNotFound, abs, err)
	}
	return filepath.Join(realCase(parent), filepath.Base(abs)), nil
}

// realCase rewrites each component to its on-disk spelling, which differs
// from the given one only on a case-insensitive volume.
func realCase(p string) string {
	vol := filepath.VolumeName(p)
	out := vol + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(p[len(vol):], string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		name := part
		if ents, err := os.ReadDir(out); err == nil {
			exact := false
			fold := ""
			for _, e := range ents {
				if e.Name() == part {
					exact = true
					break
				}
				if strings.EqualFold(e.Name(), part) {
					fold = e.Name()
				}
			}
			if !exact && fold != "" {
				name = fold
			}
		}
		out = filepath.Join(out, name)
	}
	return out
}

// Exists reports whether a persistent target's path exists. Adapters call it
// before an engine open that would otherwise create the file or directory.
func Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("%w: %v", ErrBackend, err)
}

// Options apply to Create, Open and Drop.
type Options struct {
	// Mem is the owner's in-memory space. Required for memory targets.
	Mem *MemSpace
	// ReadOnly opens the engine (or connection) read-only, so a mutation fails
	// in the engine with ErrReadOnly. Not valid for Create.
	ReadOnly bool
	// Exclusive takes the backend-native exclusive lock (or the in-memory
	// admission lock) for the life of the handle, excluding other readers and
	// writers where Caps.Exclusion says so. It is the native half of a
	// `db hold`; the claim half belongs to `bashy claim`.
	Exclusive bool
	// Wait bounds the wait for a lock or admission; 0 means DefaultWait.
	Wait time.Duration
}

// WaitOrDefault returns the bounded wait.
func (o Options) WaitOrDefault() time.Duration {
	if o.Wait > 0 {
		return o.Wait
	}
	return DefaultWait
}

// Validate checks the options against an identity before any engine call.
func (o Options) Validate(id Identity, create bool) error {
	if create && o.ReadOnly {
		return fmt.Errorf("%w: create cannot be read-only", ErrInvalidInput)
	}
	if o.ReadOnly && o.Exclusive {
		return fmt.Errorf("%w: an exclusive hold opens a writer; drop --read-only", ErrInvalidInput)
	}
	if id.Mode == Memory {
		if o.Mem == nil {
			return fmt.Errorf("%w: memory target %s needs an owner session", ErrInvalidInput, id.ID)
		}
		return o.Mem.check(id.Session)
	}
	return nil
}

// Handle is an open database. Each adapter's concrete handle also exposes the
// engine itself for the category operations built on top.
type Handle interface {
	Identity() Identity
	ReadOnly() bool
	Close() error
}

// Adapter is one engine. Create fails with ErrExists when the target exists;
// Open fails with ErrNotFound when it does not and never creates it; Drop needs
// the target to be idle and fails with ErrBusy otherwise.
type Adapter interface {
	Engine() string
	Caps() Caps
	Create(ctx context.Context, t Target, o Options) (Handle, error)
	Open(ctx context.Context, t Target, o Options) (Handle, error)
	Drop(ctx context.Context, t Target, o Options) error
}

// Retry calls try until it stops returning ErrBusy, the wait elapses or ctx
// ends. It is the one bounded-wait loop adapters share for locks that the
// engine itself does not wait on.
func Retry(ctx context.Context, wait time.Duration, try func() error) error {
	deadline := time.Now().Add(wait)
	for {
		if err := ctx.Err(); err != nil {
			return FromContext(err)
		}
		err := try()
		if err == nil || !errors.Is(err, ErrBusy) || !time.Now().Before(deadline) {
			return err
		}
		t := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return FromContext(ctx.Err())
		case <-t.C:
		}
	}
}
