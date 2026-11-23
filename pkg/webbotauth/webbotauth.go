// Package webbotauth signs outbound HTTP requests with the outpost host
// identity in the Web Bot Auth shape: RFC 9421 HTTP Message Signatures,
// alg="ed25519", tag="web-bot-auth", keyid = the RFC 7638 JWK thumbprint of
// the Ed25519 public key, optionally naming the signer's public key
// directory (/.well-known/http-message-signatures-directory) in a
// Signature-Agent header that the signature covers.
//
// Signing is the owner's choice and off by default: callers enable it with
// a --sign flag or BASHY_SIGN_REQUESTS=1. A site verifies the signature to
// recognise a bashy agent; it never gates content on it.
package webbotauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// Tag is the RFC 9421 tag parameter Web Bot Auth signatures carry.
	Tag = "web-bot-auth"
	// DirectoryPath is the well-known path of a public key directory.
	DirectoryPath = "/.well-known/http-message-signatures-directory"
	// DirectoryMediaType is the directory's Content-Type.
	DirectoryMediaType = "application/http-message-signatures-directory+json"

	// EnvSign enables signing ("1", "true", "yes", "on").
	EnvSign = "BASHY_SIGN_REQUESTS"
	// EnvKey overrides the key file path (tests, non-default identities).
	EnvKey = "BASHY_SIGN_KEY"
	// EnvAgent names the signer's key directory URL (Signature-Agent).
	EnvAgent = "BASHY_SIGNATURE_AGENT"

	// DefaultTTL bounds a signature's validity (created..expires).
	DefaultTTL = 5 * time.Minute
)

// Enabled reports whether signing is on: the flag, else BASHY_SIGN_REQUESTS.
func Enabled(flag bool) bool {
	if flag {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvSign))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// KeyPath is the outpost host key: $BASHY_SIGN_KEY, else the outpost config
// dir ($XDG_CONFIG_HOME/matrix or ~/.config/matrix) + /ssh_host_ed25519.
func KeyPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvKey)); p != "" {
		return p, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "matrix", "ssh_host_ed25519"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "matrix", "ssh_host_ed25519"), nil
}

// LoadKey reads an OpenSSH Ed25519 private key. It never creates one: the
// identity belongs to outpost, which generates it on first run.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no outpost identity at %s (run outpost once, or set %s)", path, EnvKey)
	}
	if err != nil {
		return nil, err
	}
	raw, err := ssh.ParseRawPrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	switch k := raw.(type) {
	case ed25519.PrivateKey:
		return k, nil
	case *ed25519.PrivateKey:
		return *k, nil
	}
	return nil, fmt.Errorf("%s: not an Ed25519 key (%T)", path, raw)
}

// JWK is the public key as an RFC 8037 OKP JSON Web Key.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid,omitempty"`
}

// PublicJWK returns pub as a JWK whose kid is its thumbprint.
func PublicJWK(pub ed25519.PublicKey) JWK {
	return JWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub), Kid: Thumbprint(pub)}
}

// Thumbprint is the RFC 7638 SHA-256 JWK thumbprint, base64url: the keyid.
func Thumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Directory renders the JWK Set served at DirectoryPath.
func Directory(pub ed25519.PublicKey) []byte {
	b, _ := json.Marshal(struct {
		Keys []JWK `json:"keys"`
	}{[]JWK{PublicJWK(pub)}})
	return b
}

// Signer signs requests with one key.
type Signer struct {
	Key   ed25519.PrivateKey
	KeyID string
	// Agent is the signer's key directory URL; when set it is sent as
	// Signature-Agent and covered by the signature.
	Agent string
	TTL   time.Duration
	Now   func() time.Time
}

// NewSigner returns a signer for key with the thumbprint keyid.
func NewSigner(key ed25519.PrivateKey, agent string) (*Signer, error) {
	if agent != "" {
		u, err := url.Parse(agent)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("signature agent %q: want an https URL", agent)
		}
	}
	return &Signer{Key: key, KeyID: Thumbprint(key.Public().(ed25519.PublicKey)), Agent: agent}, nil
}

