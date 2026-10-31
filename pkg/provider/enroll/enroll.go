// Package enroll is the OSS client for the public dhnt.io satellite
// enrollment API (S472.13b item 1, executable path).
//
// The master exposes this API for externally created hosts: the caller
// names a satellite the owner owns, presents the owner's normal session
// (a bearer token resolved by the caller through the existing bashy
// login/bearer conventions — never invented here), and receives a bundle:
// a single-use enrollment token, the master's Ed25519 public key, the
// master URL, and the artifact location. The bundle's user_data script
// (the complete first-boot script rendered by the master) is passed to
// the dispatcher's create as opaque Spec.UserData.
//
// Wire shape (public contract, kept identical to what the master half
// implements; this package refers only to the public API, never to
// cloudbox internals):
//
//	POST {masterURL}/api/v1/satellites/enrollment-bundle
//	Authorization: Bearer <owner session>
//	Content-Type: application/json
//	{"satellite_id": "sat-1", "endpoint": "sat-1.example.com" (optional)}
//
//	201 Created (200 also accepted):
//	{"satellite_id": "...", "enrollment_token": "<single-use>",
//	 "master_public_key": "ed25519:...", "master_url": "https://...",
//	 "artifact_url": "https://...", "expires_at": "RFC3339" (optional),
//	 "user_data": "<complete first-boot script, plain string>"}
//
// user_data is the complete first-boot script as a plain string, rendered
// by the master with the SAME script it uses for its own provisioning
// (one install path). The executable path passes it through byte-identical
// as Spec.UserData; a VM created with anything else (e.g. a JSON encoding
// of the bundle) would never install or enroll, because cloud-init only
// runs a script/cloud-config.
//
// Provider credentials come from the local CredentialSource the caller
// wired into the dispatcher (yoke/pkg/secrets on local bashy) and are
// NEVER sent to the master: this client carries only the owner session,
// and the bundle request body names only the satellite (plus an optional
// public endpoint hint).
//
// This package is stdlib-only and runs on user-owned compute (local bashy
// or an existing satellite) with the dispatcher opened ExecAllowed.
package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// EnrollmentPath is the public enrollment-bundle endpoint path, relative
// to the master's base URL. The master half implements this exact path.
const EnrollmentPath = "/api/v1/satellites/enrollment-bundle"

// Structured error kinds returned by this package.
const (
	// KindMasterUnavailable means the master could not be reached or
	// answered 5xx/503. Provisioning fails with this kind BEFORE any
	// provider call; existing satellites keep operating (S450.3).
	KindMasterUnavailable = "master_unavailable"
	// KindEnrollFailed is any other enrollment failure (auth, unknown
	// satellite, malformed bundle). It also precedes any provider call.
	KindEnrollFailed = "enroll_failed"
	// KindBundleIncomplete means the master issued a bundle without the
	// complete first-boot script (user_data empty). UserData refuses to
	// guess: it returns this kind instead of a JSON encoding, which a VM
	// would never execute.
	KindBundleIncomplete = "bundle_incomplete"
)

