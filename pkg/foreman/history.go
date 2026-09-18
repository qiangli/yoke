package foreman

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// The history ARTIFACT versus the history the agent SEES
//
// Every prompt used to replay the whole session history: every operator
// message, every agent reply, from the first turn, on every turn. Turn N cost
// O(N) bytes, and over a long session the replay dwarfed the instruction it was
// carrying — the agent was paying to re-read a conversation in order to be
// told one new thing.
//
// The record still has to be complete: an operator auditing a run, or an agent
// asked to verify what was decided in turn 12, needs the verbatim turn. So the
// history is split into two representations with two jobs:
//
//   - history.jsonl in the session directory is the ARTIFACT: every entry,
//     verbatim, sequenced, with a chained digest. It is append-only, it is
//     never replayed into a prompt, and it is what a reference in a prompt
//     points at ("entry 81 of history.jsonl").
//   - continuity is the bounded PROJECTION kept in memory: counters, the digest
//     of the whole chain, and previews of the last few turns, the last result
//     and the operator's recent decisions. It is what the checkpoint renders.
//
// The projection is recomputed from the artifact on Open, so a restarted
// session continues with the same continuity and the same references.

// HistorySchemaVersion is the wire contract of one history.jsonl line.
const HistorySchemaVersion = "bashy-foreman-history-v1"

// Roles a history entry can carry.
const (
	RoleHuman   = "human"
	RoleAgent   = "agent"
	RoleMidTurn = "human (mid-turn)" // a steer delivered while the agent was working
)

// HistoryEntry is one verbatim turn in the session artifact.
type HistoryEntry struct {
	SchemaVersion string    `json:"schema_version"`
	Seq           int64     `json:"seq"`
	At            time.Time `json:"at"`
	Role          string    `json:"role"`
	Target        string    `json:"target,omitempty"` // DAG target the entry belongs to
	Text          string    `json:"text"`
	Bytes         int       `json:"bytes"`
	// Digest chains: sha256(previous digest, role, text). The last entry's digest
	// therefore fingerprints the entire history, which is what a checkpoint
	// quotes as the stable reference to "the history as of this prompt".
	Digest string `json:"digest"`
}

// HistoryPath is the complete, verbatim session history — the audit artifact.
func (s Store) HistoryPath() string {
	return filepath.Join(s.Dir(), "history.jsonl")
}

// LoadHistory reads the whole artifact. It exists for audit and explicit
// retrieval and is deliberately NOT used by prompt composition.
func (s Store) LoadHistory() ([]HistoryEntry, error) {
	var out []HistoryEntry
	err := s.scanHistory(func(e HistoryEntry) { out = append(out, e) })
	return out, err
}

func (s Store) scanHistory(fn func(HistoryEntry)) error {
	f, err := os.Open(s.HistoryPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("foreman: read history: %w", err)
		}
		fn(e)
	}
	return sc.Err()
}

func chainDigest(prev, role, text string) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{0})
	h.Write([]byte(role))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// continuity is the bounded in-memory projection of the history artifact. It
// has its own lock: a mid-turn steer is recorded while Apply holds s.mu for the
// running turn, and a projection that needed s.mu would lose exactly the
// corrections that land while the agent is busy — the only time one lands.
type continuity struct {
	mu sync.Mutex

	entries int64
	bytes   int64
	digest  string

	recent         []HistoryEntry // previews of the last RecentWindow entries
	decisions      []HistoryEntry // previews of the last MaxDecisions operator entries
	decisionsTotal int
	lastResult     HistoryEntry            // preview of the last agent entry; Seq 0 => none
	targets        map[string]HistoryEntry // last agent result per DAG target, preview
}

// continuitySnapshot is a copy a renderer can use without holding the lock.
type continuitySnapshot struct {
	entries        int64
	bytes          int64
	digest         string
	recent         []HistoryEntry
	decisions      []HistoryEntry
	decisionsTotal int
	lastResult     HistoryEntry
	targets        map[string]HistoryEntry
}

