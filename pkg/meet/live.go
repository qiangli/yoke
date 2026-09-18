package meet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Watching a meeting should feel like sitting in one, not like reading the
// minutes afterwards. A turn is a whole model call — often minutes long — so a
// transcript that only gains an entry when an agent FINISHES leaves a watcher
// staring at nothing and wondering whether anything is alive.
//
// So the meeting also writes a LIVE channel: each line of an agent's answer, as
// the agent writes it. `observe` tails it, and you watch the argument being made
// rather than being told, later, that it was.
//
// Two channels, one truth:
//
//   - transcript.jsonl is the RECORD. One sanitized event per completed turn.
//     It is what the minutes are built from and what gets replayed as prompt
//     context to the next agent. Interleaving thousands of partial lines into it
//     would bloat it and quietly change what "the record" means.
//   - live.jsonl is the VIEW. Ephemeral, derived, line-granular, and safe to
//     lose: delete it and the meeting is unharmed, because every line it carried
//     also lands, whole, in the transcript when the turn completes.
//
// The live channel is a tee of the agent's stdout (see chat.Options.Stream), so
// it cannot show a watcher anything the record will not also contain. Observing
// never changes what is recorded.
//
// Granularity is a LINE, not a token. The agent CLIs bashy drives emit complete
// lines on stdout; there is no token channel to subscribe to without going
// around them straight to a provider API, which would abandon the harness (and
// its tools, its sandbox, its shell-forcing) entirely. A line is the finest
// grain honestly available here.

// liveKind values, kept distinct from transcript Event kinds so a reader can
// never mistake a view record for a record record.
const (
	liveSpeaking = "speaking" // an agent took the floor
	liveLine     = "line"     // one line of what it is saying
	liveSpoke    = "spoke"    // it finished; the whole turn is now in the transcript
)

// LiveEvent is one line of the live channel.
type LiveEvent struct {
	Kind    string    `json:"kind"`
	Round   int       `json:"round"`
	Speaker string    `json:"speaker"`
	Role    string    `json:"role,omitempty"`
	Text    string    `json:"text,omitempty"`
	Status  string    `json:"status,omitempty"` // on `spoke`
	TS      time.Time `json:"ts"`
	// Lines, Bytes, and ElapsedMS are populated by the DM observer's bounded
	// progress projection. They are counters over the live prose seen so far,
	// not transcript fields, and therefore remain absent from ordinary meeting
	// live events.
	Lines     int   `json:"lines,omitempty"`
	Bytes     int64 `json:"bytes,omitempty"`
	ElapsedMS int64 `json:"elapsed_ms,omitempty"`

	// CtlSock is set on `speaking`: the socket that steers THIS turn.
	//
	// It rides the live channel because the live channel already answers "who has
	// the floor right now" — a `speaking` with no matching `spoke` IS the current
	// speaker. Keeping the steer address in the same record means `meet say`
	// cannot address an agent that has already finished, which is the one way a
	// steer goes quietly nowhere.
	CtlSock string `json:"ctl_sock,omitempty"`
}

func livePath(id string) (string, error) {
	dir, err := storeDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "live.jsonl"), nil
}

