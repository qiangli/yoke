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
	"errors"
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

// MailSource yields an instance's retained PERSONAL mail as opaque lines.
//
// It is the seam between this leaf package and the bus, which owns the records
// and their format. Retire takes one rather than reading the bus itself (that
// would put a consumer in fleet's import graph) and rather than defaulting to
// "no mail" (that is the empty-placeholder archive of finding 5).
type MailSource func(Instance) ([]string, error)

// ErrMailSourceRequired reports a retirement with no way to collect the mail
// it claims to archive.
//
// Fail closed, on purpose. The alternative — accept nil and write an empty
// archive — produces a retirement that REPORTS archived mail and holds none,
// and the operator has no way to tell that from an instance that genuinely
// received nothing.
var ErrMailSourceRequired = errors.New("fleet: retiring an instance requires a mail source; archiving nothing is not archiving")

// ArchiveMail appends an instance's retained mail to its retirement archive
// and returns the archive path.
//
// APPENDS, never truncates: "retiring archives mail without deleting it" is
// the contract, and a second archive call that replaced the first would delete
// exactly the evidence the first one preserved. Records are opaque lines — the
// caller owns their format.
//
// It takes the store lock, because it updates the record's archive pointer and
// must not race Retire doing the same.
func (s *InstanceStore) ArchiveMail(id string, records []string) (string, error) {
	var path string
	err := s.withLock("archive instance mail", func() error {
		inst, err := s.Get(id)
		if err != nil {
			return err
		}
		path, err = s.writeArchive(inst, records, false)
		if err != nil {
			return err
		}
		if inst.MailArchive != path {
			inst.MailArchive = path
			return s.write(inst)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// writeArchive is ArchiveMail's body, without the lock and without touching the
// record: Retire already holds the lock and writes the record once itself.
//
// The path is ALWAYS archivePath(inst). A stored MailArchive is data in a
// user-writable record and must not redirect where mail is appended.
// skipPresent drops records whose exact line is already in the archive, which
// is what makes a retried retirement idempotent.
func (s *InstanceStore) writeArchive(inst Instance, records []string, skipPresent bool) (string, error) {
	path := s.archivePath(inst)
	if skipPresent {
		have, err := readLines(path)
		if err != nil {
			return "", err
		}
		present := make(map[string]bool, len(have))
		for _, l := range have {
			present[l] = true
		}
		fresh := records[:0:0]
		for _, rec := range records {
			if !present[strings.TrimRight(rec, "\n")] {
				fresh = append(fresh, rec)
			}
		}
		records = fresh
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
	return path, nil
}

// readLines returns the non-empty trimmed lines of a file; a missing file is none.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, checkMissingDir(filepath.Dir(path))
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
		return nil, fmt.Errorf("fleet: read mail archive %s: %w", path, err)
	}
	return out, nil
}

// archivePath keys the archive on the UUID, which is what makes a retired
// instance's mail still findable under the identity it was addressed to after
// its label has been handed to somebody else.
func (s *InstanceStore) archivePath(inst Instance) string {
	return filepath.Join(s.dir, "archive", inst.UUID+".jsonl")
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
	return readLines(s.archivePath(inst))
}
