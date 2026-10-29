package llmbudget

import (
	"context"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/quotaobs"
)

// Observation is the shared, redacted quota journal record.
type Observation = quotaobs.Observation
type ParsedSignal = quotaobs.ParsedSignal
type Analysis = quotaobs.Analysis
type ObservationFilter = quotaobs.Filter

// AccountObserver, when set, receives the account identity that one recorded
// observation PROVED: a real run reached this provider seat. pkg/accounts
// installs it at init and this package never imports the inventory — accounts
// already imports llmbudget for its cached report, so the dependency can only
// run one way. The hook is best-effort: its failures never reach the caller.
var AccountObserver func(ctx context.Context, provider, account, model, agent string)

// observedIdentity reduces one journal record to the seat it proves. An
// unknown provider, a local model, or an unresolved account proves no
// identity and is reported as nothing rather than as a guess.
func observedIdentity(o Observation) (string, string) {
	provider, account := "", strings.TrimSpace(o.Account)
	if m, ok := fleet.New().Model(o.Model); ok {
		provider = m.Provider
		if account == "" {
			account = quotaAccount(m)
		}
	}
	if account == "" {
		account = strings.TrimSpace(o.Plan)
	}
	switch strings.ToLower(provider) {
	case "", "unknown", "none":
		return "", ""
	}
	switch strings.ToLower(account) {
	case "", "local", "unresolved", "unknown":
		return "", ""
	}
	return provider, account
}

func RecordObservation(ctx context.Context, o Observation) error {
	if err := quotaobs.Record(ctx, o); err != nil {
		return err
	}
	// A journaled observation is proof that this seat ran, so the account
	// inventory learns the identity here rather than waiting for a refresh.
	if hook := AccountObserver; hook != nil {
		if provider, account := observedIdentity(o); provider != "" {
			hook(ctx, provider, account, o.Model, o.Agent)
		}
	}
	// The deterministic pass is cheap and runs even without analyzer consent.
	_, _ = MatchObservations(ctx)
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
