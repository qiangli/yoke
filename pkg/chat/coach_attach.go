package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// ATTACH — point a coach at a session that is ALREADY RUNNING.
//
// `coach --agent A -m TASK` LAUNCHES the agent and watches it. There was no way
// to attach to a session already in flight, and that gap has a cost: an agy
// worker running under weave executed a completely unrelated task from stale
// session state, never touched its assigned issue, and reported success — exit
// 0, zero commits. A supervisor could have caught it in the first minute.
// Nothing could be attached, so nobody did.
//
// This is WIRING, not new machinery. The three primitives already existed:
// NewCtlSteerer (steer through a control socket), NewLineCoach (a coach fed any
// line stream) and feedPty/tripPty (pty-scrape loop detection). What was missing
// was the composition: resolving a live room member into a coach.
//
// THE INVARIANT (docs/live-agent-coaching-design.md): a coach is a REPORT
// CHANNEL, NEVER AN AUTHOR. It may press ESC and say a sentence; it may NOT
// write files, commit, or merge. Attach does not widen that authority — it is
// ESC + Say, exactly as a launched coach. --read-only makes even that a
// detect-only report (no intervention at all).
//
// ESC FIRST, THEN SPEAK: every agent TUI queues a Say and reads it only between
// turns, so an agent stuck in a tool loop never reaches the moment it would read
// it. ctlSteerer already interrupts before speaking; the shared intervene()
// preserves that ordering.
//
// DETACH IS CLEAN: attach never owns the coachee. --timeout or Ctrl-C cancels
// the watch; the watcher goroutine exits; the coachee keeps running. Attach
// never kills, never sends an ESC storm on exit.

// attachCoach resolves a live room member and runs a coach over it, returning the
// coach (so a caller can Wait/Report) and starting a watcher goroutine that ends
// when ctx does. The card must be live and steerable; this function refuses a
// card with no control socket or a dead pid.
//
// readOnly makes the coach DETECT-ONLY: it watches and records trips but its
// steerer is a no-op, so nothing is sent to the coachee. A coach is already a
// report channel, never an author; read-only drops even ESC+Say, for an operator
// who wants to observe with zero chance of intervention. The control-socket
// requirement still applies — a member that cannot be steered is not
// "attachable", even for observation.
//
// It runs the SAME trip implementation a launched coach uses: pty mode goes
// through NewLineCoach → Write → feedPty → tripPty → (*Coach).intervene (the
// exact path watchPty drives), and event mode goes through onToolCall → decide →
// (*Coach).intervene. Both modes funnel every steer through (*Coach).intervene —
// that one function is the shared trip delivery for launched and attached coaches
// alike.
//
// detachSafe by construction: the watcher only tails the member's output/events
// and (at most) writes frames to its control socket; cancelling ctx ends the
// watch and leaves the coachee untouched.
//
// It is the readOnly-shaped front door onto attachCoachAs, retained for callers
// that predate roles: readOnly IS RoleObserver (watch, emit nothing) and
// everything else IS RoleSupervisor (watch, say, ESC). Those two roles are not an
// approximation of the old behavior — they are it, which is what makes the role
// generalization a widening rather than a change.
func attachCoach(ctx context.Context, card room.Card, pol CoachPolicy, readOnly bool) (*Coach, error) {
	role := RoleSupervisor
	if readOnly {
		role = RoleObserver
	}
	coach, _, err := attachCoachAs(ctx, card, pol, role)
	return coach, err
}

