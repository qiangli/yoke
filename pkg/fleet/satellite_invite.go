package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// HostInvite is the invite code response from POST /api/v1/hosts/invites.
// It mirrors handlers.CreateInvite / registerHostLifecycle.
type HostInvite struct {
	Code      string    `json:"code"`
	Expires   time.Time `json:"expires,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// TokenExchangeResponse is the 201 response from POST /api/v1/tokens/exchange.
type TokenExchangeResponse struct {
	Token     string    `json:"token"`
	TokenID   string    `json:"token_id,omitempty"`
	Scopes    []string  `json:"scopes,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// SatelliteClient interacts with a satellite control plane.
type SatelliteClient struct {
	BaseURL string // e.g. https://zoo.dhnt.io (no trailing slash)
	Token   string // Owner Bearer token
	HTTP    *http.Client
}

// NewSatelliteClient resolves satellite base URL and owner Bearer token
// through ResolveCloud.
func NewSatelliteClient(satelliteURL, tokenOverride string) (SatelliteClient, error) {
	satelliteURL = strings.TrimSpace(satelliteURL)
	if satelliteURL == "" {
		return SatelliteClient{}, fmt.Errorf("fleet: satellite invite needs --satellite")
	}
	base, tok := ResolveCloud(satelliteURL, tokenOverride)
	if tok == "" {
		return SatelliteClient{}, fmt.Errorf("fleet: no cloudbox token (set $BASHY_FLEET_TOKEN or pass --token)")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return SatelliteClient{
		BaseURL: strings.TrimRight(base, "/"),
		Token:   tok,
		HTTP:    httpClient(),
	}, nil
}

func (c SatelliteClient) getHTTPClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return httpClient()
}

// ExchangeToken exchanges the owner Bearer token for a short-lived token
// with the requested scopes on the satellite.
func (c SatelliteClient) ExchangeToken(scopes []string, ttl string) (string, error) {
	if ttl == "" {
		ttl = "10m"
	}
	payload := map[string]any{
		"scopes": scopes,
		"ttl":    ttl,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/v1/tokens/exchange", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.getHTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("fleet: POST /api/v1/tokens/exchange: 403: owner token lacks host:invite-mint; see docs")
	}
	if resp.StatusCode/100 != 2 {
		return "", provisionAPIError("POST /api/v1/tokens/exchange", resp.StatusCode, raw)
	}

	var env TokenExchangeResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("fleet: POST /api/v1/tokens/exchange: decode: %w", err)
	}
	if env.Token == "" {
		return "", fmt.Errorf("fleet: POST /api/v1/tokens/exchange: response missing token")
	}
	return env.Token, nil
}

// MintInvite mints an owner-scoped, single-use invite code on the satellite
// using the exchanged token.
func (c SatelliteClient) MintInvite(exchangedToken, name string) (*HostInvite, error) {
	var rdr io.Reader
	if name != "" {
		payload, err := json.Marshal(map[string]string{"name": name})
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/v1/hosts/invites", rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+exchangedToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.getHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, provisionAPIError("POST /api/v1/hosts/invites", resp.StatusCode, raw)
	}

	var inv HostInvite
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("fleet: POST /api/v1/hosts/invites: decode: %w", err)
	}
	if inv.ExpiresAt.IsZero() && !inv.Expires.IsZero() {
		inv.ExpiresAt = inv.Expires
	}
	if inv.Expires.IsZero() && !inv.ExpiresAt.IsZero() {
		inv.Expires = inv.ExpiresAt
	}
	if inv.Code == "" {
		return nil, fmt.Errorf("fleet: POST /api/v1/hosts/invites: response missing code")
	}
	return &inv, nil
}

// Invite exchanges the owner Bearer token for a host:invite-mint satellite token,
// then mints an owner-scoped, single-use invite code. The exchanged token is never
// retained or exposed.
func (c SatelliteClient) Invite(name, ttl string) (*HostInvite, error) {
	if ttl == "" {
		ttl = "10m"
	}
	exchangedToken, err := c.ExchangeToken([]string{"host:invite-mint"}, ttl)
	if err != nil {
		return nil, err
	}
	return c.MintInvite(exchangedToken, name)
}