// Error is a structured enrollment failure.
type Error struct {
	Kind    string
	Message string
	Status  int // HTTP status when the master answered, else 0
	Err     error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("enroll: %s: %s (http %d)", e.Kind, e.Message, e.Status)
	}
	return fmt.Sprintf("enroll: %s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf reports the structured kind of err, or "" when err carries none.
func KindOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// enrollmentRequest is the wire request document.
type enrollmentRequest struct {
	SatelliteID string `json:"satellite_id"`
	Endpoint    string `json:"endpoint,omitempty"`
}

// Bundle is one enrollment bundle as issued by the master. UserDataScript
// is the complete first-boot script (plain string) rendered by the master.
type Bundle struct {
	SatelliteID     string    `json:"satellite_id"`
	EnrollmentToken string    `json:"enrollment_token"`
	MasterPublicKey string    `json:"master_public_key"`
	MasterURL       string    `json:"master_url"`
	ArtifactURL     string    `json:"artifact_url"`
	UserDataScript  string    `json:"user_data"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
}

// UserData returns the master's first-boot script byte-identical for use
// as the opaque create user-data the dispatcher passes through unread
// (Spec.UserData). It never returns a JSON encoding of the bundle: a VM
// created with JSON user-data would never install or enroll. An empty
// script fails with kind bundle_incomplete.
func (b Bundle) UserData() ([]byte, error) {
	if b.UserDataScript == "" {
		return nil, &Error{Kind: KindBundleIncomplete,
			Message: "master issued a bundle without user_data (the first-boot script)"}
	}
	return []byte(b.UserDataScript), nil
}

// Client is an enrollment client for one master. Session is the owner's
// normal session bearer (or a satellite's owner-delegated identity),
// supplied by the caller. HTTP may be nil for a default 30s client.
type Client struct {
	BaseURL string
	Session string
	HTTP    *http.Client
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// RequestBundle obtains an enrollment bundle for the named satellite. The
// only credential on the wire is the owner session; provider credentials
// are never in scope here and are never sent.
func (c Client) RequestBundle(ctx context.Context, satelliteID, endpoint string) (Bundle, error) {
	if strings.TrimSpace(satelliteID) == "" {
		return Bundle{}, &Error{Kind: KindEnrollFailed, Message: "satellite_id is required"}
	}
	if strings.TrimSpace(c.Session) == "" {
		return Bundle{}, &Error{Kind: KindEnrollFailed, Message: "no owner session (pair with `bashy login` or set the session token)"}
	}
	body, err := json.Marshal(enrollmentRequest{SatelliteID: satelliteID, Endpoint: endpoint})
	if err != nil {
		return Bundle{}, &Error{Kind: KindEnrollFailed, Message: fmt.Sprintf("encode request: %v", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+EnrollmentPath, bytes.NewReader(body))
	if err != nil {
		return Bundle{}, &Error{Kind: KindEnrollFailed, Message: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.Session)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Bundle{}, &Error{Kind: KindMasterUnavailable, Message: fmt.Sprintf("master unreachable: %v", err), Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Bundle{}, &Error{Kind: KindEnrollFailed, Message: fmt.Sprintf("read bundle: %v", err), Status: resp.StatusCode}
	}
	if resp.StatusCode >= 500 {
		return Bundle{}, &Error{Kind: KindMasterUnavailable,
			Message: fmt.Sprintf("master answered %s", resp.Status), Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return Bundle{}, &Error{Kind: KindEnrollFailed,
			Message: fmt.Sprintf("master refused the bundle request: %s: %s", resp.Status, strings.TrimSpace(string(raw))),
			Status:  resp.StatusCode}
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return Bundle{}, &Error{Kind: KindEnrollFailed, Message: fmt.Sprintf("decode bundle: %v", err), Status: resp.StatusCode}
	}
	if b.EnrollmentToken == "" || b.MasterPublicKey == "" || b.MasterURL == "" || b.ArtifactURL == "" || b.UserDataScript == "" {
		return Bundle{}, &Error{Kind: KindBundleIncomplete,
			Message: "master issued an incomplete bundle (need enrollment_token, master_public_key, master_url, artifact_url, user_data)",
			Status:  resp.StatusCode}
	}
	if b.SatelliteID == "" {
		b.SatelliteID = satelliteID
	}
	return b, nil
}

// ProvisionRequest is one executable-path provisioning.
type ProvisionRequest struct {
	// SatelliteID is the satellite the owner names at the master.
	SatelliteID string
	// Endpoint is the optional public endpoint hint sent to the master.
	Endpoint string
	// Spec is the provider create spec; its UserData is replaced with the
	// bundle's user_data first-boot script (opaque to the provider).
	Spec provider.Spec
	// IdempotencyKey is the create's idempotency key (required).
	IdempotencyKey string
	// Authorization is an optional temporary-lifecycle authorization
	// recorded at create (A6); nil creates a persistent resource.
	Authorization *provider.Authorization
	// ModelDir, when set, makes this a GPU worker: the plan's model_dir
	// (e.g. the EC2 adapter's /var/lib/ollama on the root gp3 volume). The
	// user data becomes GPUWorkerUserData — a prologue exporting
	// OLLAMA_MODELS=ModelDir, then the master's script unchanged.
	ModelDir string
	// Progress receives one human-readable line per stage; nil is silent.
	Progress func(string)
	// Reserved, when set, receives the stable address reserved for the
	// host before the enrollment bundle was requested. It is not called
	// when Endpoint was given (the caller named the endpoint, so nothing
	// is reserved) or when the provider reserves nothing.
	Reserved func(provider.Reservation)
}

// ProvisionExecutable provisions one host on the executable path, in this
// order:
//
//  1. RESERVE the host's stable public address, when the provider can
//     hold one and the caller did not name an endpoint itself
//     (provider.EndpointReserver). This is first for a load-bearing
//     reason: the master renders the first-boot script with the
//     satellite's endpoint baked in, so the endpoint has to be known
//     before the bundle is asked for — and an address reserved ahead of
//     the host cannot race its own attachment at first boot.
//  2. Obtain the enrollment bundle from the master, with the reserved
//     address as the endpoint hint. An unreachable master or a 5xx/503
//     answer fails with kind master_unavailable BEFORE any create.
//  3. Create through the dispatcher (opened ExecAllowed: this runs on
//     user-owned compute) with the bundle's user_data script as opaque
//     Spec.UserData. The provider attaches the reserved address as part
//     of the create.
//
// Provider credentials stay in the dispatcher's local CredentialSource
// and never reach the master.
func ProvisionExecutable(ctx context.Context, c Client, d *provider.Dispatcher, req ProvisionRequest) (provider.Result, error) {
	report := func(s string) {
		if req.Progress != nil {
			req.Progress(s)
		}
	}
	if req.ModelDir != "" {
		// Refuse a bad model_dir before anything is reserved or asked for.
		if err := validModelDir(req.ModelDir); err != nil {
			return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate}, err
		}
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		res, rerr := provider.ReserveEndpoint(ctx, d.Adapter(), req.Spec)
		if rerr != nil {
			// Without the address the bundle would be rendered for an
			// endpoint the satellite never answers on, which installs a
			// host that silently never enrolls. Fail before the master
			// is asked for anything.
			return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate}, rerr
		}
		if res.Held() {
			verb := "reserved"
			if res.Reused {
				verb = "reusing"
			}
			report(fmt.Sprintf("%s stable address %s (%s) for satellite %q", verb, res.Address, res.Name, req.SatelliteID))
			if res.Endpoint != "" {
				endpoint = res.Endpoint
				report(fmt.Sprintf("endpoint hint for the enrollment bundle: %s", endpoint))
			}
			if req.Reserved != nil {
				req.Reserved(res)
			}
		}
	}
	report(fmt.Sprintf("requesting enrollment bundle for satellite %q", req.SatelliteID))
	bundle, err := c.RequestBundle(ctx, req.SatelliteID, endpoint)
	if err != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate}, err
	}
	userData, err := bundle.UserData()
	if err != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate}, err
	}
	if req.ModelDir != "" {
		if userData, err = GPUWorkerUserData(userData, req.ModelDir); err != nil {
			return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate}, err
		}
		report(fmt.Sprintf("GPU worker first boot keeps models in %s (%s)", req.ModelDir, ModelsEnv))
	}
	spec := req.Spec
	spec.UserData = userData
	report(fmt.Sprintf("creating host for satellite %q through provider %q", req.SatelliteID, d.Manifest().Name))
	return d.Do(ctx, provider.Request{
		ProtocolVersion: provider.ProtocolVersion,
		Op:              provider.OpCreate,
		IdempotencyKey:  req.IdempotencyKey,
		Spec:            &spec,
		Authorization:   req.Authorization,
	})
}

// DestroyRequest is one executable-path destroy.
type DestroyRequest struct {
	ResourceID     string
	IdempotencyKey string
	// Authorization carries the per-operation user confirmation (A6).
	// Without it the dispatcher refuses the destroy before any provider
	// call; temporary (TTL/work-bound) resources are the only exception.
	Authorization *provider.Authorization
}

// DestroyExecutable destroys one host on the executable path through the
// dispatcher's A6 per-operation confirmation: without a confirmation bound
// to this resource ID and idempotency key the destroy is refused and the
// provider is never called.
func DestroyExecutable(ctx context.Context, d *provider.Dispatcher, req DestroyRequest) (provider.Result, error) {
	return d.Do(ctx, provider.Request{
		ProtocolVersion: provider.ProtocolVersion,
		Op:              provider.OpDestroy,
		IdempotencyKey:  req.IdempotencyKey,
		ResourceID:      req.ResourceID,
		Authorization:   req.Authorization,
	})
}
