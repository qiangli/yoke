// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package audit is the tamper-evident record of what a shell actually
// executed. It exists because the one question a security team cannot answer
// today about an agent-driven machine — "what did the agents run here, and
// prove it" — has no defensible artifact: auditd is root-only and cannot see a
// shell's own dispatch, and every agent CLI's self-reported log is written by
// the thing being audited. bashy is the execution point, so it is the one place
// the answer exists.
//
// Each executed command becomes one Record. Records form a hash chain
// (Record.Hash = H(prev.Hash ‖ this-record-without-its-hash)), so deleting or
// altering any record breaks every record after it and Verify catches it. The
// record shape maps to NIST SP 800-53 AU-3 (who / what / when / where /
// outcome) and serializes close to the OCSF process_activity class, so it drops
// into a SIEM with little mapping.
//
// What this is NOT: it is a governance and evidence record, not a containment
// boundary. It records that a command ran; it does not stop it (that is the
// policy engine's job) and it cannot see across an execve into the process the
// command spawns (that is the OS sandbox's job). It also is not a total
// chokepoint: any binary can spawn children without asking bashy. It is the
// complete, un-bypassable record of the agentic + interactive command path,
// which is the new, unmonitored surface — and it composes with auditd/EDR for
// the rest.
//
// stdlib-only. The caller supplies the classification (Effects) and identity
// (Actor); this package owns the record shape, the chain, and durable append.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

// SchemaVersion identifies the record shape for consumers. Bump on a
// breaking field change.
const SchemaVersion = "bashy-audit-v1"

// genesis is the chain's public root: the PrevHash of the very first record in
// a fresh log. A fixed, known value so a verifier needs no side channel to
// check the head.
const genesis = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// Actor is the accountable identity behind a command: the human the machine
// belongs to, the agent tool and model acting on their behalf, and the session
// that ties a run together. The agentic novelty over auditd is exactly this —
// attribution to a tool+model, not just a uid.
type Actor struct {
	Human   string `json:"human,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Model   string `json:"model,omitempty"`
	Session string `json:"session,omitempty"`
	UID     int    `json:"uid"`
	PID     int    `json:"pid,omitempty"`
}

// Record is one executed command. Field order is the canonical serialization
// order for the hash, so DO NOT reorder without bumping SchemaVersion.
type Record struct {
	Schema     string   `json:"schema"`
	Seq        uint64   `json:"seq"`
	PrevHash   string   `json:"prev_hash"`
	Time       string   `json:"time"` // RFC3339Nano UTC
	Actor      Actor    `json:"actor"`
	Action     string   `json:"action"` // "exec"
	Argv       []string `json:"argv"`   // post-expansion, redacted
	Binary     string   `json:"binary,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	Effects    []string `json:"effects,omitempty"` // from the Command Atlas
	Host       string   `json:"host,omitempty"`
	Decision   string   `json:"decision,omitempty"` // allow | ask | deny (allow-only until enforcement ships)
	Exit       int      `json:"exit"`
	DurationMs int64    `json:"duration_ms,omitempty"`
	Redactions int      `json:"redactions,omitempty"` // # of secret values masked in argv
	// Hash is H(prev_hash ‖ canonical(record with Hash="")). Last field so the
	// canonical form is simply the record with this one zeroed.
	Hash string `json:"hash"`
}

