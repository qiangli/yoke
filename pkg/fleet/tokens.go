package fleet

// Scoped coordination credentials (S490 story 2607).
//
// cloudbox mints attenuated Bearer [REDACTED] over POST /api/v1/tokens next to the
// session-only /tokens routes: the minted scopes must be a subset of the
// caller token's scopes (never escalate; *:admin never mintable over
// Bearer), name and TTL are required, the owner is the calling token's
// owner. This file is the yoke side of exactly that wire contract —
// client, cobra constructors, nothing else. The scope vocabulary itself
// is unchanged.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/secrets"
)

// TokenInfo is one row of GET /api/v1/tokens: everything but the value.
// The value is returned only once, inside POST's 201 response.
type TokenInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at"`
	Created   string   `json:"created,omitempty"`
}

// MintedToken is POST /api/v1/tokens's 201 response: the row plus the
// value, returned ONCE.
type MintedToken struct {
	TokenInfo
	Token string `json:"token"`
}

// TokenError is a cloudbox API refusal with its HTTP status kept machine
// readable: callers branch on Status (409 duplicate name, 403 escalation,
// 404 cross-owner) while humans read Message.
type TokenError struct {
	Op      string // e.g. "POST /api/v1/tokens"
	Status  int
	Message string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("fleet: %s: %d: %s", e.Op, e.Status, e.Message)
}

// TokenClient talks to the cloudbox token surface. Zero network state —
// one *http.Client, a resolved base URL, and a caller token.
type TokenClient struct {
	BaseURL string // e.g. https://ai.dhnt.io (no trailing /api/v1)
	Token   string // Bearer [REDACTED] used as the minting credential
	HTTP    *http.Client
}

// httpClient is the shared timeout posture for token and vault calls.
func httpClient() *http.Client { return &http.Client{Timeout: 20 * time.Second} }

// NewTokenClient resolves base + caller token through ResolveCloud, the
// ONE ladder every bashy-side cloudbox client walks (so a host still
// paired to the base needs no extra setup, and a migrated host uses its
// saved master credential).
func NewTokenClient(urlOverride, tokenOverride string) (TokenClient, error) {
	base, tok := ResolveCloud(urlOverride, tokenOverride)
	if tok == "" {
		return TokenClient{}, fmt.Errorf("fleet: no cloudbox token (set $BASHY_FLEET_TOKEN or pass --token)")
	}
	return TokenClient{BaseURL: base, Token: tok, HTTP: &http.Client{Timeout: 20 * time.Second}}, nil
}

var tokenNameRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

func validateTokenName(name string) error {
	if !tokenNameRe.MatchString(name) {
		return fmt.Errorf("fleet: invalid token name %q (want 1-64 chars of [a-z0-9._-])", name)
	}
	return nil
}

var tokenTTLRe = regexp.MustCompile(`^([0-9]+)(h|d)$`)

// maxTokenTTL is the contract's ceiling: 365 days.
const maxTokenTTL = 365 * 24 * time.Hour

func validateTokenTTL(ttl string) error {
	m := tokenTTLRe.FindStringSubmatch(strings.TrimSpace(ttl))
	if m == nil {
		return fmt.Errorf("fleet: invalid ttl %q (want e.g. 24h or 90d, max 365d)", ttl)
	}
	var n int
	fmt.Sscanf(m[1], "%d", &n)
	d := time.Duration(n) * time.Hour
	if m[2] == "d" {
		d = time.Duration(n) * 24 * time.Hour
	}
	if n <= 0 || d > maxTokenTTL {
		return fmt.Errorf("fleet: invalid ttl %q (want e.g. 24h or 90d, max 365d)", ttl)
	}
	return nil
}

// splitScopes accepts repeated --scope flags and comma-separated lists,
// trims whitespace, and drops empties.
func splitScopes(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func (c TokenClient) do(method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(req)
}

// apiTokenError renders a refusal body as a *TokenError, keeping the
// status (403 escalation, 409 duplicate, 404 cross-owner) readable.
func apiTokenError(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	} else {
		var env struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &env) == nil && env.Error != "" {
			msg = env.Error
		}
	}
	return &TokenError{Op: op, Status: resp.StatusCode, Message: msg}
}

// Create mints a token: POST /api/v1/tokens with the required name,
// scopes, and ttl. A 403 means the attenuation rule fired — a scope the
// caller token does not hold, or any *:admin scope — and the error names
// the scope.
func (c TokenClient) Create(name string, scopes []string, ttl string) (MintedToken, error) {
	var zero MintedToken
	if err := validateTokenName(name); err != nil {
		return zero, err
	}
	if len(scopes) == 0 {
		return zero, fmt.Errorf("fleet: at least one scope is required")
	}
	if err := validateTokenTTL(ttl); err != nil {
		return zero, err
	}
	const op = "POST /api/v1/tokens"
	resp, err := c.do(http.MethodPost, "/api/v1/tokens", map[string]any{
		"name": name, "scopes": scopes, "ttl": ttl,
	})
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return zero, apiTokenError(op, resp)
	}
	var m MintedToken
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return zero, err
	}
	if m.Token == "" {
		return zero, fmt.Errorf("fleet: POST /api/v1/tokens: server minted no token value")
	}
	return m, nil
}

// List returns the caller owner's token rows, never their values.
func (c TokenClient) List() ([]TokenInfo, error) {
	const op = "GET /api/v1/tokens"
	resp, err := c.do(http.MethodGet, "/api/v1/tokens", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiTokenError(op, resp)
	}
	var rows []TokenInfo
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []TokenInfo{}
	}
	return rows, nil
}

