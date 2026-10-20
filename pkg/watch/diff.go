package watch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultKeys is the record identity a diff uses when the caller names none:
// the first field present wins. An EXPLICIT --key list is composite instead
// (every named field, joined), because a caller naming two fields means the
// pair identifies the row.
var DefaultKeys = []string{"id", "name", "key", "seq", "path"}

// envelopeFields are the array fields a structured command conventionally wraps
// its rows in. Anything else with exactly ONE array of objects is unambiguous
// enough to use; two are not, and guessing there would silently watch the wrong
// list.
var envelopeFields = []string{"records", "items", "rows", "data", "results", "events", "posts", "stories", "list"}

// Record is one decoded row of a structured snapshot.
type Record struct {
	Key    string
	Fields map[string]any
	Raw    json.RawMessage
}

// Meta labels the events a diff produces.
type Meta struct {
	Source  string
	Locator string
	At      time.Time
}

// ParseSnapshot decodes a command's output into records. ok is false when the
// output is not structured, which is the caller's signal to fall back to a line
// diff rather than to report an error.
//
// Numbers are decoded as json.Number, so a sequence or a byte count compares
// and prints exactly as the command wrote it — a float64 round-trip turns
// 1739000000000000000 into 1.739e+18 and makes an unchanged field look changed.
func ParseSnapshot(out []byte, keys []string) (recs []Record, ok bool) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, false
	}
	composite := len(keys) > 0
	if !composite {
		keys = DefaultKeys
	}
	switch trimmed[0] {
	case '[':
		var rows []json.RawMessage
		if err := decode(trimmed, &rows); err != nil {
			return nil, false
		}
		return recordsFrom(rows, keys, composite), true
	case '{':
		// NDJSON and a single object both start with '{'. NDJSON first: a
		// pretty-printed object's first line is not valid JSON on its own, so
		// the ambiguity resolves itself.
		if rows, isNDJSON := ndjsonRows(trimmed); isNDJSON {
			return recordsFrom(rows, keys, composite), true
		}
		var obj map[string]json.RawMessage
		if err := decode(trimmed, &obj); err != nil {
			return nil, false
		}
		if rows, found := envelopeRows(obj); found {
			return recordsFrom(rows, keys, composite), true
		}
		return []Record{recordFrom(trimmed, keys, composite)}, true
	}
	return nil, false
}

func decode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Trailing content means this was not one document (NDJSON, or junk after
	// the object). The caller's other shapes handle those.
	var rest json.RawMessage
	if err := dec.Decode(&rest); err == nil {
		return fmt.Errorf("trailing content")
	}
	return nil
}

// ndjsonRows reads one JSON object per line. It requires at least two rows:
// a single line is just an object, and the single-object path reads it with
// envelope detection the NDJSON path must not do.
func ndjsonRows(b []byte) ([]json.RawMessage, bool) {
	var rows []json.RawMessage
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(line) == 0 || line[0] != '{' {
			return nil, false
		}
		var one json.RawMessage
		if err := decode(line, &one); err != nil {
			return nil, false
		}
		rows = append(rows, one)
	}
	return rows, len(rows) > 1
}

func envelopeRows(obj map[string]json.RawMessage) ([]json.RawMessage, bool) {
	rowsOf := func(raw json.RawMessage) ([]json.RawMessage, bool) {
		if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' {
			return nil, false
		}
		var rows []json.RawMessage
		if err := decode(raw, &rows); err != nil {
			return nil, false
		}
		return rows, true
	}
	for _, name := range envelopeFields {
		if raw, ok := obj[name]; ok {
			if rows, ok := rowsOf(raw); ok {
				return rows, true
			}
		}
	}
	var only []json.RawMessage
	found := 0
	for _, name := range sortedNames(obj) {
		if rows, ok := rowsOf(obj[name]); ok {
			found++
			only = rows
		}
	}
	if found == 1 {
		return only, true
	}
	return nil, false
}

func sortedNames(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func recordsFrom(rows []json.RawMessage, keys []string, composite bool) []Record {
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, recordFrom(row, keys, composite))
	}
	return out
}

func recordFrom(raw json.RawMessage, keys []string, composite bool) Record {
	rec := Record{Raw: append(json.RawMessage(nil), raw...)}
	fields := map[string]any{}
	if err := decode(raw, &fields); err != nil {
		// A non-object row (a bare string or number in an array) is still a
		// record: it has a value, so it can appear and disappear.
		var v any
		if decode(raw, &v) == nil {
			fields = map[string]any{"value": v}
		}
	}
	rec.Fields = fields
	rec.Key = keyOf(fields, keys, composite)
	return rec
}

// keyOf picks the record's identity. A record with none is keyed by its own
// canonical content, so an unkeyable stream degrades to added/removed instead
// of inventing a "changed" event out of a reordering.
func keyOf(fields map[string]any, keys []string, composite bool) string {
	if composite {
		parts := make([]string, 0, len(keys))
		present := false
		for _, k := range keys {
			v, ok := fields[k]
			if ok {
				present = true
			}
			parts = append(parts, scalar(v))
		}
		if present {
			return strings.Join(parts, "|")
		}
		return canonical(fields)
	}
	for _, k := range keys {
		if v, ok := fields[k]; ok {
			if s := scalar(v); s != "" {
				return s
			}
		}
	}
	return canonical(fields)
}

