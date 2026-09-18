// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// EventsFile is the run directory's append-only NDJSON event log.
const EventsFile = "events.jsonl"

// Event is one observable moment in a run.
//
// The event log is what makes a run watchable WHILE it runs: report.json only
// exists once the run is over. It carries the same discipline as [RunRecord] —
// no hostname, no free error text — because it is the same information arriving
// earlier, and an event stream is exactly where a hostname would sneak into a
// view someone screenshots.
type Event struct {
	Kind    string    `json:"t"`
	At      time.Time `json:"at"`
	File    string    `json:"file,omitempty"`    // run.start only
	Targets []string  `json:"targets,omitempty"` // run.start only
	Task    string    `json:"task,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	// Log is the journal-relative path of this attempt's log, carried on
	// task.start so a viewer can begin tailing the output immediately. Only the
	// engine knows the sanitized filename, so publishing it here saves every
	// consumer from reimplementing safeFileName.
	Log        string `json:"log,omitempty"`
	Status     string `json:"status,omitempty"`
	ExitCode   int    `json:"exit,omitempty"`
	DurationMS int64  `json:"ms,omitempty"`
	Failed     bool   `json:"failed,omitempty"` // run.end only
}

// Event kinds. Kept few on purpose: a viewer needs to know a run started, what
// each target is doing, and that it ended.
const (
	EventRunStart  = "run.start"
	EventTaskStart = "task.start"
	EventTaskEnd   = "task.end"
	EventRunEnd    = "run.end"
)

// emit appends one event to the run's log. It is safe for concurrent use: the
// parallel scheduler runs targets on many goroutines, so the observer it feeds
// is called concurrently by construction.
//
// Failures are swallowed. A run must not die because its event log could not be
// written — the same rule the rest of the journal follows.
func (j *Journal) emit(ev Event) {
	if j == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(j.dir, EventsFile),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// Observer returns the engine observer that records this journal's events, or
// nil when there is no journal. Assigning it to [Engine.Observer] is what makes
// a run watchable.
func (j *Journal) Observer() func(Event) {
	if j == nil {
		return nil
	}
	return j.emit
}

// ReadEvents replays a run's event log. A truncated final line is skipped
// rather than failing the read: the log is appended to while it is being read,
// so observing a partial write is normal, not corruption.
func ReadEvents(dir string) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, EventsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, nil
}

// noteResult emits the terminal event for a target that reached a result
// WITHOUT running a body — up-to-date, skipped, condition-skipped, or
// unplaceable. runOne emits its own start/end pair around the body; these paths
// have no body to bracket, so they emit only the end.
//
// This is not a detail. Without it a live viewer shows a cache hit as "pending"
// for the whole run, and on an incremental re-run that is most of the graph —
// a monitor that misreports the common case is one nobody trusts.
func (e *Engine) noteResult(name string, res TaskResult) {
	e.emitEvent(Event{
		Kind:       EventTaskEnd,
		Task:       name,
		Status:     res.Status.String(),
		ExitCode:   res.ExitCode,
		DurationMS: res.Duration.Milliseconds(),
	})
}

// emitEvent is the engine's nil-safe observer call.
func (e *Engine) emitEvent(ev Event) {
	if e.Observer == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	e.Observer(ev)
}
