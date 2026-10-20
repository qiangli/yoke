package watch

import "io"

// FuncSink adapts a function to Sink.
type FuncSink func(Alert) error

// Deliver implements Sink.
func (f FuncSink) Deliver(a Alert) error { return f(a) }

// ConsoleSink writes one human line per alert.
type ConsoleSink struct{ W io.Writer }

// Deliver implements Sink.
func (s ConsoleSink) Deliver(a Alert) error { return nil }

// JSONSink writes one bashy-watch-v1 object per line.
type JSONSink struct{ W io.Writer }

// Deliver implements Sink.
func (s JSONSink) Deliver(a Alert) error { return nil }

// FormatAlert renders the console line for an alert.
func FormatAlert(a Alert) string { return "" }
