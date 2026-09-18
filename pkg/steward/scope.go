// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HostIDEnv names the machine when — and ONLY when — the OS cannot.
//
// IT IS A FALLBACK, NOT AN OVERRIDE, and the previous revision had that backwards. It
// consulted the environment FIRST and the OS only if the variable was unset, which made
// the machine identity settable by the process whose isolation depends on it:
//
//	BASHY_HOST_ID=somewhere-else bashy steward claim
//
// produced a DIFFERENT seat on the same machine — a different scope digest, a different
// store, a different epoch ladder — so an agent that did not like the steward it found
// could simply declare itself to be on another computer and take a fresh one. That is the
// same class of hole as the `USER=root` trick this file was written to close, reintroduced
// by the escape hatch meant to make it usable.
//
// So the order is now: ask the OS; believe it if it answers. The environment is consulted
// only where the OS has nothing to say — a container with no /etc/machine-id, an exotic
// platform — which is the case the variable was actually for. It remains an ANSWER to "I
// cannot establish this", never a shortcut past it: with no OS source and no fallback, the
// seat fails closed rather than guessing (see machineID and ErrNoStableIdentity).
//
// A host that must resolve identity its own way has a trusted, in-process hook for it —
// WithScopeProvider — which an agent cannot reach by setting a variable.
const HostIDEnv = "BASHY_HOST_ID"

// Scope is WHICH SEAT this is: one stable machine, one stable OS account.
//
// Both halves used to come from the ambient environment — hostname, and $USER — and
// both were wrong, in the same way and for the same reason: THE ENVIRONMENT IS NOT AN
// IDENTITY, it is a suggestion the process can rewrite. `USER=root bashy steward
// claim` was a different seat from `bashy steward claim`, so an agent that wanted a
// seat someone else held could simply ask for a different one, take it, and be the
// steward — of a host that already had one. And a hostname is not a machine: two
// laptops both called "localhost" sharing a synced home directory would have merged
// their journals, their epoch ladders, and their stewards.
//
// So identity now comes from the OS and not from the environment:
//
//	Machine  a stable machine id — /etc/machine-id (linux), kern.uuid (darwin),
//	         kern.hostuuid (freebsd), the MachineGuid registry value (windows) — or
//	         $BASHY_HOST_ID when the OS has none. Never the hostname, which is a label
//	         a machine wears, not a machine.
//	Account  the OS account: the numeric UID on unix, the account SID on windows.
//	         Never $USER/$LOGNAME/$USERNAME, which are strings the process inherits and
//	         can set.
//
// Host is carried too, but ONLY as a human-readable label in the directory name. It is
// deliberately not part of the identity digest: rename a host and its seat must still
// be its seat.
type Scope struct {
	ID      string `json:"id"`      // the store key: <host-label>-<account-label>-<digest>
	Machine string `json:"machine"` // opaque, stable, from the OS (or $BASHY_HOST_ID)
	Account string `json:"account"` // opaque, stable, from the OS
	Host    string `json:"host"`    // a LABEL. Not identity — see above.
	Source  string `json:"source"`  // where the machine identity came from
}

