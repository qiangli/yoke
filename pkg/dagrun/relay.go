// relay.go — the IN-POD expose relay contract, shared by `bashy dks expose`
// (which writes the token Secret), `dks run` (which plans the ports), Render
// (which wires the wrapper and the secretKeyRefs) and `dks relay --all`
// (which runs the listeners beside the run's own command).
//
// Why in-pod and not a relay Deployment behind a Service: a vk-podman
// executor's pods are NOT on the cluster network. Their DNS is podman's
// resolver, so a cluster Service name does not resolve and its ClusterIP is
// unroutable; the backend also runs exactly one container per Pod, so there
// is no sidecar either. The relay therefore runs as the pod's own PID 1,
// listening on 127.0.0.1 and exec'ing the run's command — which is portable
// to any Kubernetes and strictly tighter: only the run that was given the
// exposes can reach them, with no cluster-wide Service to find.
//
// Nothing here carries a secret. The relay list travels as plain JSON in
// $DKS_RELAYS; each token travels on its own, as $DKS_EXPOSE_TOKEN_<NAME>
// projected from the expose's Secret by secretKeyRef — never a literal
// value, never argv.
package dagrun

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The relay modes. A git expose rides the plain http mode: the smart-HTTP
// filtering and the host credential injection are Outpost's, beyond the
// app route.
const (
	RelayModeHTTP   = "http"
	RelayModeTCP    = "tcp"
	RelayModeEgress = "egress"
)

// RelaysEnv carries the run's relay list as JSON. It holds NO secret.
const RelaysEnv = "DKS_RELAYS"

// RelayTokenEnvPrefix + the expose name is the variable one relay reads its
// token from, and the only way a token reaches the pod.
const RelayTokenEnvPrefix = "DKS_EXPOSE_TOKEN_"

// ExposeSecretPrefix + the expose name is the Secret `bashy dks expose`
// writes: the one object of an expose, and the only place its token exists.
const ExposeSecretPrefix = "dks-expose-"

// ExposeTokenKey is that Secret's only key.
const ExposeTokenKey = "EXPOSE_TOKEN"

// relayNameRE is an expose name: a lowercase DNS label starting with a
// letter, because the name is both a Secret name suffix and an environment
// variable name suffix.
var relayNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// RelayTokenEnvName is the variable the relay named name reads its token
// from: the name upper-cased, hyphens folded to underscores.
func RelayTokenEnvName(name string) string {
	return RelayTokenEnvPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// ExposeSecretName is the Secret holding the expose named name's token.
func ExposeSecretName(name string) string { return ExposeSecretPrefix + name }

// Relay is one in-pod relay as the manifest records it (`metadata.relays`):
// which expose it serves, how it bridges, the app route it reaches, the
// loopback port it listens on, and when it stops. Every field is public
// knowledge — the token is NOT here.
type Relay struct {
	Name       string `yaml:"name" json:"name"`
	Mode       string `yaml:"mode" json:"mode"`
	Upstream   string `yaml:"upstream" json:"upstream"`
	LocalPort  int    `yaml:"local-port" json:"local_port"`
	Expires    string `yaml:"expires,omitempty" json:"expires,omitempty"`
	PathPrefix string `yaml:"path-prefix,omitempty" json:"path_prefix,omitempty"`
}

// TokenEnv is the variable this relay reads its token from.
func (r Relay) TokenEnv() string { return RelayTokenEnvName(r.Name) }

// SecretName is the Secret this relay's token is projected from.
func (r Relay) SecretName() string { return ExposeSecretName(r.Name) }

// Addr is the loopback address this relay listens on.
func (r Relay) Addr() string { return fmt.Sprintf("127.0.0.1:%d", r.LocalPort) }

// URL is the http address a pod reaches this relay at.
func (r Relay) URL() string { return "http://" + r.Addr() }

// Validate checks one relay carries only what the pod side can act on. It
// is deliberately strict: an unusable relay must refuse the run rather than
// leave the pod with a listener pointed somewhere unexpected.
func (r Relay) Validate() error {
	if !relayNameRE.MatchString(r.Name) {
		return fmt.Errorf("relay name %q is not a lowercase DNS label starting with a letter", r.Name)
	}
	switch r.Mode {
	case RelayModeHTTP, RelayModeTCP, RelayModeEgress:
	default:
		return fmt.Errorf("relay %s: mode %q, want %s, %s or %s", r.Name, r.Mode, RelayModeHTTP, RelayModeTCP, RelayModeEgress)
	}
	u, err := url.Parse(r.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("relay %s: upstream %q is not an http(s) app route", r.Name, r.Upstream)
	}
	if r.LocalPort < 1 || r.LocalPort > 65535 {
		return fmt.Errorf("relay %s: local port %d is not a port (1-65535)", r.Name, r.LocalPort)
	}
	if r.Expires != "" {
		if _, err := time.Parse(time.RFC3339, r.Expires); err != nil {
			return fmt.Errorf("relay %s: expires %q is not an RFC3339 instant", r.Name, r.Expires)
		}
	}
	if hasControl(r.PathPrefix) || strings.ContainsAny(r.PathPrefix, " \t") {
		return fmt.Errorf("relay %s: path prefix %q is not a path", r.Name, r.PathPrefix)
	}
	return nil
}

// ExpiresAt is the parsed expiry, zero when the relay never expires.
func (r Relay) ExpiresAt() (time.Time, error) {
	if r.Expires == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, r.Expires)
}

// ValidateRelays checks a whole list: every relay on its own, then that no
// two share a name (one Secret, one variable) or a loopback port (one
// listener). A collision is refused rather than resolved, because which
// relay won would decide where a pod's traffic went.
func ValidateRelays(rs []Relay) error {
	names := make(map[string]struct{}, len(rs))
	ports := make(map[int]string, len(rs))
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			return err
		}
		if _, dup := names[r.Name]; dup {
			return fmt.Errorf("relay %s is listed twice", r.Name)
		}
		names[r.Name] = struct{}{}
		if other, taken := ports[r.LocalPort]; taken {
			return fmt.Errorf("relays %s and %s both want local port %d", other, r.Name, r.LocalPort)
		}
		ports[r.LocalPort] = r.Name
	}
	return nil
}
