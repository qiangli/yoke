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

func RecordObservation(ctx context.Context, o Observation) error { return quotaobs.Record(ctx, o) }
func ReadObservations(since time.Time, filter ObservationFilter) ([]Observation, error) {
	return quotaobs.Read(since, filter)
}