// FromEnv loads the host key and builds a signer; agent falls back to
// $BASHY_SIGNATURE_AGENT.
func FromEnv(agent string) (*Signer, error) {
	path, err := KeyPath()
	if err != nil {
		return nil, err
	}
	key, err := LoadKey(path)
	if err != nil {
		return nil, err
	}
	if agent == "" {
		agent = strings.TrimSpace(os.Getenv(EnvAgent))
	}
	return NewSigner(key, agent)
}

// Sign sets Signature-Agent (when configured), Signature-Input and
// Signature on req, covering @authority, @method, @target-uri and, when
// present, signature-agent.
func (s *Signer) Sign(req *http.Request) error {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	components := []string{"@authority", "@method", "@target-uri"}
	if s.Agent != "" {
		req.Header.Set("Signature-Agent", strconv.Quote(s.Agent))
		components = append(components, "signature-agent")
	}
	created := now().Unix()
	params := `("` + strings.Join(components, `" "`) + `")` +
		";created=" + strconv.FormatInt(created, 10) +
		";expires=" + strconv.FormatInt(created+int64(ttl/time.Second), 10) +
		`;keyid="` + s.KeyID + `";alg="ed25519"` +
		`;nonce="` + base64.StdEncoding.EncodeToString(nonce) + `";tag="` + Tag + `"`
	base, err := SignatureBase(req, components, params)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(s.Key, []byte(base))
	req.Header.Set("Signature-Input", "sig1="+params)
	req.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(sig)+":")
	return nil
}

// SignatureBase builds the RFC 9421 signature base for the components this
// package signs.
func SignatureBase(req *http.Request, components []string, params string) (string, error) {
	var b strings.Builder
	for _, c := range components {
		var v string
		switch c {
		case "@authority":
			v = Authority(req)
		case "@method":
			v = req.Method
		case "@target-uri":
			v = TargetURI(req)
		default:
			if strings.HasPrefix(c, "@") {
				return "", fmt.Errorf("unsupported component %s", c)
			}
			values := req.Header.Values(c)
			if len(values) == 0 {
				return "", fmt.Errorf("missing header %s", c)
			}
			for i := range values {
				values[i] = strings.TrimSpace(values[i])
			}
			v = strings.Join(values, ", ")
		}
		fmt.Fprintf(&b, "%q: %s\n", c, v)
	}
	b.WriteString(`"@signature-params": ` + params)
	return b.String(), nil
}

// Authority is the request's lowercased host with a default port removed.
func Authority(req *http.Request) string {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	host = strings.ToLower(host)
	if h, port, err := net.SplitHostPort(host); err == nil {
		if (port == "443" && req.URL.Scheme == "https") || (port == "80" && req.URL.Scheme == "http") {
			if strings.Contains(h, ":") {
				return "[" + h + "]"
			}
			return h
		}
	}
	return host
}

// TargetURI is the absolute request target: scheme://authority plus the
// request-target (an empty path is "/"), as the server reconstructs it.
func TargetURI(req *http.Request) string {
	return req.URL.Scheme + "://" + Authority(req) + req.URL.RequestURI()
}

// Transport signs every request, and each redirect hop that stays on the
// first request's scheme and authority. Like net/http's Authorization
// stripping, a cross-origin redirect hop is sent unsigned, with any copied
// signature headers removed, so the identity is never presented to a site
// the caller did not address.
type Transport struct {
	Base   http.RoundTripper
	Signer *Signer
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	if sameOrigin(clone, firstRequest(req)) {
		if err := t.Signer.Sign(clone); err != nil {
			return nil, err
		}
	} else {
		for _, h := range []string{"Signature", "Signature-Input", "Signature-Agent"} {
			clone.Header.Del(h)
		}
	}
	return base.RoundTrip(clone)
}

// firstRequest walks a redirect chain back to the request the caller made.
func firstRequest(req *http.Request) *http.Request {
	for req.Response != nil && req.Response.Request != nil {
		req = req.Response.Request
	}
	return req
}

func sameOrigin(a, b *http.Request) bool {
	return a == b || (strings.EqualFold(a.URL.Scheme, b.URL.Scheme) && Authority(a) == Authority(b))
}

// UserAgent is the stable bashy user agent: product/version, or product
// alone when no version is known.
func UserAgent(product, version string) string {
	if version = strings.TrimSpace(version); version == "" {
		return product
	}
	return product + "/" + version
}