// Revoke deletes one owner-scoped token row: 204. Cross-owner ids read
// as 404, and deleting the very token the caller authenticates with is
// 409.
func (c TokenClient) Revoke(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("fleet: token id is required")
	}
	const op = "DELETE /api/v1/tokens/:id"
	resp, err := c.do(http.MethodDelete, "/api/v1/tokens/"+id, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return apiTokenError(op, resp)
	}
	return nil
}

// writeSecretToVault stores a minted value under name through the
// secrets credential chain (secrets.Config{}.Resolve, exactly like
// `bashy secret set`), NEVER the minting caller's token: the caller is
// typically the host's Outpost pairing token (outpost scopes, NO
// secrets:write), which the vault refuses with 403. The fresh token
// itself carries no secrets:write scope yet — and must never need one
// to be storable.
func writeSecretToVault(name, value string) error {
	sc, err := secrets.Config{}.Resolve()
	if err != nil {
		return err
	}
	return sc.Put([]secrets.Item{{Name: name, Value: value}})
}

// NewTokenCmd is `token create|list|revoke`, the cobra surface hosts
// mount as `bashy token ...`. The minted value goes straight into the
// vault with --to-secret and is NEVER printed; --stdout (explicit, for
// humans at a terminal) is the only way to print it.
func NewTokenCmd() *cobra.Command {
	var urlOverride, tokenOverride string
	var acct accountFlag
	c := &cobra.Command{
		Use:   "token",
		Short: "Mint and manage scoped coordination tokens (attenuated Bearer [REDACTED])",
	}
	c.PersistentFlags().StringVar(&urlOverride, "url", "", "control-plane base URL (default $BASHY_CLOUDBOX_URL)")
	c.PersistentFlags().StringVar(&tokenOverride, "token", "", "Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY)")
	acct.bindPersistent(c)

	var name, ttl, toSecret string
	var scopeVals []string
	var stdout, asJSON bool
	create := &cobra.Command{
		Use:   "create --name N --scope a,b --ttl D (--to-secret VAULT_NAME | --stdout)",
		Short: "Mint an attenuated token; the value goes to the vault, never to the terminal",
		Long: "Mint an attenuated token.\n\n" +
			"The minted scopes must be a SUBSET of the calling token's scopes:\n" +
			"minting a scope the caller lacks is refused (403), and *:admin\n" +
			"scopes are never mintable over Bearer. With --to-secret the value\n" +
			"is written straight into the cloudbox vault and only\n" +
			"id/name/scopes/expires print. With --stdout (explicit, for humans)\n" +
			"the value prints instead. One of the two is required: with nowhere\n" +
			"safe to put the value, create refuses rather than printing it.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if toSecret == "" && !stdout {
				return fmt.Errorf("fleet: give --to-secret VAULT_NAME or --stdout (the value is never printed by default)")
			}
			client, err := newTokenClientFor(cmd.Context(), &acct, urlOverride, tokenOverride)
			if err != nil {
				return err
			}
			m, err := client.Create(name, splitScopes(scopeVals), ttl)
			if err != nil {
				return err
			}
			if toSecret != "" {
				if err := writeSecretToVault(toSecret, m.Token); err != nil {
					return fmt.Errorf("fleet: minted %q (%s) but the vault write failed: %w", m.Name, m.ID, err)
				}
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{
					"id": m.ID, "name": m.Name, "scopes": m.Scopes,
					"expires_at": m.ExpiresAt,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created %s name=%s scopes=%s expires=%s\n",
				m.ID, m.Name, strings.Join(m.Scopes, ","), m.ExpiresAt)
			if toSecret != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "stored in vault as %s\n", toSecret)
			}
			if stdout {
				fmt.Fprintln(cmd.OutOrStdout(), m.Token)
			}
			return nil
		},
	}
	create.Flags().StringVar(&name, "name", "", "token name (1-64 chars of [a-z0-9._-])")
	create.Flags().StringSliceVar(&scopeVals, "scope", nil, "scope to grant; repeat or comma-separate (subset of the caller's scopes)")
	create.Flags().StringVar(&ttl, "ttl", "", "time to live, e.g. 24h or 90d (required, max 365d)")
	create.Flags().StringVar(&toSecret, "to-secret", "", "vault secret name to store the minted value under (never printed)")
	create.Flags().BoolVar(&stdout, "stdout", false, "print the minted value (explicit, for humans only)")
	create.Flags().BoolVar(&asJSON, "json", false, "emit the minted metadata as JSON (never the value)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List the owner's tokens (never their values)",
		Args:  cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newTokenClientFor(cmd.Context(), &acct, urlOverride, tokenOverride)
			if err != nil {
				return err
			}
			rows, err := client.List()
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), rows)
			}
			for _, r := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n",
					r.ID, r.Name, strings.Join(r.Scopes, ","), r.ExpiresAt)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "emit JSON")

	revoke := &cobra.Command{
		Use:   "revoke ID",
		Short: "Revoke one token by id (owner-scoped)",
		Args:  cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newTokenClientFor(cmd.Context(), &acct, urlOverride, tokenOverride)
			if err != nil {
				return err
			}
			if err := client.Revoke(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s\n", args[0])
			return nil
		},
	}
	c.AddCommand(create, list, revoke)
	return c
}