// Digest is the identity of the seat, reduced to a comparison token. The store binds
// itself to this (see bindScope), so a store carried onto another machine — in a
// synced home, a restored backup, a container image — is REFUSED rather than silently
// adopted as that machine's seat.
func (sc Scope) Digest() string {
	sum := sha256.Sum256([]byte("bashy-steward-scope\x00" + sc.Machine + "\x00" + sc.Account))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ScopeProvider resolves the seat's identity. Injectable so the isolation properties
// can be TESTED — same account on two machines, same hostname on two machines, the
// same machine under two accounts — none of which is reachable by setting an env var
// any more, which was the entire point of removing the env vars.
type ScopeProvider interface {
	Scope() (Scope, error)
}

// ScopeFunc adapts a function to ScopeProvider.
type ScopeFunc func() (Scope, error)

func (f ScopeFunc) Scope() (Scope, error) { return f() }

// StaticScope is a fixed scope, for tests and for a host that resolves identity its
// own way.
func StaticScope(sc Scope) ScopeProvider {
	return ScopeFunc(func() (Scope, error) { return sc, nil })
}

// OSScope resolves the seat from the operating system.
type OSScope struct{}

func (OSScope) Scope() (Scope, error) { return resolveOSScope() }

// ErrNoStableIdentity is returned when neither the OS nor the operator can say which
// machine this is. It FAILS CLOSED, and the alternative is why.
//
// The tempting fallback is to generate an id and persist it under $HOME. That is
// exactly wrong: a shared or synced home is the case machine identity exists to
// detect, and an id stored there would travel with the home directory to every machine
// mounting it — handing all of them the same seat, which is the failure, wearing the
// costume of the fix.
type ErrNoStableIdentity struct{ Why string }

func (e *ErrNoStableIdentity) Error() string {
	return fmt.Sprintf("steward: cannot establish a stable machine identity (%s).\n"+
		"The seat is one-per-machine-and-account, and a seat that cannot say WHICH machine it belongs to would merge "+
		"the journals of every machine sharing a home directory. Refusing to guess.\n"+
		"Set $%s to something stable and unique to this machine (a UUID is ideal; it is compared, never interpreted).",
		e.Why, HostIDEnv)
}

func resolveOSScope() (Scope, error) {
	acct, err := accountID()
	if err != nil {
		return Scope{}, fmt.Errorf("steward: cannot establish the OS account this seat belongs to: %w", err)
	}

	// THE OS FIRST, AND THE ENVIRONMENT ONLY IF IT HAS NOTHING. See HostIDEnv: consulting
	// the variable first let a process rename the machine it was running on, and a machine
	// identity a process can choose is not one.
	machine, source, err := machineID()
	if err != nil {
		machine, source = strings.TrimSpace(os.Getenv(HostIDEnv)), "env:"+HostIDEnv
		if machine == "" {
			return Scope{}, &ErrNoStableIdentity{Why: err.Error()}
		}
	}

	sc := Scope{Machine: machine, Account: acct, Host: hostLabel(), Source: source}
	sc.ID = scopeIDFor(sc)
	return sc, nil
}

// scopeIDFor builds the store key. The readable halves are a courtesy for whoever
// looks in ~/.bashy/steward; the digest carries the actual isolation.
func scopeIDFor(sc Scope) string {
	sum := sha256.Sum256([]byte(sc.Machine + "\x00" + sc.Account))
	return slug(sc.Host) + "-" + slug(accountLabel(sc.Account)) + "-" + hex.EncodeToString(sum[:5])
}

// hostLabel is the hostname, and it is ONLY a label. See Scope.
func hostLabel() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "host"
	}
	return h
}

// accountLabel reduces an account id to something readable in a path. The SID form on
// windows is long and its tail is the distinguishing part.
func accountLabel(acct string) string {
	acct = strings.TrimPrefix(acct, "uid:")
	if i := strings.LastIndex(acct, "-"); i >= 0 && i+1 < len(acct) {
		acct = acct[i+1:]
	}
	return "u" + acct
}

// slug reduces a name to a filesystem-safe token. Lossy on purpose — the digest in
// scopeIDFor carries the precision, this half carries the readability.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
		if b.Len() >= 32 {
			break
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return strings.Trim(b.String(), "-")
}

// ScopeID is the default seat's store key.
func ScopeID() (string, error) {
	sc, err := OSScope{}.Scope()
	if err != nil {
		return "", err
	}
	return sc.ID, nil
}

