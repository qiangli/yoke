package webbotauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// RFC 8037 appendix A.3: the Ed25519 JWK thumbprint test vector.
func TestThumbprintRFC8037(t *testing.T) {
	x, _ := base64.RawURLEncoding.DecodeString("11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo")
	if got, want := Thumbprint(ed25519.PublicKey(x)), "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"; got != want {
		t.Fatalf("thumbprint %s, want %s", got, want)
	}
}

func writeKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "outpost@test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ssh_host_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, priv
}

func TestLoadKeyAndEnv(t *testing.T) {
	path, priv := writeKey(t)
	t.Setenv(EnvKey, path)
	t.Setenv(EnvAgent, "https://agent.example/.well-known/http-message-signatures-directory")
	s, err := FromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Key.Equal(priv) || s.KeyID != Thumbprint(priv.Public().(ed25519.PublicKey)) {
		t.Fatal("loaded key or keyid mismatch")
	}
	if s.Agent == "" {
		t.Fatal("signature agent not taken from env")
	}
	t.Setenv(EnvKey, filepath.Join(t.TempDir(), "missing"))
	if _, err := FromEnv(""); err == nil || !strings.Contains(err.Error(), "no outpost identity") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := NewSigner(priv, "http://insecure.example/"); err == nil {
		t.Fatal("http signature agent accepted")
	}
	t.Setenv(EnvSign, "")
	if Enabled(false) || !Enabled(true) {
		t.Fatal("Enabled flag")
	}
	t.Setenv(EnvSign, "1")
	if !Enabled(false) {
		t.Fatal("Enabled env")
	}
}

var memberRE = regexp.MustCompile(`^sig1=(\(.*\).*)$`)

// verify is an independent RFC 9421 check of what Sign emitted.
func verify(t *testing.T, r *http.Request, pub ed25519.PublicKey, authority, target string) {
	t.Helper()
	m := memberRE.FindStringSubmatch(r.Header.Get("Signature-Input"))
	if m == nil {
		t.Fatalf("Signature-Input %q", r.Header.Get("Signature-Input"))
	}
	params := m[1]
	for _, want := range []string{`alg="ed25519"`, `tag="web-bot-auth"`, `keyid="` + Thumbprint(pub) + `"`, ";created=", ";expires=", `;nonce="`} {
		if !strings.Contains(params, want) {
			t.Fatalf("params %s lack %s", params, want)
		}
	}
	lines := []string{`"@authority": ` + authority, `"@method": ` + r.Method, `"@target-uri": ` + target}
	if agent := r.Header.Get("Signature-Agent"); agent != "" {
		lines = append(lines, `"signature-agent": `+agent)
	}
	lines = append(lines, `"@signature-params": `+params)
	sig := strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Signature"), "sig1=:"), ":")
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(strings.Join(lines, "\n")), raw) {
		t.Fatalf("signature does not verify over\n%s", strings.Join(lines, "\n"))
	}
}

func TestTransportSignsEachRequest(t *testing.T) {
	_, priv := writeKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	agent := "https://agent.example/.well-known/http-message-signatures-directory"
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new?x=1", http.StatusFound)
			return
		}
	}))
	defer srv.Close()
	signer, err := NewSigner(priv, agent)
	if err != nil {
		t.Fatal(err)
	}
	signer.Now = func() time.Time { return time.Unix(1700000000, 0) }
	client := &http.Client{Transport: &Transport{Signer: signer}}
	resp, err := client.Get(srv.URL + "/old?y=%2F")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(seen) != 2 {
		t.Fatalf("saw %d requests", len(seen))
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	verify(t, seen[0], pub, host, srv.URL+"/old?y=%2F")
	verify(t, seen[1], pub, host, srv.URL+"/new?x=1")
	if got := seen[0].Header.Get("Signature-Agent"); got != `"`+agent+`"` {
		t.Fatalf("Signature-Agent %q", got)
	}
	if !strings.Contains(seen[0].Header.Get("Signature-Input"), ";created=1700000000;expires=1700000300;") {
		t.Fatalf("window %s", seen[0].Header.Get("Signature-Input"))
	}
}

// A redirect hop is re-signed only while it stays on the first request's
// origin; a cross-origin hop goes out unsigned.
func TestTransportCrossOriginRedirectUnsigned(t *testing.T) {
	_, priv := writeKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	var other []*http.Request
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other = append(other, r)
	}))
	defer away.Close()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/next", http.StatusFound)
		case "/next":
			http.Redirect(w, r, away.URL+"/landing", http.StatusFound)
		}
	}))
	defer srv.Close()
	signer, err := NewSigner(priv, "https://agent.example/.well-known/http-message-signatures-directory")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &Transport{Signer: signer}}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/start", nil)
	// Caller-set signature headers are copied to every hop by net/http.
	req.Header.Set("Signature-Agent", `"https://stale.example/"`)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(seen) != 2 || len(other) != 1 {
		t.Fatalf("saw %d same-origin and %d cross-origin requests", len(seen), len(other))
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	verify(t, seen[0], pub, host, srv.URL+"/start")
	verify(t, seen[1], pub, host, srv.URL+"/next")
	for _, h := range []string{"Signature", "Signature-Input", "Signature-Agent"} {
		if v := other[0].Header.Get(h); v != "" {
			t.Fatalf("cross-origin redirect carried %s: %q", h, v)
		}
	}
}

func TestAuthorityDropsDefaultPort(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Example.COM:443/a": "example.com",
		"http://example.com:80/":    "example.com",
		"http://example.com:8080/":  "example.com:8080",
		"https://[::1]:443/":        "[::1]",
	} {
		r, _ := http.NewRequest(http.MethodGet, raw, nil)
		if got := Authority(r); got != want {
			t.Errorf("%s: %s, want %s", raw, got, want)
		}
	}
}

func TestTargetURINormalized(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "https://Example.com:443", nil)
	if got := TargetURI(r); got != "https://example.com/" {
		t.Fatalf("TargetURI %s", got)
	}
}

func TestDirectoryJWKSet(t *testing.T) {
	_, priv := writeKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	var set struct{ Keys []JWK }
	if err := json.Unmarshal(Directory(pub), &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 1 || set.Keys[0].Kty != "OKP" || set.Keys[0].Crv != "Ed25519" || set.Keys[0].Kid != Thumbprint(pub) {
		t.Fatalf("directory %+v", set)
	}
}

func TestUserAgent(t *testing.T) {
	if UserAgent("bashy-fetch", "") != "bashy-fetch" || UserAgent("bashy-site", "v1.2") != "bashy-site/v1.2" {
		t.Fatal("UserAgent")
	}
}
