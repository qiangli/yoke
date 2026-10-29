package llmbudget

import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

func saveObserveFixtures(t *testing.T) {
	t.Helper()
	isolateReportCatalog(t)
	cat := fleet.New()
	for _, err := range []error{
		cat.SaveModel(fleet.Model{Name: "seat-model", Provider: "fixture-vendor", Plan: "seat", Kind: fleet.ModelKindSubscription}),
		cat.SaveModel(fleet.Model{Name: "local-model", Provider: "ollama", Kind: fleet.ModelKindLocal}),
		cat.SaveModel(fleet.Model{Name: "keyless-model", Provider: "fixture-vendor"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A journal record names a seat only when it really proves one. A local model,
// an unknown model, or a record with no account proves nothing, and reporting
// a guess would put an invented account in the host inventory.
func TestObservedIdentityNamesOnlyProvenSeats(t *testing.T) {
	saveObserveFixtures(t)
	for _, tc := range []struct {
		name             string
		in               Observation
		provider, result string
	}{
		{"catalog seat", Observation{Model: "seat-model"}, "fixture-vendor", "seat"},
		{"explicit account wins", Observation{Model: "seat-model", Account: "other-seat"}, "fixture-vendor", "other-seat"},
		{"catalog plan beats journal plan", Observation{Model: "seat-model", Plan: "plan-seat"}, "fixture-vendor", "seat"},
		{"journal plan as fallback", Observation{Model: "keyless-model", Plan: "plan-seat"}, "fixture-vendor", "plan-seat"},
		{"nothing names the seat", Observation{Model: "keyless-model"}, "", ""},
		{"local model", Observation{Model: "local-model"}, "", ""},
		{"unknown model", Observation{Model: "nope", Account: "seat"}, "", ""},
		{"no model", Observation{Account: "seat"}, "", ""},
	} {
		provider, account := observedIdentity(tc.in)
		if provider != tc.provider || account != tc.result {
			t.Fatalf("%s: got (%q,%q) want (%q,%q)", tc.name, provider, account, tc.provider, tc.result)
		}
	}
}

// Recording an observation reports the seat to whoever installed the hook, and
// a hook that does nothing useful never changes the journal's own result.
func TestRecordObservationReportsSeat(t *testing.T) {
	saveObserveFixtures(t)
	type call struct{ provider, account, model, agent string }
	var calls []call
	previous := AccountObserver
	AccountObserver = func(_ context.Context, provider, account, model, agent string) {
		calls = append(calls, call{provider, account, model, agent})
	}
	t.Cleanup(func() { AccountObserver = previous })

	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	if err := RecordObservation(context.Background(), Observation{At: at, Source: "test", Model: "seat-model", Agent: "claude", Excerpt: "rate limit reached"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != (call{"fixture-vendor", "seat", "seat-model", "claude"}) {
		t.Fatalf("seat not reported: %+v", calls)
	}
	// A local run proves no seat, so nothing is reported.
	if err := RecordObservation(context.Background(), Observation{At: at, Source: "test", Model: "local-model", Excerpt: "fine"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("local run reported an account: %+v", calls)
	}
}