// DefaultDir is the machine/account-scoped seat: ~/.bashy/steward/<scope>, or
// $BASHY_STEWARD_DIR verbatim when set.
//
// HOST-WIDE and cwd-INDEPENDENT, deliberately. A steward is not a property of a
// checkout — it is the human's continuous point of contact across every project on the
// machine. Keying it to a repository would produce one steward per clone, which is
// precisely what the singleton exists to prevent.
func DefaultDir() (string, error) {
	if v := os.Getenv("BASHY_STEWARD_DIR"); v != "" {
		return v, nil
	}
	sc, err := OSScope{}.Scope()
	if err != nil {
		return "", err
	}
	return defaultDirFor(sc)
}

// defaultDirFor picks the default STORE directory, and $HOME is allowed to move it — for
// the same reason --dir and $BASHY_STEWARD_DIR are allowed to move it: saying WHERE the seat
// keeps its bytes is not the same as being allowed to have a second one. Which directory a
// seat lives in is settable; HOW MANY it lives in is not, and that is the registry's job
// (registry.go), whose own root is rooted in the OS account precisely so this knob cannot
// reach it. Point $HOME somewhere new and you do not get a fresh seat — you get
// ErrScopeDirConflict from the same canonical registry, naming the store you already have.
func defaultDirFor(sc Scope) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bashy-steward", sc.ID), nil
	}
	return filepath.Join(home, ".bashy", "steward", sc.ID), nil
}

// scopeBinding is the store's record of WHOSE seat it is, written once and checked on
// every open.
//
// This is what makes the identity rework enforceable rather than merely correct. The
// store directory is a path, and a path can be pointed at deliberately (--dir), carried
// by a synced home, or restored from a backup onto a different machine. Without a
// binding, the store simply becomes whoever opens it. With one, a store that was born
// on another machine — or under another account — is REFUSED, loudly, naming both.
type scopeBinding struct {
	SchemaVersion string    `json:"schema_version"`
	Scope         string    `json:"scope"`
	Digest        string    `json:"digest"` // sha256 over (machine, account) — see Scope.Digest
	Host          string    `json:"host"`   // the label at bind time, for the error message
	Source        string    `json:"source"`
	BoundAt       time.Time `json:"bound_at"`
}

// ErrScopeMismatch is returned when a store belongs to a different machine or account
// than the process opening it.
type ErrScopeMismatch struct {
	Dir   string
	Bound scopeBinding
	Now   Scope
}

func (e *ErrScopeMismatch) Error() string {
	return fmt.Sprintf("steward: the store at %s belongs to another seat.\n"+
		"  bound to:  %s (host label %q at bind time)\n"+
		"  this seat: %s (host label %q)\n"+
		"The seat is one-per-machine-and-account, and the two do not match — which is exactly what a synced home "+
		"directory, a restored backup, or a container image with a baked-in $HOME produces. Adopting it would give two "+
		"machines one journal, one epoch ladder, and two stewards that fence each other forever.\n"+
		"If this store really is this machine's, move it to `bashy steward scope`'s directory; do that only on the machine "+
		"the journal actually belongs to, because that judgement is the whole reason this check exists.",
		e.Dir, e.Bound.Scope, e.Bound.Host, e.Now.ID, e.Now.Host)
}

// bindScope writes the binding on a fresh store, and validates it on every later open.
func (s *Store) bindScope() error {
	path := filepath.Join(s.dir, "scope.json")
	var b scopeBinding
	found, err := readJSON(path, &b)
	if err != nil {
		return fmt.Errorf("steward: the store's scope binding is unreadable (%s): %w — "+
			"refusing to open a store that cannot say whose seat it is", path, err)
	}
	if found {
		if b.Digest != s.scope.Digest() {
			return &ErrScopeMismatch{Dir: s.dir, Bound: b, Now: s.scope}
		}
		return nil
	}
	return writeJSONAtomic(path, scopeBinding{
		SchemaVersion: SchemaVersion,
		Scope:         s.scope.ID,
		Digest:        s.scope.Digest(),
		Host:          s.scope.Host,
		Source:        s.scope.Source,
		BoundAt:       time.Now().UTC(),
	})
}
