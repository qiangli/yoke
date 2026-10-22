package fleet

// `infra credential` — thin client over the master role-scoped
// provider-credential endpoints (S489.6d, story 9ea7c26e068f).
//
// The credential value travels one way only: it is read from stdin
// (never a flag — the operator pipes `bashy ask --stdout` straight
// into add/rotate) and POSTed to a write-only master endpoint that
// validates it non-destructively, encrypts it at rest, and never
// returns it. `ls` shows metadata only. The master Bearer [REDACTED] resolved
// through ResolveCloud, exactly like `infra request`.
//
// Wire notes (kept identical to the master half in cloudbox
// hub/internal/handlers/provisioning_credentials.go; this package
// refers only to the public API, never to cloudbox internals):
//
//	POST /api/v1/provisioning/credentials         {"provider","role","value","expires_at?"} -> 201 {"credential": {...}}
//	GET  /api/v1/provisioning/credentials        -> {"credentials": [...]}
//	DELETE /api/v1/provisioning/credentials/:id  -> {"status":"revoked", ...}
//	POST /api/v1/provisioning/credentials/:id/rotate {"value","expires_at?"} -> {"credential": {...}}

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// CredentialRecord is one master credential as the v1 API returns it:
// metadata only. There is deliberately no value field — the master
// never sends one, so a client cannot print or log one.
type CredentialRecord struct {
	ID              uint64  `json:"id"`
	Owner           string  `json:"owner,omitempty"`
	Provider        string  `json:"provider,omitempty"`
	Role            string  `json:"role,omitempty"`
	AccountEmail    string  `json:"account_email,omitempty"`
	AccountUUID     string  `json:"account_uuid,omitempty"`
	ValidatedScopes string  `json:"validated_scopes,omitempty"`
	CreatedAt       string  `json:"created_at,omitempty"`
	ExpiresAt       *string `json:"expires_at,omitempty"`
	LastValidatedAt *string `json:"last_validated_at,omitempty"`
	ExpiryStatus    string  `json:"expiry_status,omitempty"`
}

// CredentialClient talks to the master credential surface. Zero
// network state — one *http.Client, a resolved base URL, and a caller
// token, exactly like ProvisioningClient.
type CredentialClient struct {
	BaseURL string // e.g. https://ai.dhnt.io (no trailing /api/v1)
	Token   string // Bearer [REDACTED] for the v1 provisioning API
	HTTP    *http.Client
}

// NewCredentialClient resolves base + caller token through
// ResolveCloud, the same ladder `bashy token` walks.
func NewCredentialClient(urlOverride, tokenOverride string) (CredentialClient, error) {
	base, tok := ResolveCloud(urlOverride, tokenOverride)
	if tok == "" {
		return CredentialClient{}, fmt.Errorf("fleet: no cloudbox token (set $BASHY_FLEET_TOKEN or pass --token)")
	}
	return CredentialClient{BaseURL: base, Token: tok, HTTP: httpClient()}, nil
}

