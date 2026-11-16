package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// keyHTTP is the member CLI's HTTP client (a seam for tests). It never
// follows a redirect, so the account bearer reaches only the named origin.
var keyHTTP = func() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// keyAccountToken is the member's Cloudbox account credential (a seam for
// tests). It is only ever the explicit BASHY_FLEET_TOKEN / BASHY_API_KEY:
// DOOR_URL names the origin it is sent to, so a host's paired outpost token
// is never a silent fallback.
var keyAccountToken = func() string {
	return firstNonEmpty(os.Getenv("BASHY_FLEET_TOKEN"), os.Getenv("BASHY_API_KEY"))
}

func newKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key DOOR_URL",
		Short: "mint (or rotate) your own key for a door that allows self-service",
		Long: `key gets YOUR key for another owner's door through its satellite app, e.g.

  bashy llm key https://SATELLITE/matrix/h/HOST/app/llm
  eval "$(bashy llm env --from HOST --via cloudbox)"

You must be an active mutual member of the door owner's account, in a circle
the owner allowed (` + "`bashy llm self-service on`" + `). Your Cloudbox account token
(BASHY_FLEET_TOKEN, with the satellites:delegate scope) is sent to the
satellite named in DOOR_URL — use only a door URL its owner gave you — and
exchanged there for a short-lived token that proves who you are; sign in to that
satellite once in a browser first. The key is stored owner-only as the door
grant for HOST and never printed. Running key again rotates it: the previous
key stops working.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			appURL, origin, host, err := parseDoorAppURL(args[0])
			if err != nil {
				return fmt.Errorf("llm key: %w", err)
			}
			account := strings.TrimSpace(keyAccountToken())
			if account == "" || strings.ContainsAny(account, "\r\n") {
				return errors.New("llm key: not signed in: set BASHY_FLEET_TOKEN to a Cloudbox account token with the satellites:delegate scope")
			}
			ctx := cmd.Context()
			bearer, err := exchangeSatelliteToken(ctx, origin, account)
			if err != nil {
				return fmt.Errorf("llm key: %w", err)
			}
			peer, cred, err := requestSelfKey(ctx, appURL, bearer)
			if err != nil {
				return fmt.Errorf("llm key: %w", err)
			}
			g := DoorGrant{Schema: GrantSchemaVersion, Door: host, Peer: strings.TrimPrefix(peer, PeerPrincipalPrefix),
				CloudboxURL: appURL, Credential: cred, Issued: time.Now().UTC()}
			// Keep a mesh route an owner share already gave this door.
			if prev, err := LoadDoorGrant(host); err == nil {
				g.MeshPeerID, g.MeshService = prev.MeshPeerID, prev.MeshService
			}
			if err := SaveDoorGrant(g); err != nil {
				return fmt.Errorf("llm key: the door issued a new key but it could not be stored (%v); fix that and run `llm key` again to rotate", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored key for %s on door %s; configure a client with: eval \"$(bashy llm env --from %s --via cloudbox)\"\n", peer, host, host)
			return nil
		},
	}
}

// parseDoorAppURL checks DOOR_URL is https://SATELLITE/matrix/h/HOST/app/llm
// (http only on loopback) and returns it normalized, the satellite origin and
// the door host name.
func parseDoorAppURL(raw string) (appURL, origin, host string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", "", fmt.Errorf("%q is not a door URL (want https://SATELLITE/matrix/h/HOST/app/llm)", raw)
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !(strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())) {
			return "", "", "", fmt.Errorf("%q must use https", raw)
		}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "matrix" || parts[1] != "h" || parts[3] != "app" || parts[4] != doorServiceName {
		return "", "", "", fmt.Errorf("%q is not a door URL (want .../matrix/h/HOST/app/%s)", raw, doorServiceName)
	}
	host = strings.ToLower(parts[2])
	if !ValidPeerName(host) {
		return "", "", "", fmt.Errorf("invalid door host %q in %s", parts[2], raw)
	}
	origin = u.Scheme + "://" + u.Host
	return origin + "/matrix/h/" + host + "/app/" + doorServiceName, origin, host, nil
}

// exchangeSatelliteToken trades the account credential for a short-lived
// satellite token; the satellite then vouches the caller's email to the door.
func exchangeSatelliteToken(ctx context.Context, origin, account string) (string, error) {
	body := []byte(`{"scopes":["host:proxy"],"ttl":"10m"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/v1/tokens/exchange", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+account)
	req.Header.Set("Content-Type", "application/json")
	resp, err := keyHTTP().Do(req)
	if err != nil {
		return "", fmt.Errorf("satellite unreachable: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("not signed in to this satellite (HTTP %d): sign in to %s once in a browser, and use an account token with the satellites:delegate scope", resp.StatusCode, origin)
	case resp.StatusCode/100 != 2:
		return "", fmt.Errorf("satellite token exchange failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Token == "" || strings.ContainsAny(out.Token, "\r\n") {
		return "", errors.New("satellite token exchange returned no token")
	}
	return out.Token, nil
}

// requestSelfKey calls the door's self-service route and returns the peer
// principal and its fresh credential, or the door's one-line refusal.
func requestSelfKey(ctx context.Context, appURL, bearer string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, appURL+SelfServicePath, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	resp, err := keyHTTP().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("door unreachable: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		var coded struct{ Code, Message string }
		if json.Unmarshal(e.Error, &coded) == nil && coded.Message != "" {
			return "", "", errors.New(coded.Message)
		}
		var plain string
		if json.Unmarshal(e.Error, &plain) == nil && plain != "" {
			return "", "", fmt.Errorf("door refused (HTTP %d): %s", resp.StatusCode, plain)
		}
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadGateway {
			return "", "", fmt.Errorf("door unreachable or too old for self-service (HTTP %d)", resp.StatusCode)
		}
		return "", "", fmt.Errorf("door refused: HTTP %d", resp.StatusCode)
	}
	var ok struct{ Peer, Credential string }
	if err := json.Unmarshal(raw, &ok); err != nil || !strings.HasPrefix(ok.Credential, "bpeer_") || !strings.HasPrefix(ok.Peer, PeerPrincipalPrefix+SelfPeerPrefix) {
		return "", "", errors.New("door returned no key")
	}
	return ok.Peer, ok.Credential, nil
}
