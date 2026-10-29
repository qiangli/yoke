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
var ErrMixedClassification = errors.New("accounts: mixed classifications")

// SumMetrics accepts only available, comparable, nonoverlapping cumulative
// values with one shared classification. Mixed classifications (for example an
// estimate added to a provider-reported total) are refused: the sum would
// imply a certainty the inputs do not share. Coverage is never upgraded: any
// Unknown (or undisclosed) input makes the result Unknown, otherwise any
// Partial input makes the result Partial. Context gauges are not cumulative
// usage and are refused.
func SumMetrics(metrics ...Metric) (Metric, error) {
	if len(metrics) == 0 {
		return Metric{}, ErrIncompatibleMetric
	}
	for _, m := range metrics {
		if m.Value == nil || m.Unit == "" || m.Source == "" {
			return Metric{}, ErrIncompatibleMetric
		}
		name := strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(m.Name))
		if strings.Contains(name, "context") {
			return Metric{}, ErrCurrentContext
		}
	}
	first := metrics[0]
	for _, m := range metrics[1:] {
		if m.Unit != first.Unit || m.Currency != first.Currency || m.Name != first.Name {
			return Metric{}, ErrIncompatibleMetric
		}
		if m.Classification != first.Classification {
			return Metric{}, ErrMixedClassification
		}
	}
	for i, m := range metrics {
		for j := 0; j < i; j++ {
			p := metrics[j]
			if sourceFamily(m.Source) == sourceFamily(p.Source) {
				if m.WindowStart == nil || m.WindowEnd == nil || p.WindowStart == nil || p.WindowEnd == nil {
					return Metric{}, ErrOverlappingMetric
				}
				if m.WindowStart.Before(*p.WindowEnd) && p.WindowStart.Before(*m.WindowEnd) {
					return Metric{}, ErrOverlappingMetric
				}
			}
		}
	}
	out := Metric{Name: first.Name, Unit: first.Unit, Currency: first.Currency, Classification: first.Classification, Coverage: Complete, Period: first.Period}
	out.Source = first.Source
	for _, m := range metrics[1:] {
		if m.Source != out.Source {
			out.Source = "aggregate"
		}
		if m.Period != out.Period {
			out.Period = ""
		}
	}
	total := 0.0
	var start, end *time.Time
	for _, m := range metrics {
		total += *m.Value
		switch m.Coverage {
		case Complete:
		case Partial:
			if out.Coverage == Complete {
				out.Coverage = Partial
			}
		default:
			out.Coverage = Unknown
		}
		if !m.ObservedAt.IsZero() && m.ObservedAt.After(out.ObservedAt) {
			out.ObservedAt = m.ObservedAt
		}
		if m.Limitations != nil {
			for _, l := range m.Limitations {
				found := false
				for _, have := range out.Limitations {
					if have == l {
						found = true
						break
					}
				}
				if !found {
					out.Limitations = append(out.Limitations, l)
				}
			}
		}
		if m.WindowStart != nil && m.WindowEnd != nil {
			if start == nil || m.WindowStart.Before(*start) {
				v := *m.WindowStart
				start = &v
			}
			if end == nil || m.WindowEnd.After(*end) {
				v := *m.WindowEnd
				end = &v
			}
		} else {
			start, end = nil, nil
		}
	}
	// A span over discontiguous windows overstates coverage, so only a fully
	// windowed set keeps a window; a partially windowed set keeps none rather
	// than implying a measured range.
	complete := true
	for _, m := range metrics {
		if m.WindowStart == nil || m.WindowEnd == nil {
			complete = false
			break
		}
	}
	if complete {
		out.WindowStart, out.WindowEnd = start, end
	}
	out.Value = &total
	return out, nil
}
func sourceFamily(s string) string {
	if i := strings.IndexAny(s, "/:#"); i >= 0 {
		return s[:i]
	}
	return s
}
