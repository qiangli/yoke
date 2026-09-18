// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package meet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Recall is "stop that message", and the whole design is in one sentence: the
// SERVER decides which of the two things happened, because only the server
// knows whether the record was written.
//
// A send passes exactly one boundary that matters — the append of the human's
// own message (recordAsked for a room, appendRelayDMEvent for a chat). Before
// it, nothing exists anywhere: no transcript line, no directed mail, no agent
// process holding the text, so the message can simply not happen. After it, the
// text IS the addressee's mail and may already be inside a running agent, so the
// only honest move is to say so in the room.
//
// WHY THE CLIENT MUST NOT DECIDE. A browser can abort its own request, but an
// abort cancels the RESPONSE, never the effect: the server may have appended a
// microsecond earlier. A UI that reported "canceled" off its own abort would be
// claiming a fact it cannot observe — the same class of lie as a success state
// reached by the absence of evidence. So the client asks, and this file answers
// with what actually happened.
//
// WHY A RETRACTION IS APPENDED RATHER THAN THE RECORD DELETED. The transcript is
// append-only and the minutes are a projection of it; a hole in the log is
// unexplainable to every later reader, and to the agent that may already have
// read the line. The retraction carries the same addressee as the message it
// withdraws, so it lands in the same inbox, and the reader sees the withdrawal
// where they saw the message.

// RecallVerdict is what a recall actually achieved. It is a closed set: a client
// renders one sentence per value and must never infer a third state.
type RecallVerdict string

const (
	// RecallCanceled: the send was stopped before anything was written. Nothing
	// was delivered, nothing is in the transcript, and there is nothing to undo.
	RecallCanceled RecallVerdict = "canceled"
	// RecallRetracted: the message was already recorded, so a retraction was
	// appended beside it. Both records stay visible.
	RecallRetracted RecallVerdict = "retracted"
	// RecallGone: there is nothing left to act on — the job finished and the
	// caller named no record to retract. Distinct from canceled ON PURPOSE:
	// reporting "canceled" here would tell a reader their message never went out
	// when it did.
	RecallGone RecallVerdict = "gone"
)

// RecallResult is the answer a caller renders.
type RecallResult struct {
	Verdict RecallVerdict `json:"verdict"`
	// Event is the retraction that was appended, present only for
	// RecallRetracted so a client can show it without waiting for the socket.
	Event *Event `json:"event,omitempty"`
	// Retracted is the timestamp of the record that was withdrawn, so a client
	// can mark it immediately.
	Retracted string `json:"retracted,omitempty"`
}

// errRecalled is returned by a run that stopped because its caller recalled it.
// It is distinguished from an ordinary failure so the job wrapper does not write
// a "did not run" note into the room: the recall already said what happened,
// and two records for one event read as two events.
var errRecalled = errors.New("meet: recalled by the sender")

// liveJob is one in-flight dispatch, addressable by the id its 202 handed back.
//
// committed is the boundary described at the top of this file. It is set under
// the mutex by the run itself, the instant the human's record is appended, so a
// recall arriving one instruction later sees it.
type liveJob struct {
	id   string
	room string

	cancel context.CancelFunc

	mu        sync.Mutex
	committed *Event
	recalled  bool
	finished  bool
}

var liveJobs sync.Map // job id -> *liveJob

func registerJob(id, room string, cancel context.CancelFunc) *liveJob {
	j := &liveJob{id: id, room: room, cancel: cancel}
	liveJobs.Store(id, j)
	return j
}

// retire keeps a finished job addressable for a short while rather than dropping
// it the instant the work ends.
//
// A recall that arrives just after the turn completes must be able to tell
// "finished" from "never existed": the first is a real answer (the message went
// out — retract it), the second is a bug report. Forgetting immediately would
// collapse both into "gone".
func (j *liveJob) retire() {
	j.mu.Lock()
	j.finished = true
	j.mu.Unlock()
	time.AfterFunc(jobRetention, func() { liveJobs.Delete(j.id) })
}

// jobRetention is how long a finished job stays addressable. Long enough that a
// human who clicked cancel as the turn landed gets a truthful answer; short
// enough that the map is bounded by recent activity rather than uptime.
var jobRetention = 10 * time.Minute

