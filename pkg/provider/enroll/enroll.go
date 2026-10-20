// Package enroll is the OSS client for the public dhnt.io satellite
// enrollment API (S472.13b item 1, executable path).
//
// The master exposes this API for externally created hosts: the caller
// names a satellite the owner owns, presents the owner's normal session
// (a bearer token resolved by the caller through the existing bashy
// login/bearer conventions — never invented here), and receives a bundle:
// a single-use enrollment token, the master's Ed25519 public key, the
// master URL, and the artifact location. The bundle is passed to the
// dispatcher's create as opaque Spec.UserData.
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
//	 "artifact_url": "https://...", "expires_at": "RFC3339" (optional)}
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

// Bundle is one enrollment bundle as issued by the master.
type Bundle struct {
	SatelliteID     string    `json:"satellite_id"`
	EnrollmentToken string    `json:"enrollment_token"`
	MasterPublicKey string    `json:"master_public_key"`
	MasterURL       string    `json:"master_url"`
	ArtifactURL     string    `json:"artifact_url"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
}

// UserData renders the bundle as the opaque create user-data the
// dispatcher passes through unread (Spec.UserData).
func (b Bundle) UserData() ([]byte, error) {
	return json.Marshal(b)
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
	if b.EnrollmentToken == "" || b.MasterPublicKey == "" || b.MasterURL == "" || b.ArtifactURL == "" {
		return Bundle{}, &Error{Kind: KindEnrollFailed,
			Message: "master issued an incomplete bundle (need enrollment_token, master_public_key, master_url, artifact_url)",
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
	// enrollment bundle (opaque to the provider).
	Spec provider.Spec
	// IdempotencyKey is the create's idempotency key (required).
	IdempotencyKey string
	// Authorization is an optional temporary-lifecycle authorization
	// recorded at create (A6); nil creates a persistent resource.
	Authorization *provider.Authorization
	// Progress receives one human-readable line per stage; nil is silent.
	Progress func(string)
}

// ProvisionExecutable provisions one host on the executable path: FIRST it
// obtains the enrollment bundle from the master — an unreachable master or
// a 5xx/503 answer fails with kind master_unavailable BEFORE any provider
// call — then it creates through the dispatcher (opened ExecAllowed: this
// runs on user-owned compute) with the bundle as opaque Spec.UserData,
// and reports progress. Provider credentials stay in the dispatcher's
// local CredentialSource and never reach the master.
func ProvisionExecutable(ctx context.Context, c Client, d *provider.Dispatcher, req ProvisionRequest) (provider.Result, error) {
	report := func(s string) {
		if req.Progress != nil {
			req.Progress(s)
		}
	}
	report(fmt.Sprintf("requesting enrollment bundle for satellite %q", req.SatelliteID))
	bundle, err := c.RequestBundle(ctx, req.SatelliteID, req.Endpoint)
	if err != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate}, err
	}
	userData, err := bundle.UserData()
	if err != nil {
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpCreate},
			&Error{Kind: KindEnrollFailed, Message: fmt.Sprintf("encode bundle: %v", err)}
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
