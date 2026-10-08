// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

// An instance's MAILBOX is an address plus a watermark. It is not a store.
//
// The bus already holds every message and already keeps a per-subscriber drain
// cursor, so a per-instance queue would be a second copy of mail that could
// disagree with the first. What an instance actually needs is two facts the
// bus cannot derive on its own:
//
//   - WHICH records are mine — Accepts, keyed on the UUID address, so a label
//     handed to a later instance never answers for an earlier one's mail.
//   - WHERE my mailbox begins — MailFrom, so a FRESH context is empty without
//     anything being deleted to make it so.
//
// Archiving on retire is here for the same reason: the retirement is a fleet
// fact and the records are a bus fact, so fleet owns the destination and takes
// the records as opaque lines. That keeps the bus's format out of this package
// and this package out of the bus's import graph (pkg/fleet is a leaf), and it
// is the seam the recipient-rewriting work in #1109 consumes.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InstanceAddressPrefix is the scheme of a personal mail address. Mail to an
// instance is addressed to its UUID and never to its display label: a label is
// released on retirement and re-issued, and an address that can be re-issued
// is an address that eventually delivers one conversation's mail to another.
const InstanceAddressPrefix = "instance/"

// ParseInstanceAddress extracts the UUID from a personal mail address.
func ParseInstanceAddress(addr string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(addr), InstanceAddressPrefix)
	if !ok || strings.TrimSpace(rest) == "" {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// Accepts reports whether a bus record belongs in this instance's PERSONAL
// mailbox: addressed to this UUID, and at or after the mailbox watermark.
//
// Both halves are load-bearing and they fail differently. Without the address
// check, a reused label would read its predecessor's mail. Without the
// watermark, a fresh instance on a reused label would open onto a backlog it
// was never sent — the acceptance case "retiring and reusing Esme-2 does not
// expose old mail" needs both to hold, since a determined sender can address
// the new UUID about old business and that mail IS the new instance's.
//
// Role mail is deliberately not this function's business. conductor:321
// belongs to the responsibility, so it resolves through the bus's existing
// role topics and survives a holder handoff; only personal mail follows a
// UUID. Keeping the two apart here is what makes handoff lossless.
func (i Instance) Accepts(to string, seq int64) bool {
	if strings.TrimSpace(i.UUID) == "" {
		return false
	}
	id, ok := ParseInstanceAddress(to)
	if !ok || !strings.EqualFold(id, i.UUID) {
		return false
	}
	return seq >= i.MailFrom
}

// MailboxStart is the bus sequence this instance's mailbox begins at.
func (i Instance) MailboxStart() int64 { return i.MailFrom }

// ArchiveMail appends an instance's retained mail to its retirement archive
// and returns the archive path.
//
// APPENDS, never truncates: "retiring archives mail without deleting it" is
// the contract, and a second archive call that replaced the first would delete
// exactly the evidence the first one preserved. Records are opaque lines — the
// caller owns their format.
func (s *InstanceStore) ArchiveMail(id string, records []string) (string, error) {
	inst, err := s.Get(id)
	if err != nil {
		return "", err
	}
	path := inst.MailArchive
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(s.dir, "archive", inst.UUID+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	w := bufio.NewWriter(f)
	for _, rec := range records {
		rec = strings.TrimRight(rec, "\n")
		if rec == "" {
			continue
		}
		if _, err := w.WriteString(rec + "\n"); err != nil {
			_ = f.Close()
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if inst.MailArchive != path {
		inst.MailArchive = path
		if err := s.write(inst); err != nil {
			return path, err
		}
	}
	return path, nil
}

// ArchivedMail reads back an instance's archived mail.
//
// It is readable on purpose. An archive nothing can open is indistinguishable
// from a deletion, and "archived, not deleted" is a claim somebody has to be
// able to check — including for a retired instance whose label now means
// somebody else.
func (s *InstanceStore) ArchivedMail(id string) ([]string, error) {
	inst, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	path := inst.MailArchive
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(s.dir, "archive", inst.UUID+".jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("fleet: read mail archive for %s: %w", inst.UUID, err)
	}
	return out, nil
}
