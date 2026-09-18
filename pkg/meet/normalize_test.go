package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures below are the two transports meet actually captures, in the exact
// shape they land on the live channel and in an old-build transcript. They are
// hermetic — literal strings, no process, no network — and mirror what room 12
// stored on disk (system/rate_limit/assistant/user/result for Claude; the
// turn.*/tool.* NDJSON for a first-party harness). If the wire format drifts,
// these are the fixtures to update.
const (
	// Claude `--output-format stream-json --verbose`: one JSON envelope per line.
	// Every line carries session_id — that is the fingerprint that marks a line as
	// Claude transport even for an event type this seam does not enumerate.
	fxClaudeSystemInit    = `{"type":"system","subtype":"init","cwd":"/tmp","tools":["Read","Bash"],"session_id":"s-1"}`
	fxClaudeRateLimit     = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"},"uuid":"u-1","session_id":"s-1"}`
	fxClaudeAssistantText = `{"type":"assistant","message":{"id":"m-1","role":"assistant","content":[{"type":"text","text":"The cache should be write-through."}]},"session_id":"s-1"}`
	// A turn that both narrated AND called a tool: only the prose is human-readable.
	fxClaudeAssistantMixed = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me check the file."},{"type":"tool_use","id":"t-1","name":"Read","input":{"file_path":"/x"}}]},"session_id":"s-1"}`
	// A turn that was pure tool_use / thinking said nothing to a human.
	fxClaudeAssistantToolOnly = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t-2","name":"Bash","input":{"command":"ls"}}]},"session_id":"s-1"}`
	fxClaudeAssistantThinking = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"}]},"session_id":"s-1"}`
	// A tool_result rides back as a `user` event; it is machine state, not speech.
	fxClaudeUserToolResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t-1","content":"file contents"}]},"session_id":"s-1"}`
	fxClaudeResult         = `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"usage":{"input_tokens":100,"output_tokens":20},"session_id":"s-1"}`
	// Defensive: an assistant message whose content is a bare string, not blocks.
	fxClaudeAssistantString = `{"type":"assistant","message":{"role":"assistant","content":"plain answer"},"session_id":"s-1"}`

	// First-party / ycode NDJSON (see chat/events.go): dotted event names, only the
	// turn's end carries the answer.
	fxNDJSONTurnStart = `{"type":"turn.start","data":{"prompt":"what should the cache do?"}}`
	fxNDJSONToolCall  = `{"type":"tool.call","data":{"name":"read_file","input":{"path":"cache.go"}}}`
	fxNDJSONTurnEnd   = `{"type":"turn.end","data":{"status":"ok","text":"Write-through, to avoid lost writes."}}`

	// FOREIGN JSON that merely resembles our transport — a real answer from some
	// other agent/harness, not Claude and not ycode. It must be preserved
	// byte-for-byte: extracting "text" out of it would silently eat the answer.
	// The Claude look-alike carries NO session_id (Claude stamps one on every
	// line); the ycode look-alike carries NO configured `status` on turn.end.
	fxForeignAssistant = `{"type":"assistant","message":{"content":[{"type":"text","text":"42 is the answer"}]}}`
	fxForeignTurnEnd   = `{"type":"turn.end","data":{"text":"the deploy finished"}}`
)

func TestClassifyEventLine(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		want  string
		class lineClass
	}{
		// --- Claude stream-json: only assistant prose survives. ---
		{"claude system init", fxClaudeSystemInit, "", lineDrop},
		{"claude rate limit", fxClaudeRateLimit, "", lineDrop},
		{"claude result", fxClaudeResult, "", lineDrop},
		{"claude tool_result (user)", fxClaudeUserToolResult, "", lineDrop},
		{"claude assistant text", fxClaudeAssistantText, "The cache should be write-through.", lineText},
		{"claude assistant mixed keeps only prose", fxClaudeAssistantMixed, "Let me check the file.", lineText},
		{"claude assistant tool-only drops", fxClaudeAssistantToolOnly, "", lineDrop},
		{"claude assistant thinking drops", fxClaudeAssistantThinking, "", lineDrop},
		{"claude assistant bare-string content", fxClaudeAssistantString, "plain answer", lineText},

		// --- ycode / first-party NDJSON. ---
		{"ndjson turn.start drops", fxNDJSONTurnStart, "", lineDrop},
		{"ndjson tool.call drops", fxNDJSONToolCall, "", lineDrop},
		{"ndjson turn.end keeps text", fxNDJSONTurnEnd, "Write-through, to avoid lost writes.", lineText},

		// --- Malformed / torn transport: fail closed, never leak half an event. ---
		{"torn claude line", `{"type":"assistant","message":{"content":[{"type":"te`, "", lineDrop},
		{"open brace, not json", `{ this is not json`, "", lineDrop},

		// --- Plain output: preserved byte-for-byte. ---
		{"plain prose", "the cache should be write-through", "the cache should be write-through", linePlain},
		{"empty line", "", "", linePlain},
		{"prose containing a brace", "use the {config} block", "use the {config} block", linePlain},
		// A JSON object that is neither configured transport (no session_id, unknown
		// type) is an agent's own answer or another harness — preserving it beats
		// eating a real answer.
		{"foreign json answer", `{"answer":42}`, `{"answer":42}`, linePlain},
		{"unknown type without session_id", `{"type":"whatever","foo":"bar"}`, `{"type":"whatever","foo":"bar"}`, linePlain},
		// Foreign JSON that merely RESEMBLES transport must be preserved, not rewritten:
		// a type=="assistant" line with no session_id is not Claude, and a turn.end
		// with no configured status is not ycode. Extracting text would eat the answer.
		{"foreign assistant without session_id", fxForeignAssistant, fxForeignAssistant, linePlain},
		{"foreign turn.end without status", fxForeignTurnEnd, fxForeignTurnEnd, linePlain},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, class, _ := classifyEventLine(c.line)
			if class != c.class {
				t.Errorf("class = %d, want %d", class, c.class)
			}
			if got != c.want {
				t.Errorf("text = %q, want %q", got, c.want)
			}
		})
	}
}

