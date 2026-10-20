package watch

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// FuncSink adapts a function to Sink, so an embedding host can add a sink
// inline without declaring a type.
type FuncSink func(Alert) error

// Deliver implements Sink.
func (f FuncSink) Deliver(a Alert) error { return f(a) }

// ConsoleSink writes one human line per alert.
//
// It writes to STDERR by convention of the caller: in poll mode stdout belongs
// to the watched command, and an alert mixed into a command's output is a
// change to that command's contract.
type ConsoleSink struct{ W io.Writer }

// Deliver implements Sink.
func (s ConsoleSink) Deliver(a Alert) error {
	_, err := fmt.Fprint(s.W, FormatAlert(a))
	return err
}

// JSONSink writes one bashy-watch-v1 object per line (NDJSON).
type JSONSink struct{ W io.Writer }

// alertRecord is the wire shape: the schema version first, then the alert.
type alertRecord struct {
	SchemaVersion string `json:"schema_version"`
	Alert
}

// Deliver implements Sink.
func (s JSONSink) Deliver(a Alert) error {
	a.Message = SanitizeText([]byte(a.Message))
	b, err := json.Marshal(alertRecord{SchemaVersion: SchemaVersion, Alert: a})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.W, "%s\n", b)
	return err
}

// FormatAlert renders the console line for an alert. Everything that could
// carry raw bytes goes through SanitizeText: an alert whose message truncates
// the line at a NUL is worse than no alert, because it looks like one.
func FormatAlert(a Alert) string {
	var sb strings.Builder
	if !a.At.IsZero() {
		sb.WriteString(a.At.Format("15:04:05"))
		sb.WriteByte(' ')
	}
	fmt.Fprintf(&sb, "%s %s %s", a.Rule, a.State, a.Reason)
	if a.Locator != "" {
		fmt.Fprintf(&sb, " %s", SanitizeText([]byte(a.Locator)))
	}
	if a.Message != "" {
		fmt.Fprintf(&sb, ": %s", SanitizeText([]byte(a.Message)))
	}
	if len(a.Fields) > 0 {
		names := make([]string, 0, len(a.Fields))
		for k := range a.Fields {
			names = append(names, k)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, k := range names {
			parts = append(parts, fmt.Sprintf("%s=%s", k, SanitizeText([]byte(fmt.Sprint(a.Fields[k])))))
		}
		fmt.Fprintf(&sb, " [%s]", strings.Join(parts, " "))
	}
	sb.WriteByte('\n')
	return sb.String()
}