// computeHash returns the chain hash for r given the previous record's hash.
// It hashes the record with its own Hash field cleared, prefixed by prevHash,
// so each link binds to the full content of the one before it.
func (r Record) computeHash(prevHash string) string {
	r.Hash = ""
	body, _ := json.Marshal(r)
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write([]byte{0}) // separator so prev‖body is unambiguous
	h.Write(body)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Writer appends records to a JSONL log as an unbroken hash chain. It is safe
// for concurrent processes writing the same file: every append takes an
// exclusive file lock adjacent to the log, re-reads the current head under
// that lock, links to it, and writes — so two bashy processes sharing one
// agent's log cannot fork the chain or interleave a torn line. Keeping the
// lock adjacent lets it carry Holder metadata without overwriting the JSONL.
type Writer struct {
	path string
}

// Open returns a Writer for the log at path, creating the file (0600) and its
// directory (0700) if absent. The audit log records command lines and argument
// values, so it must not be readable or writable by other users (NIST AU-9).
func Open(path string) (*Writer, error) {
	if path == "" {
		return nil, fmt.Errorf("audit: empty log path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: log dir: %w", err)
	}
	// Create with restrictive mode; a pre-existing file's mode is tightened
	// best-effort so an older 0644 log does not stay world-readable.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	_ = f.Close()
	_ = os.Chmod(path, 0o600)
	return &Writer{path: path}, nil
}

// Append links r to the current chain head and writes it, returning the stored
// record (with Seq, PrevHash, and Hash filled in). Time/Actor/Argv/etc. must
// already be set by the caller.
func (w *Writer) Append(r Record) (Record, error) {
	f, err := os.OpenFile(w.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	holder := r.Actor.Agent
	if holder == "" {
		holder = "audit"
	}
	l, err := lockfile.Acquire(w.path+".lock", lockfile.Holder{
		Name: holder, PID: r.Actor.PID, Intent: "append audit log", Since: time.Now(),
	})
	if err != nil {
		return Record{}, fmt.Errorf("audit: lock: %w", err)
	}
	defer l.Release()

	prevHash, prevSeq, err := lastHead(f)
	if err != nil {
		return Record{}, err
	}
	r.Schema = SchemaVersion
	r.Seq = prevSeq + 1
	r.PrevHash = prevHash
	if r.Action == "" {
		r.Action = "exec"
	}
	sort.Strings(r.Effects)
	r.Hash = r.computeHash(prevHash)

	line, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Record{}, err
	}
	return r, nil
}

// lastHead returns the hash and seq of the last record in the open file, or the
// genesis hash and seq 0 for an empty log. It reads only the file's tail, so
// cost does not grow with the log.
func lastHead(f *os.File) (hash string, seq uint64, err error) {
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := fi.Size()
	if size == 0 {
		return genesis, 0, nil
	}
	const tail = 64 << 10
	start := max(size-tail, 0)
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", 0, err
	}
	// The last complete line is the head; trailing newline(s) are trimmed.
	text := strings.TrimRight(string(buf), "\n")
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	var last Record
	if err := json.Unmarshal([]byte(text), &last); err != nil {
		return "", 0, fmt.Errorf("audit: unreadable log head (corrupt tail?): %w", err)
	}
	return last.Hash, last.Seq, nil
}

// VerifyResult is the outcome of walking a chain.
type VerifyResult struct {
	Records int    // records read
	OK      bool   // chain intact
	BadSeq  uint64 // seq of the first broken record (0 if OK)
	Reason  string // why it broke
}

// Verify walks the JSONL chain in r and confirms every link: the first record
// chains to genesis, each subsequent record's PrevHash equals the prior
// record's Hash, seq increments by one, and each stored Hash matches a
// recomputation of its own content. The first failure is reported — a deletion,
// a reorder, or a single edited byte all surface here.
func Verify(r io.Reader) VerifyResult {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	prevHash := genesis
	var prevSeq uint64
	var n int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return VerifyResult{Records: n, BadSeq: prevSeq + 1, Reason: "unparsable record: " + err.Error()}
		}
		n++
		if rec.PrevHash != prevHash {
			return VerifyResult{Records: n, BadSeq: rec.Seq, Reason: "prev_hash mismatch (a prior record was deleted or altered)"}
		}
		if rec.Seq != prevSeq+1 {
			return VerifyResult{Records: n, BadSeq: rec.Seq, Reason: "seq gap (expected " + strconv.FormatUint(prevSeq+1, 10) + ")"}
		}
		if want := rec.computeHash(prevHash); want != rec.Hash {
			return VerifyResult{Records: n, BadSeq: rec.Seq, Reason: "hash mismatch (this record was altered)"}
		}
		prevHash, prevSeq = rec.Hash, rec.Seq
	}
	if err := sc.Err(); err != nil {
		return VerifyResult{Records: n, Reason: "read error: " + err.Error()}
	}
	return VerifyResult{Records: n, OK: true}
}