// An unenumerated Claude event type still carries session_id, so it is recognized
// as transport and dropped rather than leaked. This is the fail-closed rule: a
// future Claude event we have never seen must not appear raw in the meeting.
func TestClassifyDropsUnknownClaudeEventByFingerprint(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_delta"},"session_id":"s-1"}`
	if got, class, _ := classifyEventLine(line); class != lineDrop || got != "" {
		t.Errorf("an unknown Claude event must be dropped by its session_id fingerprint, got (%q, %d)", got, class)
	}
}

func TestNormalizeLiveLine(t *testing.T) {
	// Transport with no human text is dropped (keep=false), so the observer's idle
	// notice can still tell a quiet meeting from a busy one.
	if _, keep := normalizeLiveLine(fxClaudeSystemInit); keep {
		t.Error("a system line must be dropped from the live view")
	}
	if _, keep := normalizeLiveLine(fxClaudeRateLimit); keep {
		t.Error("a rate-limit line must be dropped from the live view")
	}
	// Assistant prose is kept.
	if text, keep := normalizeLiveLine(fxClaudeAssistantText); !keep || text != "The cache should be write-through." {
		t.Errorf("assistant prose must be kept: (%q, %v)", text, keep)
	}
	// Plain output passes through with keep=true.
	if text, keep := normalizeLiveLine("plain line"); !keep || text != "plain line" {
		t.Errorf("plain output must pass through: (%q, %v)", text, keep)
	}
}

// A whole captured turn — the concatenated stdout of a one-shot — is filtered to
// just the assistant's words, dropping every machine envelope.
func TestNormalizeTurnTextClaudeStream(t *testing.T) {
	raw := strings.Join([]string{
		fxClaudeSystemInit,
		fxClaudeAssistantMixed, // "Let me check the file." + a tool_use
		fxClaudeUserToolResult,
		fxClaudeRateLimit,
		fxClaudeAssistantText, // "The cache should be write-through."
		fxClaudeResult,
	}, "\n")
	got := normalizeTurnText(raw)
	want := "Let me check the file.\nThe cache should be write-through."
	if got != want {
		t.Errorf("normalizeTurnText =\n%q\nwant\n%q", got, want)
	}
	for _, leak := range []string{"session_id", "rate_limit", "tool_use", "tool_result", "system", "input_tokens"} {
		if strings.Contains(got, leak) {
			t.Errorf("machine transport leaked into the turn: %q present in %q", leak, got)
		}
	}
}

func TestNormalizeTurnTextNDJSON(t *testing.T) {
	raw := strings.Join([]string{fxNDJSONTurnStart, fxNDJSONToolCall, fxNDJSONTurnEnd}, "\n")
	if got := normalizeTurnText(raw); got != "Write-through, to avoid lost writes." {
		t.Errorf("ndjson turn = %q", got)
	}
}

// Output with no recognized transport line is returned byte-for-byte, so running
// the seam on already-clean text (a marker, a human note, a turn a fixed build
// already stored clean) is an exact no-op.
func TestNormalizeTurnTextPreservesPlainOutput(t *testing.T) {
	cases := []string{
		"",
		"a single plain line",
		"line one\nline two\nline three",
		"a paragraph with a { brace but no json envelope\nand a second line",
		`{"answer":42}`, // a foreign JSON answer, not our transport
		"mixed prose\n{\"answer\":42}\nmore prose",
	}
	for _, in := range cases {
		if got := normalizeTurnText(in); got != in {
			t.Errorf("plain output must be preserved exactly:\n in  = %q\n got = %q", in, got)
		}
	}
}

// Foreign JSON that only RESEMBLES a configured transport envelope is a real
// answer from another agent/harness, and normalizeTurnText must return it
// byte-for-byte. A type=="assistant" line with no session_id is not Claude
// transport; a turn.end with no configured status is not ycode transport.
// Extracting text out of either would silently eat the answer.
func TestNormalizeTurnTextPreservesForeignLookAlikeJSON(t *testing.T) {
	cases := map[string]string{
		"foreign assistant (no session_id)": fxForeignAssistant,
		"foreign turn.end (no status)":      fxForeignTurnEnd,
		"foreign turn.end among prose": strings.Join([]string{
			"here is the structured result:",
			fxForeignTurnEnd,
		}, "\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := normalizeTurnText(in); got != in {
				t.Errorf("foreign answer must be preserved exactly:\n in  = %q\n got = %q", in, got)
			}
		})
	}
}

