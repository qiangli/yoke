package cligw

// Sprint: #322; Story: #1134; Story-ID: eab66bea9561
//
// gpt-5.5 on the codex seat batched several calls in one reply as concatenated
// envelopes, each closing `}}}` where `}]}` belongs (SWE-bench django-11880).
// None decoded as a whole, so every call fell through as text: the edit never
// ran and the model went on to narrate results it never got. The first
// complete call is recovered as a well-formed one-call envelope; the model sees
// its real result and issues the next.

import "testing"

func TestToolEnvelopeTailRecoversFirstBatchedCall(t *testing.T) {
	edit := `{"name":"bashy","arguments":{"script":"python - <<'PY'\np.write_text(s.replace(old, new))\nPY\ngit diff","timeout_ms":10000}}`
	diff := `{"name":"bashy","arguments":{"script":"git diff -- django/forms/fields.py","timeout_ms":10000}}`
	in := "Applying the fix.\n" + `{"tool_calls":[` + edit + `}{"tool_calls":[` + diff + `}}`
	want := `{"tool_calls":[` + edit + `]}`
	if got := toolEnvelopeTail(in); got != want {
		t.Fatalf("toolEnvelopeTail = %q\nwant %q", got, want)
	}
}

// A well-formed envelope still wins over an earlier malformed one.
func TestToolEnvelopeTailPrefersWellFormedEnvelope(t *testing.T) {
	good := `{"tool_calls":[{"name":"bashy","arguments":{"script":"ls"}}]}`
	in := `{"tool_calls":[{"name":"bashy","arguments":{"script":"pwd"}}}` + good
	if got := toolEnvelopeTail(in); got != good {
		t.Fatalf("toolEnvelopeTail = %q, want %q", got, good)
	}
}
