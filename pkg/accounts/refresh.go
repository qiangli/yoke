package accounts

import (
	"context"
	"errors"
	"sort"
	"time"
)

const (
	RefreshTimeout = 30 * time.Second
	RefreshFailed  = "refresh_failed"
)

var RefreshPeriods = []string{"day", "week", "month"}

var ErrAdapterFailed = errors.New("accounts: adapter failed")

func AdapterFor(reg *Registry, a Account, cap Capability) (Adapter, error) {
	if reg == nil {
		reg = DefaultRegistry()
	}
	for _, name := range []string{a.Provenance, a.Provider, string(a.Category)} {
		if name == "" || name == "manual" {
			continue
		}
		if adapter, err := reg.Adapter(name, cap); err == nil {
			return adapter, nil
		}
	}
	return nil, ErrUnsupported
}

// RefreshOne re-reads one account through its adapter and persists a snapshot
// (resources, metrics per period, observed_at). Only refresh calls adapters:
// usage and resources render the snapshot it writes. An adapter error records
// a safe code and keeps the previous resources and metrics.
func RefreshOne(ctx context.Context, s *Store, reg *Registry, a Account) (Account, error) {
	adapter, err := AdapterFor(reg, a, Verify)
	if err != nil {
		return Account{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, RefreshTimeout)
	defer cancel()
	now := time.Now()
	failSnapshot := func() error {
		if rerr := s.RecordSnapshotError(a.ID, RefreshFailed, now); rerr != nil {
			return rerr
		}
		return ErrAdapterFailed
	}
	observation, err := adapter.Verify(ctx, a)
	if err != nil {
		return Account{}, failSnapshot()
	}
	if err := s.Observe(adapter.Name(), []Observation{observation}); err != nil {
		return Account{}, err
	}
	items, err := materialize(adapter.Name(), []Observation{observation})
	if err != nil || len(items) != 1 {
		return Account{}, fail("invalid_observation", "adapter returned invalid account metadata")
	}
	var resources []ResourceRef
	if hasCapability(adapter, Resources) {
		resources, err = adapter.Resources(ctx, a)
		if err != nil {
			return Account{}, failSnapshot()
		}
		sort.Slice(resources, func(i, j int) bool {
			if resources[i].Kind != resources[j].Kind {
				return resources[i].Kind < resources[j].Kind
			}
			return resources[i].ID < resources[j].ID
		})
	}
	metrics := map[string][]Metric{}
	if hasCapability(adapter, Usage) || hasCapability(adapter, Billing) {
		for _, period := range RefreshPeriods {
			var combined []Metric
			if hasCapability(adapter, Usage) {
				used, err := adapter.Usage(ctx, a, period)
				if err != nil {
					return Account{}, failSnapshot()
				}
				combined = append(combined, used...)
			}
			if hasCapability(adapter, Billing) {
				billed, err := adapter.Billing(ctx, a, period)
				if err != nil {
					return Account{}, failSnapshot()
				}
				combined = append(combined, billed...)
			}
			sortMetrics(combined)
			metrics[period] = combined
		}
	}
	if err := s.SaveSnapshot(Snapshot{AccountID: a.ID, Resources: resources, Metrics: metrics, ObservedAt: now}); err != nil {
		return Account{}, err
	}
	return items[0], nil
}

func RefreshAll(ctx context.Context, s *Store, reg *Registry) ([]Account, error) {
	targets, err := s.List()
	if err != nil {
		return nil, err
	}
	var refreshed []Account
	for _, a := range targets {
		if a.Status == Suppressed {
			continue
		}
		item, err := RefreshOne(ctx, s, reg, a)
		if err != nil {
			if errors.Is(err, ErrUnsupported) {
				continue
			}
			return nil, err
		}
		refreshed = append(refreshed, item)
	}
	return refreshed, nil
}