// A turn that interleaves genuine prose with transport keeps the prose and drops
// only the transport.
func TestNormalizeTurnTextMixedProseAndTransport(t *testing.T) {
	raw := strings.Join([]string{
		"a preamble the harness printed as plain text",
		fxClaudeSystemInit,
		fxClaudeAssistantText,
		fxClaudeResult,
	}, "\n")
	got := normalizeTurnText(raw)
	want := "a preamble the harness printed as plain text\nThe cache should be write-through."
	if got != want {
		t.Errorf("mixed turn =\n%q\nwant\n%q", got, want)
	}
}

// --- streaming boundaries -------------------------------------------------

// The live writer receives whatever chunks a process flushes, which do NOT align
// with JSON envelopes. It must buffer to a whole line before classifying, then
// show only the assistant prose and drop the surrounding transport — even when a
// single envelope is split across two writes.
func TestLiveWriterFiltersStreamJSONAcrossChunkBoundaries(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "claude-fable5", "", "")
	// A system line, then an assistant envelope torn across two writes, then a
	// result line — exactly how a flush might split the stream.
	w.Write([]byte(fxClaudeSystemInit + "\n" + `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text",`))
	w.Write([]byte(`"text":"streamed answer"}]},"session_id":"s-1"}` + "\n" + fxClaudeResult + "\n"))
	w.close(statusOK)

	path, err := livePath(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := readLive(&lineTail{path: path})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, l := range lines {
		if l.Kind == liveLine {
			texts = append(texts, l.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "streamed answer" {
		t.Fatalf("only the assistant prose may reach the live channel, got %q", texts)
	}
	// And nothing machine-shaped survived.
	for _, l := range lines {
		if strings.Contains(l.Text, "session_id") || strings.Contains(l.Text, "\"type\"") {
			t.Errorf("transport leaked to the live channel: %q", l.Text)
		}
	}
}

// liveTexts reads the live channel and returns just the liveLine texts.
func liveTexts(t *testing.T, id string) []string {
	t.Helper()
	path, err := livePath(id)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := readLive(&lineTail{path: path})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, l := range lines {
		if l.Kind == liveLine {
			texts = append(texts, l.Text)
		}
	}
	return texts
}

func countText(texts []string, want string) int {
	n := 0
	for _, s := range texts {
		if s == want {
			n++
		}
	}
	return n
}

// --- cross-channel deduplication ------------------------------------------

// ycode reports each turn TWICE into the writer chat.Invoke hands it: once as
// prose on stdout (the Stream tee) and once as the `text` of its turn.end event
// (the side-file drained by streamEventFile into the EventStream sink). The two
// arrive on DIFFERENT sinks of one liveWriter, so without cross-channel dedup the
// same answer is shown to a watcher and stored in live.jsonl twice. The live view
// must show the answer once — the cross-channel echo is plumbing, not a second
// thing the agent said.
func TestLiveWriterDropsCrossChannelDuplicate(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	// answer is what ycode both prints and reports; it matches fxNDJSONTurnEnd.
	const answer = "Write-through, to avoid lost writes."

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()
	// stdout prose (the Stream tee), then the turn.end event carrying the SAME
	// words (drained from the event side-file into the SEPARATE event sink).
	w.Write([]byte(answer + "\n"))
	ev.Write([]byte(fxNDJSONTurnEnd + "\n"))
	w.close(statusOK)

	texts := liveTexts(t, st.ID)
	if got := countText(texts, answer); got != 1 {
		t.Fatalf("the answer must reach the live channel exactly once, got %d occurrence(s): %q", got, texts)
	}
	for _, l := range texts {
		if strings.Contains(l, "turn.end") || strings.Contains(l, "\"data\"") {
			t.Errorf("transport leaked to the live channel: %q", l)
		}
	}
}

// The dedup is turn/channel-aware, not a blanket "show each line once": prose an
// agent deliberately repeats on its OWN channel is real content and must survive.
// Only the SAME words arriving on the OTHER channel are suppressed.
func TestLiveWriterKeepsSameChannelRepeats(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	// The agent says the same emphatic line twice on stdout — intentional prose.
	w.Write([]byte("Ship it.\n"))
	w.Write([]byte("Ship it.\n"))
	w.close(statusOK)

	texts := liveTexts(t, st.ID)
	if got := countText(texts, "Ship it."); got != 2 {
		t.Fatalf("intentional same-channel repeats must be preserved, got %d: %q", got, texts)
	}
}

// A turn.end event carries the whole answer as one envelope even when that answer
// already streamed to stdout as several separate lines. The dedup works
// line-by-line, so the multi-line echo is recognized as the same content and the
// watcher sees each line once, not twice in a different shape.
func TestLiveWriterDropsMultiLineCrossChannelDuplicate(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()
	// stdout streamed the answer as two lines...
	w.Write([]byte("First point.\nSecond point.\n"))
	// ...and the turn.end event (on the SEPARATE event sink) reports the same two
	// lines as one envelope. Without line-by-line dedup that envelope slips through
	// as a third, differently-shaped event ("First point.\nSecond point."), so the
	// watcher sees the answer twice.
	ev.Write([]byte(`{"type":"turn.end","data":{"status":"ok","text":"First point.\nSecond point."}}` + "\n"))
	w.close(statusOK)

	texts := liveTexts(t, st.ID)
	if len(texts) != 2 {
		t.Fatalf("only the two streamed lines may reach the live channel, got %q", texts)
	}
	// The content must appear exactly once total — no combined-envelope echo.
	if got := strings.Count(strings.Join(texts, "\n"), "First point."); got != 1 {
		t.Errorf("first line must appear once across all events, got %d: %q", got, texts)
	}
	if got := strings.Count(strings.Join(texts, "\n"), "Second point."); got != 1 {
		t.Errorf("second line must appear once across all events, got %d: %q", got, texts)
	}
}

// --- adversarial framing: partial prose vs. a complete event -------------

// The load-bearing case the single-buffer design got wrong: a stdout chunk that
// ends MID-LINE, followed by a COMPLETE event line on the side-channel, before the
// rest of the prose line arrives. With one shared partial-line buffer the two
// splice into "the cache should be {\"type\":\"turn.end\"…}", which no longer
// matches any transport fingerprint — so it classifies as prose and the raw JSON
// envelope leaks to the watcher and into live.jsonl. Separate per-source framing
// buffers make the splice impossible: each sink keeps its own line boundaries.
func TestLiveWriterPartialProseThenEventDoesNotSplice(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()

	// stdout flushes a chunk that ends mid-line (no newline)...
	w.Write([]byte("the cache should be "))
	// ...and before the rest of that line arrives, the event side-channel delivers
	// a COMPLETE turn.end envelope on its OWN sink.
	ev.Write([]byte(fxNDJSONTurnEnd + "\n"))
	// the rest of the stdout line finally lands.
	w.Write([]byte("write-through.\n"))
	w.close(statusOK)

	texts := liveTexts(t, st.ID)
	// The prose line is whole and clean — never spliced with the event envelope.
	if got := countText(texts, "the cache should be write-through."); got != 1 {
		t.Fatalf("the stdout line must be framed intact and appear once, got: %q", texts)
	}
	// The event's assistant text appears (distinct prose, not a cross-channel dup).
	if got := countText(texts, "Write-through, to avoid lost writes."); got != 1 {
		t.Errorf("the event's assistant text must appear once, got: %q", texts)
	}
	// No raw transport anywhere — not even a fragment of the envelope.
	for _, l := range texts {
		for _, leak := range []string{"turn.end", `"data"`, `"type"`, `"status"`} {
			if strings.Contains(l, leak) {
				t.Fatalf("raw transport leaked into the live channel: %q", l)
			}
		}
	}
}

// The two sinks are written by two concurrent goroutines (chat.Invoke tees stdout
// while a separate goroutine drains the event side-file). This drives both at once
// — the stdout side ONE BYTE AT A TIME, so nearly every Write ends mid-line, the
// worst case for framing — and asserts that no line is ever torn, merged, or
// leaked as transport. Run with -race, it also pins the mutex discipline that
// guards the shared dedup map and the two buffers.
func TestLiveWriterConcurrentProseAndEventsStayFramed(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()

	const n = 40
	var wg sync.WaitGroup
	wg.Add(2)

	// stdout prose, written one byte per Write so most calls end mid-line.
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			line := fmt.Sprintf("prose line %02d\n", i)
			for j := 0; j < len(line); j++ {
				w.Write([]byte{line[j]})
			}
		}
	}()
	// structured events on the SEPARATE sink, concurrently, as whole lines (that is
	// how chat's streamEventFile delivers them).
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			ev.Write([]byte(fmt.Sprintf(`{"type":"turn.end","data":{"status":"ok","text":"event answer %02d"}}`+"\n", i)))
		}
	}()
	wg.Wait()
	w.close(statusOK)

	texts := liveTexts(t, st.ID)
	// No emitted line may carry raw transport: separate framing buffers make the
	// "half a prose line{json}" splice impossible.
	for _, l := range texts {
		for _, leak := range []string{"turn.end", `"data"`, `"type"`, `"status"`} {
			if strings.Contains(l, leak) {
				t.Fatalf("transport leaked into the live channel under concurrency: %q", l)
			}
		}
	}
	// Every prose line survives intact, exactly once, never merged with another.
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("prose line %02d", i)
		if got := countText(texts, want); got != 1 {
			t.Errorf("prose line %d must appear once intact, got %d occurrence(s)", i, got)
		}
	}
	// Every event's extracted text survives too — distinct from the prose, so
	// cross-channel dedup never drops it.
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("event answer %02d", i)
		if got := countText(texts, want); got != 1 {
			t.Errorf("event answer %d must appear once, got %d occurrence(s)", i, got)
		}
	}
}

