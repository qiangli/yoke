package chat

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Vendor event-stream decoding for the two harnesses whose stdout is a JSON event
// stream with no `type` key: Muse Code (`exec --json`, MSP records) and AGY
// (`step_update` events). pkg/cligw owns the capture-side decoder for the same
// shapes (eventText/terminalText) but cannot be imported here — it drags the whole
// LLM gateway in — so the field names below mirror it. Update both together.
//
// Recognition is fingerprint-bound and fail-closed in the same way meet's
// normalizer is: a foreign JSON answer that merely resembles an envelope is
// StreamPlain, never eaten.

// StreamKind is what DecodeStreamLine decided one line is.
type StreamKind int

const (
	// StreamPlain is anything not recognized as a Muse/AGY envelope.
	StreamPlain StreamKind = iota
	// StreamMeta is recognized transport carrying no answer text: lifecycle,
	// status, usage, init, tool and thinking records. It is machinery, not prose.
	StreamMeta
	// StreamDelta is a fragment of the answer. Text is verbatim — never trimmed,
	// because the whitespace between fragments is part of the answer.
	StreamDelta
	// StreamFinal is a complete answer snapshot that REPEATS the deltas already
	// streamed. A consumer uses it only when no delta was seen.
	StreamFinal
)

// StreamLine is one decoded line.
type StreamLine struct {
	Vendor string // "muse" | "agy" | ""
	Kind   StreamKind
	Text   string
	// Activity is set on a StreamMeta record that is a real agent ACTION or a
	// FAILURE (a tool call/result, an error, a failed task) rather than lifecycle
	// bookkeeping. It is a signature with ids, sequence numbers, timestamps and
	// numeric fields removed, so the same action repeated is the same Activity —
	// which is what lets the coach see a genuine loop through transport it
	// otherwise ignores. Empty for machinery, thinking and usage.
	Activity string
	// Failed reports that the record itself says it failed.
	Failed bool
}

// DecodeStreamLine classifies one physical line of agent stdout.
func DecodeStreamLine(line string) StreamLine {
	s := strings.TrimSpace(line)
	if s == "" || s[0] != '{' {
		return StreamLine{}
	}
	var p struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
		Stream        json.RawMessage `json:"stream"`
		RecordType    string          `json:"record_type"`
		PayloadType   string          `json:"payload_type"`
		Payload       json.RawMessage `json:"payload"`

		Event json.RawMessage `json:"event"`
	}
	if json.Unmarshal([]byte(s), &p) != nil {
		return StreamLine{}
	}
	if isJSONNumber(p.SchemaVersion) && isJSONObject(p.Stream) && isJSONObject(p.Payload) &&
		p.RecordType != "" && p.PayloadType != "" {
		return decodeMuse(p.PayloadType, p.Payload)
	}
	var event string
	if json.Unmarshal(p.Event, &event) == nil && event != "" {
		var top map[string]json.RawMessage
		if json.Unmarshal([]byte(s), &top) != nil {
			return StreamLine{}
		}
		return decodeAGY(top, event)
	}
	return StreamLine{}
}

func decodeMuse(payloadType string, payload json.RawMessage) StreamLine {
	out := StreamLine{Vendor: "muse", Kind: StreamMeta}
	var body struct {
		Text string `json:"text"`
	}
	switch {
	case payloadType == "run.output.delta":
		if json.Unmarshal(payload, &body) == nil && body.Text != "" {
			out.Kind, out.Text = StreamDelta, body.Text
		}
	case strings.HasPrefix(payloadType, "run.terminal."):
		if json.Unmarshal(payload, &body) == nil && strings.TrimSpace(body.Text) != "" {
			out.Kind, out.Text = StreamFinal, body.Text
		}
	default:
		out.Activity, out.Failed = museActivity(payloadType, payload)
	}
	return out
}

var (
	reMuseInterior = regexp.MustCompile(`(?:^|[._])(?:thought|thinking|reasoning|usage|token|tokens|cost|heartbeat|ping)(?:$|[._])`)
	reMuseAction   = regexp.MustCompile(`(?:^|[._])(?:tool|tools|tool_call|tool_result|exec|shell|action)(?:$|[._])`)
	reMuseFailure  = regexp.MustCompile(`(?:^|[._])(?:failed|failure|error|errored|denied|rejected|timeout|timed_out)$`)
	reMuseFailKind = regexp.MustCompile(`^(?:failed|failure|error|errored|denied|rejected|timed_out)$`)
)

// museActivity decides whether a non-answer MSP record is meaningful to a loop
// detector. Lifecycle bookkeeping (task proposed/accepted/scheduled/started/
// completed, stream links, side-effect intents, model config, status) repeats
// identically in every healthy task and is NOT activity; thinking and usage are
// interior state. A tool/command/action record, or any record that says it
// failed, is. The signature is volatile-stripped so ids/sequence/timestamps do
// not make a repeated action look new.
func museActivity(payloadType string, payload json.RawMessage) (sig string, failed bool) {
	pt := strings.ToLower(payloadType)
	if reMuseInterior.MatchString(pt) {
		return "", false
	}
	var v map[string]any
	if json.Unmarshal(payload, &v) != nil {
		return "", false
	}
	failed = reMuseFailure.MatchString(pt) || jsonReportsError(v)
	if ev, ok := v["event"].(map[string]any); ok {
		if k, _ := ev["kind"].(string); reMuseFailKind.MatchString(strings.ToLower(k)) || jsonReportsError(ev) {
			failed = true
		}
	}
	if st, _ := v["status"].(string); reMuseFailKind.MatchString(strings.ToLower(st)) {
		failed = true
	}
	if !failed && !reMuseAction.MatchString(pt) {
		return "", false
	}
	b, err := json.Marshal(stripVolatile(v))
	if err != nil {
		return "", false
	}
	sig = "muse " + pt + " " + string(b)
	if len(sig) > 512 {
		sig = sig[:200] + "#" + hashInput([]byte(sig))
	}
	return sig, failed
}

