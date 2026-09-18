package resources

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const maxObservedProcesses = 8192

// Compact persisted native facts; never includes argv/environment. CPUSeconds
// is lifetime CPU, CPUPercent is a rate against the previous same-birth sample.
type processSample struct {
	Identity   ProcessIdentity `json:"identity"`
	PPID       int             `json:"ppid"`
	Name       string          `json:"name"`
	CPUSeconds *float64        `json:"cpu_seconds,omitempty"`
	CPUPercent *float64        `json:"cpu_percent,omitempty"`
	RSS        *uint64         `json:"rss,omitempty"`
	Source     string          `json:"source"`
	Reason     string          `json:"reason,omitempty"`
}

func processRates(current, previous []processSample, elapsed float64) {
	old := make(map[ProcessIdentity]processSample, len(previous))
	for _, p := range previous {
		old[p.Identity] = p
	}
	for i := range current {
		p := &current[i]
		p.CPUPercent = nil
		prior, ok := old[p.Identity]
		if elapsed <= 0 || elapsed > 300 || !ok || p.Identity.StartID == "" || p.CPUSeconds == nil || prior.CPUSeconds == nil || *p.CPUSeconds < *prior.CPUSeconds {
			continue
		}
		v := 100 * (*p.CPUSeconds - *prior.CPUSeconds) / elapsed
		p.CPUPercent = &v
	}
}
func processObservations(samples []processSample, at, previous time.Time, now time.Time) []ProcessObservation {
	out := make([]ProcessObservation, 0, len(samples))
	for _, p := range samples {
		row := ProcessObservation{Identity: p.Identity, PPID: p.PPID, Name: p.Name, Attribution: "unattributed", CPU: unknownValue[float64](p.Source, "no comparable CPU sample", at), RSS: unknownValue[uint64](p.Source, p.Reason, at)}
		if p.CPUPercent != nil {
			row.CPU = ObservationValue[float64]{Value: p.CPUPercent, Status: observationStatus("actual", p.Source, "", at, HostObservationTTL)}
			row.CPU.Status.WindowStart = previous
			row.CPU.Status.WindowEnd = at
		}
		if p.RSS != nil {
			row.RSS = ObservationValue[uint64]{Value: p.RSS, Status: observationStatus("actual", p.Source, "", at, HostObservationTTL)}
		}
		row.CPU.Status.Stale = now.Before(at) || !now.Before(at.Add(HostObservationTTL))
		row.RSS.Status.Stale = row.CPU.Status.Stale
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity.PID < out[j].Identity.PID })
	return out
}
func processBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Second)
}

// LookupProcessIdentity reads one process's native birth marker for launch-time
// recording. It never enumerates the host, reads argv, or grants process control.
func LookupProcessIdentity(ctx context.Context, pid int) (ProcessIdentity, error) {
	if pid <= 0 || pid > 1<<31-1 {
		return ProcessIdentity{}, fmt.Errorf("resources: PID must be a positive 32-bit value")
	}
	if err := ctx.Err(); err != nil {
		return ProcessIdentity{}, err
	}
	identity, err := lookupNativeProcessIdentity(pid)
	if cancelled := ctx.Err(); cancelled != nil {
		return ProcessIdentity{}, cancelled
	}
	if err == nil && (identity.PID != pid || identity.StartID == "") {
		return ProcessIdentity{}, fmt.Errorf("resources: process birth identity unavailable")
	}
	return identity, err
}