// --- mirrored-answer ordering (the issue-288 REJECT blocker) --------------

// liveRenderedLines flattens the live channel to the sequence of lines a watcher
// would actually see: every liveLine's Text split on '\n', in the order recorded.
// A liveLine Text may be multi-line (a turn.end envelope carries the whole answer
// as one blob), and writeLive splits it the same way, so this is the honest view.
func liveRenderedLines(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, text := range liveTexts(t, id) {
		for ln := range strings.SplitSeq(text, "\n") {
			out = append(out, ln)
		}
	}
	return out
}

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertNoTransportLeak(t *testing.T, texts []string) {
	t.Helper()
	for _, l := range texts {
		for _, leak := range []string{"turn.end", `"data"`, `"type"`, `"status"`, "session_id"} {
			if strings.Contains(l, leak) {
				t.Fatalf("raw transport leaked into the live channel: %q", l)
			}
		}
	}
}

// THE issue-288 REJECT, reproduced exactly. A production-valid ycode answer
// "A\nA\nB" is mirrored on BOTH channels, and the transport interleaves it as:
// stdout prefix "A", then the COMPLETE turn.end event carrying the whole answer,
// then the remaining stdout "A\nB". The old text-key online dedup emitted A,B,A —
// it showed the event's not-yet-streamed "B" tail before stdout caught up. The
// live view must instead read A,A,B: stdout is the order-authoritative stream and
// the mirrored event is a cross-channel echo that adds nothing.
func TestLiveWriterMirroredAnswerPreservesOrder(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()

	w.Write([]byte("A\n"))                                                                 // stdout prefix
	ev.Write([]byte(`{"type":"turn.end","data":{"status":"ok","text":"A\nA\nB"}}` + "\n")) // whole answer mid-stream
	w.Write([]byte("A\nB\n"))                                                              // stdout remainder
	w.close(statusOK)

	got := liveRenderedLines(t, st.ID)
	want := []string{"A", "A", "B"}
	if !sameLines(got, want) {
		t.Fatalf("mirrored answer must stay in prose order: got %v want %v", got, want)
	}
	assertNoTransportLeak(t, liveTexts(t, st.ID))
}

