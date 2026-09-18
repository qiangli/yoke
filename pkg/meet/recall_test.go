// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package meet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

// THE WHOLE FEATURE IS ONE BOUNDARY, so these tests sit on both sides of it.
//
// Before the human's record is appended, a recall means the message never
// happened and the transcript is the proof. After, the text is already the
// addressee's mail — possibly inside a running agent — so the only truthful
// move is to append a withdrawal beside it. A test that only checked the
// verdict string would pass against an implementation that reported "canceled"
// while leaving the message in the room, which is the exact lie this design
// exists to prevent; so each one asserts the verdict AND the transcript.

// blockingRunner stands in for an agent CLI that has started and is thinking.
// It returns only when its context ends, which is what a recall does to it.
type blockingRunner struct{ started chan struct{} }

func (b blockingRunner) Run(ctx context.Context, _ string, _ []string, _ string) (string, int, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", 130, ctx.Err()
}

func humanEventsIn(t *testing.T, id string) []Event {
	t.Helper()
	events, err := readTranscript(id)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	var out []Event
	for _, e := range events {
		if e.Kind == "human" {
			out = append(out, e)
		}
	}
	return out
}

func retractionsIn(t *testing.T, id string) []Event {
	t.Helper()
	events, err := readTranscript(id)
	if err != nil {
		t.Fatalf("readTranscript: %v", err)
	}
	var out []Event
	for _, e := range events {
		if e.Kind == "retraction" {
			out = append(out, e)
		}
	}
	return out
}

// IN TIME: recalled before the record was written, so nothing was sent.
//
// The cancel arrives while the run is still preparing — the run then refuses to
// write at the boundary. The verdict may say "not sent" only because the
// transcript has no human record in it at the end.
func TestRecallBeforeTheRecordIsWrittenSendsNothing(t *testing.T) {
	st := newRoom(t)
	seatEverything(t)

	ctx, cancel := context.WithCancel(context.Background())
	job := registerJob("address-test-1", st.ID, cancel)

	// Cancelled before the run reaches the boundary: exactly the state a recall
	// leaves behind when it wins the race.
	result, err := job.recall(st, "qiangli")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result.Verdict != RecallCanceled {
		t.Fatalf("verdict = %q, want %q", result.Verdict, RecallCanceled)
	}

	if _, err := addressJob(ctx, st.ID, "codex", "please do the thing", job); err == nil {
		t.Fatal("a recalled run must report that it did not run")
	}
	if got := humanEventsIn(t, st.ID); len(got) != 0 {
		t.Fatalf("a canceled send left %d message(s) in the room: %+v", len(got), got)
	}
	if got := retractionsIn(t, st.ID); len(got) != 0 {
		t.Errorf("nothing was sent, so nothing may be retracted; got %d", len(got))
	}
}

// TOO LATE: the agent is already reading it, so the message is withdrawn in the
// room rather than pretended away.
//
// This is the irreversible case — the text is the agent's directed mail and is
// inside a live process — and it is the only case that may append a retraction.
func TestRecallAfterTheAgentHasItAppendsARetraction(t *testing.T) {
	st := newRoom(t)
	seatEverything(t)

	started := make(chan struct{}, 1)
	old := apiRunner
	apiRunner = func() chat.Runner { return blockingRunner{started: started} }
	t.Cleanup(func() { apiRunner = old })

	ctx, cancel := context.WithCancel(context.Background())
	job := registerJob("address-test-2", st.ID, cancel)
	done := make(chan error, 1)
	go func() {
		_, err := addressJob(ctx, st.ID, "codex", "cancel this one", job)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("the turn never started; nothing to recall")
	}

	result, err := job.recall(st, "qiangli")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result.Verdict != RecallRetracted {
		t.Fatalf("verdict = %q, want %q — the agent already had the text", result.Verdict, RecallRetracted)
	}
	<-done

	sent := humanEventsIn(t, st.ID)
	if len(sent) != 1 {
		t.Fatalf("want the original message still in the room, got %d", len(sent))
	}
	retractions := retractionsIn(t, st.ID)
	if len(retractions) != 1 {
		t.Fatalf("want one retraction, got %d", len(retractions))
	}

	// The withdrawal must be findable FROM the message it withdraws, or a
	// reader (and an agent) sees two unrelated records.
	stamp := sent[0].TS.Format(time.RFC3339Nano)
	if retractions[0].Retracts != stamp {
		t.Errorf("retraction points at %q, want the original's %q", retractions[0].Retracts, stamp)
	}
	// And it must reach the same inbox: an agent holding a withdrawn request
	// with no way to learn it was withdrawn is worse off than before.
	if retractions[0].To != sent[0].To {
		t.Errorf("retraction addressed to %q, the message to %q", retractions[0].To, sent[0].To)
	}
	if !strings.Contains(retractions[0].Text, "cancel this one") {
		t.Errorf("retraction does not say what it withdraws: %q", retractions[0].Text)
	}
	if retractions[0].Speaker != "qiangli" {
		t.Errorf("retraction signed by %q, want the sender", retractions[0].Speaker)
	}
}

