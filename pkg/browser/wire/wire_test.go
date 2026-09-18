package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResultMarshalJSON_StructuredData covers the retro pain point:
// tools that pre-marshal a JSON object/array into Data must surface
// it as decoded JSON in the agent-facing envelope — not as a
// stringified string-of-JSON the caller has to parse twice.
func TestResultMarshalJSON_StructuredData(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		wantField string // exact JSON fragment that must appear under "data":
	}{
		{
			name:      "cookies array",
			data:      `[{"name":"sid","value":"abc","domain":".example.com","httpOnly":true}]`,
			wantField: `"data": [`,
		},
		{
			name:      "capabilities object",
			data:      `{"mode":"live","methods":["navigate","click"]}`,
			wantField: `"data": {`,
		},
		{
			name:      "storage_get object",
			data:      `{"foo":"bar","n":1}`,
			wantField: `"data": {`,
		},
		{
			name:      "indented object with leading whitespace",
			data:      "  \n{\n  \"k\": 1\n}",
			wantField: `"data": {`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := json.MarshalIndent(Result{Success: true, Data: tc.data}, "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			s := string(out)
			if !strings.Contains(s, tc.wantField) {
				t.Fatalf("expected %q in output, got:\n%s", tc.wantField, s)
			}
			// Round-trip: the marshaled envelope must itself be valid JSON
			// and the data field must decode as a structured value, not a
			// string.
			var env map[string]json.RawMessage
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatalf("envelope not valid JSON: %v\n%s", err, s)
			}
			raw, ok := env["data"]
			if !ok {
				t.Fatalf("data field missing; got:\n%s", s)
			}
			if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
				t.Fatalf("data field should be object/array, got %s", raw)
			}
		})
	}
}

// TestResultMarshalJSON_PlainText guards the legacy contract for tools
// that put human-readable strings in Data ("pressed=Enter",
// "tracing started", clipboard text, evaluate scalars). These must
// continue to serialize as JSON strings.
func TestResultMarshalJSON_PlainText(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"keyboard_press ack", "pressed=Enter"},
		{"wait_for_selector state", "state=visible"},
		{"perf_start ack", "tracing started"},
		{"clipboard text", "hello world"},
		{"eval string with quotes", `"page-title"`},
		{"eval scalar", "42"},
		{"eval boolean", "true"},
		{"eval null literal", "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := json.Marshal(Result{Success: true, Data: tc.data})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var env map[string]json.RawMessage
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatalf("envelope not valid JSON: %v\n%s", err, out)
			}
			raw, ok := env["data"]
			if !ok {
				t.Fatalf("data field missing; got: %s", out)
			}
			if raw[0] != '"' {
				t.Fatalf("plain-text data must serialize as JSON string, got %s", raw)
			}
			var got string
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("data not a JSON string: %v", err)
			}
			if got != tc.data {
				t.Fatalf("round-trip mismatch: got %q want %q", got, tc.data)
			}
		})
	}
}

// TestResultMarshalJSON_EmptyData ensures Data is omitted when empty
// (matches the legacy omitempty behavior).
func TestResultMarshalJSON_EmptyData(t *testing.T) {
	out, err := json.Marshal(Result{Success: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), `"data"`) {
		t.Fatalf("expected no data field for empty Data, got %s", out)
	}
}

// TestResultMarshalJSON_MalformedJSONLike treats data that LOOKS
// structured ('{' or '[' prefix) but isn't valid JSON as plain text.
// Better to surface the raw payload than to drop a malformed
// envelope on the agent.
func TestResultMarshalJSON_MalformedJSONLike(t *testing.T) {
	out, err := json.Marshal(Result{Success: true, Data: `{not really json`})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope not valid JSON: %v\n%s", err, out)
	}
	raw := env["data"]
	if len(raw) == 0 || raw[0] != '"' {
		t.Fatalf("malformed JSON-ish data must fall back to string, got %s", raw)
	}
}

// TestResultMarshalJSON_OtherFieldsUnchanged confirms the custom
// marshaler preserves every sibling field — the regression risk of
// shadowing an embedded alias.
func TestResultMarshalJSON_OtherFieldsUnchanged(t *testing.T) {
	r := Result{
		Success:      true,
		Title:        "Example",
		URL:          "https://example.com",
		Content:      "body text",
		Elements:     "[1] button",
		Image:        "iVBORw0KGgo=",
		Error:        "",
		Hints:        []string{"try again"},
		OutcomeClass: "ok",
		Path:         "/tmp/x.png",
		Total:        42,
		Truncated:    true,
		Data:         `{"k":1}`,
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"success":true`,
		`"title":"Example"`,
		`"url":"https://example.com"`,
		`"content":"body text"`,
		`"elements":"[1] button"`,
		`"image":"iVBORw0KGgo="`,
		`"hints":["try again"]`,
		`"outcome_class":"ok"`,
		`"path":"/tmp/x.png"`,
		`"total":42`,
		`"truncated":true`,
		`"data":{"k":1}`,
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("expected %q in marshaled output, got:\n%s", want, out)
		}
	}
}

// TestActionUnmarshalEvaluateAlias covers the `expression` → `script`
// alias for browser_eval, so Chrome-DevTools-flavored callers don't
// trip on the canonical key name.
func TestActionUnmarshalEvaluateAlias(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "canonical script", in: `{"script":"document.title"}`, want: "document.title"},
		{name: "expression alias", in: `{"expression":"document.title"}`, want: "document.title"},
		{name: "both set, script wins", in: `{"script":"a","expression":"b"}`, want: "a"},
		{name: "neither", in: `{}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Action
			if err := json.Unmarshal([]byte(tc.in), &a); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if a.Script != tc.want {
				t.Fatalf("Script: got %q, want %q", a.Script, tc.want)
			}
		})
	}
}
