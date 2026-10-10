// Package stateaccess tells a DENIED coordination store apart from a BUSY one.
//
// Agents coordinate through host state under ~/.bashy (sprint queue.lock, coord
// claims.lock, room member-claims.lock). A sandboxed session whose profile does
// not grant that state can still read it, so every read-only probe looks
// healthy, and the first O_RDWR open fails with EPERM — sometimes after the
// command already mutated a sibling store. Two rules follow:
//
//   - Preflight before the first mutation, so a denied store fails the command
//     while nothing has changed yet.
//   - A denial is never contention. It does not wrap lockfile.ErrHeld, and the
//     remedy it names is an explicit grant at the agent's launcher — never
//     disabling the sandbox, forcing, or deleting the lock.
//
// Preflight takes no kernel lock, so it cannot block or be blocked by a holder.
package stateaccess

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DeniedError reports that the session may not write a coordination store.
type DeniedError struct {
	Store string // human name, e.g. "coord claim ledger"
	Path  string
	Err   error
}

func (e *DeniedError) Error() string {
	root := filepath.Dir(e.Path)
	return fmt.Sprintf("%s: write access to %s is DENIED by this session's sandbox (%v). "+
		"This is not lock contention and no lock was taken. Relaunch the agent with an "+
		"explicit grant for %s (e.g. foreman/chat --sandbox workspace-write --writable-root %s), "+
		"or run the command from a session whose profile already grants it. "+
		"Do not delete or force the lock",
		e.Store, e.Path, rootCause(e.Err), root, root)
}

func (e *DeniedError) Unwrap() error { return e.Err }

// IsDenied reports whether err is (or wraps) a DeniedError.
func IsDenied(err error) bool {
	var d *DeniedError
	return errors.As(err, &d)
}

// permissionDenied matches what a sandbox or read-only mount returns on open.
func permissionDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// Diagnose turns a permission failure from opening a coordination lock into a
// DeniedError. Any other error — including lockfile.ErrHeld — is returned as is.
func Diagnose(store, path string, err error) error {
	if err == nil || IsDenied(err) || !permissionDenied(err) {
		return err
	}
	return &DeniedError{Store: store, Path: path, Err: err}
}

// CheckLock proves the session may open path the way lockfile does
// (O_CREATE|O_RDWR) without locking it. The lock sentinel is the only file
// it may create; its contents are never touched.
func CheckLock(store, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		if merr := os.MkdirAll(filepath.Dir(path), 0o755); merr != nil {
			return Diagnose(store, path, merr)
		}
		f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	}
	if err != nil {
		if d := Diagnose(store, path, err); IsDenied(d) {
			return d
		}
		return fmt.Errorf("%s: open %s: %w", store, path, err)
	}
	return f.Close()
}

func rootCause(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}
