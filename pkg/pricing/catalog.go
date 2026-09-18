// Package pricing provides a local, data-driven token price catalog.
//
// Rates are expressed in USD per token. A caller supplies the instant being
// billed; this deliberately keeps accounting reproducible and independent of
// the host's time zone.
package pricing

import (
	"embed"
	"fmt"
	"math"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed catalog.yaml
var catalogYAML embed.FS

// Table is a model-price catalog. Models are keyed by their provider model ID.
type Table struct {
	Models map[string]Model `yaml:"models" json:"models"`
}

// Model contains the prices for every billing item a provider exposes. A peak
// schedule applies to every item, preventing input, output, or cache pricing
// from silently being billed at the wrong time of day.
type Model struct {
	Items       map[string]float64 `yaml:"items" json:"items"` // USD per token
	PeakWindows []HourRange        `yaml:"peak_windows,omitempty" json:"peak_windows,omitempty"`
	PeakFactor  float64            `yaml:"peak_factor,omitempty" json:"peak_factor,omitempty"`
	PeakItems   map[string]float64 `yaml:"peak_items,omitempty" json:"peak_items,omitempty"`
}

// HourRange is a half-open UTC hour range: Start <= hour < End. End may be
// lower than Start to describe a window that crosses midnight.
type HourRange struct {
	Start int `yaml:"start" json:"start"`
	End   int `yaml:"end" json:"end"`
}

// Default is the repository's local price table. It never fetches a provider
// catalog and therefore remains usable by the client-side budget gate.
var Default = mustDefault()

func mustDefault() Table {
	b, err := catalogYAML.ReadFile("catalog.yaml")
	if err != nil {
		panic(err)
	}
	var table Table
	if err := yaml.Unmarshal(b, &table); err != nil {
		panic(fmt.Errorf("pricing: parse embedded catalog: %w", err))
	}
	if err := table.Validate(); err != nil {
		panic(fmt.Errorf("pricing: invalid embedded catalog: %w", err))
	}
	return table
}

// Validate rejects malformed local catalog data before a budget gate can use
// it. A model can use either PeakFactor or explicit PeakItems, but not both.
func (t Table) Validate() error {
	for name, model := range t.Models {
		if name == "" || len(model.Items) == 0 {
			return fmt.Errorf("model %q must have billing items", name)
		}
		for item, rate := range model.Items {
			if item == "" || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
				return fmt.Errorf("model %q has invalid base rate for %q", name, item)
			}
		}
		if len(model.PeakItems) > 0 && model.PeakFactor != 0 {
			return fmt.Errorf("model %q declares both peak_factor and peak_items", name)
		}
		if len(model.PeakWindows) == 0 && (model.PeakFactor != 0 || len(model.PeakItems) > 0) {
			return fmt.Errorf("model %q declares peak prices without peak_windows", name)
		}
		if model.PeakFactor != 0 && (model.PeakFactor <= 0 || math.IsNaN(model.PeakFactor) || math.IsInf(model.PeakFactor, 0)) {
			return fmt.Errorf("model %q has invalid peak_factor", name)
		}
		for _, window := range model.PeakWindows {
			if window.Start < 0 || window.Start > 23 || window.End < 0 || window.End > 24 || window.Start == window.End {
				return fmt.Errorf("model %q has invalid peak window %02d:00-%02d:00", name, window.Start, window.End)
			}
		}
		for item, rate := range model.PeakItems {
			if _, ok := model.Items[item]; !ok || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
				return fmt.Errorf("model %q has invalid peak rate for %q", name, item)
			}
		}
	}
	return nil
}

// Rate returns the USD-per-token price for a model billing item at at. at is
// normalized to UTC, so callers get identical results on hosts in any zone.
func (t Table) Rate(modelName, item string, at time.Time) (float64, bool) {
	model, ok := t.Models[modelName]
	if !ok {
		return 0, false
	}
	rate, ok := model.Items[item]
	if !ok {
		return 0, false
	}
	if !model.inPeak(at.UTC().Hour()) {
		return rate, true
	}
	if explicit, ok := model.PeakItems[item]; ok {
		return explicit, true
	}
	if model.PeakFactor != 0 {
		return rate * model.PeakFactor, true
	}
	return rate, true
}

func (m Model) inPeak(hour int) bool {
	for _, window := range m.PeakWindows {
		if window.Start < window.End && hour >= window.Start && hour < window.End {
			return true
		}
		if window.Start > window.End && (hour >= window.Start || hour < window.End) {
			return true
		}
	}
	return false
}

// Cost returns the cost of tokens for one billing item at the supplied instant.
func (t Table) Cost(model, item string, tokens int64, at time.Time) (float64, bool) {
	rate, ok := t.Rate(model, item, at)
	return float64(tokens) * rate, ok
}

// TurnCost prices every reported billing item at the instant of that turn.
// Individual turns, rather than a run-end clock, are what make an accrual
// crossing a peak boundary correct.
func (t Table) TurnCost(model string, tokens map[string]int64, at time.Time) (float64, bool) {
	var total float64
	for item, count := range tokens {
		cost, ok := t.Cost(model, item, count, at)
		if !ok {
			return 0, false
		}
		total += cost
	}
	return total, true
}

// Accrual accumulates already-priced turns for a budget cap. The caller passes
// each turn's timestamp, so a run that spans a tariff boundary remains exact.
type Accrual struct {
	Table Table
	Total float64
}

// AddTurn prices and adds one turn, returning the new total.
func (a *Accrual) AddTurn(model string, tokens map[string]int64, at time.Time) (float64, bool) {
	cost, ok := a.Table.TurnCost(model, tokens, at)
	if !ok {
		return a.Total, false
	}
	a.Total += cost
	return a.Total, true
}
