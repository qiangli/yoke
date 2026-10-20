package watch

import (
	"encoding/json"
	"time"
)

// DefaultKeys is the record identity a diff uses when the caller names none.
var DefaultKeys = []string{"id", "name", "key", "seq", "path"}

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
func ParseSnapshot(out []byte, keys []string) (recs []Record, ok bool) {
	return nil, false
}

// DiffRecords compares two snapshots by record key.
func DiffRecords(prev, next []Record, meta Meta) []Event {
	return nil
}

// DiffLines is the unstructured fallback: whole lines, compared as bytes.
func DiffLines(prev, next []byte, meta Meta) []Event {
	return nil
}
