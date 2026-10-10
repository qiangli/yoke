// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// The Host tile fronts outpost's admin UI, which has its own login. When the
// console has already established who the caller is, that second prompt proves
// nothing, so the console presents outpost's local MCP bearer on the caller's
// behalf (outbound only; the browser never sees it).
//
// "Established" is exactly one of three things, decided by the console gate
// and carried in the request context (never in a header a client can send):
//
//   - bridgeOperator: a console session cookie for the OS user (or the ungated
//     loopback dev row). The browser may be on the LAN: the console already
//     authenticated it, and the hop to outpost is loopback either way.
//   - bridgeDevice: a paired-device session that deviceAdmits accepted for this
//     very request path — live, unexpired, operator grant intact, Host in scope.
//   - bridgeCloudOwner: a tunnel-arrived request whose identity signature
//     verifies under the SSO secret of THIS console's app in the outpost
//     config AND whose signed identity is the owner named by that same config
//     (admin_users, else oauth2_email). Remote-Groups and unsigned headers
//     never count; a signed sharee is not an owner; no secret or no owner
//     means no cloud bridge.
//
// The credential itself is scoped by construction, every one of these holds:
//
//   - the panel is the stock Host tile (panel.hostAdmin, set only by
//     hostBuiltinPanel) and its endpoint is same-host loopback;
//   - the token, the admin endpoint and the owner/secret all come from ONE
//     trusted outpost config file (single canonical path, 0600), and the
//     panel's endpoint equals that config's effective admin endpoint
//     ($OUTPOST_ADMIN_ADDR if valid, else admin_addr, else 127.0.0.1:17777);
//   - the outbound path is the admin REST surface (/api/…), cleaned;
//   - the dial is loopback-only (see loopbackOnlyTransport).
//
// Whatever Authorization the browser sent is discarded; ours is added to the
// outbound request only.

type bridgeVia string

const (
	bridgeOperator   bridgeVia = "operator"
	bridgeDevice     bridgeVia = "device"
	bridgeCloudOwner bridgeVia = "cloud-owner"
)

type hostBridgeKey struct{}

func markHostBridge(r *http.Request, via bridgeVia) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), hostBridgeKey{}, via))
}

func hostBridgeOf(r *http.Request) (bridgeVia, bool) {
	v, ok := r.Context().Value(hostBridgeKey{}).(bridgeVia)
	return v, ok && v != ""
}

// defaultCloudApp is the app name outpost publishes this console under; the
// SSO secret that signs cloud requests for it is keyed by that name.
const defaultCloudApp = "apps"