func lookupJob(id string) (*liveJob, bool) {
	v, ok := liveJobs.Load(strings.TrimSpace(id))
	if !ok {
		return nil, false
	}
	j, ok := v.(*liveJob)
	return j, ok
}

// markCommitted records that the human's own message is now in the transcript.
// Called by the run at the boundary, never by a handler.
func (j *liveJob) markCommitted(ev Event) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	copied := ev
	j.committed = &copied
}

// stopped reports whether this run was recalled, so the run can return errRecalled
// rather than a bare context error and the job wrapper can stay quiet.
func (j *liveJob) stopped() bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.recalled
}

// recall stops the job and reports what that achieved.
//
// The order is deliberate: the flag and the committed record are read under the
// same lock that the run writes them under, so the decision is made against one
// consistent view. The cancel itself happens after, because cancelling is what
// makes the run stop touching the room.
func (j *liveJob) recall(st *State, by string) (RecallResult, error) {
	j.mu.Lock()
	j.recalled = true
	committed := j.committed
	j.mu.Unlock()

	if j.cancel != nil {
		j.cancel()
	}
	if committed == nil {
		// Nothing was written. This is the only path that may say "not sent",
		// and it may say it because the transcript is the proof.
		return RecallResult{Verdict: RecallCanceled}, nil
	}
	return retractRecord(st, *committed, by)
}

// retractRecord appends the retraction beside the record it withdraws.
//
// The addressee is copied from the original: a retraction that did not reach the
// inbox the message reached would leave the agent holding a withdrawn request
// and no way to learn it was withdrawn.
func retractRecord(st *State, original Event, by string) (RecallResult, error) {
	who := strings.TrimSpace(by)
	if who == "" {
		who = strings.TrimSpace(st.Human)
	}
	if who == "" {
		// Attribution is the one thing a transcript guarantees. An unattributed
		// retraction is worse than none: it reads as the room withdrawing a
		// human's words.
		return RecallResult{}, errors.New("meet: a retraction needs a sender")
	}
	stamp := original.TS.Format(time.RFC3339Nano)
	ev, err := recordFull(st, Event{
		Round:    st.Round,
		Speaker:  who,
		Role:     string(RoleHuman),
		Kind:     "retraction",
		To:       original.To,
		Text:     retractionText(original),
		Retracts: stamp,
		TS:       nowFn(),
	})
	if err != nil {
		return RecallResult{}, err
	}
	return RecallResult{Verdict: RecallRetracted, Event: &ev, Retracted: stamp}, nil
}

// retractionText is the prose an agent reads.
//
// It quotes the withdrawn message rather than pointing at it, because the reader
// that matters most is an agent whose context may hold the original with no way
// to correlate a bare timestamp — and because a retraction that does not say
// what it retracts is unusable in the minutes.
func retractionText(original Event) string {
	quoted := strings.TrimSpace(original.Text)
	if len(quoted) > 240 {
		quoted = quoted[:240] + "…"
	}
	if quoted == "" {
		return "Retracted by the sender. Disregard the previous message."
	}
	return "Retracted by the sender — disregard the previous message: " + quoted
}

// findEventAt locates a record by its timestamp, which is how a client names one
// it is looking at: events carry no id, and (kind, speaker, ts) is already the
// key the transcript and the browser both dedupe on.
func findEventAt(st *State, stamp string) (Event, bool) {
	stamp = strings.TrimSpace(stamp)
	if stamp == "" {
		return Event{}, false
	}
	events, err := readTranscript(st.ID)
	if err != nil {
		return Event{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].TS.Format(time.RFC3339Nano) == stamp {
			return events[i], true
		}
	}
	return Event{}, false
}

// Recall is the whole verb, for one room, as the HTTP layer and the tests both
// use it. job names an in-flight dispatch; stamp names a record already on the
// transcript. A caller sends whichever it has, and may send both — the job is
// tried first because only it can still produce a clean cancel.
func Recall(st *State, job, stamp, by string) (RecallResult, error) {
	if j, ok := lookupJob(job); ok && j.room == st.ID {
		return j.recall(st, by)
	}
	if original, ok := findEventAt(st, stamp); ok {
		if original.Kind == "retraction" {
			// Retracting a retraction is not a second withdrawal, it is noise.
			return RecallResult{Verdict: RecallGone}, nil
		}
		return retractRecord(st, original, by)
	}
	return RecallResult{Verdict: RecallGone}, nil
}