func canonical(fields map[string]any) string {
	b, err := json.Marshal(fields)
	if err != nil {
		return fmt.Sprint(fields)
	}
	return string(b)
}

func scalar(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return fmt.Sprint(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// DiffRecords compares two snapshots by record key, emitting one event per
// added record, one per removed record, and one PER CHANGED FIELD — a watcher
// reporting "record a changed" makes its reader re-read the record to find out
// what the alert was about.
func DiffRecords(prev, next []Record, meta Meta) []Event {
	prevIdx, nextIdx := index(prev), index(next)
	keys := map[string]bool{}
	for k := range prevIdx {
		keys[k] = true
	}
	for k := range nextIdx {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	var out []Event
	for _, k := range ordered {
		pr, inPrev := prevIdx[k]
		nr, inNext := nextIdx[k]
		switch {
		case !inPrev:
			out = append(out, Event{
				At: meta.At, Source: meta.Source, Locator: meta.Locator, Kind: KindAdded,
				Key: k, After: nr.Fields, Raw: nr.Raw, Text: "+ " + SanitizeText([]byte(k)),
			})
		case !inNext:
			out = append(out, Event{
				At: meta.At, Source: meta.Source, Locator: meta.Locator, Kind: KindRemoved,
				Key: k, Before: pr.Fields, Raw: pr.Raw, Text: "- " + SanitizeText([]byte(k)),
			})
		default:
			out = append(out, diffFields(pr, nr, k, meta)...)
		}
	}
	return out
}

func diffFields(pr, nr Record, key string, meta Meta) []Event {
	names := map[string]bool{}
	for n := range pr.Fields {
		names[n] = true
	}
	for n := range nr.Fields {
		names[n] = true
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	var out []Event
	for _, n := range ordered {
		before, after := pr.Fields[n], nr.Fields[n]
		if sameValue(before, after) {
			continue
		}
		out = append(out, Event{
			At: meta.At, Source: meta.Source, Locator: meta.Locator, Kind: KindChanged,
			Key: key, Field: n, Before: before, After: after, Raw: nr.Raw,
			Text: fmt.Sprintf("%s %s: %s -> %s", SanitizeText([]byte(key)), n,
				SanitizeText([]byte(scalar(before))), SanitizeText([]byte(scalar(after)))),
		})
	}
	return out
}

func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return fmt.Sprint(a) == fmt.Sprint(b)
	}
	return bytes.Equal(ab, bb)
}

func index(recs []Record) map[string]Record {
	out := make(map[string]Record, len(recs))
	for _, r := range recs {
		out[r.Key] = r
	}
	return out
}

// DiffLines is the unstructured fallback: whole lines, compared as bytes, the
// way classic -d compares output. Lines are counted rather than set-compared,
// so a duplicated line is a change.
func DiffLines(prev, next []byte, meta Meta) []Event {
	prevCount, nextCount := lineCounts(prev), lineCounts(next)
	seen := map[string]bool{}
	var ordered []string
	for _, m := range []map[string]int{prevCount, nextCount} {
		for line := range m {
			if !seen[line] {
				seen[line] = true
				ordered = append(ordered, line)
			}
		}
	}
	sort.Strings(ordered)

	var out []Event
	for _, line := range ordered {
		delta := nextCount[line] - prevCount[line]
		text := SanitizeText([]byte(line))
		raw, _ := json.Marshal(text)
		for i := 0; i < delta; i++ {
			out = append(out, Event{
				At: meta.At, Source: meta.Source, Locator: meta.Locator, Kind: KindAdded,
				Text: text, After: text, Raw: raw,
			})
		}
		for i := 0; i < -delta; i++ {
			out = append(out, Event{
				At: meta.At, Source: meta.Source, Locator: meta.Locator, Kind: KindRemoved,
				Text: text, Before: text, Raw: raw,
			})
		}
	}
	return out
}

func lineCounts(b []byte) map[string]int {
	out := map[string]int{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out[string(line)]++
	}
	return out
}

// SanitizeText renders arbitrary bytes as printable text.
//
// Watching is exactly the job where binary turns up uninvited — a `cat` of a
// log that rotated mid-write, a tool that prints a progress spinner, a
// truncated UTF-8 sequence — and the conductor's hand-written watcher died on
// it. A NUL truncates a console line; invalid UTF-8 is replaced silently by
// encoding/json, so two different outputs can serialize identically. Escaping
// at the boundary means neither can happen downstream.
func SanitizeText(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			fmt.Fprintf(&sb, `\x%02x`, b[i])
			i++
			continue
		}
		switch {
		case r == '\t':
			sb.WriteString(`\t`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, `\x%02x`, byte(r))
		default:
			sb.WriteRune(r)
		}
		i += size
	}
	return sb.String()
}