func (c CredentialClient) do(method, path string, body any) ([]byte, error) {
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

// Add stores a role-scoped credential. The value is write-only: it is
// sent once in this POST and never appears in any response.
func (c CredentialClient) Add(provider, role, value, expiresAt string) (CredentialRecord, json.RawMessage, error) {
	var zero CredentialRecord
	body := map[string]any{"provider": provider, "role": role, "value": value}
	if expiresAt != "" {
		body["expires_at"] = expiresAt
	}
	raw, err := c.do(http.MethodPost, "/api/v1/provisioning/credentials", body)
	if err != nil {
		return zero, nil, err
	}
	return decodeCredentialItem("POST /api/v1/provisioning/credentials", raw)
}

// List returns the caller's credential metadata plus the raw envelope
// for --json passthrough.
func (c CredentialClient) List() ([]CredentialRecord, json.RawMessage, error) {
	raw, err := c.do(http.MethodGet, "/api/v1/provisioning/credentials", nil)
	if err != nil {
		return nil, nil, err
	}
	var env struct {
		Credentials []CredentialRecord `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("fleet: GET /api/v1/provisioning/credentials: decode: %w", err)
	}
	if env.Credentials == nil {
		env.Credentials = []CredentialRecord{}
	}
	return env.Credentials, json.RawMessage(raw), nil
}

// Revoke deletes one credential by id.
func (c CredentialClient) Revoke(id uint64) error {
	_, err := c.do(http.MethodDelete, fmt.Sprintf("/api/v1/provisioning/credentials/%d", id), nil)
	return err
}

// Rotate swaps one credential's value in place after re-validation.
func (c CredentialClient) Rotate(id uint64, value, expiresAt string) (CredentialRecord, json.RawMessage, error) {
	var zero CredentialRecord
	body := map[string]any{"value": value}
	if expiresAt != "" {
		body["expires_at"] = expiresAt
	}
	raw, err := c.do(http.MethodPost, fmt.Sprintf("/api/v1/provisioning/credentials/%d/rotate", id), body)
	if err != nil {
		return zero, nil, err
	}
	return decodeCredentialItem("POST /api/v1/provisioning/credentials/:id/rotate", raw)
}

func decodeCredentialItem(op string, raw []byte) (CredentialRecord, json.RawMessage, error) {
	var zero CredentialRecord
	var env struct {
		Credential CredentialRecord `json:"credential"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return zero, nil, fmt.Errorf("fleet: %s: decode: %w", op, err)
	}
	reraw, err := json.Marshal(env.Credential)
	if err != nil {
		return zero, nil, err
	}
	return env.Credential, json.RawMessage(reraw), nil
}

// normalizeCredentialProvider applies the operator shorthand client
// side too, so `rm --provider do` matches the `digitalocean` row the
// master stored. The master is authoritative; this only aligns the
// match.
func normalizeCredentialProvider(p string) string {
	if strings.EqualFold(strings.TrimSpace(p), "do") {
		return "digitalocean"
	}
	return strings.ToLower(strings.TrimSpace(p))
}

// findCredential resolves (provider, role) to the stored row. The pair
// is unique per owner, so at most one row matches.
func findCredential(rows []CredentialRecord, provider, role string) (CredentialRecord, error) {
	var zero CredentialRecord
	wantProvider, wantRole := normalizeCredentialProvider(provider), strings.ToLower(strings.TrimSpace(role))
	for _, r := range rows {
		if normalizeCredentialProvider(r.Provider) == wantProvider && strings.ToLower(r.Role) == wantRole {
			return r, nil
		}
	}
	return zero, fmt.Errorf("fleet: no %s %s credential (see `infra credential ls`)", provider, role)
}

// readCredentialStdin reads the credential value from stdin only.
// There is intentionally no --value/--token flag: a flag value would
// land in shell history and process listings, while a pipe
// (`bashy ask --stdout | bashy infra credential add …`) never does.
// A terminal is refused outright so an operator cannot type a secret
// into a scrollback buffer by accident.
func readCredentialStdin(cmd *cobra.Command) (string, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("fleet: refusing to read a credential from a terminal; pipe it instead (bashy ask --stdout | bashy infra credential add --provider P --role R)")
		}
	}
	raw, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return "", fmt.Errorf("fleet: read credential from stdin: %w", err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("fleet: no credential on stdin; pipe it (bashy ask --stdout | bashy infra credential add --provider P --role R)")
	}
	return value, nil
}

// parseCredentialExpiry accepts an RFC3339 timestamp or a bare
// YYYY-MM-DD date (midnight UTC) and renders it back canonical
// RFC3339 for the master. Empty means "unknown expiry".
func parseCredentialExpiry(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("fleet: invalid --expires-at %q (want RFC3339 or YYYY-MM-DD)", s)
}