// The same mirror, but stdout never streamed the tail — only the turn.end event
// carries the last two lines. The echoed prefix "A" is dropped (it mirrors the one
// streamed "A"), and the event-only remainder "A\nB" survives, appended after the
// prose so the total reads A,A,B. This pins "event-only output still survives"
// against the ordering fix: deferring the event channel must not eat content that
// prose never showed.
func TestLiveWriterPartiallyMirroredAnswerKeepsEventOnlyRemainder(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()

	w.Write([]byte("A\n")) // stdout streamed only the first line
	ev.Write([]byte(`{"type":"turn.end","data":{"status":"ok","text":"A\nA\nB"}}` + "\n"))
	w.close(statusOK)

	got := liveRenderedLines(t, st.ID)
	want := []string{"A", "A", "B"}
	if !sameLines(got, want) {
		t.Fatalf("event-only remainder must survive in order: got %v want %v", got, want)
	}
	assertNoTransportLeak(t, liveTexts(t, st.ID))
}

// No stdout mirror at all: the whole answer, repeats and all, arrives only on the
// turn.end event. Nothing is a cross-channel echo, so every line survives in the
// event's own order — a purely event-reporting turn is shown whole.
func TestLiveWriterEventOnlyAnswerSurvivesInOrder(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()

	ev.Write([]byte(`{"type":"turn.end","data":{"status":"ok","text":"A\nA\nB"}}` + "\n"))
	w.close(statusOK)

	got := liveRenderedLines(t, st.ID)
	want := []string{"A", "A", "B"}
	if !sameLines(got, want) {
		t.Fatalf("event-only answer must survive whole in order: got %v want %v", got, want)
	}
	assertNoTransportLeak(t, liveTexts(t, st.ID))
}

// The mirrored answer under CONCURRENCY: one goroutine streams stdout a byte at a
// time (worst-case framing, most writes end mid-line) while another delivers the
// whole turn.end envelope, racing. However they interleave, the deferred event
// channel is reconciled only at close against the complete prose stream, so the
// live view is DETERMINISTICALLY A,A,B once — never A,B,A, never a torn or
// duplicated line, never raw transport. Run under -race it also pins the mutex
// discipline over proseSeen, eventPending, and the two framing buffers.
func TestLiveWriterMirroredAnswerConcurrentPreservesOrder(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	w := newLiveWriter(st, "ycode", "", "")
	ev := w.eventStream()

	const answer = "A\nA\nB\n"
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for j := 0; j < len(answer); j++ {
			w.Write([]byte{answer[j]})
		}
	}()
	go func() {
		defer wg.Done()
		ev.Write([]byte(`{"type":"turn.end","data":{"status":"ok","text":"A\nA\nB"}}` + "\n"))
	}()
	wg.Wait()
	w.close(statusOK)

	got := liveRenderedLines(t, st.ID)
	want := []string{"A", "A", "B"}
	if !sameLines(got, want) {
		t.Fatalf("mirrored answer under concurrency must stay A,A,B: got %v", got)
	}
	assertNoTransportLeak(t, liveTexts(t, st.ID))
}

