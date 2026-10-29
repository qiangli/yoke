package accounts

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

func init() {
	MustRegisterAdapter(NewDigitalOceanAdapter())
	MustRegisterAdapter(NewAWSAdapter())
}

var errCloudRead = errors.New("accounts: provider read failed")
var errCloudIdentity = errors.New("accounts: provider identity mismatch")

// ManagedAdapter is the generic hook for manually recorded managed services.
// Embed it and implement only the capabilities a service actually supports.
type ManagedAdapter struct{ ProviderName string }

func (a ManagedAdapter) Name() string                                  { return a.ProviderName }
func (ManagedAdapter) Capabilities() Capability                        { return 0 }
func (ManagedAdapter) Discover(context.Context) ([]Observation, error) { return nil, ErrUnsupported }
func (ManagedAdapter) Verify(context.Context, Account) (Observation, error) {
	return Observation{}, ErrUnsupported
}
func (ManagedAdapter) Resources(context.Context, Account) ([]ResourceRef, error) {
	return nil, ErrUnsupported
}
func (ManagedAdapter) Usage(context.Context, Account, string) ([]Metric, error) {
	return nil, ErrUnsupported
}
func (ManagedAdapter) Billing(context.Context, Account, string) ([]Metric, error) {
	return nil, ErrUnsupported
}
func (ManagedAdapter) Select(context.Context, Account, Operation) (Selection, error) {
	return Selection{}, ErrUnsupported
}

func cloudConnection(a Account, provider string) (Connection, error) {
	if a.Provider != provider {
		return Connection{}, ErrUnsupported
	}
	if len(a.Connections) != 1 {
		return Connection{}, ErrAmbiguous
	}
	c := a.Connections[0]
	if !ValidCredentialRef(c.CredentialRef) || strings.HasPrefix(c.CredentialRef, "ssh-key:") {
		return Connection{}, ErrUnsupported
	}
	return c, nil
}
func cloudPeriod(period string) error {
	switch period {
	case "", "day", "week", "month":
		return nil
	}
	return ErrUnsupported
}
func cloudMoney(name, source, period string, at time.Time) Metric {
	return Metric{Name: name, Unit: "usd", Currency: "USD", Source: source, Classification: ProviderReported, ObservedAt: at, Period: period, Coverage: Partial}
}
func cloudNumber(s string) *float64 {
	v, e := strconv.ParseFloat(s, 64)
	if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
func cloudMonth(m *Metric, at time.Time) {
	start := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	m.WindowStart = &start
	m.WindowEnd = &at
	m.Limitations = append(m.Limitations, "Month-to-date source window; not a full requested period.")
}
