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

// Alert LEVELS. Two rungs and no more: the first is "look at this", the
// second is "this is not coming back on its own". A level is not a reason —
// the same condition escalates, and a consumer filtering `reason=idle` must
// keep seeing it when it does.
const (
	LevelWarning  = "warning"
	LevelCritical = "critical"
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
	Level   string         `json:"level,omitempty"`
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

// Expecter is implemented by a source that knows UP FRONT which locators it
// intends to report on.
//
// It exists for the case absence cannot otherwise reach: a locator that has
// been silent since before the watcher started has no event to hang a timer on,
// so an idle rule over a poll source would arm only on the first change — which
// is to say, never, for the exact resource that is stuck.
type Expecter interface {
	Expected() []string
}

// Armer is implemented by a rule that can be told which locators to expect, so
// a rule FILE does not have to name a locator the command line already did.
type Armer interface {
	Arm(locators []string)
}

// Starter is implemented by a rule that must be armed with a start time —
// a liveness rule has to know when the clock started to report a heartbeat
// that NEVER arrived.
type Starter interface {
	Start(now time.Time)
}

// TASK CLASSES. Idleness applies to LONG-RUNNING work and nothing else: a
// weave run, a managed session, a service, a background or schedule job. A
// short foreground command that printed nothing for twenty minutes is not
// stalled — it is a command, and whoever started it is watching it.
//
// The engine cannot guess which one it is looking at, and guessing from a
// locator string would be wrong in both directions. So a class is DECLARED:
// by the source that spawned the work (Task), or on the event itself (the
// AttrTask label). An undeclared locator stays eligible — the operator who
// pointed `watch --idle` at it has said what it is — but a locator declared
// short is never flagged.
const (
	ClassRun        = "run"      // a weave run
	ClassSession    = "session"  // a managed agent session
	ClassService    = "service"  // a supervised service
	ClassJob        = "job"      // a background job
	ClassSchedule   = "schedule" // a schedule job
	ClassCommand    = "command"  // a short foreground command — never idle
	ClassForeground = "foreground"
)

// LongRunning reports whether idleness applies to a task class. An unknown or
// empty class is long-running: see the note above on why the default is
// eligible rather than exempt.
func LongRunning(class string) bool {
	switch class {
	case ClassCommand, ClassForeground:
		return false
	default:
		return true
	}
}

// Event labels a task declares about itself.
const (
	// AttrTask is the task class (see LongRunning).
	AttrTask = "task"
	// AttrQuiet is the task's own expected quiet window ("45m"): how long it
	// may legitimately say nothing. A task younger than its window is never
	// flagged, and the window replaces the rule's default warning threshold.
	AttrQuiet = "quiet_window"
)

// Task is what a source knows about the thing it watches, up front: what kind
// of work it is, how long it may legitimately be quiet, and when it began.
//
// Started is the one fact no event can supply. A journal entry an hour old
// does not mean the task is an hour old, and without a start time a watcher
// that back-fills history flags a run that began a minute ago.
type Task struct {
	Locator string
	Class   string
	Quiet   time.Duration
	Started time.Time
}

// TaskDeclarer is implemented by a source that knows the task it watches.
type TaskDeclarer interface {
	Tasks() []Task
}

// Declarer is implemented by a rule that can be told about a task, so a rule
// file does not have to repeat what the source already knows.
type Declarer interface {
	Declare(Task)
}

// Sink delivers an alert. One method, so an agent can add one inline.
type Sink interface {
	Deliver(Alert) error
}

// Predicate selects events.
type Predicate func(Event) bool
