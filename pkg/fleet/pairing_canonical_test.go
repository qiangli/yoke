package fleet

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
)

// S606 S2: a host pairs ONCE with the cloudbox portal (agent.json `cloudbox`)
// and reaches satellites as attachments (agent.json `attachments`). The
// coordination base is the attached satellite, account reads go to the
// portal, and neither needs a master-token file or a satellite pairing code.

func fakeCall(t *testing.T, method, url, tok string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func mustStatus(t *testing.T, want int, method, url, tok string, body any) map[string]any {
	t.Helper()
	got, out := fakeCall(t, method, url, tok, body)
	if got != want {
		t.Fatalf("%s %s: status %d, want %d (%v)", method, url, got, want, out)
	}
	return out
}

func writeAgentJSON(t *testing.T, cfg string, doc map[string]any) {
	t.Helper()
	dir := filepath.Join(cfg, "outpost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// pairOnceAndAttach drives the wire contract against the fake exactly as
// `outpost register --code` + `outpost satellite attach` do and returns the
// canonical agent.json those writers produce.
func pairOnceAndAttach(t *testing.T, f *cloudboxfake.Fake, owner string, attach bool) (doc map[string]any, hostTok, satURL, satTok string) {
	t.Helper()
	sat := f.AddSatellite(t, "alice@example.com", "zoo")
	code, _ := mustStatus(t, 201, "POST", f.URL()+"/api/v1/hosts/invites", owner, nil)["code"].(string)
	pr := mustStatus(t, 201, "POST", f.URL()+"/api/v1/hosts/pair", "", map[string]string{"code": code, "host": "studio"})
	if pr["tunnel"] != false {
		t.Fatalf("pair = %v", pr)
	}
	hostTok, _ = pr["token"].(string)
	doc = map[string]any{
		"agent_name": "studio",
		"cloudbox": map[string]any{
			"portal_url": f.URL(), "host": "studio", "token": hostTok,
			"scopes": []string{"host"}, "owner_email": "alice@example.com",
		},
	}
	if !attach {
		return doc, hostTok, "", ""
	}
	at := mustStatus(t, 201, "POST", f.URL()+"/api/v1/satellites/"+sat+"/attach", hostTok, nil)
	satURL, _ = at["satellite"].(map[string]any)["endpoint"].(string)
	attachTok, _ := at["attach_token"].(string)
	red := mustStatus(t, 201, "POST", satURL+"/api/v1/hosts/attach", attachTok, map[string]string{"host": "studio"})
	satTok, _ = red["token"].(string)
	doc["attachments"] = []map[string]any{{
		"satellite_id": sat, "name": "zoo", "endpoint": satURL, "token": satTok, "scopes": []string{"satellite-host"},
	}}
	return doc, hostTok, satURL, satTok
}

func TestCanonicalPairingAttachedSatelliteIsCoordinationBase(t *testing.T) {
	cfg := pairedHost(t, "")
	f := cloudboxfake.New(t)
	owner := f.AddOwner("alice@example.com")
	doc, hostTok, satURL, satTok := pairOnceAndAttach(t, f, owner, true)
	writeAgentJSON(t, cfg, doc)

	if _, err := os.Stat(MasterTokenPath()); !os.IsNotExist(err) {
		t.Fatalf("master-token must not exist: %v", err)
	}
	// Coordination verbs reach the attached satellite with its own token.
	base, tok := ResolveCloud("", "")
	if base != satURL || tok != satTok {
		t.Fatalf("ResolveCloud = %q %q, want the satellite and its host token", base, tok)
	}
	// Account reads go to the portal with the cloudbox host token; the host
	// stays owned by its account and sees the satellite as attached.
	mbase, mtok := ResolveMaster("", "")
	if mbase != f.URL() || mtok != hostTok {
		t.Fatalf("ResolveMaster = %q %q, want the portal and the host token", mbase, mtok)
	}
	sats := mustStatus(t, 200, "GET", mbase+"/api/v1/satellites", mtok, nil)["satellites"].([]any)
	if len(sats) != 1 || sats[0].(map[string]any)["attached"] != true {
		t.Fatalf("host satellites = %v", sats)
	}
	// The satellite accepts the credential the resolver chose for it.
	mustStatus(t, 204, "DELETE", base+"/api/v1/hosts/self", tok, nil)
}

// A legacy satellite-primary top-level pairing coexists until migrated; the
// canonical attachment still decides the coordination base, and the legacy
// Outpost token is never sent to the satellite or the portal.
func TestCanonicalPairingCoexistsWithLegacyTopLevel(t *testing.T) {
	cfg := pairedHost(t, "")
	f := cloudboxfake.New(t)
	owner := f.AddOwner("alice@example.com")
	doc, hostTok, satURL, satTok := pairOnceAndAttach(t, f, owner, true)
	doc["server_addr"] = "legacy-sat.example"
	doc["access_token"] = "pairing-token"
	writeAgentJSON(t, cfg, doc)
	writeMaster(t, cfg, "master-token\nhttps://ai.dhnt.io\n")

	if base, tok := ResolveCloud("", ""); base != satURL || tok != satTok {
		t.Fatalf("ResolveCloud = %q %q", base, tok)
	}
	if _, tok := ResolveMaster("", ""); tok != hostTok {
		t.Fatalf("ResolveMaster token = %q", tok)
	}
	// The legacy pairing's own server still gets the legacy token.
	if _, tok := ResolveCloud("https://legacy-sat.example", ""); tok != "pairing-token" {
		t.Fatalf("legacy server token = %q", tok)
	}
	// A base nothing here was issued by: the read-only legacy fallback only.
	if _, tok := ResolveCloud("https://ai.dhnt.io", ""); tok != "master-token" {
		t.Fatalf("legacy fallback token = %q", tok)
	}
}

// Paired with cloudbox, attached to nothing: the same features as an
// unpaired host plus the account record — the portal is the only base and
// its host token the only credential.
func TestCanonicalPairingUnattachedUsesPortal(t *testing.T) {
	cfg := pairedHost(t, "")
	f := cloudboxfake.New(t)
	owner := f.AddOwner("alice@example.com")
	doc, hostTok, _, _ := pairOnceAndAttach(t, f, owner, false)
	writeAgentJSON(t, cfg, doc)

	for name, fn := range map[string]func(string, string) (string, string){"ResolveCloud": ResolveCloud, "ResolveMaster": ResolveMaster} {
		if base, tok := fn("", ""); base != f.URL() || tok != hostTok {
			t.Fatalf("%s = %q %q", name, base, tok)
		}
	}
}

// A token goes only to the server that issued it. Two servers on one host
// are two servers: the port is part of the match.
func TestCanonicalTokensStayWithTheirIssuer(t *testing.T) {
	cfg := pairedHost(t, "")
	writeAgentJSON(t, cfg, map[string]any{
		"cloudbox":    map[string]any{"portal_url": "https://portal.example", "host": "studio", "token": "host-tok"},
		"attachments": []map[string]any{{"satellite_id": "sat_1", "endpoint": "http://127.0.0.1:8001/", "token": "sat-tok"}},
	})
	cases := map[string]string{
		"http://127.0.0.1:8001":        "sat-tok",
		"http://127.0.0.1:8002":        "", // same host, different server
		"https://PORTAL.example:443/":  "host-tok",
		"https://portal.example:8443":  "",
		"https://elsewhere.example":    "",
		"https://portal.example/x/y/z": "host-tok",
	}
	for base, want := range cases {
		if _, tok := ResolveCloud(base, ""); tok != want {
			t.Errorf("ResolveCloud(%q) token = %q, want %q", base, tok, want)
		}
	}
	// Explicit env still wins over the canonical base.
	t.Setenv("BASHY_CLOUDBOX_URL", "https://portal.example")
	if base, tok := ResolveCloud("", ""); base != "https://portal.example" || tok != "host-tok" {
		t.Fatalf("env base = %q %q", base, tok)
	}
}

func TestMasterTokenWritersPrintDeprecation(t *testing.T) {
	cfg := pairedHost(t, "sat1.dhnt.io")
	src := filepath.Join(t.TempDir(), "agent.json")
	os.WriteFile(src, []byte(`{"server_addr":"ai.dhnt.io","access_token":"old-master"}`), 0o600)

	c := NewMasterTokenCmd()
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)
	c.SetArgs([]string{"save", "--from", src})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "deprecated") || !strings.Contains(errb.String(), "outpost satellite attach") {
		t.Fatalf("stderr = %q, want the deprecation", errb.String())
	}
	if strings.Contains(out.String(), "deprecated") {
		t.Fatalf("deprecation leaked to stdout: %q", out.String())
	}
	// Still a working (legacy) writer, and the file is read as the fallback.
	if data, _ := os.ReadFile(filepath.Join(cfg, "bashy", "master-token")); string(data) != "old-master\nhttps://ai.dhnt.io\n" {
		t.Fatalf("file = %q", data)
	}

	errb.Reset()
	c = NewMasterTokenCmd()
	c.SetOut(&out)
	c.SetErr(&errb)
	c.SetArgs([]string{"mint", "--from", filepath.Join(t.TempDir(), "missing.json")})
	_ = c.Execute()
	if !strings.Contains(errb.String(), "deprecated") {
		t.Fatalf("mint stderr = %q, want the deprecation", errb.String())
	}
}
