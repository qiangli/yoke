package watch

import (
	"context"
	"time"
)

// Runner executes one snapshot of the watched command.
type Runner func(ctx context.Context) (out []byte, exit int, err error)

// PollSource is the universal fallback source: run a command on the interval
// and compare successive snapshots.
type PollSource struct {
	SourceName string
	Locator    string
	Run        Runner
	Keys       []string
	AllowEmpty bool
}

// NewPollSource builds a poll source over a command runner.
func NewPollSource(name, locator string, run Runner) *PollSource {
	return &PollSource{SourceName: name, Locator: locator, Run: run}
}

// Name implements Source.
func (p *PollSource) Name() string { return p.SourceName }

// Poll implements Source.
func (p *PollSource) Poll(ctx context.Context, now time.Time) ([]Event, error) {
	return nil, nil
}