func appendLive(id string, e LiveEvent) {
	// Best-effort by design. The live channel is a view: if it cannot be written
	// the meeting must still run and still be recorded. A watcher's convenience
	// is never a reason to fail a turn.
	path, err := livePath(id)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// liveChannel names WHICH SOURCE a live line arrived from. It is the SINK the
// bytes came in on — not a guess made from how the line classified — which is the
// whole point: a tool that reports its answer as structured events (ycode) streams
// on two file descriptors that chat.Invoke tees into this one writer, the stdout
// prose tee and the drained event side-channel (see chat's streamEventFile). Only
// the sink honestly says which is which.
type liveChannel int

const (
	chanProse liveChannel = iota // the agent's stdout tee (Write)
	chanEvent                    // the tool's structured event side-channel (eventSink)
)

// liveWriter turns an agent's output into line events on the live channel.
//
// It is fed by chat as io.Writers and receives whatever chunks the process
// happens to flush — which do NOT align with lines. So it buffers, emits only
// complete lines, and holds the trailing partial one until it is finished. A
// half-line published as a line would show a watcher a sentence the agent never
// wrote.
//
// It exposes TWO sinks, because chat.Invoke feeds it from TWO concurrent sources:
// the stdout prose tee (this type's Write) and, for an events-reporting tool, the
// drained NDJSON side-channel (eventSink, wired via chat.Options.EventStream).
// Each sink FRAMES INTO ITS OWN BUFFER. That separation is load-bearing: with one
// shared partial-line buffer, a stdout chunk that ended mid-line would splice onto
// the front of the next complete JSON event the side-channel goroutine wrote, and
// the concatenation ("half a prose line{\"type\":\"turn.end\"…}") no longer matches
// a transport fingerprint — so it is classified as prose and the whole envelope
// leaks to the watcher and into live.jsonl. Per-source framing makes that splice
// impossible; the mutex still serializes the two goroutines' emits.
type liveWriter struct {
	id      string
	round   int
	speaker string
	role    string
	// activity optionally projects a structured transport line into a short,
	// human-readable progress line. Meeting turns leave it nil; Chat enables it
	// so tool activity can prove motion before the assistant starts its answer.
	activity func(string) string

	mu       sync.Mutex
	proseBuf bytes.Buffer // partial-line buffer for the stdout tee (chanProse)
	eventBuf bytes.Buffer // partial-line buffer for the event side-channel (chanEvent)

	// proseSeen is the per-turn multiset of lines the STDOUT prose channel has
	// already shown, keyed by normalized line text with a count. It exists to drop
	// the CROSS-channel echo: an events-reporting tool (ycode) streams its answer on
	// BOTH stdout and its turn.end event, and the deferred event reconciliation
	// (flushEvents) consumes a credit here for each line it recognizes as a mirror
	// of prose, so the answer reaches the watcher once. Counting (not a set) is what
	// lets a line an agent legitimately repeats survive: three streamed "A"s credit
	// three, and an event mirroring three "A"s consumes exactly three — a fourth "A"
	// present only in the event is genuine event-only content and is kept. Per-
	// liveWriter, hence per-turn, so a repeat in a later turn is never the echo.
	// Guarded by mu (emit holds it).
	proseSeen map[string]int

	// eventPending holds the STRUCTURED-EVENT channel's extracted text blobs until
	// the turn closes, instead of emitting them the moment they arrive.
	//
	// This is the ordering fix. A turn.end event is a COMPLETE snapshot of the whole
	// answer, and the drain goroutine can deliver it BEFORE the stdout tee has
	// finished streaming that same answer — production interleave: stdout prefix
	// "A", then the whole turn.end "A\nA\nB", then the stdout remainder "A\nB". An
	// online line-by-line dedup that emitted the event's not-yet-streamed tail right
	// away reordered the live view to A,B,A. Holding the event channel and
	// reconciling it against the WHOLE prose stream at close (chat.Invoke guarantees
	// the drain goroutine has stopped before close runs) keeps stdout the single
	// order-authoritative live stream — prose stays A,A,B — while event-only content
	// still survives as the reconciled remainder. Guarded by mu.
	eventPending []string

	// closeOnce makes `spoke` exactly-once. Every path out of a turn now closes
	// the floor — including a deferred close that fires on a panic or an early
	// return someone adds later — and a floor freed twice would show a watcher two
	// endings for one turn, or leave `currentSpeaker` reading a stale record.
	closeOnce sync.Once
}

func newLiveWriter(st *State, speaker, role, ctlSock string) *liveWriter {
	w := &liveWriter{id: st.ID, round: st.Round, speaker: speaker, role: role}
	appendLive(w.id, LiveEvent{
		Kind: liveSpeaking, Round: w.round, Speaker: speaker, Role: role,
		CtlSock: ctlSock, TS: nowFn(),
	})
	return w
}

// setCtlSock publishes where this speaker can be reached, once it is actually
// reachable.
//
// A live turn has a chicken-and-egg: the session needs the writer (to stream
// into), and the writer needs the session's socket (to advertise). So the floor is
// claimed first WITHOUT an address, and the address is added the moment the agent
// is up. Re-emitting `speaking` is the whole mechanism — currentSpeaker reads the
// last one — and it means a chair is never handed a socket to steer an agent that
// has not started yet.
func (w *liveWriter) setCtlSock(sock string) {
	if sock == "" {
		return
	}
	appendLive(w.id, LiveEvent{
		Kind: liveSpeaking, Round: w.round, Speaker: w.speaker, Role: w.role,
		CtlSock: sock, TS: nowFn(),
	})
}

// ctlSockPath is where this turn's steer socket lives.
//
// Deliberately SHORT, and NOT under the meeting's own directory. A unix socket
// address is capped at ~104 bytes, and a meeting id is a slug of its topic —
// `~/.bashy/meet/2026-07-13-should-the-cache-be-write-through-0e1e/…` blows the
// cap and the bind fails. agentpty degrades to a polling file channel when that
// happens, so steering would still work, just silently worse. Hashing keeps the
// address inside the limit for any topic anyone can type.
func ctlSockPath(st *State, speaker string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".bashy", "ctl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s", st.ID, st.Round, speaker))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:12]+".sock")
}

