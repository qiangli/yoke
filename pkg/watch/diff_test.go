package watch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var at0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func recKeys(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Key)
	}
	return out
}

func TestParseSnapshotShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		keys []string
		want []string
		ok   bool
	}{
		{name: "array", in: `[{"id":"a"},{"id":"b"}]`, want: []string{"a", "b"}, ok: true},
		{name: "ndjson", in: "{\"id\":\"a\"}\n\n{\"id\":\"b\"}\n", want: []string{"a", "b"}, ok: true},
		{name: "envelope", in: `{"schema_version":"x","records":[{"name":"a"},{"name":"b"}]}`, want: []string{"a", "b"}, ok: true},
		{name: "sole array field", in: `{"stories":[{"id":"s1"}]}`, want: []string{"s1"}, ok: true},
		{name: "single object", in: `{"id":"solo","status":"open"}`, want: []string{"solo"}, ok: true},
		{name: "explicit keys", in: `[{"sprint":480,"story":"abc","stage":"in"}]`, keys: []string{"sprint", "story"}, want: []string{"480|abc"}, ok: true},
		{name: "text", in: "total 4\n-rw-r--r--  1 dev  staff  0 Oct 20 12:00 a\n", ok: false},
		{name: "empty", in: "", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs, ok := ParseSnapshot([]byte(tc.in), tc.keys)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got := recKeys(recs); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("keys = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiffRecordsAddRemoveChangeFieldLevel(t *testing.T) {
	prev, ok := ParseSnapshot([]byte(`[{"id":"a","status":"open"},{"id":"b","status":"done"}]`), nil)
	if !ok {
		t.Fatal("prev did not parse")
	}
	next, ok := ParseSnapshot([]byte(`[{"id":"a","status":"done","note":"blocked on review"},{"id":"c","status":"open"}]`), nil)
	if !ok {
		t.Fatal("next did not parse")
	}
	evs := DiffRecords(prev, next, Meta{Source: "poll", Locator: "sprint:480", At: at0})
	type row struct{ kind, key, field, before, after string }
	var got []row
	for _, e := range evs {
		if e.Source != "poll" || e.Locator != "sprint:480" || !e.At.Equal(at0) {
			t.Fatalf("event metadata not propagated: %+v", e)
		}
		got = append(got, row{e.Kind, e.Key, e.Field, fmt.Sprint(e.Before), fmt.Sprint(e.After)})
	}
	want := []row{
		{KindChanged, "a", "note", "<nil>", "blocked on review"},
		{KindChanged, "a", "status", "open", "done"},
		{KindRemoved, "b", "", "", ""},
		{KindAdded, "c", "", "", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].kind != want[i].kind || got[i].key != want[i].key || got[i].field != want[i].field {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
		if want[i].kind == KindChanged && (got[i].before != want[i].before || got[i].after != want[i].after) {
			t.Fatalf("event %d values = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A removed record keeps the record it lost as Before, and an added one as
	// After: the alert has to be able to say WHAT appeared, not only that
	// something did.
	if evs[2].Before == nil || evs[3].After == nil {
		t.Fatalf("removed/added events carry no record: %+v", evs[2:])
	}
	if len(evs[3].Raw) == 0 || !json.Valid(evs[3].Raw) {
		t.Fatalf("added event raw = %q", evs[3].Raw)
	}
}

func TestDiffRecordsUnkeyedNeverReportsChanged(t *testing.T) {
	// No id/name/key field anywhere: a positional match would invent "changed"
	// events out of a reordering, so an unkeyable stream degrades to
	// added/removed instead.
	prev, _ := ParseSnapshot([]byte(`[{"line":"one"},{"line":"two"}]`), nil)
	next, _ := ParseSnapshot([]byte(`[{"line":"two"},{"line":"three"}]`), nil)
	evs := DiffRecords(prev, next, Meta{Source: "poll", At: at0})
	kinds := map[string]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	if kinds[KindChanged] != 0 || kinds[KindAdded] != 1 || kinds[KindRemoved] != 1 {
		t.Fatalf("kinds = %v (want 1 added, 1 removed, 0 changed): %+v", kinds, evs)
	}
}

func TestDiffLinesBinarySafe(t *testing.T) {
	prev := []byte("alpha\nbeta\n")
	next := []byte("alpha\n\x00\xff gamma\n")
	evs := DiffLines(prev, next, Meta{Source: "poll", Locator: "@dev-box", At: at0})
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want 2", evs)
	}
	var added, removed *Event
	for i := range evs {
		switch evs[i].Kind {
		case KindAdded:
			added = &evs[i]
		case KindRemoved:
			removed = &evs[i]
		}
	}
	if added == nil || removed == nil {
		t.Fatalf("want one added and one removed: %+v", evs)
	}
	if removed.Text != "beta" {
		t.Fatalf("removed text = %q", removed.Text)
	}
	// The raw byte must never reach a consumer: a NUL truncates a console line
	// and invalid UTF-8 breaks the JSON sink.
	for _, e := range evs {
		if bytes.ContainsRune([]byte(e.Text), 0) || !json.Valid(mustJSON(t, e)) {
			t.Fatalf("binary leaked into event: %q", e.Text)
		}
	}
	if !strings.Contains(added.Text, `\x00`) || !strings.Contains(added.Text, "gamma") {
		t.Fatalf("added text = %q, want escaped bytes and the readable tail", added.Text)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
