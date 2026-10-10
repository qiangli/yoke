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
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// The Host tile fronts outpost's admin UI, which has its own login. On the same
// machine, for the OS user who already passed this console's gate, that second
// prompt proves nothing. The console therefore presents outpost's local MCP
// bearer (the credential `outpost` CLI subcommands read from the same 0600
// file) on the operator's behalf.
//
// The credential is scoped by construction, every one of these must hold:
//
//   - the panel is the stock Host tile AND its admin address is loopback
//     (panel.hostAdmin, set only by hostBuiltinPanel). A registered or --app
//     panel never carries it, and an explicit remote endpoint never gets it;
//   - the console gate admitted an operator for this request (ctx value set by
//     consoleGate for a session cookie or the ungated loopback row only — not
//     cloud-vouched, public, open-path or paired-device requests);
//   - the browser's TCP peer is itself loopback;
//   - the outbound path is the admin REST surface (/api/…), cleaned.
//
// The browser never sees the token: whatever Authorization header it sent is
// discarded and ours is added to the outbound request only.

type localOperatorKey struct{}

func markLocalOperator(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), localOperatorKey{}, true))
}

func isLocalOperator(r *http.Request) bool {
	v, _ := r.Context().Value(localOperatorKey{}).(bool)
	return v
}

// hostAdminToken reads outpost's MCP bearer. A var so tests supply a
// synthetic one without touching the real config.
var hostAdminToken = readOutpostMCPToken

var tokenShape = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,512}$`)

var outpostTokenCache struct {
	sync.Mutex
	path  string
	mtime time.Time
	size  int64
	token string
}

func outpostConfigCandidates() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "matrix"), filepath.Join(xdg, "outpost"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "matrix"), filepath.Join(home, ".config", "outpost"))
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(d, "agent.json"))
	}
	return out
}

// readOutpostMCPToken returns the token from the first candidate config that
// has one. It refuses a file other users can read, and a token of unexpected
// shape (which also rules out header injection).
func readOutpostMCPToken() (string, bool) {
	for _, p := range outpostConfigCandidates() {
		if tok, ok := readMCPTokenFile(p); ok {
			return tok, true
		}
	}
	return "", false
}

func readMCPTokenFile(p string) (string, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return "", false
	}
	c := &outpostTokenCache
	c.Lock()
	defer c.Unlock()
	if c.path == p && c.mtime.Equal(st.ModTime()) && c.size == st.Size() {
		return c.token, c.token != ""
	}
	tok := ""
	if b, err := os.ReadFile(p); err == nil {
		var fc struct {
			Token string `json:"mcp_bearer_token"`
		}
		if json.Unmarshal(b, &fc) == nil {
			tok = fc.Token
		}
	}
	if !tokenShape.MatchString(tok) {
		tok = ""
	}
	c.path, c.mtime, c.size, c.token = p, st.ModTime(), st.Size(), tok
	return tok, tok != ""
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

// applyHostAdminAuth runs in the Host proxy's director on the outbound clone,
// which keeps the inbound request's RemoteAddr and context. It always discards the
// browser's Authorization and adds ours only when every condition holds.
func applyHostAdminAuth(out *http.Request) {
	out.Header.Del("Authorization")
	if !isLocalOperator(out) || !coopauth.IsLoopbackAddr(out.RemoteAddr) || !adminAPIPath(out.URL.Path) {
		return
	}
	if tok, ok := hostAdminToken(); ok {
		out.Header.Set("Authorization", "Bearer "+tok)
	}
}