// --- default observe filtering (record path) ------------------------------

// A meeting recorded by an OLDER build still holds raw Claude transport in its
// transcript Text. The default observe view must show only the assistant prose,
// never the system/rate-limit/result envelopes — normalization at render is what
// fixes an already-recorded meeting.
func TestObserveDefaultFiltersRecordedTransport(t *testing.T) {
	st := testState()
	pinStore(t, st)
	raw := strings.Join([]string{
		fxClaudeSystemInit,
		fxClaudeRateLimit,
		fxClaudeAssistantText,
		fxClaudeResult,
	}, "\n")
	turn(t, st.ID, Event{Round: 1, Speaker: "claude-fable5", Kind: "turn", Text: raw, TS: time.Now()})

	var out, errW bytes.Buffer
	if err := observeMeeting(context.Background(), &out, &errW, st, observeOpts{follow: false}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "The cache should be write-through.") {
		t.Errorf("the assistant prose must be shown:\n%s", got)
	}
	for _, leak := range []string{"session_id", "rate_limit_event", "\"type\":\"system\"", "input_tokens", "subtype"} {
		if strings.Contains(got, leak) {
			t.Errorf("raw transport %q leaked into the default observe view:\n%s", leak, got)
		}
	}
}

// --- --json behavior ------------------------------------------------------

// `observe --json` emits the CANONICAL meeting Event with the assistant prose in
// its Text — not the raw nested provider transport. A parser piping the stream
// gets clean, readable text and the meeting-event schema, never the Claude
// envelope.
func TestObserveJSONEmitsCanonicalEventWithNormalizedText(t *testing.T) {
	st := testState()
	pinStore(t, st)
	raw := strings.Join([]string{fxClaudeSystemInit, fxClaudeAssistantText, fxClaudeResult}, "\n")
	turn(t, st.ID, Event{Round: 1, Speaker: "claude-fable5", Kind: "turn", Text: raw, TS: time.Now()})

	var out, errW bytes.Buffer
	if err := observeMeeting(context.Background(), &out, &errW, st, observeOpts{follow: false, json: true}); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(out.String())
	var e Event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("--json must emit a canonical meeting Event per line: %v\n%s", err, line)
	}
	if e.Kind != "turn" || e.Speaker != "claude-fable5" {
		t.Errorf("the canonical event fields must survive: %+v", e)
	}
	if e.Text != "The cache should be write-through." {
		t.Errorf("--json Text must be the normalized prose, got %q", e.Text)
	}
	if strings.Contains(line, "session_id") || strings.Contains(line, "rate_limit") {
		t.Errorf("--json leaked raw provider transport:\n%s", line)
	}
}

// --- error path (failed-turn record) --------------------------------------

// A turn that FAILED still has transport on its stdout: a Claude/ycode call that
// streamed some words and then errored leaves stream-json / NDJSON envelopes in
// `out`. The error path builds the transcript marker from that stdout, so it must
// normalize FIRST — otherwise the raw envelope (session_id, {"type":"system"…},
// turn.start/tool.call) becomes the recorded failure reason and, replayed, poisons
// the transcript, the minutes, the next agent's context, and the default/--json
// views. This pins the error path to the same normalize-before-store rule the
// success path already follows.
func TestClassifyTurnErrorPathNormalizesTransport(t *testing.T) {
	st := testState()
	st.Round = 1

	t.Run("claude keeps prose, drops transport", func(t *testing.T) {
		out := strings.Join([]string{fxClaudeSystemInit, fxClaudeAssistantText, fxClaudeResult}, "\n")
		ev := classifyTurn(st, "claude-fable5", "q", out, 1, errors.New("claude exited with code 1"), time.Second, 0)
		if ev.Status != statusError {
			t.Fatalf("a failed turn must be statusError, got %q", ev.Status)
		}
		if !strings.Contains(ev.Text, "The cache should be write-through.") {
			t.Errorf("the assistant prose from the failed stream must survive: %q", ev.Text)
		}
		for _, leak := range []string{"session_id", "\"type\"", "subtype", "input_tokens", "rate_limit"} {
			if strings.Contains(ev.Text, leak) {
				t.Errorf("raw transport %q leaked into the failure marker: %q", leak, ev.Text)
			}
		}
	})

	t.Run("ycode transport with no words falls back to the error", func(t *testing.T) {
		out := strings.Join([]string{fxNDJSONTurnStart, fxNDJSONToolCall}, "\n")
		ev := classifyTurn(st, "ycode", "q", out, 1, errors.New("ycode: storage is locked"), time.Second, 0)
		if ev.Status != statusError {
			t.Fatalf("a failed turn must be statusError, got %q", ev.Status)
		}
		if !strings.Contains(ev.Text, "storage is locked") {
			t.Errorf("with no human words, the marker must carry the error, got %q", ev.Text)
		}
		for _, leak := range []string{"turn.start", "tool.call", "\"data\"", "read_file"} {
			if strings.Contains(ev.Text, leak) {
				t.Errorf("raw transport %q leaked into the failure marker: %q", leak, ev.Text)
			}
		}
	})
}