// NewInfraCredentialCmd builds the `infra credential` verb tree: add,
// ls, rm and rotate over the master v1 credential endpoints.
func NewInfraCredentialCmd() *cobra.Command {
	var master, token string
	var asJSON bool
	c := &cobra.Command{
		Use:   "credential",
		Short: "Manage master-held provider credentials (value via stdin only)",
	}
	c.PersistentFlags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	c.PersistentFlags().StringVar(&token, "token", "", "master Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY, else the paired credential)")
	c.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")

	var addProvider, addRole, addExpires string
	add := &cobra.Command{
		Use:   "add --provider P --role R",
		Short: "Validate and store a provider credential (value read from stdin only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(addProvider) == "" || strings.TrimSpace(addRole) == "" {
				return fmt.Errorf("fleet: infra credential add: --provider and --role are required")
			}
			value, err := readCredentialStdin(cmd)
			if err != nil {
				return err
			}
			expires, err := parseCredentialExpiry(addExpires)
			if err != nil {
				return err
			}
			client, err := NewCredentialClient(master, token)
			if err != nil {
				return err
			}
			rec, raw, err := client.Add(addProvider, addRole, value, expires)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				_, err := out.Write(append(raw, '\n'))
				return err
			}
			fmt.Fprintf(out, "added %s/%s (id %d, account %s, %s)\n",
				rec.Provider, rec.Role, rec.ID, displayAccount(rec), rec.ValidatedScopes)
			return nil
		},
	}
	add.Flags().StringVar(&addProvider, "provider", "", "provider (e.g. do)")
	add.Flags().StringVar(&addRole, "role", "", "provision or purge")
	add.Flags().StringVar(&addExpires, "expires-at", "", "operator-known expiry (RFC3339 or YYYY-MM-DD); empty = unknown")

	var lsProvider, lsRole string
	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List credential metadata (never the value)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := NewCredentialClient(master, token)
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
			shown := 0
			for _, r := range rows {
				if lsProvider != "" && normalizeCredentialProvider(r.Provider) != normalizeCredentialProvider(lsProvider) {
					continue
				}
				if lsRole != "" && !strings.EqualFold(r.Role, lsRole) {
					continue
				}
				expires := "-"
				if r.ExpiresAt != nil && *r.ExpiresAt != "" {
					expires = *r.ExpiresAt
				}
				status := ""
				if r.ExpiryStatus != "" && r.ExpiryStatus != "valid" {
					status = "\t" + r.ExpiryStatus
				}
				fmt.Fprintf(out, "%d\t%s\t%s\t%s\texpires:%s%s\n",
					r.ID, r.Provider, r.Role, displayAccount(r), expires, status)
				shown++
			}
			if shown == 0 {
				fmt.Fprintln(out, "no credentials")
			}
			return nil
		},
	}
	ls.Flags().StringVar(&lsProvider, "provider", "", "only show this provider")
	ls.Flags().StringVar(&lsRole, "role", "", "only show this role")

	var rmProvider, rmRole string
	rm := &cobra.Command{
		Use:   "rm --provider P --role R",
		Short: "Revoke a provider credential",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(rmProvider) == "" || strings.TrimSpace(rmRole) == "" {
				return fmt.Errorf("fleet: infra credential rm: --provider and --role are required")
			}
			client, err := NewCredentialClient(master, token)
			if err != nil {
				return err
			}
			rows, _, err := client.List()
			if err != nil {
				return err
			}
			rec, err := findCredential(rows, rmProvider, rmRole)
			if err != nil {
				return err
			}
			if err := client.Revoke(rec.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s/%s (id %d)\n", rec.Provider, rec.Role, rec.ID)
			return nil
		},
	}
	rm.Flags().StringVar(&rmProvider, "provider", "", "provider (e.g. do)")
	rm.Flags().StringVar(&rmRole, "role", "", "provision or purge")

	var rotProvider, rotRole, rotExpires string
	rotate := &cobra.Command{
		Use:   "rotate --provider P --role R",
		Short: "Re-validate and replace a provider credential (value read from stdin only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(rotProvider) == "" || strings.TrimSpace(rotRole) == "" {
				return fmt.Errorf("fleet: infra credential rotate: --provider and --role are required")
			}
			value, err := readCredentialStdin(cmd)
			if err != nil {
				return err
			}
			expires, err := parseCredentialExpiry(rotExpires)
			if err != nil {
				return err
			}
			client, err := NewCredentialClient(master, token)
			if err != nil {
				return err
			}
			rows, _, err := client.List()
			if err != nil {
				return err
			}
			rec, err := findCredential(rows, rotProvider, rotRole)
			if err != nil {
				return err
			}
			updated, raw, err := client.Rotate(rec.ID, value, expires)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				_, err := out.Write(append(raw, '\n'))
				return err
			}
			fmt.Fprintf(out, "rotated %s/%s (id %d, account %s, %s)\n",
				updated.Provider, updated.Role, updated.ID, displayAccount(updated), updated.ValidatedScopes)
			return nil
		},
	}
	rotate.Flags().StringVar(&rotProvider, "provider", "", "provider (e.g. do)")
	rotate.Flags().StringVar(&rotRole, "role", "", "provision or purge")
	rotate.Flags().StringVar(&rotExpires, "expires-at", "", "operator-known expiry (RFC3339 or YYYY-MM-DD); empty = unknown")

	c.AddCommand(add, ls, rm, rotate)
	return c
}

func displayAccount(r CredentialRecord) string {
	if r.AccountEmail != "" {
		return r.AccountEmail
	}
	if r.AccountUUID != "" {
		return r.AccountUUID
	}
	return "-"
}
