package accounts

import (
	"errors"
	"strings"
	"time"
)

type Classification string

const (
	ProviderReported Classification = "provider-reported"
	LocalObservation Classification = "local-observation"
	Estimate         Classification = "estimate"
)

type Coverage string

const (
	Complete Coverage = "complete"
	Partial  Coverage = "partial"
	Unknown  Coverage = "unknown"
)

type Metric struct {
	Name           string         `json:"name"`
	Value          *float64       `json:"value"`
	Unit           string         `json:"unit"`
	Currency       string         `json:"currency,omitempty"`
	Source         string         `json:"source"`
	Classification Classification `json:"classification"`
	ObservedAt     time.Time      `json:"observed_at"`
	WindowStart    *time.Time     `json:"window_start,omitempty"`
	WindowEnd      *time.Time     `json:"window_end,omitempty"`
	Period         string         `json:"period,omitempty"`
	Coverage       Coverage       `json:"coverage"`
	Limitations    []string       `json:"limitations,omitempty"`
}

var ErrIncompatibleMetric = errors.New("accounts: incompatible metrics")
var ErrOverlappingMetric = errors.New("accounts: overlapping source windows")
var ErrCurrentContext = errors.New("accounts: current context is not cumulative usage")

// SumMetrics accepts only available, comparable, nonoverlapping cumulative values.
func SumMetrics(metrics ...Metric) (float64, error) {
	if len(metrics) == 0 {
		return 0, ErrIncompatibleMetric
	}
	total := 0.0
	for i, m := range metrics {
		if m.Value == nil || m.Unit == "" || m.Source == "" {
			return 0, ErrIncompatibleMetric
		}
		name := strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(m.Name))
		if strings.Contains(name, "current context") || strings.Contains(name, "context current") {
			return 0, ErrCurrentContext
		}
		if i > 0 && (m.Unit != metrics[0].Unit || m.Currency != metrics[0].Currency || m.Name != metrics[0].Name) {
			return 0, ErrIncompatibleMetric
		}
		for j := 0; j < i; j++ {
			p := metrics[j]
			if sourceFamily(m.Source) == sourceFamily(p.Source) {
				if m.WindowStart == nil || m.WindowEnd == nil || p.WindowStart == nil || p.WindowEnd == nil {
					return 0, ErrOverlappingMetric
				}
				if m.WindowStart.Before(*p.WindowEnd) && p.WindowStart.Before(*m.WindowEnd) {
					return 0, ErrOverlappingMetric
				}
			}
		}
		total += *m.Value
	}
	return total, nil
}
func sourceFamily(s string) string {
	if i := strings.IndexAny(s, "/:#"); i >= 0 {
		return s[:i]
	}
	return s
}
