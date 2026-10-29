package accounts

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

func sumFixture(name, unit, source string, class Classification, cover Coverage, value float64) Metric {
	return Metric{Name: name, Value: &value, Unit: unit, Source: source, Classification: class, Coverage: cover, ObservedAt: time.Now()}
}

func TestSumMetricsMixedClassificationRefused(t *testing.T) {
	a := sumFixture("usage.tokens", "tokens", "owl-meter", ProviderReported, Complete, 10)
	b := sumFixture("usage.tokens", "tokens", "owl-meter", Estimate, Complete, 5)
	b.Limitations = []string{"Estimated value; not a vendor measurement."}
	b.WindowStart = nil
	// Non-overlapping windows keep the overlap rule from masking the verdict.
	start := time.Now().Add(-48 * time.Hour)
	mid := start.Add(24 * time.Hour)
	end := mid.Add(24 * time.Hour)
	a.WindowStart, a.WindowEnd = &start, &mid
	b.WindowStart, b.WindowEnd = &mid, &end
	a.Source, b.Source = "owl-meter:a", "owl-meter:b"
	if _, err := SumMetrics(a, b); !errors.Is(err, ErrMixedClassification) {
		t.Fatalf("estimate + provider-reported = %v, want ErrMixedClassification", err)
	}
	c := a
	c.Classification = LocalObservation
	if _, err := SumMetrics(a, c); !errors.Is(err, ErrMixedClassification) {
		t.Fatalf("provider-reported + local-observation = %v, want ErrMixedClassification", err)
	}
	d := a
	d.Classification = ""
	if _, err := SumMetrics(a, d); !errors.Is(err, ErrMixedClassification) {
		t.Fatalf("classified + unclassified = %v, want ErrMixedClassification", err)
	}
	accountstest.AssertNoSecret(t, a, b, c, d)
}

func TestSumMetricsCoveragePropagates(t *testing.T) {
	mk := func(cover Coverage, v float64, src string) Metric {
		m := sumFixture("usage.input_tokens", "tokens", src, ProviderReported, cover, v)
		start := time.Now().Add(-time.Hour)
		end := start.Add(time.Hour)
		m.WindowStart, m.WindowEnd = &start, &end
		return m
	}
	complete, err := SumMetrics(mk(Complete, 3, "owla"), mk(Complete, 4, "owlb"))
	if err != nil {
		t.Fatal(err)
	}
	if *complete.Value != 7 || complete.Coverage != Complete {
		t.Fatalf("complete sum = %v %v", *complete.Value, complete.Coverage)
	}
	partial, err := SumMetrics(mk(Complete, 3, "owla"), mk(Partial, 4, "owlb"))
	if err != nil {
		t.Fatal(err)
	}
	if partial.Coverage != Partial {
		t.Fatalf("complete+partial coverage = %v, want partial", partial.Coverage)
	}
	unknown, err := SumMetrics(mk(Partial, 3, "owla"), mk(Unknown, 4, "owlb"))
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Coverage != Unknown {
		t.Fatalf("partial+unknown coverage = %v, want unknown", unknown.Coverage)
	}
	// Undisclosed coverage is unknown: it must drag the result down too.
	blank := mk(Complete, 1, "owl:c")
	blank.Coverage = ""
	mixed, err := SumMetrics(mk(Complete, 3, "owla"), blank)
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Coverage != Unknown {
		t.Fatalf("complete+undisclosed coverage = %v, want unknown", mixed.Coverage)
	}
	for _, m := range []Metric{complete, partial, unknown, mixed} {
		accountstest.AssertNoSecret(t, m)
		if m.Classification != ProviderReported || m.Name != "usage.input_tokens" || m.Unit != "tokens" {
			t.Fatalf("sum dropped identity: %+v", m)
		}
	}
}

func TestSumMetricsContextGaugesRefused(t *testing.T) {
	for _, name := range []string{"context.input_tokens", "context.output_tokens", "usage.context_remaining_percent", "current-context-tokens", "context.cached_input_tokens"} {
		m := sumFixture(name, "tokens", "owl-bridge", ProviderReported, Partial, 9)
		if name == "usage.context_remaining_percent" {
			m.Unit = "percent"
		}
		if _, err := SumMetrics(m); !errors.Is(err, ErrCurrentContext) {
			t.Fatalf("%s summed: %v, want ErrCurrentContext", name, err)
		}
		accountstest.AssertNoSecret(t, m)
	}
}

func TestSumMetricsResultShape(t *testing.T) {
	a := sumFixture("billing.spend", "usd", "owl-billing-a", ProviderReported, Complete, 1.5)
	a.Currency = "USD"
	a.Limitations = []string{"Audited."}
	a.Period = "day"
	b := sumFixture("billing.spend", "usd", "owl-billing-b", ProviderReported, Complete, 2.5)
	b.Currency = "USD"
	b.Limitations = []string{"Audited.", "Settled."}
	b.Period = "day"
	got, err := SumMetrics(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Value != 4 || got.Currency != "USD" || got.Period != "day" {
		t.Fatalf("bad sum: %+v", got)
	}
	if len(got.Limitations) != 2 {
		t.Fatalf("limitations not unioned: %v", got.Limitations)
	}
	raw, _ := json.Marshal(got)
	accountstest.AssertNoSecret(t, string(raw))
	eur := b
	eur.Currency = "EUR"
	if _, err := SumMetrics(a, eur); !errors.Is(err, ErrIncompatibleMetric) {
		t.Fatalf("cross-currency sum = %v, want ErrIncompatibleMetric", err)
	}
}