// A live turn recorded as raw transport by an older build must also be filtered
// when replayed through writeLive (the mid-stream path), not just the record path.
func TestWriteLiveNormalizesRecordedTransport(t *testing.T) {
	streamed := map[string]bool{turnKey(1, "claude-fable5"): true}
	var buf bytes.Buffer

	// A dropped transport line shows nothing.
	if writeLive(&buf, LiveEvent{Kind: liveLine, Round: 1, Speaker: "claude-fable5", Text: fxClaudeSystemInit}, streamed) {
		t.Error("a system transport line must not render")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should have been written for transport, got %q", buf.String())
	}

	// An assistant prose line renders its text.
	if !writeLive(&buf, LiveEvent{Kind: liveLine, Round: 1, Speaker: "claude-fable5", Text: fxClaudeAssistantText}, streamed) {
		t.Error("assistant prose must render")
	}
	if got := buf.String(); !strings.Contains(got, "The cache should be write-through.") || strings.Contains(got, "session_id") {
		t.Errorf("writeLive text = %q", got)
	}
}

// --- codex `--json` ---------------------------------------------------------

// The exact lines codex 0.141 emits, captured from a real meeting turn. Before
// these were recognized, a whole codex turn — thread id, every shell command it
// ran, its token usage — was stored and DISPLAYED verbatim as the agent's
// message.
const (
	fxCodexThread   = `{"type":"thread.started","thread_id":"01a06b24-5eba-7ae2-8b94-347dbe1aa92a"}`
	fxCodexTurnUp   = `{"type":"turn.started"}`
	fxCodexMsg      = `{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"The sh lane is green."}}`
	fxCodexCmd      = `{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"git status"}}`
	fxCodexTurnDone = `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}`
)

func TestClassifyCodexTransport(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		want  string
		class lineClass
	}{
		{"thread id", fxCodexThread, "", lineDrop},
		{"turn started", fxCodexTurnUp, "", lineDrop},
		{"agent message", fxCodexMsg, "The sh lane is green.", lineText},
		{"command execution", fxCodexCmd, "", lineDrop},
		{"turn completed", fxCodexTurnDone, "", lineDrop},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, class, _ := classifyEventLine(c.line)
			if class != c.class || got != c.want {
				t.Errorf("classifyEventLine = (%q, %d), want (%q, %d)", got, class, c.want, c.class)
			}
		})
	}
}

// A whole codex turn reduces to the words, in order.
func TestNormalizeCodexTurn(t *testing.T) {
	raw := strings.Join([]string{fxCodexThread, fxCodexTurnUp, fxCodexCmd, fxCodexMsg, fxCodexTurnDone}, "\n")
	if got := normalizeTurnText(raw); got != "The sh lane is green." {
		t.Errorf("normalizeTurnText = %q, want the agent message alone", got)
	}
}

// A partial item superseded by its completed form is ONE message, not two — the
// answer must not be preceded by every prefix of itself.
func TestNormalizeCodexSupersedesPartialItem(t *testing.T) {
	partial := `{"type":"item.updated","item":{"id":"item_0","type":"agent_message","text":"The sh"}}`
	raw := strings.Join([]string{fxCodexTurnUp, partial, fxCodexMsg}, "\n")
	if got := normalizeTurnText(raw); got != "The sh lane is green." {
		t.Errorf("normalizeTurnText = %q, want only the completed item", got)
	}
}

// The fingerprint rule, applied to codex: a foreign answer that merely borrows an
// event name — without the shape codex actually emits — is PRESERVED. Eating a
// real answer is the worst failure this seam has.
func TestClassifyPreservesCodexLookalikes(t *testing.T) {
	for _, line := range []string{
		`{"type":"thread.started","summary":"how I started the thread"}`,
		`{"type":"turn.started","text":"my answer about turns"}`,
		`{"type":"turn.completed","answer":"done, and here is why"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"no id"}}`,
	} {
		got, class, _ := classifyEventLine(line)
		if class != linePlain || got != line {
			t.Errorf("a codex look-alike must be preserved: %s -> (%q, %d)", line, got, class)
		}
	}
}

// --- opencode `--format json` -----------------------------------------------

const (
	fxOpencodeStep = `{"type":"step_start","timestamp":1786578934733,"sessionID":"ses_007","part":{"id":"prt_1","messageID":"msg_1","sessionID":"ses_007","type":"step-start"}}`
	fxOpencodeTool = `{"type":"tool_use","timestamp":1786578938126,"sessionID":"ses_007","part":{"type":"tool","tool":"read","callID":"call_1","state":{"status":"completed"}}}`
	fxOpencodeText = `{"type":"text","timestamp":1786578956736,"sessionID":"ses_007","part":{"id":"prt_9","messageID":"msg_2","sessionID":"ses_007","type":"text","text":"Two weave campaigns, one foreman track."}}`
)

