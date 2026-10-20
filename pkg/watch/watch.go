// Package watch is the generic watch engine behind `bashy watch`.
//
// It exists because every agent writes the same loop by hand — poll a command,
// diff it against a baseline, exit on the first new line — and every hand-written
// copy fails the same four ways: a filter that never matches, a source nobody
// polled, an empty snapshot diffed against a full baseline (so the whole history
// replays as "new"), and binary bytes that break the text tooling. The worst of
// them is the one no such loop can even express: ABSENCE. A stalled session emits
// nothing, so "tell me when a line appears" reports healthy forever.
//
// The answer is one engine rather than a `watch` subcommand on every command:
//
//	sources ──▶ events ──▶ rules ──▶ alerts ──▶ sinks
//
// Four small interfaces and one data model between them. The engine knows
// nothing about sprints, messages or processes; the sources know nothing about
// rules; the rules know nothing about delivery policy. A command becomes
// watchable by emitting to the journal it already owns (see pkg/watch/hostsrc)
// or simply by being able to print its state (see PollSource) — it never
// implements watching.
//
// See docs/watch-design.md.
package watch

import (
	"context"
	"encoding/json"
	"time"
)

// SchemaVersion versions the NDJSON alert record (--json).
const SchemaVersion = "bashy-watch-v1"

// RulesSchemaVersion versions a rule file / preset.
const RulesSchemaVersion = "bashy-watch-rules-v1"

// Event kinds. These are OBSERVATIONS, never alarms: an alarm is an Alert, and
// only a rule may raise one.
const (
	// KindAdded is a record that was not in the previous snapshot.
	KindAdded = "added"
	// KindRemoved is a record that is no longer there.
	KindRemoved = "removed"
	// KindChanged is one field of one record taking a new value.
	KindChanged = "changed"
	// KindSample is "I observed a value, unchanged" — what a threshold or rate
	// rule needs and a change rule ignores.
	KindSample = "sample"
	// KindAbsent is an observation a SOURCE can make on its own: the thing it
	// was asked to read is not there. (Absence DEDUCED from silence is the
	// inverse rule's job, not a source's.)
	KindAbsent = "absent"
)

// Event is one observation, in the one shape every source speaks.
//
// Attrs is what keeps this generic. A rule predicate matches on labels it is
// handed; it never learns that "to" came from a message board. Adding a source
// adds labels, not engine code.
type Event struct {
	At      time.Time         `json:"at"`
	Source  string            `json:"source"`
	Locator string            `json:"locator,omitempty"`
	Kind    string            `json:"kind"`
	Key     string            `json:"key,omitempty"`
	Field   string            `json:"field,omitempty"`
	Before  any               `json:"before,omitempty"`
	After   any               `json:"after,omitempty"`
	Text    string            `json:"text,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
	Raw     json.RawMessage   `json:"raw,omitempty"`
}

// Attr reads a label, absent or not.
func (e Event) Attr(name string) string { return e.Attrs[name] }

// Alert states. A rule reports a CONDITION; the engine decides what to deliver.
const (
	StateFire  = "fire"
	StateClear = "clear"
)

// Alert reasons. The vocabulary is small on purpose: an operator reading an
// alert stream should not have to learn a word per command.
const (
	ReasonMatch      = "match"
	ReasonChange     = "change"
	ReasonIdle       = "idle"
	ReasonMissing    = "missing"
	ReasonAbsent     = "absent"
	ReasonResponded  = "responded"
	ReasonLate       = "late"
	ReasonUnanswered = "unanswered"
	ReasonError      = "error"
)

// Alert is what a rule raises. Fields carries the rule's measurements
// (idle_for, latency, level, …) so a sink never has to parse a message.
type Alert struct {
	At      time.Time      `json:"at"`
	Rule    string         `json:"rule"`
	State   string         `json:"state"`
	Reason  string         `json:"reason"`
	Locator string         `json:"locator,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
	Event   *Event         `json:"event,omitempty"`
}

// Source produces events. One method, polled.
//
// Poll rather than callback, and the reason is the stores: the board, the room
// timeline and a command's stdout are all things that can be READ, so a watcher
// that was not running when a record landed still sees it on the next read,
// there is no daemon to be down, and a fake clock plus a fixture directory is a
// complete test harness. Phase 2's fsnotify source is genuinely push and will
// gain a streaming variant of this interface rather than change it.
//
// A source that cannot read its backing store returns an error. It must NOT
// advance its own baseline on a failed read: a read that failed is not a
// change, and treating it as one is exactly how a watcher replays its whole
// history the first time a command exits non-zero.
type Source interface {
	Name() string
	Poll(ctx context.Context, now time.Time) ([]Event, error)
}

// Rule turns events and the passage of time into alerts.
//
// Tick is the half a hand-written poll loop never has, and it is why absence
// works at all: nothing arrives, so only the clock can fire.
//
// A Phase 2 threshold or rate evaluator is this interface and nothing more — it
// reads a numeric field off KindSample events and keeps its hysteresis state in
// itself — which is why the interface is defined now rather than after
// pkg/resources' evaluator moves in behind it.
type Rule interface {
	Name() string
	On(ev Event, now time.Time) []Alert
	Tick(now time.Time) []Alert
}

// Starter is implemented by a rule that must be armed with a start time —
// a liveness rule has to know when the clock started to report a heartbeat
// that NEVER arrived.
type Starter interface {
	Start(now time.Time)
}

// Sink delivers an alert. One method, so an agent can add one inline.
type Sink interface {
	Deliver(Alert) error
}

// Predicate selects events.
type Predicate func(Event) bool
