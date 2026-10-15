package llmbudget

import (
	"context"
	"time"

	"github.com/qiangli/yoke/pkg/quotaobs"
)

// Observation is the shared, redacted quota journal record.
type Observation = quotaobs.Observation
type ParsedSignal = quotaobs.ParsedSignal
type Analysis = quotaobs.Analysis
type ObservationFilter = quotaobs.Filter

func RecordObservation(ctx context.Context, o Observation) error {
	if err := quotaobs.Record(ctx, o); err != nil {
		return err
	}
	// Journal writes may arrive between explicit quota refreshes. Bound this
	// opportunistic ingestion so the observation path never waits for telemetry.
	quick, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	_ = Ingest(quick, o.At)
	return nil
}
func ReadObservations(since time.Time, filter ObservationFilter) ([]Observation, error) {
	return quotaobs.Read(since, filter)
}