// Write is the STDOUT PROSE sink: it frames the agent's stdout tee into lines and
// emits each on the prose channel.
func (w *liveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.feed(&w.proseBuf, chanProse, p)
	w.mu.Unlock()
	// Always report the full write: the tee must never make the agent's own
	// stdout write appear to fail.
	return len(p), nil
}

// eventSink is the STRUCTURED-EVENT sink: the tool's NDJSON side-channel, drained
// by chat's streamEventFile goroutine and wired here via chat.Options.EventStream.
// It has its OWN framing buffer so a partial stdout write on the prose channel can
// never concatenate with a complete event line — the two concurrent sources are
// demuxed at the byte level, not merged into one partial-line buffer where a torn
// stdout chunk would splice onto the front of a JSON envelope and smuggle raw
// transport into the prose sink.
type eventSink struct{ w *liveWriter }

func (e eventSink) Write(p []byte) (int, error) {
	e.w.mu.Lock()
	e.w.feed(&e.w.eventBuf, chanEvent, p)
	e.w.mu.Unlock()
	return len(p), nil
}

// eventStream returns the sink chat.Invoke should hand its event side-channel, so
// structured events are framed and tagged separately from stdout prose.
func (w *liveWriter) eventStream() io.Writer { return eventSink{w: w} }

// feed frames whatever chunk arrived on ONE channel into complete lines and emits
// each, holding a trailing partial line in THAT channel's own buffer. Callers hold
// w.mu. The per-channel buffer is the fix: two goroutines write this writer (the
// stdout tee and the event side-channel), and a single shared buffer let a torn
// stdout chunk splice onto the front of a complete JSON event on the next write,
// so a transport envelope was classified as prose and leaked. Separate buffers
// keep each source's line boundaries its own.
func (w *liveWriter) feed(buf *bytes.Buffer, ch liveChannel, p []byte) {
	buf.Write(p)
	for {
		line, err := buf.ReadString('\n')
		if err != nil {
			// Not a whole line yet — put it back and wait for the rest.
			buf.Reset()
			buf.WriteString(line)
			break
		}
		w.emit(line, ch)
	}
}

// emit publishes one line, sanitized the same way the recorded turn will be.
//
// Sanitizing here is not cosmetic. The raw stream carries ANSI escapes and
// control bytes; shown verbatim they would garble the watcher's terminal, and —
// worse — the watcher would see something different from what the transcript
// ends up storing. The view must agree with the record.
func (w *liveWriter) emit(line string, ch liveChannel) {
	// A tool streaming structured transport (Claude stream-json on stdout,
	// first-party NDJSON on the side-channel) writes one JSON envelope per line.
	// Show only the assistant text a human would read; drop system/thinking/tool/
	// tool-result/usage/control lines entirely. Plain output passes through
	// unchanged. Classification FILTERS and EXTRACTS the words; the SINK (ch), not
	// this classification, decides how the line is reconciled — the sink is the only
	// honest signal of which source a byte came from. Prose emits live in order;
	// the event channel is held and reconciled against prose at close.
	human, class, _ := classifyEventLine(line)
	if w.activity != nil {
		if activity := w.activity(line); activity != "" {
			w.publish(activity)
		}
	}
	if class == lineDrop {
		return
	}
	text := strings.TrimRight(sanitizeTurn(human), "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	if ch == chanEvent {
		// Hold the structured-event channel. It is a complete-answer snapshot that
		// can land mid-stdout-stream, so emitting it now would reorder the live view
		// (A,B,A). flushEvents reconciles it against the whole prose stream at close.
		w.eventPending = append(w.eventPending, text)
		return
	}
	// chanProse is the order-authoritative LIVE stream: publish every line the
	// moment it is framed, and credit it so the deferred event reconciliation can
	// recognize its mirror as a cross-channel echo. Prose is never suppressed —
	// same-channel repeats survive and ordinary live streaming is unweakened.
	w.creditProse(text)
	w.publish(text)
}

