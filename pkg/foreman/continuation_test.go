package foreman

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// sizeRunner answers every turn with a fixed reply and remembers the size of
// each prompt it was handed — the only fact the ceiling tests need.
type sizeRunner struct {
	mu    sync.Mutex
	reply string
	sizes []int
	last  string
}

func (r *sizeRunner) Run(_ context.Context, _ string, args []string, _ string) (string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) > 0 {
		r.sizes = append(r.sizes, len(args[len(args)-1]))
		r.last = args[len(args)-1]
	}
	return r.reply, 0, nil
}

func startReplaySession(t *testing.T, id string, r *sizeRunner) *Session {
	t.Helper()
	s, err := Start(context.Background(), Options{ID: id, Goal: "ship the bounded foreman", Agent: "stub", Root: t.TempDir(), Runner: r})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

func tell(t *testing.T, s *Session, msg string) {
	t.Helper()
	if err := s.Apply(context.Background(), Command{Verb: CommandTell, Message: msg}); err != nil {
		t.Fatalf("tell %q: %v", msg, err)
	}
}

// THE CEILING. A 250-turn session must hand its agent a prompt the same size
// as a 120-turn session did: the checkpoint + recent window replaced the
// whole-history replay, so turn N costs O(1) bytes, not O(N).
func TestPromptReachesSteadyCeilingOverLongHistory(t *testing.T) {
	r := &sizeRunner{reply: strings.Repeat("r", 1000)}
	s := startReplaySession(t, "long", r)
	const turns = 250
	for i := 1; i <= turns; i++ {
		tell(t, s, fmt.Sprintf("turn %03d", i))
	}
	if len(r.sizes) != turns {
		t.Fatalf("runner saw %d prompts, want %d", len(r.sizes), turns)
	}
	// Same-width counters on both sides (3-digit seqs and entry counts,
	// 6-digit byte totals), so the sizes are comparable byte for byte rather
	// than merely "close".
	if r.sizes[119] != r.sizes[249] {
		t.Fatalf("prompt size grew with history: turn 120 = %d bytes, turn 250 = %d bytes", r.sizes[119], r.sizes[249])
	}
	frame := len(s.State().Goal) + len(s.kbPreamble()) + len("turn 000") + 64
	for i, n := range r.sizes {
		if n > ContinuationBudget+frame {
			t.Fatalf("turn %d prompt is %d bytes, above the %d-byte ceiling", i+1, n, ContinuationBudget+frame)
		}
	}
	if strings.Contains(r.last, "Session history:") {
		t.Fatal("the prompt still replays the session history")
	}
	if strings.Contains(r.last, "turn 001") {
		t.Fatal("the first turn leaked into the 250th prompt — that is a replay, not a window")
	}
	// The record is still complete: every turn, verbatim, in the artifact.
	entries, err := s.Store().LoadHistory()
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(entries) != 2*turns {
		t.Fatalf("artifact holds %d entries, want %d", len(entries), 2*turns)
	}
	if entries[0].Text != "turn 001" || entries[0].Role != RoleHuman || entries[1].Role != RoleAgent {
		t.Fatalf("artifact head = %+v / %+v", entries[0], entries[1])
	}
	for i, e := range entries {
		if e.Seq != int64(i+1) {
			t.Fatalf("entry %d has seq %d", i, e.Seq)
		}
	}
	// In-memory continuity is bounded too: a long session must not grow the
	// daemon's memory the way the old []string did.
	snap := s.hist.snapshot()
	if len(snap.recent) != RecentWindow || len(snap.decisions) != MaxDecisions {
		t.Fatalf("projection kept %d recent / %d decisions, want %d / %d", len(snap.recent), len(snap.decisions), RecentWindow, MaxDecisions)
	}
}

// The checkpoint must carry the facts the plan names — goal, accepted
// decisions, current step, blockers, last result, stable references — and
// each fact must be verifiable against the artifact it references.
func TestCheckpointCarriesContinuityFacts(t *testing.T) {
	r := &sizeRunner{reply: "first reply"}
	s := startReplaySession(t, "facts", r)
	for i := 1; i <= 7; i++ {
		tell(t, s, fmt.Sprintf("decision %d", i))
	}
	r.mu.Lock()
	r.reply = "the final answer"
	r.mu.Unlock()
	tell(t, s, "decision 8")
	if err := s.Apply(context.Background(), Command{Verb: CommandPause}); err != nil {
		t.Fatal(err)
	}

	cp := s.Checkpoint()
	if cp.SchemaVersion != CheckpointSchemaVersion || cp.Goal != "ship the bounded foreman" {
		t.Fatalf("checkpoint header = %+v", cp)
	}
	if cp.CurrentStep != "decision 8" {
		t.Errorf("current step = %q, want the last instruction", cp.CurrentStep)
	}
	if cp.DecisionsTotal != 8 || len(cp.Decisions) != MaxDecisions {
		t.Fatalf("decisions = %d of %d, want %d of 8", len(cp.Decisions), cp.DecisionsTotal, MaxDecisions)
	}
	if cp.Decisions[0].Text != "decision 4" || cp.Decisions[4].Text != "decision 8" {
		t.Errorf("decisions window = %q .. %q, want decision 4 .. decision 8", cp.Decisions[0].Text, cp.Decisions[4].Text)
	}
	if len(cp.Blockers) != 1 || cp.Blockers[0] != "paused by operator" {
		t.Errorf("blockers = %q, want the pause", cp.Blockers)
	}
	if cp.LastResult == nil || cp.LastResult.Text != "the final answer" || cp.LastResult.Seq != 16 {
		t.Fatalf("last result = %+v, want seq 16 'the final answer'", cp.LastResult)
	}
	if cp.Status != StatusBlocked || cp.Seq == 0 || !strings.HasPrefix(cp.Digest, "sha256:") {
		t.Errorf("state facts = status %q seq %d digest %q", cp.Status, cp.Seq, cp.Digest)
	}

	// The reference is stable: the artifact exists at that path, holds that
	// many entries and bytes, and its chain ends in that digest.
	if cp.History.Path != s.Store().HistoryPath() || cp.History.Entries != 16 {
		t.Fatalf("history ref = %+v", cp.History)
	}
	entries, err := s.Store().LoadHistory()
	if err != nil {
		t.Fatal(err)
	}
	var bytesTotal int64
	for _, e := range entries {
		bytesTotal += int64(e.Bytes)
	}
	if bytesTotal != cp.History.Bytes || entries[len(entries)-1].Digest != cp.History.Digest {
		t.Fatalf("history ref %+v does not match the artifact (%d bytes, tail %s)", cp.History, bytesTotal, entries[len(entries)-1].Digest)
	}
	// The chain is verifiable from scratch.
	prev := ""
	for _, e := range entries {
		if want := chainDigest(prev, e.Role, e.Text); e.Digest != want {
			t.Fatalf("entry %d digest %s, recomputed %s", e.Seq, e.Digest, want)
		}
		prev = e.Digest
	}

	// And the rendering says all of it, labelled as a projection.
	out := cp.render()
	for _, want := range []string{
		"a projection", "Accepted decisions (last 5 of 8", "decision 8", "Blockers:", "paused by operator",
		"Last agent result [seq 16, 16 bytes]:\nthe final answer", "Recent turns (last 6 of 16)",
		"History artifact: " + cp.History.Path, cp.History.Digest, "never replayed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered checkpoint lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "decision 1\n") || strings.Contains(out, "[seq 1 ") {
		t.Errorf("rendered checkpoint replays the first turn:\n%s", out)
	}
	if len(out) > ContinuationBudget {
		t.Errorf("rendered checkpoint is %d bytes, budget %d", len(out), ContinuationBudget)
	}
}

// A mid-turn steer is an accepted decision like any other: it reaches the
// artifact and the checkpoint without taking the turn's lock.
func TestMidTurnSteerIsRecordedAsADecision(t *testing.T) {
	s := startReplaySession(t, "midturn", &sizeRunner{reply: "ok"})
	s.mu.Lock() // a turn in flight
	if err := s.noteSteer("not that file — the other one"); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	cp := s.Checkpoint()
	if cp.DecisionsTotal != 1 || cp.Decisions[0].Role != RoleMidTurn || cp.Decisions[0].Text != "not that file — the other one" {
		t.Fatalf("checkpoint decisions = %+v", cp.Decisions)
	}
}

// Previews are byte budgets that never split a rune and never lie about how
// much was cut.
func TestPreviewRespectsUTF8Bounds(t *testing.T) {
	samples := []string{
		strings.Repeat("é", 400),            // 2-byte runes
		strings.Repeat("日本語", 300),          // 3-byte runes
		strings.Repeat("🙂", 250),            // 4-byte runes
		strings.Repeat("a🙂é", 300),          // mixed widths
		"short",                             // under every budget
		strings.Repeat("x", 5000) + "🙂",     // cut lands before the wide tail
		strings.Repeat("\u00e9\u0301", 500), // combining sequences
	}
	for _, in := range samples {
		if !utf8.ValidString(in) {
			t.Fatal("sample is not valid UTF-8")
		}
		for _, max := range []int{32, 64, DecisionPreviewBytes, EntryPreviewBytes, LastResultBytes, len(in), len(in) + 1} {
			out := preview(in, max)
			if len(out) > max {
				t.Fatalf("preview(%d-byte input, %d) is %d bytes", len(in), max, len(out))
			}
			if !utf8.ValidString(out) {
				t.Fatalf("preview(%d-byte input, %d) split a rune: %q", len(in), max, out)
			}
			if len(in) <= max {
				if out != in {
					t.Fatalf("preview must pass a fitting string through unchanged")
				}
				continue
			}
			i := strings.LastIndex(out, " …[+")
			if i < 0 || !strings.HasSuffix(out, " bytes]") {
				t.Fatalf("preview(%d, %d) lacks the omission marker: %q", len(in), max, out)
			}
			var omitted int
			if _, err := fmt.Sscanf(out[i:], " …[+%d bytes]", &omitted); err != nil {
				t.Fatalf("marker unreadable in %q: %v", out, err)
			}
			if omitted != len(in)-i {
				t.Fatalf("marker claims %d omitted bytes, actually %d", omitted, len(in)-i)
			}
			if !strings.HasPrefix(in, out[:i]) {
				t.Fatalf("preview kept text that is not a prefix of the input")
			}
		}
	}

	// End to end: a multibyte agent result is bounded in the checkpoint and the
	// verbatim text is intact in the artifact.
	wide := strings.Repeat("🙂", 2000)
	s := startReplaySession(t, "wide", &sizeRunner{reply: wide})
	tell(t, s, "go")
	cp := s.Checkpoint()
	if cp.LastResult == nil || len(cp.LastResult.Text) > LastResultBytes || !utf8.ValidString(cp.LastResult.Text) {
		t.Fatalf("last result preview is unbounded or invalid: %d bytes", len(cp.LastResult.Text))
	}
	if cp.LastResult.Bytes != len(wide) {
		t.Fatalf("last result reports %d bytes, want %d", cp.LastResult.Bytes, len(wide))
	}
	entries, _ := s.Store().LoadHistory()
	if entries[1].Text != wide {
		t.Fatal("artifact does not hold the verbatim result")
	}
	if out := cp.render(); !utf8.ValidString(out) || len(out) > ContinuationBudget {
		t.Fatalf("rendered checkpoint invalid or over budget (%d bytes)", len(out))
	}
}

// Even adversarial content cannot push the rendered continuation past its
// budget: the window shrinks, then the result goes, then the text is cut.
func TestRenderNeverExceedsContinuationBudget(t *testing.T) {
	huge := strings.Repeat("h", 4*ContinuationBudget)
	var recent []HistoryEntry
	for i := 1; i <= RecentWindow; i++ {
		recent = append(recent, HistoryEntry{Seq: int64(i), Role: RoleAgent, Text: huge, Bytes: len(huge)})
	}
	cp := Checkpoint{
		Status:      StatusWorking,
		CurrentStep: huge,
		Decisions:   []HistoryEntry{{Seq: 1, Role: RoleHuman, Text: huge}},
		Blockers:    []string{huge},
		LastResult:  &HistoryEntry{Seq: 2, Role: RoleAgent, Text: huge, Bytes: len(huge)},
		Recent:      recent,
		History:     HistoryRef{Path: "/x/history.jsonl", Entries: 9, Bytes: 99, Digest: "sha256:abc"},
	}
	out := cp.render()
	if len(out) > ContinuationBudget {
		t.Fatalf("rendered %d bytes, budget %d", len(out), ContinuationBudget)
	}
	if !utf8.ValidString(out) {
		t.Fatal("rendered continuation is not valid UTF-8")
	}
}

// A restarted daemon continues from the same continuity: same counters, same
// digest, same decisions and last result, same references in its next prompt.
func TestRestartRecoversContinuityFromArtifact(t *testing.T) {
	root := t.TempDir()
	r := &sizeRunner{reply: "kept"}
	s, err := Start(context.Background(), Options{ID: "restart", Goal: "survive a restart", Agent: "stub", Root: root, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 9; i++ {
		tell(t, s, fmt.Sprintf("step %d", i))
	}
	if err := s.saveState(); err != nil { // what the daemon does after every Apply
		t.Fatal(err)
	}
	before := s.Checkpoint()
	s.Close()

	re, err := Open(root, "restart", r)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	after := re.Checkpoint()
	if after.History != before.History {
		t.Fatalf("history ref changed across restart:\n before %+v\n after  %+v", before.History, after.History)
	}
	if after.DecisionsTotal != before.DecisionsTotal || len(after.Decisions) != len(before.Decisions) {
		t.Fatalf("decisions changed across restart: %d/%d -> %d/%d", len(before.Decisions), before.DecisionsTotal, len(after.Decisions), after.DecisionsTotal)
	}
	for i := range before.Decisions {
		if before.Decisions[i].Seq != after.Decisions[i].Seq || before.Decisions[i].Text != after.Decisions[i].Text {
			t.Fatalf("decision %d differs: %+v vs %+v", i, before.Decisions[i], after.Decisions[i])
		}
	}
	if after.LastResult == nil || *after.LastResult != *before.LastResult {
		t.Fatalf("last result differs: %+v vs %+v", before.LastResult, after.LastResult)
	}
	if after.Seq != before.Seq || after.Digest != before.Digest {
		t.Fatalf("state seq/digest differ: %d/%s vs %d/%s", before.Seq, before.Digest, after.Seq, after.Digest)
	}
	// The next turn continues the chain, and the sequence, where they left off.
	tell(t, re, "step 10")
	if err := re.saveState(); err != nil {
		t.Fatal(err)
	}
	if got := re.Checkpoint(); got.History.Entries != 20 || got.Seq != before.Seq+2 {
		t.Fatalf("after restart+turn: entries %d (want 20), seq %d (want %d)", got.History.Entries, got.Seq, before.Seq+2)
	}
	if !strings.Contains(r.last, "step 9") || strings.Contains(r.last, "step 1\n") {
		t.Fatalf("post-restart prompt has the wrong window:\n%s", r.last)
	}
}

// Persisting the same state again is a no-op at every layer: same seq, same
// digest, no journal line, no file change. This is what makes an unchanged
// healthy poll silent.
func TestIdenticalStateIsSuppressed(t *testing.T) {
	store := NewStore(t.TempDir(), "same")
	st := State{ID: "same", Goal: "g", Status: StatusIdle, CreatedAt: time.Now().UTC()}
	first, err := store.Commit(st)
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || first.Digest != CanonicalDigest(st) {
		t.Fatalf("first commit = seq %d digest %s", first.Seq, first.Digest)
	}
	stateBytes, _ := os.ReadFile(store.StatePath())
	journalBytes, _ := os.ReadFile(store.TransitionsPath())

	// Volatile fields differ; content does not.
	again := st
	again.UpdatedAt = time.Now().Add(time.Hour)
	again.Seq = 99
	again.Digest = "sha256:stale"
	for i := 0; i < 3; i++ {
		got, err := store.Commit(again)
		if err != nil {
			t.Fatal(err)
		}
		if got.Seq != 1 || got.Digest != first.Digest || !got.UpdatedAt.Equal(first.UpdatedAt) {
			t.Fatalf("identical commit %d produced seq %d digest %s updated %v", i, got.Seq, got.Digest, got.UpdatedAt)
		}
	}
	if b, _ := os.ReadFile(store.StatePath()); string(b) != string(stateBytes) {
		t.Fatal("state.json was rewritten for identical content")
	}
	if b, _ := os.ReadFile(store.TransitionsPath()); string(b) != string(journalBytes) {
		t.Fatal("a transition was journaled for identical content")
	}
	if trs, _ := store.Changes(0); len(trs) != 1 {
		t.Fatalf("Changes(0) = %d records, want 1", len(trs))
	}
	if trs, _ := store.Changes(1); len(trs) != 0 {
		t.Fatalf("Changes(1) = %d records, want none", len(trs))
	}
	// A real change advances by exactly one.
	st.Status = StatusWorking
	got, err := store.Commit(st)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 2 || got.Digest == first.Digest {
		t.Fatalf("changed commit = seq %d digest %s", got.Seq, got.Digest)
	}
}

// The exact lifecycle, in order, each step once, with its own seq and digest —
// and a cursor reader sees precisely the steps after its cursor.
func TestExactStateTransitionsAreSequenced(t *testing.T) {
	root := t.TempDir()
	r := &stubRunner{out: "ack"}
	s, err := Start(context.Background(), Options{ID: "seq", Goal: "sequence", Agent: "stub", Root: root, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, verb := range []string{CommandTell, CommandPause, CommandResume, CommandStop} {
		if err := s.Enqueue(Command{Verb: verb, Message: "do the thing"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ProcessPending(ctx); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
	}
	trs, err := s.Store().Changes(0)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ prev, status, blocker string }{
		{"", StatusIdle, ""},
		{StatusIdle, StatusWorking, ""},
		{StatusWorking, StatusIdle, ""},
		{StatusIdle, StatusBlocked, "paused by operator"},
		{StatusBlocked, StatusIdle, ""},
		{StatusIdle, StatusDone, ""},
	}
	if len(trs) != len(want) {
		t.Fatalf("got %d transitions, want %d:\n%s", len(trs), len(want), dump(trs))
	}
	for i, tr := range trs {
		w := want[i]
		if tr.Seq != int64(i+1) || tr.PreviousStatus != w.prev || tr.Status != w.status || tr.Blocker != w.blocker {
			t.Fatalf("transition %d = %+v, want seq %d %s->%s blocker %q", i, tr, i+1, w.prev, w.status, w.blocker)
		}
		if tr.SchemaVersion != TransitionSchemaVersion || tr.ID != "seq" || !strings.HasPrefix(tr.Digest, "sha256:") {
			t.Fatalf("transition %d envelope = %+v", i, tr)
		}
		// A digest is CONTENT, not history: consecutive records always differ
		// (that is what made them records), while seq 3 and seq 5 — idle after
		// the tell, idle after the resume — are legitimately the same state.
		if i > 0 && tr.Digest == trs[i-1].Digest {
			t.Fatalf("transition %d repeats the previous digest %s", i, tr.Digest)
		}
	}
	if trs[2].Digest != trs[4].Digest {
		t.Fatalf("seq 3 and seq 5 are the same state and must share a digest: %s vs %s", trs[2].Digest, trs[4].Digest)
	}
	if trs[1].CurrentStep != "do the thing" || !trs[5].Stopped || trs[5].StopReason != "stopped by operator" {
		t.Fatalf("transition payloads: %s", dump(trs))
	}
	// Cursor semantics.
	if tail, _ := s.Store().Changes(3); len(tail) != 3 || tail[0].Seq != 4 {
		t.Fatalf("Changes(3) = %s", dump(tail))
	}
	if none, _ := s.Store().Changes(6); len(none) != 0 {
		t.Fatalf("Changes(6) = %s, want nothing", dump(none))
	}
	// Silence: a bounded wait on an unchanged session returns no payload.
	if got, err := s.Store().WaitChanges(ctx, 6, 150*time.Millisecond); err != nil || got != nil {
		t.Fatalf("WaitChanges on unchanged = %v, %v", got, err)
	}
	// The state's own seq agrees with the journal, so `status --json` hands a
	// caller a cursor it can use.
	if st, _ := s.Store().LoadState(); st.Seq != 6 || st.Digest != trs[5].Digest {
		t.Fatalf("state seq/digest %d/%s, journal tail %d/%s", st.Seq, st.Digest, trs[5].Seq, trs[5].Digest)
	}
}

// Wait wakes on the change and returns exactly the missed transitions — the
// idle → working → idle pair is delivered even though the poller was asleep
// through both.
func TestWaitChangesWakesOnTransition(t *testing.T) {
	root := t.TempDir()
	s, err := Start(context.Background(), Options{ID: "wake", Goal: "wake", Agent: "stub", Root: root, Runner: &stubRunner{out: "ack"}})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		trs []Transition
		err error
	}
	got := make(chan result, 1)
	go func() {
		trs, err := s.Store().WaitChanges(context.Background(), 1, 5*time.Second)
		got <- result{trs, err}
	}()
	time.Sleep(150 * time.Millisecond) // the waiter is parked on stat polls
	tell(t, s, "wake up")
	if err := s.saveState(); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-got:
		if res.err != nil || len(res.trs) == 0 || res.trs[0].Seq != 2 || res.trs[0].Status != StatusWorking {
			t.Fatalf("wait returned %s, %v", dump(res.trs), res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitChanges did not wake on a transition")
	}
	// Nothing was lost: a follow-up read from the returned cursor delivers the
	// rest of the pair.
	trs, _ := s.Store().Changes(2)
	if len(trs) != 1 || trs[0].Seq != 3 || trs[0].PreviousStatus != StatusWorking || trs[0].Status != StatusIdle {
		t.Fatalf("Changes(2) = %s", dump(trs))
	}
	// Cancellation is an error, not a silent timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Store().WaitChanges(ctx, 3, time.Minute); err == nil {
		t.Fatal("cancelled wait returned no error")
	}
}

// The DAG driver emits one transition per step: working(target) → idle,
// for every target, in order.
func TestDAGTransitionsAreExact(t *testing.T) {
	root := t.TempDir()
	dagPath := filepath.Join(root, "dag.md")
	if err := os.WriteFile(dagPath, []byte("## Tasks\n\n### a\n```bash\necho a\n```\n\n### b\nRequires: a\n```bash\necho b\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{seen: make(chan string, 8)}
	s, err := Start(context.Background(), Options{ID: "dagseq", Goal: "run dag", Agent: "stub", Root: root, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunDAG(context.Background(), DAGOptions{Path: dagPath, SteerPause: time.Millisecond}); err != nil {
		t.Fatalf("RunDAG: %v", err)
	}
	trs, _ := s.Store().Changes(0)
	var got []string
	for _, tr := range trs {
		got = append(got, tr.PreviousStatus+">"+tr.Status+":"+tr.CurrentStep)
	}
	want := []string{">idle:", "idle>working:a", "working>idle:a", "idle>working:b", "working>idle:b"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("dag transitions = %v, want %v", got, want)
	}
	// Target b's packet references a's output instead of quoting a conversation.
	prompts := r.snapshot()
	if !strings.Contains(prompts[1], "Dependency outputs (by reference") || !strings.Contains(prompts[1], "- a: [seq 1, 3 bytes] ack") {
		t.Fatalf("target b packet lacks a's output by reference:\n%s", prompts[1])
	}
	if strings.Contains(prompts[1], "Session history:") {
		t.Fatal("DAG packet still replays the session history")
	}
}

// Recovery: the journal is a delta view, never the truth. If state.json is
// ahead (crash between the two writes; a session from before the contract),
// the reader still sees the current state exactly once and the writer
// continues the sequence without reusing a number.
func TestChangesRecoverWhenJournalLagsOrLeads(t *testing.T) {
	// 1. Legacy state.json: no seq, no digest, no journal.
	store := NewStore(t.TempDir(), "legacy")
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(map[string]any{"id": "legacy", "goal": "old", "status": StatusWorking})
	if err := os.WriteFile(store.StatePath(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	trs, err := store.Changes(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) != 1 || trs[0].Seq != 1 || trs[0].Status != StatusWorking || trs[0].Digest == "" {
		t.Fatalf("legacy Changes(0) = %s", dump(trs))
	}
	st, _ := store.LoadState()
	st.Status = StatusIdle
	committed, err := store.Commit(st)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Seq != 2 {
		t.Fatalf("commit after legacy state = seq %d, want 2 (1 is the synthesized legacy record)", committed.Seq)
	}
	if trs, _ = store.Changes(1); len(trs) != 1 || trs[0].Seq != 2 || trs[0].PreviousStatus != StatusWorking {
		t.Fatalf("Changes(1) after legacy = %s", dump(trs))
	}

	// 2. Journal lags: state.json landed, the journal append did not.
	store2 := NewStore(t.TempDir(), "lag")
	st2 := State{ID: "lag", Goal: "g", Status: StatusIdle}
	if _, err := store2.Commit(st2); err != nil {
		t.Fatal(err)
	}
	st2.Status = StatusWorking
	st2.Seq, st2.Digest = 2, CanonicalDigest(st2)
	if err := store2.writeState(st2); err != nil { // truth moved, delta did not
		t.Fatal(err)
	}
	if trs, _ = store2.Changes(1); len(trs) != 1 || trs[0].Seq != 2 || trs[0].Status != StatusWorking || trs[0].PreviousStatus != StatusIdle {
		t.Fatalf("lagging journal: Changes(1) = %s", dump(trs))
	}
	st2.Status = StatusBlocked
	if c, err := store2.Commit(st2); err != nil || c.Seq != 3 {
		t.Fatalf("commit after lag = seq %d, %v (want 3)", c.Seq, err)
	}
	if trs, _ = store2.Changes(0); len(trs) != 3 || trs[1].Seq != 2 || trs[2].Seq != 3 {
		t.Fatalf("after lag+commit: Changes(0) = %s", dump(trs))
	}

	// 3. Journal leads: the append landed, the state rename did not.
	store3 := NewStore(t.TempDir(), "lead")
	st3 := State{ID: "lead", Goal: "g", Status: StatusIdle}
	if _, err := store3.Commit(st3); err != nil {
		t.Fatal(err)
	}
	if err := store3.appendTransition(Transition{SchemaVersion: TransitionSchemaVersion, ID: "lead", Seq: 5, Status: StatusWorking}); err != nil {
		t.Fatal(err)
	}
	st3.Status = StatusDone
	if c, err := store3.Commit(st3); err != nil || c.Seq != 6 {
		t.Fatalf("commit after leading journal = seq %d, %v (want 6: never reuse a journaled seq)", c.Seq, err)
	}
	// A torn tail line does not poison the journal.
	f, _ := os.OpenFile(store3.TransitionsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(`{"schema_version":"bashy-foreman-transition-v1","seq":7,"stat`)
	_ = f.Close()
	if trs, err = store3.Changes(0); err != nil || len(trs) != 3 || trs[2].Seq != 6 {
		t.Fatalf("torn tail: Changes(0) = %s, %v", dump(trs), err)
	}
	st3.Status = StatusBlocked
	if c, err := store3.Commit(st3); err != nil || c.Seq != 7 {
		t.Fatalf("commit after torn tail = seq %d, %v", c.Seq, err)
	}
	if trs, err = store3.Changes(6); err != nil || len(trs) != 1 || trs[0].Seq != 7 || trs[0].Status != StatusBlocked {
		t.Fatalf("transition after torn tail: Changes(6) = %s, %v", dump(trs), err)
	}
}

func dump(trs []Transition) string {
	var b strings.Builder
	for _, tr := range trs {
		fmt.Fprintf(&b, "\n  seq %d %s->%s step=%q blocker=%q", tr.Seq, tr.PreviousStatus, tr.Status, tr.CurrentStep, tr.Blocker)
	}
	return b.String()
}
