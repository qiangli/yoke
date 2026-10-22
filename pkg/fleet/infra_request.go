package fleet

// `infra request` — thin client over the master provisioning-request
// endpoints for agents (S490 story a0e6f1ae).
//
// It speaks only the Bearer [REDACTED] under /api/v1/provisioning/requests (list,
// get, resume, stop), with the base URL and Bearer [REDACTED] resolved through
// ResolveCloud — the ONE ladder every bashy-side cloudbox client walks
// (see cloudbox.go; the same resolution `bashy token` uses). The provider
// token, when one is needed, comes from the vault-injected environment via
// --credential-ref and is sent in the request body, never printed.
//
// Wire notes (kept identical to the master half in cloudbox
// hub/internal/handlers/provisioning_master.go; this package refers only
// to the public API, never to cloudbox internals):
//
//	GET  /api/v1/provisioning/requests        -> {"requests": [...]}
//	GET  /api/v1/provisioning/requests/:id    -> {"request": {...}}
//	POST /api/v1/provisioning/requests/:id/resume {"token": "..."} (optional)
//	POST /api/v1/provisioning/requests/:id/stop
//
// A resume without --credential-ref sends no body, so the master
// continues with its vault-stored token. With --credential-ref the
// provider token resolved from the environment is sent as {"token": ...}.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// ProvisionRequest is one master provisioning request as the v1 API
// returns it. It mirrors the master's record shape; unknown fields ride
// along in --json output because the client passes the record through.
type ProvisionRequest struct {
	ID             string `json:"id"`
	Owner          string `json:"owner,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Region         string `json:"region,omitempty"`
	Size           string `json:"size,omitempty"`
	Image          string `json:"image,omitempty"`
	Name           string `json:"name,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	SatelliteID    string `json:"satellite_id,omitempty"`
	Profile        string `json:"profile,omitempty"`
	State          string `json:"state,omitempty"`
	NeedsToken     bool   `json:"needs_token,omitempty"`
	ResourceID     string `json:"resource_id,omitempty"`
}

// ProvisionError is a master refusal with its HTTP status and the
// master's code kept machine readable.
type ProvisionError struct {
	Op      string // e.g. "GET /api/v1/provisioning/requests"
	Status  int
	Code    string // e.g. "not_found", "token_required"
	Message string
}

func (e *ProvisionError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("fleet: %s: %d %s: %s", e.Op, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("fleet: %s: %d: %s", e.Op, e.Status, e.Message)
}

// ProvisioningClient talks to the master provisioning-request surface.
// Zero network state — one *http.Client, a resolved base URL, and a
// caller token, exactly like TokenClient.
type ProvisioningClient struct {
	BaseURL string // e.g. https://ai.dhnt.io (no trailing /api/v1)
	Token   string // Bearer [REDACTED] for the v1 provisioning API
	HTTP    *http.Client
}

// NewProvisioningClient resolves base + caller token through
// ResolveCloud, the same ladder `bashy token` walks.
func NewProvisioningClient(urlOverride, tokenOverride string) (ProvisioningClient, error) {
	base, tok := ResolveCloud(urlOverride, tokenOverride)
	if tok == "" {
		return ProvisioningClient{}, fmt.Errorf("fleet: no cloudbox token (set $BASHY_FLEET_TOKEN or pass --token)")
	}
	return ProvisioningClient{BaseURL: base, Token: tok, HTTP: httpClient()}, nil
}

func (c ProvisioningClient) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(payload)
	}
	op := method + " " + path
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, provisionAPIError(op, resp.StatusCode, raw)
	}
	return raw, nil
}

// provisionAPIError renders a refusal body as a *ProvisionError. The
// provisioning envelope is {code, error, ...}; a bare message body still
// reads sanely.
func provisionAPIError(op string, status int, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	code := ""
	if msg == "" {
		msg = http.StatusText(status)
	} else {
		var env struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && (env.Error != "" || env.Code != "") {
			code, msg = env.Code, env.Error
			if msg == "" {
				msg = code
			}
		}
	}
	return &ProvisionError{Op: op, Status: status, Code: code, Message: msg}
}