// NewSatelliteCmd builds the `satellite` verb tree.
func NewSatelliteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "satellite",
		Short:         "Manage satellite operations",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.AddCommand(NewSatelliteInviteCmd())
	return c
}

// NewSatelliteInviteCmd builds the `satellite invite` verb.
func NewSatelliteInviteCmd() *cobra.Command {
	var satelliteURL, name, ttl, toFile, tokenOverride string
	var stdout, asJSON bool

	cmd := &cobra.Command{
		Use:   "invite --satellite URL --name HOST [--ttl 10m] [--to-file PATH] [--json]",
		Short: "Mint an owner-scoped host pairing invite on a satellite",
		Long: "Mint an owner-scoped host pairing invite on a satellite.\n\n" +
			"Exchanges the owner Bearer token for a short-lived host:invite-mint\n" +
			"satellite token, mints a single-use invite code, and writes the code\n" +
			"to --to-file (0600; default <name>.invite) or prints it to stdout with --stdout.\n" +
			"Bearer tokens are never printed.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(satelliteURL) == "" {
				return fmt.Errorf("fleet: satellite invite needs --satellite")
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("fleet: satellite invite needs --name")
			}
			if strings.TrimSpace(ttl) == "" {
				ttl = "10m"
			}
			client, err := NewSatelliteClient(satelliteURL, tokenOverride)
			if err != nil {
				return err
			}
			inv, err := client.Invite(name, ttl)
			if err != nil {
				return err
			}

			// Destination handling:
			// --stdout: print code only to stdout (or JSON if --json).
			// default: write code to file (0600; default <name>.invite).
			if stdout {
				if toFile != "" {
					if err := writeInviteCodeFile(toFile, inv.Code); err != nil {
						return err
					}
				}
				if asJSON {
					return writeInviteJSON(cmd.OutOrStdout(), inv, name, client.BaseURL, toFile)
				}
				fmt.Fprintln(cmd.OutOrStdout(), inv.Code)
				return nil
			}

			targetFile := toFile
			if targetFile == "" {
				targetFile = name + ".invite"
			}
			if err := writeInviteCodeFile(targetFile, inv.Code); err != nil {
				return err
			}
			if asJSON {
				return writeInviteJSON(cmd.OutOrStdout(), inv, name, client.BaseURL, targetFile)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote invite for %s to %s\n", name, targetFile)
			return nil
		},
	}

	cmd.Flags().StringVar(&satelliteURL, "satellite", "", "satellite base URL (required)")
	cmd.Flags().StringVar(&name, "name", "", "host name to pair (required)")
	cmd.Flags().StringVar(&ttl, "ttl", "10m", "time to live for token exchange (default 10m)")
	cmd.Flags().StringVar(&toFile, "to-file", "", "file path to write the invite code to (0600; default <name>.invite)")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "print invite code to stdout")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	cmd.Flags().StringVar(&tokenOverride, "token", "", "owner Bearer token (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY, else paired token)")

	return cmd
}

func writeInviteCodeFile(path, code string) error {
	path = filepath.Clean(path)
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("fleet: satellite invite: create dir: %w", err)
		}
	}
	content := []byte(strings.TrimSpace(code) + "\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("fleet: satellite invite: write file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("fleet: satellite invite: chmod 0600: %w", err)
	}
	return nil
}

func writeInviteJSON(w io.Writer, inv *HostInvite, name, satelliteURL, targetFile string) error {
	exp := inv.ExpiresAt
	if exp.IsZero() {
		exp = inv.Expires
	}
	var expStr string
	if !exp.IsZero() {
		expStr = exp.UTC().Format(time.RFC3339)
	}
	out := map[string]any{
		"code":       inv.Code,
		"expires_at": expStr,
		"expires":    expStr,
		"name":       name,
		"satellite":  satelliteURL,
	}
	if targetFile != "" {
		out["file"] = targetFile
	}
	return writeJSON(w, out)
}
