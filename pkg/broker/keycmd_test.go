package broker

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/qiangli/yoke/pkg/coopauth"
)

// fakeSatellite exchanges account tokens (account-<email>) for satellite
// tokens and proxies /matrix/h/door1/app/llm/* to the door, stamping the
// signed identity the way outpost does for a trust_cloud_identity app.
func fakeSatellite(t *testing.T, h *harness) *httptest.Server {
	t.Helper()
	sessions := map[string]string{}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path == "/api/v1/tokens/exchange" {
			email, ok := strings.CutPrefix(bearer, "account-")
			if !ok {
				http.Error(w, `{"error":"delegation refused"}`, http.StatusForbidden)
				return
			}
			tok := "sat-" + strconv.Itoa(len(sessions))
			sessions[tok] = email
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"` + tok + `"}`))
			return
		}
		prefix := "/matrix/h/door1/app/llm"
		rest, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		req := r.Clone(r.Context())
		req.URL.Path = rest
		req.RequestURI = ""
		req.Header.Del("Authorization")
		req.Header.Set(coopauth.HdrForwardedPrefix, prefix)
		user := sessions[bearer]
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		if user != "" {
			req.Header.Set(coopauth.HdrRemoteUser, user)
		}
		mac := hmac.New(sha256.New, []byte(selfSecret))
		mac.Write([]byte(user + "\n\n" + prefix + "\n" + ts))
		req.Header.Set(coopauth.HdrIdentityTs, ts)
		req.Header.Set(coopauth.HdrIdentitySig, hex.EncodeToString(mac.Sum(nil)))
		h.b.ServeHTTP(w, req)
	}))
}

func runKey(t *testing.T, account, url string) (string, string, error) {
	t.Helper()
	keyAccountToken = func() string { return account }
	cmd := newKeyCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{url})
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestLLMKeyMintStoreRotateAndRefuse(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	saved := keyAccountToken
	t.Cleanup(func() { keyAccountToken = saved })
	alice := boundary.Member{Email: "alice@example.com", Circle: "team2027", Status: "active", Mutual: true}
	fixture := &memberFixture{}
	fixture.set(alice)
	members := httptest.NewServer(fixture)
	defer members.Close()
	h, peers := memberPeerHarness(t, &boundary.Client{BaseURL: members.URL, Token: "owner", HTTP: members.Client()})
	if err := SaveSelfService(SelfServiceFile(peers), SelfService{Enabled: true, Circles: []string{"team2027"}, SSOSecret: selfSecret}); err != nil {
		t.Fatal(err)
	}
	sat := fakeSatellite(t, h)
	defer sat.Close()
	savedHTTP := keyHTTP
	t.Cleanup(func() { keyHTTP = savedHTTP })
	keyHTTP = func() *http.Client {
		c := sat.Client()
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return c
	}
	doorURL := sat.URL + "/matrix/h/door1/app/llm"

	out, errOut, err := runKey(t, "account-alice@example.com", doorURL)
	if err != nil {
		t.Fatalf("llm key: %v", err)
	}
	g, err := LoadDoorGrant("door1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Peer != "self-alice@example.com" || g.CloudboxURL != doorURL {
		t.Fatalf("grant = %+v", g)
	}
	if strings.Contains(out+errOut, g.Credential) || strings.Contains(out+errOut, "bpeer_") {
		t.Fatalf("credential printed: %q %q", out, errOut)
	}
	path, _ := doorGrantPath("door1")
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("grant file mode = %v %v", fi.Mode(), err)
	}
	if c, b := peerPost(t, h, "/v1/chat/completions", g.Credential, nil); c != http.StatusOK {
		t.Fatalf("chat with stored key = %d %s", c, b)
	}

	if _, _, err := runKey(t, "account-alice@example.com", doorURL); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	g2, _ := LoadDoorGrant("door1")
	if g2.Credential == g.Credential {
		t.Fatal("rerun did not rotate")
	}
	if c, _ := peerPost(t, h, "/v1/chat/completions", g.Credential, nil); c != http.StatusUnauthorized {
		t.Fatalf("old key = %d, want 401", c)
	}

	if _, _, err := runKey(t, "account-mallory@example.com", doorURL); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("non-member = %v", err)
	}
	if _, _, err := runKey(t, "bad-token", doorURL); err == nil || !strings.Contains(err.Error(), "not signed in to this satellite") {
		t.Fatalf("bad account = %v", err)
	}
	if _, _, err := runKey(t, "", doorURL); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("no account = %v", err)
	}
}

func TestParseDoorAppURL(t *testing.T) {
	app, origin, host, err := parseDoorAppURL("https://sat.example/matrix/h/Door1/app/llm/")
	if err != nil || app != "https://sat.example/matrix/h/door1/app/llm" || origin != "https://sat.example" || host != "door1" {
		t.Fatalf("= %q %q %q %v", app, origin, host, err)
	}
	for _, bad := range []string{
		"http://sat.example/matrix/h/door1/app/llm",
		"https://sat.example/matrix/h/door1/app/other",
		"https://user@sat.example/matrix/h/door1/app/llm",
		"https://sat.example/matrix/h/door1/app/llm?x=1",
		"sat.example",
	} {
		if _, _, _, err := parseDoorAppURL(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
