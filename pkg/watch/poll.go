package watch

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// Runner executes one snapshot of the watched command and reports its exit
// status. Exit status is part of the result, not an error, because a watcher
// has to be able to tell "the command said no" from "the command could not be
// run at all".
type Runner func(ctx context.Context) (out []byte, exit int, err error)

// PollSource is the universal fallback source: run a command on the interval
// and compare successive snapshots. With structured output it diffs RECORDS by
// key; without, it falls back to whole lines the way classic -d does.
//
// One source, and it already covers `sprint show`, `mb --peek`, `weave list`,
// `resources system`, a quota, `doctl`, `kubectl`, `ls -l`/`stat` and
// `curl /version` — with no per-command code anywhere.
type PollSource struct {
	SourceName string
	Locator    string
	Run        Runner
	// Keys names the record identity. Empty means DefaultKeys (first present
	// wins); two or more make a composite key.
	Keys []string
	// AllowEmpty reads an empty snapshot literally — as zero records — for a
	// command whose empty output genuinely means empty. Off by default: an
	// empty snapshot is far more often a command that failed to say anything,
	// and reporting every record as removed is the loudest possible way to be
	// wrong.
	AllowEmpty bool

	started    bool
	structured bool
	prevRecs   []Record
	prevRaw    []byte
}

// NewPollSource builds a poll source over a command runner.
func NewPollSource(name, locator string, run Runner) *PollSource {
	if name == "" {
		name = "poll"
	}
	return &PollSource{SourceName: name, Locator: locator, Run: run}
}

// Name implements Source.
func (p *PollSource) Name() string { return p.SourceName }

// Expected implements Expecter: a poll source is pointed at ONE resource, so
// an idle rule can arm it before the first cycle and report a command whose
// output never moved at all.
func (p *PollSource) Expected() []string {
	if p.Locator == "" {
		return nil
	}
	return []string{p.Locator}
}

// Poll implements Source.
//
// THE BASELINE IS ONLY ADVANCED BY A SNAPSHOT WE BELIEVE. A command that
// failed, printed nothing, or changed shape under us has told us nothing about
// the world, and the one thing a watcher must never do is turn that into "every
// record changed" — that is precisely how the hand-written watcher replayed a
// whole night's history the first time its command errored.
func (p *PollSource) Poll(ctx context.Context, now time.Time) ([]Event, error) {
	out, exit, err := p.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot failed: %w (baseline kept)", err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("command exited %d: snapshot ignored, baseline kept", exit)
	}

	empty := len(bytes.TrimSpace(out)) == 0
	if empty && p.started && !p.AllowEmpty {
		return nil, fmt.Errorf("empty snapshot ignored, baseline kept (pass --allow-empty if empty output really means empty)")
	}

	var recs []Record
	structured := false
	if empty {
		// An empty snapshot keeps the shape we were already reading, so
		// --allow-empty means "zero records" for a structured command and "no
		// lines" for a text one, rather than a shape change.
		structured = p.structured
	} else {
		recs, structured = ParseSnapshot(out, p.Keys)
	}

	if !p.started {
		p.started, p.structured = true, structured
		p.prevRecs, p.prevRaw = recs, append([]byte(nil), out...)
		return nil, nil
	}
	if structured != p.structured {
		shape := func(s bool) string {
			if s {
				return "structured"
			}
			return "text"
		}
		return nil, fmt.Errorf("snapshot shape changed (%s -> %s): ignored, baseline kept",
			shape(p.structured), shape(structured))
	}

	meta := Meta{Source: p.SourceName, Locator: p.Locator, At: now}
	var evs []Event
	if structured {
		evs = DiffRecords(p.prevRecs, recs, meta)
	} else {
		evs = DiffLines(p.prevRaw, out, meta)
	}
	p.prevRecs, p.prevRaw = recs, append([]byte(nil), out...)
	return evs, nil
}