// List returns the caller's provisioning requests plus the raw envelope
// for --json passthrough.
func (c ProvisioningClient) List() ([]ProvisionRequest, json.RawMessage, error) {
	raw, err := c.do(http.MethodGet, "/api/v1/provisioning/requests", nil)
	if err != nil {
		return nil, nil, err
	}
	var env struct {
		Requests []ProvisionRequest `json:"requests"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("fleet: GET /api/v1/provisioning/requests: decode: %w", err)
	}
	if env.Requests == nil {
		env.Requests = []ProvisionRequest{}
	}
	return env.Requests, json.RawMessage(raw), nil
}

// Get returns one provisioning request plus its raw record.
func (c ProvisioningClient) Get(id string) (ProvisionRequest, json.RawMessage, error) {
	var zero ProvisionRequest
	if strings.TrimSpace(id) == "" {
		return zero, nil, fmt.Errorf("fleet: request id is required (--id)")
	}
	raw, err := c.do(http.MethodGet, "/api/v1/provisioning/requests/"+id, nil)
	if err != nil {
		return zero, nil, err
	}
	return decodeProvisionItem("GET /api/v1/provisioning/requests/:id", raw)
}

// Resume continues an interrupted request. An empty token sends no body,
// so the master continues with its vault-stored token; a non-empty token
// is sent as {"token": ...} and never logged or printed by this client.
func (c ProvisioningClient) Resume(id, token string) (ProvisionRequest, json.RawMessage, error) {
	var zero ProvisionRequest
	if strings.TrimSpace(id) == "" {
		return zero, nil, fmt.Errorf("fleet: request id is required (--id)")
	}
	var body any
	if token != "" {
		body = map[string]any{"token": token}
	}
	raw, err := c.do(http.MethodPost, "/api/v1/provisioning/requests/"+id+"/resume", body)
	if err != nil {
		return zero, nil, err
	}
	return decodeProvisionItem("POST /api/v1/provisioning/requests/:id/resume", raw)
}

// Stop powers the request's VM off (it keeps billing until destroyed).
func (c ProvisioningClient) Stop(id string) (ProvisionRequest, json.RawMessage, error) {
	var zero ProvisionRequest
	if strings.TrimSpace(id) == "" {
		return zero, nil, fmt.Errorf("fleet: request id is required (--id)")
	}
	raw, err := c.do(http.MethodPost, "/api/v1/provisioning/requests/"+id+"/stop", nil)
	if err != nil {
		return zero, nil, err
	}
	return decodeProvisionItem("POST /api/v1/provisioning/requests/:id/stop", raw)
}

func decodeProvisionItem(op string, raw []byte) (ProvisionRequest, json.RawMessage, error) {
	var zero ProvisionRequest
	var env struct {
		Request ProvisionRequest `json:"request"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return zero, nil, fmt.Errorf("fleet: %s: decode: %w", op, err)
	}
	reraw, err := json.Marshal(env.Request)
	if err != nil {
		return zero, nil, err
	}
	return env.Request, json.RawMessage(reraw), nil
}

// NewInfraRequestCmd builds the `infra request` verb tree: ls, status,
// resume and stop over the master v1 provisioning-request endpoints.
func NewInfraRequestCmd() *cobra.Command {
	var master, token string
	var asJSON bool
	c := &cobra.Command{
		Use:   "request",
		Short: "List and drive master provisioning requests (thin v1 client)",
	}
	c.PersistentFlags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	c.PersistentFlags().StringVar(&token, "token", "", "master Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY, else the paired credential)")
	c.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the caller's provisioning requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := NewProvisioningClient(master, token)
			if err != nil {
				return err
			}
			rows, raw, err := client.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				_, err := out.Write(append(raw, '\n'))
				return err
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "no requests")
				return nil
			}
			for _, r := range rows {
				fmt.Fprintf(out, "%s\t%s\t%s\t%s/%s\t%s\n",
					r.ID, r.SatelliteID, r.Provider, r.Region, r.Size, r.State)
			}
			return nil
		},
	}
	ls.Aliases = []string{"list"}

	var statusID string
	status := &cobra.Command{
		Use:   "status --id ID",
		Short: "Show one provisioning request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := NewProvisioningClient(master, token)
			if err != nil {
				return err
			}
			r, raw, err := client.Get(statusID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				_, err := out.Write(append(raw, '\n'))
				return err
			}
			fmt.Fprintf(out, "id: %s\n", r.ID)
			fmt.Fprintf(out, "satellite: %s\n", r.SatelliteID)
			fmt.Fprintf(out, "provider: %s\n", r.Provider)
			fmt.Fprintf(out, "region: %s\n", r.Region)
			fmt.Fprintf(out, "size: %s\n", r.Size)
			if r.Image != "" {
				fmt.Fprintf(out, "image: %s\n", r.Image)
			}
			if r.Profile != "" {
				fmt.Fprintf(out, "profile: %s\n", r.Profile)
			}
			fmt.Fprintf(out, "state: %s\n", r.State)
			if r.NeedsToken {
				fmt.Fprintln(out, "needs_token: true")
			}
			if r.ResourceID != "" {
				fmt.Fprintf(out, "resource: %s\n", r.ResourceID)
			}
			return nil
		},
	}
	status.Flags().StringVar(&statusID, "id", "", "provisioning request id (required)")

	var resumeID, credentialRef string
	resume := &cobra.Command{
		Use:   "resume --id ID",
		Short: "Continue an interrupted request (vault token unless --credential-ref)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var provToken string
			if credentialRef != "" {
				tok, err := resolveProviderCredential(credentialRef)
				if err != nil {
					return err
				}
				provToken = tok
			}
			client, err := NewProvisioningClient(master, token)
			if err != nil {
				return err
			}
			r, raw, err := client.Resume(resumeID, provToken)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				_, err := out.Write(append(raw, '\n'))
				return err
			}
			fmt.Fprintf(out, "resumed %s (%s)\n", r.ID, r.State)
			return nil
		},
	}
	resume.Flags().StringVar(&resumeID, "id", "", "provisioning request id (required)")
	resume.Flags().StringVar(&credentialRef, "credential-ref", "", "pkg/secrets ref for a fresh provider token (default: the master's vault-stored token)")

	var stopID string
	stop := &cobra.Command{
		Use:   "stop --id ID",
		Short: "Power a request's VM off (it keeps billing until destroyed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := NewProvisioningClient(master, token)
			if err != nil {
				return err
			}
			r, raw, err := client.Stop(stopID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				_, err := out.Write(append(raw, '\n'))
				return err
			}
			fmt.Fprintf(out, "stopped %s (%s)\n", r.ID, r.State)
			return nil
		},
	}
	stop.Flags().StringVar(&stopID, "id", "", "provisioning request id (required)")

	c.AddCommand(ls, status, resume, stop)
	return c
}