// A recalled run does NOT also write a failure note.
//
// The job wrapper records "did not run" for a failed job, which is right for a
// crash and wrong here: the recall already said what happened, and a room that
// reports both reads as two separate things going wrong.
func TestARecalledRunIsNotAlsoReportedAsAFailure(t *testing.T) {
	st := newRoom(t)
	seatEverything(t)

	started := make(chan struct{}, 1)
	old := apiRunner
	apiRunner = func() chat.Runner { return blockingRunner{started: started} }
	t.Cleanup(func() { apiRunner = old })

	ref, err := startJob(context.Background(), st.ID, "address",
		func(ctx context.Context, j *liveJob) error {
			_, err := addressJob(ctx, st.ID, "codex", "note-free please", j)
			return err
		})
	if err != nil {
		t.Fatalf("startJob: %v", err)
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn never started")
	}

	if _, err := Recall(st, ref.ID, "", "qiangli"); err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// The note, if it comes, is written after the run returns.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := lookupJob(ref.ID); ok {
			j, _ := lookupJob(ref.ID)
			j.mu.Lock()
			finished := j.finished
			j.mu.Unlock()
			if finished {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	events, err := readTranscript(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "note" && strings.Contains(e.Text, "did not run") {
			t.Errorf("a recalled job also reported itself as a failure: %q", e.Text)
		}
	}
}

// A recall naming a record rather than a job — the shape a chat and a plain
// room post use, because they append inside the request and have no job id.
func TestRecallByTimestampRetractsThatRecord(t *testing.T) {
	st := newRoom(t)

	ev, err := Post(st.ID, "qiangli", "said too much")
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	stamp := ev.TS.Format(time.RFC3339Nano)

	result, err := Recall(st, "", stamp, "qiangli")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if result.Verdict != RecallRetracted {
		t.Fatalf("verdict = %q, want %q", result.Verdict, RecallRetracted)
	}
	if result.Retracted != stamp {
		t.Errorf("retracted %q, want %q", result.Retracted, stamp)
	}
	if got := retractionsIn(t, st.ID); len(got) != 1 || got[0].Retracts != stamp {
		t.Fatalf("want one retraction pointing at %q, got %+v", stamp, got)
	}

	// Recalling the same record twice is not a second withdrawal. It is also
	// not an error — the sender's intent is already satisfied.
	again, err := Recall(st, "", result.Event.TS.Format(time.RFC3339Nano), "qiangli")
	if err != nil {
		t.Fatalf("Recall of a retraction: %v", err)
	}
	if again.Verdict != RecallGone {
		t.Errorf("retracting a retraction = %q, want %q", again.Verdict, RecallGone)
	}
}

// A handle that names nothing gets "gone", never "canceled".
//
// The distinction is the point of the third verdict: a browser that showed
// "canceled — not sent" here would tell the sender their message never went out,
// when in fact the system simply no longer knows.
func TestRecallOfAnUnknownHandleIsGoneNotCanceled(t *testing.T) {
	st := newRoom(t)

	result, err := Recall(st, "address-nobody", "", "qiangli")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if result.Verdict != RecallGone {
		t.Errorf("verdict = %q, want %q", result.Verdict, RecallGone)
	}
	if got := retractionsIn(t, st.ID); len(got) != 0 {
		t.Errorf("an unknown handle retracted %d record(s)", len(got))
	}
}