var tokenShape = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,512}$`)

// outpostConfigPath is outpost's one canonical config file (conf.DefaultConfigPath).
func outpostConfigPath() (string, bool) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "matrix", "agent.json"), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".config", "matrix", "agent.json"), true
}

// outpostTrust is the slice of outpost's FileConfig the bridge may rely on.
type outpostTrust struct {
	token      string
	adminAddr  string // raw admin_addr from the file
	owners     map[string]bool
	ssoSecrets [][]byte // secrets for the console's app, from apps[] and bashy_services[]
}

type trustFile struct {
	Token       string   `json:"mcp_bearer_token"`
	AdminAddr   string   `json:"admin_addr"`
	OAuth2Email string   `json:"oauth2_email"`
	AdminUsers  []string `json:"admin_users"`
	Apps        []struct {
		Name               string `json:"name"`
		Enabled            *bool  `json:"enabled"`
		TrustCloudIdentity bool   `json:"trust_cloud_identity"`
		SSOSecret          string `json:"sso_secret"`
	} `json:"apps"`
	BashyServices []struct {
		Name               string `json:"name"`
		AppName            string `json:"app_name"`
		Enabled            *bool  `json:"enabled"`
		TrustCloudIdentity bool   `json:"trust_cloud_identity"`
		SSOSecret          string `json:"sso_secret"`
	} `json:"bashy_services"`
}

// loadOutpostTrust re-reads the config on every call (it is a few KB), so a
// token rotation or an owner change takes effect on the next request. It
// refuses a file other users can read. Fields that fail validation are left
// empty rather than failing the whole read: each caller checks what it needs.
func loadOutpostTrust(cloudApp string) (outpostTrust, bool) {
	p, ok := outpostConfigPath()
	if !ok {
		return outpostTrust{}, false
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return outpostTrust{}, false
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return outpostTrust{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return outpostTrust{}, false
	}
	var f trustFile
	if json.Unmarshal(b, &f) != nil {
		return outpostTrust{}, false
	}
	t := outpostTrust{adminAddr: strings.TrimSpace(f.AdminAddr)}
	if tokenShape.MatchString(f.Token) {
		t.token = f.Token
	}
	// Mirror outpost: a populated admin_users list is the authority for who is
	// an admin; oauth2_email (the account owner) stands in only when it is empty.
	if len(f.AdminUsers) > 0 {
		t.owners = coopauth.AdminSet(f.AdminUsers...)
	} else {
		t.owners = coopauth.AdminSet(f.OAuth2Email)
	}
	addSecret := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			t.ssoSecrets = append(t.ssoSecrets, []byte(s))
		}
	}
	for _, a := range f.Apps {
		if a.Name == cloudApp && a.TrustCloudIdentity && (a.Enabled == nil || *a.Enabled) {
			addSecret(a.SSOSecret)
		}
	}
	for _, s := range f.BashyServices {
		name := s.AppName
		if name == "" {
			name = s.Name
		}
		if name == cloudApp && s.TrustCloudIdentity && (s.Enabled == nil || *s.Enabled) {
			addSecret(s.SSOSecret)
		}
	}
	return t, true
}

// adminEndpoint is the admin address outpost itself would use, minus the CLI
// flag the console cannot see: $OUTPOST_ADMIN_ADDR when valid, else the
// file's admin_addr, else the default. A present-but-unusable file value is
// "unknown", never a fallback.
func (t outpostTrust) adminEndpoint() (string, int, bool) {
	if v := strings.TrimSpace(os.Getenv("OUTPOST_ADMIN_ADDR")); v != "" {
		if h, p, err := splitAdminAddr(v); err == nil {
			return h, p, true
		}
	}
	addr := t.adminAddr
	if addr == "" {
		addr = outpostAdminDefaultAddr
	}
	h, p, err := splitAdminAddr(addr)
	return h, p, err == nil
}

func loopbackIP(host string) net.IP {
	host = strings.Trim(host, "[]")
	if host == "" {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		return net.IPv4(127, 0, 0, 1)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ip
	}
	return nil
}

func sameLoopbackEndpoint(h1 string, p1 int, h2 string, p2 int) bool {
	if p1 != p2 {
		return false
	}
	a, b := loopbackIP(h1), loopbackIP(h2)
	return a != nil && b != nil && a.Equal(b)
}

// hostAdminEligible is true when the resolved admin endpoint is a same-host
// loopback address. It is evaluated once, where the panel is built.
func hostAdminEligible(host string, port int) bool {
	return isLoopbackHostPort(net.JoinHostPort(host, strconv.Itoa(port)))
}

// adminAPIPath reports whether the outbound path is the admin REST surface in
// canonical form. Anything with dot segments or doubled slashes is left
// alone so a crafted path cannot pivot the credential onto /mcp or an app.
func adminAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") && path.Clean(p) == p
}

// loopbackOnlyTransport refuses to connect anywhere but loopback, so a
// "localhost" that resolves elsewhere cannot receive the credential. Proxy
// settings are ignored for the same reason.
func loopbackOnlyTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !coopauth.IsLoopbackAddr(c.RemoteAddr().String()) {
			_ = c.Close()
			return nil, errors.New("host admin: refusing non-loopback connection")
		}
		return c, nil
	}
	return tr
}

// cloudOwnerVerified is true only for a tunnel-arrived request from the host
// owner: loopback peer (outpost dials the console over loopback), a FRESH
// signature that verifies under the console app's SSO secret from the trusted
// config, and a SIGNED identity that is a configured owner. The signed value is
// Remote-User, else Remote-Email; an unsigned Remote-Email that disagrees with
// it is tampering and refuses the bridge. Remote-Groups is never consulted.
func cloudOwnerVerified(r *http.Request, cloudApp string) bool {
	if !coopauth.ArrivedViaCloud(r) || !coopauth.IsLoopbackAddr(r.RemoteAddr) {
		return false
	}
	t, ok := loadOutpostTrust(cloudApp)
	if !ok || len(t.owners) == 0 || len(t.ssoSecrets) == 0 {
		return false
	}
	verified := false
	for _, s := range t.ssoSecrets {
		if coopauth.VerifyOutpost(r, s) {
			verified = true
			break
		}
	}
	if !verified {
		return false
	}
	signed := r.Header.Get(coopauth.HdrRemoteUser)
	if signed == "" {
		signed = r.Header.Get(coopauth.HdrRemoteEmail)
	}
	if signed == "" {
		return false
	}
	if e := r.Header.Get(coopauth.HdrRemoteEmail); e != "" && !strings.EqualFold(e, signed) {
		return false
	}
	return t.owners[strings.ToLower(strings.TrimSpace(signed))]
}

// applyHostAdminAuth runs in the Host proxy's director on the outbound clone,
// which keeps the inbound request's context. It always discards the browser's
// Authorization and adds ours only when every condition holds.
func applyHostAdminAuth(out *http.Request, p Panel, cloudApp string) {
	out.Header.Del("Authorization")
	if _, ok := hostBridgeOf(out); !ok || !p.hostAdmin || !adminAPIPath(out.URL.Path) {
		return
	}
	t, ok := loadOutpostTrust(cloudApp)
	if !ok || t.token == "" {
		return
	}
	ah, ap, ok := t.adminEndpoint()
	if !ok {
		return
	}
	ph := p.ProxyHost
	if ph == "" {
		ph = "127.0.0.1"
	}
	if !sameLoopbackEndpoint(ph, p.Port, ah, ap) {
		return
	}
	out.Header.Set("Authorization", "Bearer "+t.token)
}
