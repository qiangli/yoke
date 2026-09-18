package foreman

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TransitionSchemaVersion is the wire contract of one journaled state change.
// Additive changes only: a supervisor's cursor must survive a bashy upgrade.
const TransitionSchemaVersion = "bashy-foreman-transition-v1"

// Transition is one sequenced change of a session's canonical state.
//
// It is a DELTA record over state.json, not a second truth: every field here is
// copied from the state that was committed, and the journal can be rebuilt (or
// its missing head synthesized — see Changes) from state.json alone. What it
// adds is ORDER: `idle → working → blocked` is three records, and a reader that
// polled only state.json would have seen at most the last one.
type Transition struct {
	SchemaVersion  string    `json:"schema_version"`
	ID             string    `json:"id"`
	Seq            int64     `json:"seq"`
	At             time.Time `json:"at"`
	Digest         string    `json:"digest"`
	PreviousStatus string    `json:"previous_status,omitempty"`
	Status         string    `json:"status"`
	CurrentStep    string    `json:"current_step,omitempty"`
	Blocker        string    `json:"blocker,omitempty"`
	Steering       bool      `json:"steering"`
	Stopped        bool      `json:"stopped,omitempty"`
	StopReason     string    `json:"stop_reason,omitempty"`
}

// CanonicalDigest is the digest of a state's CONTENT: everything the operator
// or a supervisor could act on, and nothing that merely says when it was
// written. Two states with equal digests are the same state; persisting one
// over the other is a no-op by contract (Store.Commit), which is what lets an
// unchanged healthy poll produce no payload at all.
func CanonicalDigest(st State) string {
	st.UpdatedAt = time.Time{}
	st.Seq = 0
	st.Digest = ""
	data, err := json.Marshal(st)
	if err != nil {
		// A State is plain data; Marshal cannot fail. Keep the signature honest
		// rather than panicking a daemon over an impossibility.
		data = fmt.Appendf(nil, "%#v", st)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func transitionOf(st State) Transition {
	return Transition{
		SchemaVersion: TransitionSchemaVersion,
		ID:            st.ID,
		Seq:           st.Seq,
		At:            st.UpdatedAt,
		Digest:        st.Digest,
		Status:        st.Status,
		CurrentStep:   st.CurrentStep,
		Blocker:       st.Blocker,
		Steering:      st.Steering,
		Stopped:       st.Stopped,
		StopReason:    st.StopReason,
	}
}

// TransitionsPath is the append-only journal of committed state changes.
func (s Store) TransitionsPath() string {
	return filepath.Join(s.Dir(), "transitions")
}

func (s Store) appendTransition(tr Transition) error {
	// A crash can leave the final append without its terminating newline. Remove
	// only that incomplete tail before appending again; otherwise every reader
	// stops at the torn record and all later valid transitions become invisible.
	if err := s.trimTornTransitionTail(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.TransitionsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tr)
}

func (s Store) trimTornTransitionTail() error {
	path := s.TransitionsPath()
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	const tail = int64(1024 * 1024)
	off := info.Size() - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return err
	}
	cut := bytes.LastIndexByte(buf, '\n')
	if cut < 0 {
		return f.Truncate(0)
	}
	return f.Truncate(off + int64(cut) + 1)
}

func (s Store) loadTransitions() ([]Transition, error) {
	f, err := os.Open(s.TransitionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Transition
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var tr Transition
		if err := json.Unmarshal(line, &tr); err != nil {
			// A torn tail (crash mid-append) must not make the whole journal
			// unreadable; state.json is the truth and Changes re-synthesizes the
			// head from it. Stop at the first unreadable line.
			break
		}
		out = append(out, tr)
	}
	return out, sc.Err()
}

// lastTransitionSeq reads only the journal's tail: Commit runs on every state
// change, and a long session's journal must not make each change cost a full
// parse of every change before it. A torn or unreadable tail falls back to the
// full scan, which stops at the first bad line like any reader.
func (s Store) lastTransitionSeq() (int64, error) {
	f, err := os.Open(s.TransitionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	const tail = 8 * 1024
	size := info.Size()
	off := size - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && size-off > 0 {
		return 0, err
	}
	// The last newline-terminated line in the window is the last complete
	// record — unless the window holds no complete line at all.
	if end := bytes.LastIndexByte(buf, '\n'); end >= 0 {
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		if off == 0 || start > 0 {
			var tr Transition
			if json.Unmarshal(bytes.TrimSpace(buf[start:end]), &tr) == nil {
				return tr.Seq, nil
			}
		}
	}
	trs, err := s.loadTransitions()
	if err != nil || len(trs) == 0 {
		return 0, err
	}
	return trs[len(trs)-1].Seq, nil
}

// Changes returns every transition with seq > after, oldest first.
//
// This is the READ half of the state-change contract. state.json is the
// truth; the journal is its ordered delta view. If the truth is ahead of the
// journal — a crash between the two writes, a session written by an older
// bashy with no journal at all — the missing head record is synthesized from
// state.json so the caller still sees the current state exactly once. Nothing
// is written: a reader never repairs the store.
//
// A cursor of 0 returns the whole sequence; a cursor equal to the current seq
// returns nothing. An unknown session is an error, not an empty answer, so a
// supervisor waiting on a typo does not wait forever.
func (s Store) Changes(after int64) ([]Transition, error) {
	trs, err := s.loadTransitions()
	if err != nil {
		return nil, err
	}
	st, serr := s.LoadState()
	if serr != nil {
		if len(trs) == 0 {
			return nil, serr
		}
	} else {
		var tailSeq int64
		tailStatus := ""
		if n := len(trs); n > 0 {
			tailSeq = trs[n-1].Seq
			tailStatus = trs[n-1].Status
		}
		seq := st.Seq
		if seq == 0 {
			seq = 1 // legacy record, pre-contract; Commit continues from 2
		}
		if seq > tailSeq {
			st.Seq = seq
			if st.Digest == "" {
				st.Digest = CanonicalDigest(st)
			}
			head := transitionOf(st)
			head.PreviousStatus = tailStatus
			trs = append(trs, head)
		}
	}
	var out []Transition
	for _, tr := range trs {
		if tr.Seq > after {
			out = append(out, tr)
		}
	}
	return out, nil
}

// DefaultWaitInterval is the stat-poll backstop for WaitChanges. File
// notification would be an optimization; the stat/rescan loop is the
// correctness floor and is all a session directory on any filesystem needs.
const DefaultWaitInterval = 100 * time.Millisecond

// WaitChanges is the WAIT half of the contract: it returns as soon as there is
// at least one transition after the cursor, or nil when `bound` elapses with
// no change. A timeout is an empty successful read — the same convention as
// `inbox --wait` — so a supervisor in agent mode gets no payload for a healthy,
// unchanged session. Context cancellation is returned as its error.
//
// It re-reads the store only when the journal's or state.json's size/mtime
// moved, so an idle session costs two stats per interval and no parsing.
func (s Store) WaitChanges(ctx context.Context, after int64, bound time.Duration) ([]Transition, error) {
	trs, err := s.Changes(after)
	if err != nil || len(trs) > 0 || bound <= 0 {
		return trs, err
	}
	deadline := time.Now().Add(bound)
	last := s.stamp()
	ticker := time.NewTicker(DefaultWaitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		if cur := s.stamp(); cur != last {
			last = cur
			trs, err := s.Changes(after)
			if err != nil || len(trs) > 0 {
				return trs, err
			}
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
	}
}

// stamp is a cheap change indicator over the two files a change touches.
func (s Store) stamp() string {
	var b strings.Builder
	for _, p := range []string{s.StatePath(), s.TransitionsPath()} {
		info, err := os.Stat(p)
		if err != nil {
			b.WriteString("-;")
			continue
		}
		fmt.Fprintf(&b, "%d:%d;", info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}
