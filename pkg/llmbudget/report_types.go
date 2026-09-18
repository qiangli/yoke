package llmbudget

import (
	"context"
	"time"
)

const ReportSchemaVersion = "bashy-model-usage-v1"

// Binding names a real capacity pool. Account is an opaque operator/provider
// identity, never a credential value. Unknown accounts do not imply capacity.
type Binding struct {
	Provider     string `json:"provider"`
	Account      string `json:"account"`
	Pool         string `json:"pool"`
	Model        string `json:"model"`
	Agent        string `json:"agent,omitempty"`
	Lane         Lane   `json:"lane"`
	AccountKnown bool   `json:"account_known"`
}
type Attribution struct {
	Model  string `json:"model"`
	Agent  string `json:"agent"`
	Run    string `json:"run"`
	Host   string `json:"host"`
	Sprint int64  `json:"sprint"`
	Active bool   `json:"active"`
}
type Metric struct {
	Name           string     `json:"name"`
	Value          *float64   `json:"value"`
	Unit           string     `json:"unit"`
	Classification string     `json:"classification"`
	Source         string     `json:"source"`
	ObservedAt     time.Time  `json:"observed_at"`
	WindowStart    *time.Time `json:"window_start,omitempty"`
	WindowEnd      *time.Time `json:"window_end,omitempty"`
	ResetAt        *time.Time `json:"reset_at,omitempty"`
	Limitation     string     `json:"limitation,omitempty"`
}
type AccountReport struct {
	Provider           string        `json:"provider"`
	Account            string        `json:"account"`
	Pool               string        `json:"pool"`
	Lane               Lane          `json:"lane"`
	AccountKnown       bool          `json:"account_known"`
	Status             string        `json:"status"`
	Models             []string      `json:"models"`
	Agents             []string      `json:"agents"`
	Attribution        []Attribution `json:"attribution"`
	Metrics            []Metric      `json:"metrics"`
	ActiveReservations int           `json:"active_reservations"`
	RetryAt            *time.Time    `json:"retry_at,omitempty"`
	Limitations        []string      `json:"limitations"`
}
type Report struct {
	SchemaVersion string          `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Accounts      []AccountReport `json:"accounts"`
	Warnings      []string        `json:"warnings"`
}
type ReportOptions struct {
	Provider, Model string
	Refresh         bool
	Roster          []Binding
	Active          []Attribution
	Now             time.Time
}

// Request is a reservation, not a measurement. SpendMicroUSD=nil means no
// trustworthy estimate; a hard spend policy must refuse that uncertainty.
type Request struct {
	UnknownTokens bool          `json:"unknown_tokens,omitempty"`
	UnknownMemory bool          `json:"unknown_memory,omitempty"`
	ID            string        `json:"id"`
	Owner         string        `json:"owner"`
	Provider      string        `json:"provider"`
	Account       string        `json:"account"`
	Pool          string        `json:"pool"`
	Model         string        `json:"model"`
	Agent         string        `json:"agent,omitempty"`
	Run           string        `json:"run,omitempty"`
	Host          string        `json:"host,omitempty"`
	Sprint        int64         `json:"sprint,omitempty"`
	Lane          Lane          `json:"lane"`
	Tokens        int64         `json:"tokens"`
	SpendMicroUSD *int64        `json:"spend_micro_usd"`
	Concurrency   int           `json:"concurrency"`
	HostSlots     int           `json:"host_slots"`
	MemoryBytes   uint64        `json:"memory_bytes"`
	TTL           time.Duration `json:"ttl"`
	AllowPremium  bool          `json:"allow_premium,omitempty"`
}
type Reservation struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Request   Request   `json:"request"`
	CreatedAt time.Time `json:"created_at"`
	RenewedAt time.Time `json:"renewed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Actual struct {
	TokensEstimated   bool      `json:"tokens_estimated,omitempty"`
	InputTokens       int64     `json:"input_tokens"`
	OutputTokens      int64     `json:"output_tokens"`
	CachedInputTokens int64     `json:"cached_input_tokens"`
	SpendMicroUSD     *int64    `json:"spend_micro_usd"`
	Source            string    `json:"source"`
	ObservedAt        time.Time `json:"observed_at"`
}
type Admission struct {
	Decision    Decision     `json:"decision"`
	Reservation *Reservation `json:"reservation,omitempty"`
	RetryAt     *time.Time   `json:"retry_at,omitempty"`
}

// TerminationProof is supplied by the lifecycle authority after verifying the
// actual work ended. Supervisor death or an expired TTL is not such proof.
type TerminationProof struct {
	Owner      string    `json:"owner"`
	Run        string    `json:"run"`
	Host       string    `json:"host"`
	VerifiedAt time.Time `json:"verified_at"`
	Evidence   string    `json:"evidence"`
}

// Policy is explicit, versioned local policy. Nil ceilings are unconfigured;
// zero ceilings are enforced. Account and host axes share one transaction.
type Policy struct {
	missing     bool
	Version     int            `json:"version"`
	Bindings    []Binding      `json:"bindings"`
	Constraints []Constraint   `json:"constraints"`
	Routes      []Route        `json:"routes"`
	Sources     []SourceConfig `json:"sources"`
}
type Constraint struct {
	Provider            string  `json:"provider,omitempty"`
	Account             string  `json:"account,omitempty"`
	Pool                string  `json:"pool,omitempty"`
	Lane                Lane    `json:"lane,omitempty"`
	Host                string  `json:"host,omitempty"`
	Soft                bool    `json:"soft,omitempty"`
	DailyTokens         *int64  `json:"daily_tokens,omitempty"`
	WeeklyTokens        *int64  `json:"weekly_tokens,omitempty"`
	DailySpendMicroUSD  *int64  `json:"daily_spend_micro_usd,omitempty"`
	WeeklySpendMicroUSD *int64  `json:"weekly_spend_micro_usd,omitempty"`
	Concurrency         *int    `json:"concurrency,omitempty"`
	HostSlots           *int    `json:"host_slots,omitempty"`
	MemoryBytes         *uint64 `json:"memory_bytes,omitempty"`
}
type Route struct {
	From         string `json:"from"`
	To           string `json:"to"`
	AllowPremium bool   `json:"allow_premium,omitempty"`
}

// SourceConfig contains references only. Network sources are disabled unless
// explicitly enabled and never discover credentials from a harness store.
type SourceConfig struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"` // claude-statusline | openai-organization
	Provider       string `json:"provider"`
	Account        string `json:"account"`
	Pool           string `json:"pool"`
	Lane           Lane   `json:"lane"`
	Enabled        bool   `json:"enabled"`
	Path           string `json:"path,omitempty"`
	CredentialRef  string `json:"credential_ref,omitempty"`
	Organization   string `json:"organization,omitempty"`
	RefreshSeconds int    `json:"refresh_seconds,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}
type SourceResult struct {
	Status      string     `json:"status"`
	Metrics     []Metric   `json:"metrics"`
	Limitations []string   `json:"limitations"`
	RetryAt     *time.Time `json:"retry_at,omitempty"`
}

// Adapter supports host-provided documented sources and hermetic fixtures.
// Implementers must never make inference calls to measure usage.
type Adapter interface {
	Kind() string
	Collect(context.Context, SourceConfig, time.Time) (SourceResult, error)
}
type CredentialResolver func(context.Context, string) (string, error)
