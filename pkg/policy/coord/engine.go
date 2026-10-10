// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/principal"
)

const kindRepo = "repo"

var (
	// ErrFenced is returned when a Refresh, Release or Acquire presents an
	// epoch that is no longer the claim's: the claim was taken over (or
	// re-acquired) since the caller last held it, so the caller must stop
	// acting on it.
	ErrFenced = errors.New("coord: claim fenced — the presented epoch is stale, the claim was taken over since you held it")
	// ErrNotHeld is returned when a claim to refresh does not exist.
	ErrNotHeld = errors.New("coord: claim is not held")
	// ErrEpochMismatch is a Backend's compare-and-swap failure: the stored
	// epoch is not the one the caller read.
	ErrEpochMismatch = errors.New("coord: stored epoch changed under the commit")
)

// Request asks the ledger for a claim on one ref.
type Request struct {
	Ref Ref
	// Members are what the claim covers; empty resolves through the kind's
	// provider, then defaults to the ref's own name.
	Members []string
	Holder  principal.Ref
	Intent  string
	// Mode is ModeLease, ModeAttached or ModeAnnounce; empty takes the kind's
	// first permitted mode.
	Mode  string
	Force bool
	// Wait retries a conflicted acquisition for up to this long.
	Wait time.Duration
	// Epoch, when set, must be the epoch this holder last held; a different
	// one means the claim moved on and the request fails with ErrFenced.
	Epoch uint64
}

// Grant is a successful hold. Epoch is the fencing token: present it to
// Refresh and ReleaseRef, and hand it to whatever the claim protects.
type Grant struct {
	Claim *Claim
	Epoch uint64
}

// store is the engine over one registry directory.
type store struct{ dir string }

// AcquireRef takes or refreshes a claim on r.Ref. It is idempotent for the same
// holder (no epoch bump); a new acquisition — first, or after the previous
// holder lapsed, released or was forced out — bumps the key's epoch.
func AcquireRef(ctx context.Context, r Request) (Grant, error) {
	if r.Mode == ModeAttached {
		return Grant{}, fmt.Errorf("claim: attached holds carry a kernel lock; use AcquireAttachedRef")
	}
	return store{DefaultDir()}.acquireWait(ctx, acquireSpec{req: r}, r.Wait)
}

// Refresh heartbeats the caller's claim. epoch 0 means "my current claim".
func Refresh(ctx context.Context, ref Ref, holder principal.Ref, epoch uint64) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	return store{DefaultDir()}.refresh(ref, holder, epoch)
}

// ReleaseRef drops the caller's claim. epoch 0 means "my current claim" and
// is accepted only when the holder matches; releasing an absent claim is a
// no-op.
func ReleaseRef(ctx context.Context, ref Ref, holder principal.Ref, epoch uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store{DefaultDir()}.release(ref, holder, epoch)
}

// AcquireAttachedRef takes a kernel-backed hold on ref for one child process.
func AcquireAttachedRef(ctx context.Context, r Request) (Grant, *lockfile.Lock, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, nil, err
	}
	return store{DefaultDir()}.acquireAttached(r)
}

type acquireSpec struct {
	req Request
	// legacy is the holder-keyed project claim: Roots, one file per holder.
	legacy bool
	roots  []string
	// attached means the caller already holds the kernel lock, so any other
	// attached record for the key is a leftover from a dead process.
	attached bool
}