// record appends one verbatim entry to the artifact and folds it into the
// projection. The artifact write comes first and its failure is returned: a
// session that cannot write its audit record must say so, not go on producing
// prompts whose references point at nothing.
func (c *continuity) record(store Store, role, target, text string) (HistoryEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := HistoryEntry{
		SchemaVersion: HistorySchemaVersion,
		Seq:           c.entries + 1,
		At:            time.Now().UTC(),
		Role:          role,
		Target:        target,
		Text:          text,
		Bytes:         len(text),
		Digest:        chainDigest(c.digest, role, text),
	}
	if err := store.Ensure(); err != nil {
		return e, err
	}
	f, err := os.OpenFile(store.HistoryPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return e, err
	}
	if err := json.NewEncoder(f).Encode(e); err != nil {
		_ = f.Close()
		return e, err
	}
	if err := f.Close(); err != nil {
		return e, err
	}
	c.fold(e)
	return e, nil
}

// fold applies one entry to the projection; the caller holds c.mu.
func (c *continuity) fold(e HistoryEntry) {
	c.entries = e.Seq
	c.bytes += int64(e.Bytes)
	c.digest = e.Digest

	p := e
	p.Text = preview(e.Text, EntryPreviewBytes)
	c.recent = append(c.recent, p)
	if len(c.recent) > RecentWindow {
		c.recent = c.recent[len(c.recent)-RecentWindow:]
	}
	switch e.Role {
	case RoleAgent:
		r := e
		r.Text = preview(e.Text, LastResultBytes)
		c.lastResult = r
		if e.Target != "" {
			if c.targets == nil {
				c.targets = map[string]HistoryEntry{}
			}
			t := e
			t.Text = preview(e.Text, DependencyPreviewBytes)
			c.targets[e.Target] = t
		}
	default:
		d := e
		d.Text = preview(e.Text, DecisionPreviewBytes)
		c.decisions = append(c.decisions, d)
		if len(c.decisions) > MaxDecisions {
			c.decisions = c.decisions[len(c.decisions)-MaxDecisions:]
		}
		c.decisionsTotal++
	}
}

// replay rebuilds the projection from the artifact — what Open does so a
// restarted session continues from the same continuity, with the same
// references, without ever holding the whole history in memory.
func (c *continuity) replay(store Store) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.bytes, c.digest = 0, 0, ""
	c.recent, c.decisions, c.decisionsTotal = nil, nil, 0
	c.lastResult, c.targets = HistoryEntry{}, nil
	return store.scanHistory(func(e HistoryEntry) { c.fold(e) })
}

func (c *continuity) snapshot() continuitySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := continuitySnapshot{
		entries:        c.entries,
		bytes:          c.bytes,
		digest:         c.digest,
		recent:         append([]HistoryEntry(nil), c.recent...),
		decisions:      append([]HistoryEntry(nil), c.decisions...),
		decisionsTotal: c.decisionsTotal,
		lastResult:     c.lastResult,
	}
	if len(c.targets) > 0 {
		snap.targets = make(map[string]HistoryEntry, len(c.targets))
		for k, v := range c.targets {
			snap.targets[k] = v
		}
	}
	return snap
}

// preview bounds s to at most max UTF-8 BYTES, never splitting a rune, and
// marks what was left out so the reader knows a preview is not the record.
// The result, marker included, is <= max bytes; the omitted count is exact.
// The marker itself (under 32 bytes) is the floor: a budget too small to hold
// even the marker yields the marker alone rather than a silent cut.
func preview(s string, max int) string {
	if len(s) <= max {
		return s
	}
	marker := func(omitted int) string { return " …[+" + strconv.Itoa(omitted) + " bytes]" }
	// Reserve room for the marker using the widest count it could carry, then
	// recompute with the real count: digits can only shrink, so the bound holds.
	cut := max - len(marker(len(s)))
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker(len(s)-cut)
}
