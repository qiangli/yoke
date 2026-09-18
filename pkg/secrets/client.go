package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Item is one name→value secret on the cloudbox wire. It mirrors the
// SecretItem shape served by cloudbox's /api/v1/secrets handler.
type Item struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	URL   string `json:"url,omitempty"`
}

// Client talks to a cloudbox secrets vault over the Bearer /api/v1/secrets
// surface. Zero network state — one *http.Client, a resolved base URL, and
// a token.
type Client struct {
	BaseURL string // e.g. https://ai.dhnt.io  (no trailing /api/v1)
	Token   string // Bearer access token carrying secrets:read / secrets:write
	HTTP    *http.Client
}

// Config carries the resolved connection settings. Resolve() fills it from
// flags (highest precedence) then environment then on-disk convention.
type Config struct {
	URL   string
	Token string
}

// Resolve determines the cloudbox base URL and Bearer token, in order:
//
//	URL:   --url flag  >  $BASHY_CLOUDBOX_URL  >  https://ai.dhnt.io
//	Token: --token flag > $BASHY_SECRETS_TOKEN
//	         > ~/.config/bashy/secrets-token (XDG-aware, bashy-owned)
//	         > $BASHY_API_KEY
//
// The env surface is BASHY_* only: anything driving bashy (the dhnt ecosystem
// included) sets the BASHY_ vars like any other caller — no special-cased
// DHNT_ envs. The on-disk fallback is a dedicated, bashy-owned file so the
// secrets credential stays separate from the broader LLM-gateway token (and
// can be minted read-only). $BASHY_SECRETS_TOKEN (env / Keychain) is the
// preferred primary; the file is just a no-config convenience.
func (c Config) Resolve() (Client, error) {
	base := c.URL
	if base == "" {
		base = os.Getenv("BASHY_CLOUDBOX_URL")
	}
	if base == "" {
		base = "https://ai.dhnt.io"
	}
	base = strings.TrimRight(base, "/")

	tok := c.Token
	if tok == "" {
		tok = os.Getenv("BASHY_SECRETS_TOKEN")
	}
	if tok == "" {
		tok = readTokenFile()
	}
	if tok == "" {
		tok = os.Getenv("BASHY_API_KEY")
	}
	if tok == "" {
		return Client{}, fmt.Errorf("no cloudbox token: set --token, $BASHY_SECRETS_TOKEN, ~/.config/bashy/secrets-token, or $BASHY_API_KEY (token must carry the secrets:read scope)")
	}
	return Client{
		BaseURL: base,
		Token:   strings.TrimSpace(tok),
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// tokenFilePath is the bashy-owned on-disk secrets-token location:
// $XDG_CONFIG_HOME/bashy/secrets-token, else ~/.config/bashy/secrets-token.
// TokenFilePath is the bashy-owned secrets-token location, exported so a
// resource map can report its existence — never its contents.
func TokenFilePath() string { return tokenFilePath() }

func tokenFilePath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "bashy", "secrets-token")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "bashy", "secrets-token")
}

func readTokenFile() string {
	p := tokenFilePath()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c Client) endpoint(p string) string { return c.BaseURL + "/api/v1/secrets" + p }

func (c Client) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.endpoint(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(req)
}

// apiError reads an error response body and renders a compact message.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("cloudbox %s: %s", resp.Status, msg)
}

// List returns all of the caller's secrets, sorted by name.
func (c Client) List() ([]Item, error) {
	resp, err := c.do(http.MethodGet, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out struct {
		Secrets []Item `json:"secrets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	sort.Slice(out.Secrets, func(i, j int) bool { return out.Secrets[i].Name < out.Secrets[j].Name })
	return out.Secrets, nil
}

// Get returns one secret value by name.
func (c Client) Get(name string) (string, error) {
	resp, err := c.do(http.MethodGet, "/"+name, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	var it Item
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		return "", err
	}
	return it.Value, nil
}

// Put upserts one or more secrets in a single request.
func (c Client) Put(items []Item) error {
	payload, err := json.Marshal(struct {
		Secrets []Item `json:"secrets"`
	}{Secrets: items})
	if err != nil {
		return err
	}
	resp, err := c.do(http.MethodPost, "", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}

// Delete removes one secret by name.
func (c Client) Delete(name string) error {
	resp, err := c.do(http.MethodDelete, "/"+name, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}
