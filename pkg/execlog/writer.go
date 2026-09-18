// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dirMode  = 0o700
	fileMode = 0o600 // a no-op on Windows; see the note in Open
)

// Writer appends records to the day's episode file.
//
// One open fd, one write(2) per record, no lock. That is affordable on the hot
// path in a way the audit log's flock+fsync deliberately is not — measured on
// a working host, the median dispatched command completes in under a
// millisecond, so any per-command fsync would be a thousandfold tax on the
// common case.
//
// Concurrency safety comes from O_APPEND plus the size cap in Scrub: a record
// small enough to be one write is atomic against other appenders on a local
// filesystem. On a network filesystem it is not, which is why Open records the
// storage kind rather than assuming.
type Writer struct {
	root string

	mu   sync.Mutex
	day  string
	file *os.File

	seq atomic.Uint64

	// dropped counts records this Writer could not persist. It is surfaced by
	// Dropped() and must be reported by any reader: a store that silently ate
	// records answers "how often does X fail here?" with a confident wrong
	// number.
	dropped atomic.Int64
}

// Open prepares a writer rooted at dir. It creates nothing until the first
// append, so a bashy process that never dispatches a command pays no IO.
//
// Note for Windows: fileMode is not enforced there. The store inherits the
// user profile's ACL, which is the right practical outcome, but this package
// does not promise a permission guarantee it cannot keep on every platform.
func Open(dir string) *Writer { return &Writer{root: dir} }

// Append writes one record.
//
// It takes a Scrubbed rather than raw argv so that a caller cannot skip
// redaction. Nothing about the signature is convenience — it is the enforcement.
func (w *Writer) Append(meta Record, body Scrubbed, episode string) error {
	meta.Schema = Schema
	meta.CanonVer = CanonVer
	meta.Stage = "episode"

	// The episode argument is the ONLY source of truth for the episode: it
	// names the file AND fills the field. Letting a caller set meta.Episode
	// independently gives one value two authors, and they diverge silently —
	// records landing in ep-a.jsonl while claiming to belong to ep-b, which
	// then reads as one session having no records and another having double.
	meta.Episode = episode
	meta.Argv = body.argv
	meta.Cwd = body.cwd
	meta.Template = body.template
	meta.Truncated = body.truncated
	meta.Redaction = Stamp{Scrubber: "redact/1", N: body.n}

	// Seq is stamped at creation, never at flush. A reader that sees 1..500
	// with 412 present knows exactly 88 records died with the process; without
	// that, loss is indistinguishable from "it never ran".
	meta.Seq = w.seq.Add(1)

	if meta.At.IsZero() {
		meta.At = time.Now().UTC()
	}

	line, err := json.Marshal(&meta)
	if err != nil {
		w.dropped.Add(1)
		return err
	}
	line = append(line, '\n')

	f, err := w.fileFor(meta.At, episode)
	if err != nil {
		w.dropped.Add(1)
		return err
	}
	if _, err := f.Write(line); err != nil {
		w.dropped.Add(1)
		return err
	}
	return nil
}

// Dropped reports records this writer failed to persist.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Close releases the current day's handle.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file, w.day = nil, ""
	return err
}

// fileFor returns the handle for this record's day, rolling over at midnight.
//
// Rollover opens a NEW file. It never renames and never truncates, because
// both of those land on a path other processes still hold open: on unix their
// writes disappear into an unlinked inode, and on Windows the operation fails
// outright. Either way nothing reports it — which is why retention is a
// directory delete of a past day and nothing else.
func (w *Writer) fileFor(at time.Time, episode string) (*os.File, error) {
	day := at.UTC().Format("2006-01-02")

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file != nil && w.day == day {
		return w.file, nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}

	dir := filepath.Join(w.root, day)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, safeName(episode)+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return nil, err
	}
	w.file, w.day = f, day
	return f, nil
}

// safeName keeps an episode id usable as a filename on every platform.
func safeName(s string) string {
	if s == "" {
		return "anon"
	}
	b := []byte(s)
	for i := range b {
		c := b[i]
		ok := c == '-' || c == '_' || c == '.' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			b[i] = '_'
		}
	}
	if len(b) > 64 {
		b = b[:64]
	}
	return string(b)
}