// publish appends one already-classified, already-sanitized text blob to the live
// channel as a `line` event. Callers hold w.mu.
func (w *liveWriter) publish(text string) {
	appendLive(w.id, LiveEvent{
		Kind: liveLine, Round: w.round, Speaker: w.speaker, Role: w.role,
		Text: text, TS: nowFn(),
	})
}

// creditProse records each non-blank line of a prose blob in proseSeen, so the
// deferred event reconciliation can drop the matching cross-channel echo. Blank
// lines are structural and never credited. Callers hold w.mu.
func (w *liveWriter) creditProse(text string) {
	if w.proseSeen == nil {
		w.proseSeen = make(map[string]int)
	}
	for l := range strings.SplitSeq(text, "\n") {
		if key := strings.TrimSpace(l); key != "" {
			w.proseSeen[key]++
		}
	}
}

// flushEvents reconciles the deferred structured-event channel against the WHOLE
// prose stream and publishes what survives, in event order, after every prose line
// is already out. It runs once at close, single-threaded, so the reconciliation
// sees the complete prose credit set no matter how the two channels interleaved.
//
// Per event blob it walks lines: a line that mirrors one prose already showed is a
// cross-channel echo — dropped, consuming one prose credit; anything else is
// genuine event-only content and is kept. Blank lines are structural, always kept.
// A blob whose lines were all echoes contributes nothing (the mirrored-answer
// case); a blob with no prose behind it survives whole (the event-only case).
// Callers hold w.mu.
func (w *liveWriter) flushEvents() {
	for _, blob := range w.eventPending {
		lines := strings.Split(blob, "\n")
		out := lines[:0]
		for _, l := range lines {
			key := strings.TrimSpace(l)
			if key == "" {
				out = append(out, l) // structural blank, never an echo
				continue
			}
			if w.proseSeen[key] > 0 {
				w.proseSeen[key]-- // cross-channel echo of a line prose already showed
				continue
			}
			out = append(out, l)
		}
		survivors := strings.TrimRight(strings.Join(out, "\n"), "\n")
		if strings.TrimSpace(survivors) != "" {
			w.publish(survivors)
		}
	}
	w.eventPending = nil
}

// close flushes a trailing line with no newline and marks the floor free.
//
// The trailing flush is what makes a killed turn's last words survive. An agent
// that is timed out or cancelled mid-sentence leaves a partial line in the
// buffer with no '\n' behind it; without this flush the watcher's last view of
// that agent stops one line earlier than the transcript does, and the two
// channels disagree exactly when someone is trying to work out what happened.
// It is sanitized through the same emit() as every other line, so the view
// still cannot show anything the record would not.
//
// Idempotent: the first caller wins and later ones are no-ops, so a deferred
// close behind an explicit one never doubles the `spoke`.
func (w *liveWriter) close(status string) {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		// Flush each channel's trailing partial line on its OWN channel tag. Both
		// sinks may hold a mid-sentence remainder when a turn is cut off; each is
		// emitted through the same classify path as every other line. Prose flushes
		// live and is credited; the event partial joins eventPending.
		if rest := w.proseBuf.String(); rest != "" {
			w.proseBuf.Reset()
			w.emit(rest, chanProse)
		}
		if rest := w.eventBuf.String(); rest != "" {
			w.eventBuf.Reset()
			w.emit(rest, chanEvent)
		}
		// Now that the whole prose stream is out and its lines are credited,
		// reconcile the deferred event channel: drop the cross-channel echo, keep
		// event-only content. This is what keeps the mirrored answer in prose order.
		w.flushEvents()
		w.mu.Unlock()
		appendLive(w.id, LiveEvent{
			Kind: liveSpoke, Round: w.round, Speaker: w.speaker, Role: w.role,
			Status: status, TS: nowFn(),
		})
	})
}