// decodeAGY binds each AGY event to the envelope fields its stream documents
// (docs/validation/cligw-readiness-1230.md; pkg/cligw/testdata/agy-stream.jsonl):
// step_update carries step_type plus step_index/state, init carries the model (or
// a conversation_id), result carries status. The bare `event` name plus an object
// is far too common to claim on its own — `{"event":"init","init":{"x":1}}` is
// just as plausible in an agent's own JSON answer.
func decodeAGY(top map[string]json.RawMessage, event string) StreamLine {
	out := StreamLine{Vendor: "agy", Kind: StreamMeta}
	// An AGY envelope is {"event":E, E:{...}} and nothing else (a conversation_id
	// sibling is tolerated). Extra top-level keys mean it is someone else's JSON.
	for k := range top {
		if k != "event" && k != event && k != "conversation_id" {
			return StreamLine{}
		}
	}
	body := top[event]
	if !isJSONObject(body) {
		return StreamLine{}
	}
	switch event {
	case "init":
		var in struct {
			Model          string `json:"model"`
			ConversationID string `json:"conversation_id"`
		}
		if json.Unmarshal(body, &in) != nil || (in.Model == "" && in.ConversationID == "") {
			return StreamLine{}
		}
	case "step_update":
		var step struct {
			StepIndex *float64 `json:"step_index"`
			State     string   `json:"state"`
			StepType  string   `json:"step_type"`
			TextDelta string   `json:"text_delta"`
		}
		if json.Unmarshal(body, &step) != nil || step.StepType == "" || (step.StepIndex == nil && step.State == "") {
			return StreamLine{}
		}
		// Only agent_response steps are the answer; user_input echoes the prompt and
		// thinking is interior state.
		if step.StepType == "agent_response" && step.TextDelta != "" {
			out.Kind, out.Text = StreamDelta, step.TextDelta
		}
	case "result":
		var res struct {
			Status   string `json:"status"`
			Response string `json:"response"`
		}
		if json.Unmarshal(body, &res) != nil || res.Status == "" {
			return StreamLine{}
		}
		if strings.TrimSpace(res.Response) != "" {
			out.Kind, out.Text = StreamFinal, res.Response
		}
		if strings.EqualFold(res.Status, "error") || strings.EqualFold(res.Status, "failed") {
			out.Failed = true
		}
	default:
		return StreamLine{}
	}
	return out
}

func isJSONObject(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s[0] == '{'
}

func isJSONNumber(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && (s[0] >= '0' && s[0] <= '9')
}

var (
	reUUIDish     = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reLongHex     = regexp.MustCompile(`(?i)^[0-9a-f]{16,}$`)
	reVolatileKey = regexp.MustCompile(`^(?:.*_)?(?:id|ids|uuid|seq|sequence|ts|time|timestamp|date|created|updated|recorded|elapsed|duration|latency|at|ms|ns|us)$`)
	reCamelID     = regexp.MustCompile(`[a-z](?:Id|ID)$`)
)

// semanticJSONLine reduces an arbitrary JSON-object line to a signature of WHAT it
// says, dropping the fields that make every event unique without making it
// different: ids, sequence numbers and timestamps. Two events that differ only in
// those are the same action repeated, and counting them as distinct is exactly how
// a loop of structured events hides from the coach. ok is false for anything that
// is not a JSON object.
func semanticJSONLine(line string) (sig string, size int, isErr bool, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || s[0] != '{' {
		return "", 0, false, false
	}
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) != nil {
		return "", 0, false, false
	}
	isErr = jsonReportsError(v)
	b, err := json.Marshal(stripVolatile(v))
	if err != nil {
		return "", 0, false, false
	}
	sig, size = string(b), len(b)
	if len(sig) > 512 {
		sig = sig[:200] + "#" + hashInput(b)
	}
	return sig, size, isErr, true
}

func stripVolatile(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if reVolatileKey.MatchString(strings.ToLower(k)) || reCamelID.MatchString(k) {
				continue
			}
			out[k] = stripVolatile(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = stripVolatile(val)
		}
		return out
	case float64:
		// A bare number is a measurement (count, size, attempt, token tally), not a
		// name; it varies between repeats of the same action.
		return 0
	case string:
		if reUUIDish.MatchString(x) || reLongHex.MatchString(x) {
			return "~"
		}
	}
	return v
}

// jsonReportsError is true only when the event itself says it failed. A key merely
// NAMED "error" holding null/false is the absence of an error, and matching the
// word in the serialized text would call every healthy envelope a failure.
func jsonReportsError(v map[string]any) bool {
	if b, _ := v["is_error"].(bool); b {
		return true
	}
	if e, present := v["error"]; present && e != nil && e != "" && e != false {
		return true
	}
	t, _ := v["type"].(string)
	t = strings.ToLower(t)
	return t == "error" || strings.HasSuffix(t, ".failed") || strings.HasSuffix(t, "_failed")
}