func TestClassifyOpencodeTransport(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		want  string
		class lineClass
	}{
		{"step start", fxOpencodeStep, "", lineDrop},
		{"tool use", fxOpencodeTool, "", lineDrop},
		{"text part", fxOpencodeText, "Two weave campaigns, one foreman track.", lineText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, class, _ := classifyEventLine(c.line)
			if class != c.class || got != c.want {
				t.Errorf("classifyEventLine = (%q, %d), want (%q, %d)", got, class, c.want, c.class)
			}
		})
	}
}

func TestNormalizeOpencodeTurn(t *testing.T) {
	raw := strings.Join([]string{fxOpencodeStep, fxOpencodeTool, fxOpencodeText}, "\n")
	if got := normalizeTurnText(raw); got != "Two weave campaigns, one foreman track." {
		t.Errorf("normalizeTurnText = %q, want the text part alone", got)
	}
}

// opencode re-emits a growing text part as the model streams. Without
// supersession the record would read "Two" / "Two weave" / "Two weave campaigns"
// stacked above the answer.
func TestNormalizeOpencodeSupersedesGrowingPart(t *testing.T) {
	grow := func(text string) string {
		return `{"type":"text","timestamp":1,"sessionID":"ses_007","part":{"id":"prt_9","type":"text","text":"` + text + `"}}`
	}
	raw := strings.Join([]string{grow("Two"), grow("Two weave"), grow("Two weave campaigns.")}, "\n")
	if got := normalizeTurnText(raw); got != "Two weave campaigns." {
		t.Errorf("normalizeTurnText = %q, want only the final form of the streamed part", got)
	}
}

// An unenumerated opencode event still carries the sessionID+part fingerprint, so
// it is dropped rather than leaked — the same fail-closed rule Claude gets.
func TestClassifyDropsUnknownOpencodeEventByFingerprint(t *testing.T) {
	line := `{"type":"reasoning","timestamp":1,"sessionID":"ses_007","part":{"type":"reasoning","text":"interior"}}`
	if got, class, _ := classifyEventLine(line); class != lineDrop || got != "" {
		t.Errorf("an unknown opencode event must be dropped by its fingerprint, got (%q, %d)", got, class)
	}
}

// …but a foreign answer that names an opencode event WITHOUT the fingerprint is
// preserved.
func TestClassifyPreservesOpencodeLookalike(t *testing.T) {
	line := `{"type":"text","part":"the part I played"}`
	if got, class, _ := classifyEventLine(line); class != linePlain || got != line {
		t.Errorf("an opencode look-alike must be preserved, got (%q, %d)", got, class)
	}
}

// A turn RECORDED before the transport was recognized still holds the raw
// envelope on disk. The record is not rewritten, so the reader's seam is what
// has to fix the display — the web room read stored bytes straight through and
// showed a wall of JSON for every such turn.
func TestRenderEventNormalizesAStoredRawTurn(t *testing.T) {
	raw := strings.Join([]string{fxCodexThread, fxCodexCmd, fxCodexMsg, fxCodexTurnDone}, "\n")
	got := renderEvent(Event{Kind: "turn", Speaker: "codex", Text: raw}, false)
	if got.Text != "The sh lane is green." {
		t.Errorf("Text = %q, want the prose", got.Text)
	}
	if got.Raw != "" {
		t.Errorf("Raw = %q, want it withheld unless the reader asked for it", got.Raw)
	}
}

// ?raw=1 is the debugging view: the prose AND what it was extracted from.
func TestRenderEventCarriesRawOnlyWhenAsked(t *testing.T) {
	raw := strings.Join([]string{fxCodexMsg, fxCodexTurnDone}, "\n")
	got := renderEvent(Event{Kind: "turn", Speaker: "codex", Text: raw}, true)
	if got.Text != "The sh lane is green." || got.Raw != raw {
		t.Errorf("renderEvent(raw=true) = (%q, %q), want the prose plus the original", got.Text, got.Raw)
	}
}

// An ordinary message is not transport, so the debug view must not duplicate it:
// Raw marks "this WAS an envelope", it is not a copy of every event.
func TestRenderEventLeavesPlainTextAlone(t *testing.T) {
	got := renderEvent(Event{Kind: "human", Speaker: "qiangli", Text: "ship it"}, true)
	if got.Text != "ship it" || got.Raw != "" {
		t.Errorf("renderEvent = (%q, %q), want the text unchanged and no raw copy", got.Text, got.Raw)
	}
}

// Raw is a response-only projection, not trusted input. In particular, turning
// the debug view back off must not leak a Raw value attached by an earlier
// render (or accidentally supplied by another in-process caller).
func TestRenderEventClearsRawWhenDebugIsOff(t *testing.T) {
	got := renderEvent(Event{Kind: "turn", Speaker: "codex", Text: "answer", Raw: "transport"}, false)
	if got.Text != "answer" || got.Raw != "" {
		t.Errorf("renderEvent = (%q, %q), want prose with raw withheld", got.Text, got.Raw)
	}
}