// attachCoachAs is attachCoach with the participation ROLE made explicit — the
// generalization of the shipped attach. Everything about the watch is identical
// for every role (same resolution, same refusals, same ONE line-aware tail, same
// trip implementation); the role changes only what may reach the coachee, and it
// changes it by choosing the Steerer rather than by asking the coach to behave.
// See attach_roles.go for the capability table and why ESC is the dividing line.
//
// It returns the capped steerer alongside the coach so a caller can report how
// many interventions the role demoted to report-only — the "demote, never drop"
// half of the contract is only useful if someone can see it happened.
//
// Audit: a room note is emitted here on attach and by the watcher on detach,
// carrying actor, role, and target. Refusals below are NOT audited as
// participation — an attach that never happened had no effect to bound.
func attachCoachAs(ctx context.Context, card room.Card, pol CoachPolicy, role AttachRole) (*Coach, *roleSteerer, error) {
	if !role.CanObserve() {
		return nil, nil, fmt.Errorf("attach: unknown role %q — valid roles are %s (least to most power)",
			string(role), strings.Join(AttachRoleNames(), ", "))
	}
	if card.CtlSock == "" {
		return nil, nil, fmt.Errorf("coach attach: %s has no control socket — nothing to steer through (it is not a steerable member)", card.ID)
	}
	if !room.PidAlive(card.PID) {
		return nil, nil, fmt.Errorf("coach attach: %s is not running (pid %d is gone)", card.ID, card.PID)
	}

	// Build the steerer BEFORE starting the watcher, so there is no race between
	// choosing the role's cap and intervene() reading c.steer from the watch
	// goroutine. The cap is the enforcement: an observer is handed the existing
	// no-op steerer and an advisor's Interrupt cannot reach the socket at all.
	steer := newRoleSteerer(role, card.CtlSock)
	if pol.Steer == "" {
		pol.Steer = DefaultCoachPolicy().Steer
	}
	actor := attachActor()

	// Event path: the member speaks a structured event channel (a first-party
	// harness reporting tool.call as data). Tail its events file and feed
	// tool.call through decide — the PRECISE signal, the same one watchEvents
	// drives for a launched coach. The events file is not carried on the card
	// (the card shape is read-only here), so it is reconstructed from the card's
	// binding+pid — the exact key sessionEventsPath used when the member joined.
	// Falls back to pty when the file is not reachable, but only if that fallback
	// itself can be opened: attach must never succeed without a signal source.
	if card.Events {
		if evPath := cardEventsFile(card); evPath != "" {
			tail := &attachLineTail{path: evPath}
			if lines, err := tail.skipToEnd(); err == nil {
				c := newCoach(pol)
				c.steer = steer
				c.agent = card.Binding
				c.mode = "events"
				c.recordPriorToolCalls(eventsFromLines(lines))
				emitAttachAudit(attachAuditAttach, role, card, actor)
				go func() {
					// Emit the detach BEFORE closing done, so a caller that Waits and
					// then reads the timeline always sees the boundary it waited for.
					defer func() {
						emitAttachAudit(attachAuditDetach, role, card, actor)
						close(c.done)
					}()
					watchAttachedEvents(ctx, c, tail, card.PID)
				}()
				return c, steer, nil
			}
		}
	}

	// Pty path (and the events fallback): tail the member's capture and feed
	// NewLineCoach's Write — the GENERIC "output flowing, distinct content not
	// growing" signal that works for every tool (agy and any third-party CLI).
	// This is the same feedPty/tripPty/intervene path the launched pty-mode coach
	// drives through watchPty.
	if card.LogPath == "" {
		return nil, nil, fmt.Errorf("coach attach: %s has no log to tail and no reachable event channel — nothing to watch", card.ID)
	}
	coach := NewLineCoach(pol, steer)
	coach.agent = card.Binding
	tail := &attachLineTail{path: card.LogPath}
	prior, err := tail.skipToEnd()
	if err != nil {
		return nil, nil, fmt.Errorf("coach attach: %s cannot open log %q — nothing to watch: %w", card.ID, card.LogPath, err)
	}
	coach.recordPriorPty(completeLines(prior))
	emitAttachAudit(attachAuditAttach, role, card, actor)
	go func() {
		defer func() {
			emitAttachAudit(attachAuditDetach, role, card, actor)
			close(coach.done)
		}()
		watchAttachedLog(ctx, coach, card, tail)
	}()
	return coach, steer, nil
}

// watchAttachedLog tails card.LogPath and pumps every new byte into the coach's
// Write (which normalizes lines, runs the signal panel, and intervenes through
// the shared (*Coach).intervene). It ends when the coachee's pid is gone or ctx
// is cancelled — and in both cases it leaves the coachee alone (no kill, no ESC).
func watchAttachedLog(ctx context.Context, coach *Coach, card room.Card, tail *attachLineTail) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		// A path-based tail can follow truncation and rotation. Errors are
		// transient while a writer replaces a file, so keep polling; an attach
		// whose source is absent from the start is rejected synchronously.
		if lines, err := tail.next(); err == nil {
			for _, line := range lines {
				_, _ = coach.Write(append(append([]byte{}, line...), '\n'))
			}
		}
		if !room.PidAlive(card.PID) {
			return // coachee ended on its own — do not disturb it
		}
		select {
		case <-ctx.Done():
			return // detach: leave the coachee running
		case <-tick.C:
		}
	}
}

// watchAttachedEvents tails a reconstructed NDJSON events file from the point
// of attachment and feeds each new tool.call through onToolCall → decide →
// intervene — the precise event path, identical to a launched coach's
// watchEvents. Calls already present remain reportable session history, but
// never seed the live detector or cause an intervention.
func watchAttachedEvents(ctx context.Context, coach *Coach, tail *attachLineTail, pid int) {
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		if lines, err := tail.next(); err == nil {
			for _, ev := range eventsFromLines(lines) {
				if ev.Type == EventToolCall {
					coach.onToolCall(ev)
				}
			}
		}
		if !room.PidAlive(pid) {
			return // coachee ended on its own — do not disturb it
		}
		select {
		case <-ctx.Done():
			return // detach: leave the coachee running
		case <-tick.C:
		}
	}
}

