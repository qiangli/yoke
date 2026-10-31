// Package provider is the versioned host/VM provider contract (S472).
//
// A provider package is either a declarative manifest (data only, run by the
// built-in engine) or an executable plugin (argv-invoked, JSON over
// stdin/stdout, diagnostics on stderr). Both speak the same Request/Result
// schema below. This file freezes the shared types so the contract, loader
// and dispatcher stories can proceed in parallel; behavior lives elsewhere.
package provider

import (
	"context"
	"encoding/json"
	"time"
)

// ProtocolVersion is the only protocol version this package speaks.
const ProtocolVersion = "provider/v1"

// KindInfra is the Manifest.Kind of every provider package (the fleet kind
// and bashy verb of record are both "infra", S472.0).
const KindInfra = "infra"

// Op is one lifecycle operation.
type Op string

const (
	OpValidate Op = "validate" // validate credentials
	OpCapacity Op = "capacity" // discover regions/sizes
	OpPlan     Op = "plan"
	OpCreate   Op = "create"
	OpInspect  Op = "inspect"
	OpList     Op = "list"
	OpStart    Op = "start"
	OpStop     Op = "stop"
	OpDestroy  Op = "destroy"
	// Optional capabilities.
	OpReboot Op = "reboot"
	OpResize Op = "resize"
)

// Capability names an operation or optional feature a provider declares.
// Every Op is also a capability; "volumes" and "spot" are feature flags.
type Capability string

const (
	CapVolumes Capability = "volumes"
	CapSpot    Capability = "spot"
)

// Form is the shape of a provider package.
type Form string

const (
	FormDeclarative Form = "declarative"
	FormExecutable  Form = "executable"
	// FormBuiltin is a Go adapter linked into the host (e.g. the
	// DigitalOcean reference adapter). It has neither Argv nor Declarative.
	FormBuiltin Form = "builtin"
)

// Manifest identifies a provider package. Credentials are never manifest
// fields; they are references resolved by the caller.
type Manifest struct {
	Name            string       `json:"name" yaml:"name"`
	Kind            string       `json:"kind" yaml:"kind"` // KindInfra ("infra")
	Form            Form         `json:"form" yaml:"form"`
	Version         string       `json:"version,omitempty" yaml:"version,omitempty"`
	ProtocolVersion string       `json:"protocol_version" yaml:"protocol_version"`
	Summary         string       `json:"summary,omitempty" yaml:"summary,omitempty"`
	Capabilities    []Capability `json:"capabilities" yaml:"capabilities"`
	// Argv is the executable plugin's command line (FormExecutable only).
	// It is exec'd directly, never through a shell.
	Argv []string `json:"argv,omitempty" yaml:"argv,omitempty"`
	// Declarative holds the endpoint/template/lifecycle-mapping document
	// (FormDeclarative only). Its schema is owned by the declarative engine.
	Declarative json.RawMessage `json:"declarative,omitempty" yaml:"-"`
}

// State is the normalized lifecycle state of a provider resource. It says
// nothing about satellite installation or readiness.
type State string

const (
	StatePending  State = "pending"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateDeleting State = "deleting"
	StateDeleted  State = "deleted"
	StateError    State = "error"
	StateUnknown  State = "unknown"
)

// AuthKind is the kind of destroy authorization (S450.1 amendment A6).
type AuthKind string

const (
	// AuthConfirmation is a single-use user confirmation bound to one
	// resource ID and one idempotency key.
	AuthConfirmation AuthKind = "confirmation"
	// AuthTemporary is a temporary-lifecycle authorization (TTL or
	// work-bound) recorded at create and tagged on the resource.
	AuthTemporary AuthKind = "temporary"
)

// Authorization accompanies destroy requests (and, for AuthTemporary,
// the create that recorded it).
type Authorization struct {
	Kind           AuthKind  `json:"kind"`
	ResourceID     string    `json:"resource_id,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"` // TTL form
	WorkRef        string    `json:"work_ref,omitempty"`   // work-bound form
}

// Spec is the create/plan input.
type Spec struct {
	Name   string   `json:"name,omitempty"`
	Region string   `json:"region,omitempty"`
	Size   string   `json:"size,omitempty"`
	Image  string   `json:"image,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	// SSHKeys are provider key IDs or fingerprints installed at first boot.
	SSHKeys []string          `json:"ssh_keys,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	// UserData is opaque (e.g. an enrollment bundle) and passed through unread.
	UserData []byte `json:"user_data,omitempty"`
}

// Request is one operation call.
type Request struct {
	ProtocolVersion string         `json:"protocol_version"`
	Op              Op             `json:"op"`
	IdempotencyKey  string         `json:"idempotency_key,omitempty"` // required for mutating ops
	ResourceID      string         `json:"resource_id,omitempty"`
	Spec            *Spec          `json:"spec,omitempty"`
	Authorization   *Authorization `json:"authorization,omitempty"`
}

// Resource is a provider resource as reported by the provider.
type Resource struct {
	ID        string    `json:"id"` // provider resource ID, preserved verbatim
	Name      string    `json:"name,omitempty"`
	State     State     `json:"state"`
	RawStatus string    `json:"raw_status,omitempty"`
	Region    string    `json:"region,omitempty"`
	Size      string    `json:"size,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	PublicIPs []string  `json:"public_ips,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// CostReport describes what keeps costing money after stop or a refused /
// partial destroy (retained disks, reserved IPs, the instance itself).
type CostReport struct {
	Ongoing  bool     `json:"ongoing"`
	Retained []string `json:"retained,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// ErrorCode is a normalized error code.
type ErrorCode string

const (
	ErrUnsupportedCapability ErrorCode = "unsupported_capability"
	ErrConfirmationRequired  ErrorCode = "confirmation_required"
	ErrNotOwned              ErrorCode = "not_owned" // untagged or foreign resource
	ErrNotFound              ErrorCode = "not_found"
	ErrInvalidRequest        ErrorCode = "invalid_request"
	ErrBudgetExceeded        ErrorCode = "budget_exceeded"
	ErrQuotaExceeded         ErrorCode = "quota_exceeded"
	ErrInvalidCredentials    ErrorCode = "invalid_credentials"
	ErrProtocolMismatch      ErrorCode = "protocol_mismatch"
	ErrRateLimited           ErrorCode = "rate_limited"
	ErrTimeout               ErrorCode = "timeout"
	ErrProvider              ErrorCode = "provider_error"
	ErrNeedsManualReconcile  ErrorCode = "needs_manual_reconcile"
	ErrExecDisabled          ErrorCode = "exec_disabled" // executable plugin in a no-exec context
)

// Error is a structured, normalized error.
type Error struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Result is one operation's outcome. Exactly one of the payload fields or
// Error is meaningful.
type Result struct {
	ProtocolVersion string          `json:"protocol_version"`
	Op              Op              `json:"op"`
	Resource        *Resource       `json:"resource,omitempty"`
	Resources       []Resource      `json:"resources,omitempty"`
	Cost            *CostReport     `json:"cost,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"` // validate/capacity/plan payloads
	Error           *Error          `json:"error,omitempty"`
}

// Adapter executes contract requests for one provider. Built-in adapters,
// the declarative engine and the executable-plugin runner all implement it.
type Adapter interface {
	Manifest() Manifest
	Do(ctx context.Context, req Request) (Result, error)
}