func (s store) acquireWait(ctx context.Context, sp acquireSpec, wait time.Duration) (Grant, error) {
	deadline := time.Now().Add(wait)
	backoff := 20 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return Grant{}, err
		}
		g, err := s.acquire(sp)
		var conflict *Conflict
		if err == nil || wait <= 0 || !errors.As(err, &conflict) || !time.Now().Before(deadline) {
			return g, err
		}
		d := min(backoff, time.Until(deadline))
		if d > 0 {
			select {
			case <-ctx.Done():
				return Grant{}, ctx.Err()
			case <-time.After(d):
			}
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

func (s store) backend(kind string) Backend {
	if b := customBackend(kind); b != nil {
		return b
	}
	return fileBackend{dir: s.dir}
}

// txn runs fn with the right serialization for the backend: the whole
// read-decide-write cycle under claims.lock for the file store, a bare
// compare-and-swap for a custom one.
func (s store) txn(b Backend, fn func(locked bool) error) error {
	if _, ok := b.(fileBackend); ok {
		return withDirLock(s.dir, func() error { return fn(true) })
	}
	return fn(false)
}

func commit(b Backend, locked bool, key string, prev uint64, next *Claim) error {
	if fb, ok := b.(fileBackend); ok && locked {
		return fb.commitLocked(key, prev, next)
	}
	return b.CommitIfEpoch(key, prev, next)
}

func (s store) acquire(sp acquireSpec) (Grant, error) {
	r := sp.req
	holder := r.Holder
	var ref Ref
	var key string
	if sp.legacy {
		ref = Ref{Kind: kindRepo}
		if len(sp.roots) > 0 {
			ref.Name = filepath.Base(sp.roots[0])
		}
		key = holderKey(holder)
	} else {
		ref = Ref{Kind: strings.TrimSpace(r.Ref.Kind), Name: strings.TrimSpace(r.Ref.Name)}
		if ref.Kind == "" {
			ref.Kind = KindName
		}
		if ref.Name == "" {
			return Grant{}, fmt.Errorf("claim: resource name is required")
		}
		key = ref.String()
	}
	kind := kindOrDefault(ref.Kind)
	mode := r.Mode
	if sp.attached {
		mode = ModeAttached
	}
	if mode == "" && !sp.legacy {
		mode = ModeLease
		if len(kind.Modes) > 0 {
			mode = kind.Modes[0]
		}
	}
	if mode != "" && !kind.allows(mode) {
		return Grant{}, fmt.Errorf("claim: kind %q does not permit mode %q", kind.Name, mode)
	}
	var members []string
	if sp.legacy {
		members = sp.roots
	} else {
		var err error
		if members, err = resolveMembers(kind, ref, r.Members); err != nil {
			return Grant{}, err
		}
	}

	b := s.backend(ref.Kind)
	if sp.legacy {
		b = fileBackend{dir: s.dir}
	}
	var grant Grant
	var displaced []*Claim
	err := s.txn(b, func(locked bool) error {
		var last error
		for range 3 {
			g, forced, err := s.acquireOnce(b, locked, sp, ref, key, kind, mode, members)
			if err == nil {
				grant, displaced = g, forced
				return nil
			}
			last = err
			if !errors.Is(err, ErrEpochMismatch) || locked {
				break
			}
		}
		return last
	})
	if err == nil && len(displaced) > 0 {
		auditForce(ref, r, displaced)
	}
	return grant, err
}

func (s store) acquireOnce(b Backend, locked bool, sp acquireSpec, ref Ref, key string, kind Kind, mode string, members []string) (Grant, []*Claim, error) {
	r := sp.req
	now := time.Now().UTC()
	prev, err := b.Load(key)
	if err != nil {
		return Grant{}, nil, err
	}
	var prevEpoch uint64
	if prev != nil {
		prevEpoch = prev.Epoch
		// An attached record is diagnostic; the kernel lock is authoritative.
		if prev.Mode == ModeAttached && (sp.attached || !s.attachedHeld(b, prev)) {
			prev = nil
		}
	}

	var displaced []*Claim
	reuse := false
	if prev != nil {
		if sameHolder(prev.Holder, r.Holder) {
			if prev.Mode == ModeAttached || (mode == ModeAttached) {
				return Grant{}, nil, &Conflict{Claim: prev}
			}
			if r.Epoch != 0 && r.Epoch != prev.Epoch {
				return Grant{}, nil, ErrFenced
			}
			reuse = !sp.legacy || Intersects(prev.Roots, sp.roots)
		} else if !prev.Liveness(now).Takeable() {
			if prev.Mode == ModeAttached || !r.Force {
				return Grant{}, nil, &Conflict{Claim: prev}
			}
			displaced = append(displaced, prev)
		}
	}

	// Cross-key matching needs the whole ledger, which only the file store
	// can enumerate.
	if _, ok := b.(fileBackend); ok {
		all, err := List(s.dir)
		if err != nil {
			return Grant{}, nil, err
		}
		for _, o := range all {
			if o.key() == key || sameHolder(o.Holder, r.Holder) || o.Liveness(now).Takeable() {
				continue
			}
			if !kindsConflict(kind, ref.Name, members, kindOrDefault(o.Kind), o.name(), o.Members) {
				continue
			}
			if o.Mode == ModeAttached || !r.Force {
				return Grant{}, nil, &Conflict{Claim: o}
			}
			displaced = append(displaced, o)
		}
	}

	c := &Claim{
		SchemaVersion: SchemaVersion,
		Kind:          ref.Kind,
		Members:       members,
		Mode:          mode,
		Holder:        r.Holder,
		Intent:        r.Intent,
		AcquiredAt:    now,
		Heartbeat:     now,
		PID:           os.Getpid(),
	}
	if sp.legacy {
		c.Roots = sp.roots
		c.Project = ref.Name
	} else {
		c.Resource = ref.Name
	}
	if reuse {
		// Preserve the original acquisition time across a refresh, so "since 3pm"
		// means when the work started, not when the last command ran.
		if !prev.AcquiredAt.IsZero() {
			c.AcquiredAt = prev.AcquiredAt
		}
		if c.Intent == "" {
			c.Intent = prev.Intent
		}
		// A v1 record has no epoch; its first v2 refresh makes it epoch 1.
		c.Epoch = max(prev.Epoch, 1)
	} else {
		c.Epoch = prevEpoch + 1
	}
	if err := commit(b, locked, key, prevEpoch, c); err != nil {
		return Grant{}, nil, err
	}
	return Grant{Claim: c, Epoch: c.Epoch}, displaced, nil
}

// attachedHeld reports whether an attached record's kernel lock is still
// held. A custom backend has no lock file to consult; its record stands.
func (s store) attachedHeld(b Backend, c *Claim) bool {
	if _, ok := b.(fileBackend); !ok {
		return true
	}
	_, held := lockfile.Owner(keyedLockPath(s.dir, c.Ref()))
	return held
}

func (s store) acquireAttached(r Request) (Grant, *lockfile.Lock, error) {
	ref := Ref{Kind: strings.TrimSpace(r.Ref.Kind), Name: strings.TrimSpace(r.Ref.Name)}
	if ref.Kind == "" {
		ref.Kind = KindName
	}
	if ref.Name == "" {
		return Grant{}, nil, fmt.Errorf("claim: resource name is required")
	}
	h := lockfile.Holder{Name: r.Holder.Name, PID: os.Getpid(), Intent: r.Intent, Since: time.Now()}
	lockPath := keyedLockPath(s.dir, ref)
	var l *lockfile.Lock
	var err error
	if r.Wait > 0 {
		l, err = lockfile.AcquireWithin(lockPath, r.Wait, h)
	} else {
		l, err = lockfile.TryAcquire(lockPath, h)
	}
	if err != nil {
		if c, _ := (fileBackend{dir: s.dir}).Load(ref.String()); c != nil && c.Resource == ref.Name {
			return Grant{}, nil, &Conflict{Claim: c}
		}
		if owner, ok := lockfile.HeldBy(err); ok {
			return Grant{}, nil, &Conflict{Claim: &Claim{
				SchemaVersion: SchemaVersion, Kind: ref.Kind, Resource: ref.Name, Mode: ModeAttached,
				Holder: principal.Ref{Name: owner.Name}, Intent: owner.Intent,
				AcquiredAt: owner.Since, Heartbeat: owner.Since, PID: owner.PID,
			}}
		}
		return Grant{}, nil, err
	}
	r.Ref = ref
	g, err := s.acquire(acquireSpec{req: r, attached: true})
	if err != nil {
		_ = l.Release()
		return Grant{}, nil, err
	}
	return g, l, nil
}

func (s store) refresh(ref Ref, holder principal.Ref, epoch uint64) (Grant, error) {
	ref = normRef(ref)
	b := s.backend(ref.Kind)
	key := ref.String()
	var grant Grant
	err := s.txn(b, func(locked bool) error {
		prev, err := b.Load(key)
		if err != nil {
			return err
		}
		if prev == nil {
			return fmt.Errorf("%w: %s", ErrNotHeld, key)
		}
		if err := checkHeld(prev, holder, epoch); err != nil {
			return err
		}
		if prev.Mode == ModeAttached {
			grant = Grant{Claim: prev, Epoch: prev.Epoch}
			return nil
		}
		next := *prev
		next.Heartbeat = time.Now().UTC()
		if err := commit(b, locked, key, prev.Epoch, &next); err != nil {
			return err
		}
		grant = Grant{Claim: &next, Epoch: next.Epoch}
		return nil
	})
	return grant, err
}

func (s store) release(ref Ref, holder principal.Ref, epoch uint64) error {
	ref = normRef(ref)
	b := s.backend(ref.Kind)
	key := ref.String()
	return s.txn(b, func(locked bool) error {
		prev, err := b.Load(key)
		if err != nil || prev == nil {
			return err
		}
		if err := checkHeld(prev, holder, epoch); err != nil {
			return err
		}
		if prev.Mode == ModeAttached {
			return fmt.Errorf("claim: %s is attached to a live child and cannot be released separately", ref.Name)
		}
		return commit(b, locked, key, prev.Epoch, nil)
	})
}

// checkHeld decides whether holder, presenting epoch, may act on prev. An
// epoch other than the claim's is a fence no matter who holds it now; epoch 0
// is "my current claim", which only the holder may say.
func checkHeld(prev *Claim, holder principal.Ref, epoch uint64) error {
	if epoch != 0 && epoch != prev.Epoch {
		return ErrFenced
	}
	if !sameHolder(prev.Holder, holder) {
		return &Conflict{Claim: prev}
	}
	return nil
}

func normRef(ref Ref) Ref {
	ref.Kind, ref.Name = strings.TrimSpace(ref.Kind), strings.TrimSpace(ref.Name)
	if ref.Kind == "" {
		ref.Kind = KindName
	}
	return ref
}

func resolveMembers(kind Kind, ref Ref, given []string) ([]string, error) {
	if len(given) > 0 {
		return append([]string(nil), given...), nil
	}
	if p, ok := providerFor(ref.Kind); ok {
		return p.Members(ref.Name)
	}
	if kind.Match == MatchName {
		return nil, nil
	}
	return []string{ref.Name}, nil
}

// name is what the claim is called: the resource, or a project claim's label.
func (c *Claim) name() string {
	if c.Resource != "" {
		return c.Resource
	}
	return c.Project
}

// Ref is the claim's address.
func (c *Claim) Ref() Ref {
	c.normalize()
	return Ref{Kind: c.Kind, Name: c.name()}
}

// key is the claim's storage key: its ref, or its holder for a project claim.
func (c *Claim) key() string {
	if c.Resource != "" {
		return c.Ref().String()
	}
	return holderKey(c.Holder)
}

// normalize reads a v1 record as v2: project claims are repo claims over their
// roots, resource claims are name claims.
func (c *Claim) normalize() {
	if c.Kind == "" {
		if c.Resource != "" {
			c.Kind = KindName
		} else {
			c.Kind = kindRepo
		}
	}
	if len(c.Members) == 0 && c.Resource == "" {
		c.Members = c.Roots
	}
}

func holderKey(h principal.Ref) string { return "holder:" + holderID(h) }

func holderID(h principal.Ref) string {
	id := h.Episode
	if id == "" {
		id = h.Name + "@" + h.Host
	}
	if id == "" || id == "@" {
		id = "unattributed"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, id)
}

// keyedClaimPath is one file per (kind, name). Kind name keeps the v1
// resource-<sha> layout so existing records and kernel locks stay addressable.
func keyedClaimPath(dir string, ref Ref) string {
	return filepath.Join(dir, keyedBase(ref)+".json")
}

func keyedLockPath(dir string, ref Ref) string {
	return filepath.Join(dir, keyedBase(ref)+".lock")
}

func keyedBase(ref Ref) string {
	if ref.Kind == KindName {
		return "resource-" + resourceKey(ref.Name)
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, ref.Kind)
	sum := sha256.Sum256([]byte(ref.String()))
	return fmt.Sprintf("claim-%s-%x", safe, sum[:])
}

// fileBackend is the default Backend: one JSON file per key under dir, with a
// sidecar holding the high-water epoch so a deleted key never reuses one.
type fileBackend struct{ dir string }

func (f fileBackend) path(key string) string {
	if id, ok := strings.CutPrefix(key, "holder:"); ok {
		return filepath.Join(f.dir, id+".json")
	}
	kind, name, _ := strings.Cut(key, ":")
	return keyedClaimPath(f.dir, Ref{Kind: kind, Name: name})
}

func readClaimFile(path string) (*Claim, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c Claim
	if json.Unmarshal(b, &c) != nil || c.SchemaVersion == "" {
		return nil, nil // a corrupt record is no claim
	}
	c.normalize()
	return &c, nil
}

func (f fileBackend) Load(key string) (*Claim, error) { return readClaimFile(f.path(key)) }

func (f fileBackend) CommitIfEpoch(key string, prevEpoch uint64, next *Claim) error {
	return withDirLock(f.dir, func() error { return f.commitLocked(key, prevEpoch, next) })
}

// commitLocked is CommitIfEpoch for a caller already holding claims.lock.
func (f fileBackend) commitLocked(key string, prevEpoch uint64, next *Claim) error {
	path := f.path(key)
	cur, err := readClaimFile(path)
	if err != nil {
		return err
	}
	var curEpoch uint64
	if cur != nil {
		curEpoch = cur.Epoch
	}
	if curEpoch != prevEpoch {
		return ErrEpochMismatch
	}
	epochPath := strings.TrimSuffix(path, ".json") + ".epoch"
	if next == nil {
		if cur == nil {
			return nil
		}
		if err := os.WriteFile(epochPath, []byte(strconv.FormatUint(cur.Epoch, 10)), 0o644); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if cur == nil {
		if b, err := os.ReadFile(epochPath); err == nil {
			if hw, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); next.Epoch <= hw {
				next.Epoch = hw + 1
			}
		}
	}
	return writeClaim(path, next)
}

func withDirLock(dir string, fn func() error) error {
	_, err := withLock(dir, func() (*Claim, error) { return nil, fn() })
	return err
}