// attachLineTail mirrors pkg/meet's lineTail discipline for both attach signal
// paths. It reads only complete lines, carries a trailing fragment in rem until
// a later poll completes it, and follows a path across truncation or rotation.
type attachLineTail struct {
	path string
	off  int64
	rem  []byte
	file os.FileInfo
	end  []byte
}

// skipToEnd snapshots the attachment boundary in one open/read. Complete lines
// are returned as report-only history; a trailing incomplete line is retained
// in rem so its future continuation is delivered once, whole.
func (t *attachLineTail) skipToEnd() ([][]byte, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	t.file = fi
	t.off = int64(len(data))
	t.rememberEnd(data)
	return t.split(data), nil
}

// next returns complete lines appended since the previous call. A replacement
// inode or a shorter same-inode file establishes a fresh stream boundary:
// an unfinished record from the old generation cannot be joined to the new one.
func (t *attachLineTail) next() ([][]byte, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// Three ways the stream can break, and they do NOT get the same treatment.
	// The distinction is whether the bytes now in the file are ones this tail has
	// already delivered.
	switch {
	case fi.Size() < t.off && t.file != nil && os.SameFile(t.file, fi):
		// SAME INODE, SMALLER: a truncation. What remains is a PREFIX of what we
		// already consumed and delivered. Re-anchor at the new end and deliver
		// NOTHING — replaying it would feed the detector output the coachee
		// produced earlier and can trip a steer on already-finished work. A
		// truncation is less data, never new data. (Reporting it as prior history
		// would be a second lie: the coach did observe it, once, already.)
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		t.file = fi
		t.off = int64(len(data))
		t.rem = nil
		t.end = nil
		t.rememberEnd(data)
		return nil, nil
	case t.file == nil || !os.SameFile(t.file, fi) || !t.sameEnd(f):
		// A REPLACEMENT: either a new inode (rotation), or the same inode whose
		// content no longer matches our anchor (rewritten in place). These bytes
		// are content we have never delivered, so the new generation is read from
		// byte zero and delivered normally. A stale fragment from the old
		// generation must not be joined to it.
		t.file = fi
		t.off = 0
		t.rem = nil
		t.end = nil
	}
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	t.off += int64(len(chunk))
	t.rememberEnd(chunk)
	return t.split(chunk), nil
}

func (t *attachLineTail) sameEnd(f *os.File) bool {
	if len(t.end) == 0 {
		return true
	}
	got := make([]byte, len(t.end))
	n, err := f.ReadAt(got, t.off-int64(len(t.end)))
	return err == nil && n == len(got) && bytes.Equal(got, t.end)
}

func (t *attachLineTail) rememberEnd(chunk []byte) {
	const anchor = 64
	data := append(append([]byte{}, t.end...), chunk...)
	if len(data) > anchor {
		data = data[len(data)-anchor:]
	}
	t.end = data
}

func (t *attachLineTail) split(chunk []byte) [][]byte {
	data := append(t.rem, chunk...)
	cut := bytes.LastIndexByte(data, '\n')
	if cut < 0 {
		t.rem = append([]byte{}, data...)
		return nil
	}
	t.rem = append([]byte{}, data[cut+1:]...)
	var lines [][]byte
	for _, line := range bytes.Split(data[:cut], []byte{'\n'}) {
		lines = append(lines, append([]byte{}, line...))
	}
	return lines
}

func eventsFromLines(lines [][]byte) []Event {
	var events []Event
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err == nil {
			events = append(events, ev)
		}
	}
	return events
}

func completeLines(lines [][]byte) []byte {
	if len(lines) == 0 {
		return nil
	}
	return append(bytes.Join(lines, []byte{'\n'}), '\n')
}

// cardEventsFile is where a member streams its structured events.
//
// A card ADVERTISES this now (EventsPath), so the common path is simply to read
// what the member published. The fallback below reconstructs the pre-EventsPath
// key — binding+pid — for a card written by an older binary, which is exactly
// the behaviour such a card had when it was written. Returns "" when neither is
// available. Best-effort: a missing file makes the caller fall back to the pty
// path.
func cardEventsFile(card room.Card) string {
	if p := strings.TrimSpace(card.EventsPath); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".bashy", "events")
	return filepath.Join(dir, shortHash(card.Binding+"\x00"+strconv.Itoa(card.PID))+".ndjson")
}
