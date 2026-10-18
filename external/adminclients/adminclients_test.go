package adminclients

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/gws"
)

const canary = "CANARY-SECRET-key-0123456789"

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func credFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cred.json")
	if err := os.WriteFile(p, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func m365Scope(t *testing.T, scopes string, sandbox bool) Scope {
	t.Helper()
	m := map[string]string{EnvM365Tenant: "contoso.onmicrosoft.com", EnvM365Auth: credFile(t), EnvM365Scopes: scopes}
	if sandbox {
		m[EnvM365Sandbox] = "1"
	}
	s, e := ResolveScope("m365", envOf(m))
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestAdmissionRecords(t *testing.T) {
	for n, lic := range map[string]string{"m365": "MIT", "gam": "Apache-2.0"} {
		c, ok := Lookup(n)
		if !ok || c.License != lic || len(c.Commit) != 40 || c.Version == "" || c.Repo == "" {
			t.Errorf("%s: bad record %+v", n, c)
		}
	}
	if c, _ := Lookup("m365"); c.Artifact == "" {
		t.Error("m365 needs a pinned npm artifact")
	}
	c, _ := Lookup("gam")
	if len(c.Digests) == 0 {
		t.Error("gam needs asset digests")
	}
	for a, d := range c.Digests {
		if len(d) != 64 {
			t.Errorf("%s digest length %d", a, len(d))
		}
	}
	if fmt.Sprint(Names()) != "[gam m365]" {
		t.Errorf("names = %v", Names())
	}
	if HostOnlyError("gam") == nil || HostOnlyError("nope") == nil {
		t.Error("host-only errors expected")
	}
}

func TestOverlapWithGwsAndReversibility(t *testing.T) {
	for _, o := range Ops {
		if o.Client == "gam" {
			if gws.Deferral(o.GWSGap) == "" {
				t.Errorf("gam %s fills %q which gws does not defer — overlap, not a gap", o.Name, o.GWSGap)
			}
			if !strings.Contains(gws.Deferral(o.GWSGap), "GAM overlap retained") && !strings.Contains(gws.Deferral(o.GWSGap), "see admin:directory") {
				t.Errorf("gws deferral for %s does not hand admin coverage to this story", o.GWSGap)
			}
		}
		if o.Effect == EffectWrite {
			r, ok := LookupOp(o.Client, o.Reverse)
			if !ok || r.Effect != EffectDelete {
				t.Errorf("%s:%s has no delete reverse", o.Client, o.Name)
			}
		}
		if o.Effect == EffectRead && o.Reverse != "" {
			t.Errorf("read %s has a reverse", o.Name)
		}
	}
	for _, s := range gws.Services {
		for _, o := range Ops {
			if o.Client == "gam" && strings.Contains(o.Argv, s.Name) {
				t.Errorf("gam op %s duplicates gws service %s", o.Name, s.Name)
			}
		}
	}
}

func TestResolveScopeM365(t *testing.T) {
	f := credFile(t)
	cases := []struct {
		env  map[string]string
		code gws.Code
	}{
		{nil, gws.CodeMissingCredentials},
		{map[string]string{EnvM365Tenant: "common", EnvM365Auth: f}, gws.CodeInvalidInput},
		{map[string]string{EnvM365Tenant: "contoso.com"}, gws.CodeMissingCredentials},
		{map[string]string{EnvM365Tenant: "contoso.com", EnvM365Auth: f}, gws.CodeMissingCredentials},
		{map[string]string{EnvM365Tenant: "contoso.com", EnvM365Auth: filepath.Dir(f), EnvM365Scopes: "User.Read.All"}, gws.CodeInvalidInput},
	}
	for i, c := range cases {
		if _, e := ResolveScope("m365", envOf(c.env)); e == nil || e.Code != c.code {
			t.Errorf("case %d: %v", i, e)
		}
	}
	s := m365Scope(t, "User.Read.All", false)
	if rs := s.RedactedScope(); strings.Contains(rs, filepath.Dir(s.CredentialFile)) || strings.Contains(rs, canary) || !strings.Contains(rs, "file:cred.json") {
		t.Errorf("redacted scope leaks: %s", rs)
	}
	if _, e := ResolveScope("m365", envOf(map[string]string{EnvM365Tenant: "0f8fad5b-d9cb-469f-a165-70867728950e", EnvM365Auth: f, EnvM365Scopes: "User.Read.All"})); e != nil {
		t.Errorf("guid tenant: %v", e)
	}
}

func clearGws(t *testing.T) {
	for _, k := range []string{gws.EnvAccount, gws.EnvAuthKind, gws.EnvCredentialsFile, gws.EnvServiceAccountFile, gws.EnvProfile} {
		t.Setenv(k, "")
	}
}

func TestResolveScopeGAMReusesGwsCredentials(t *testing.T) {
	clearGws(t)
	f := credFile(t)
	env := map[string]string{EnvGAMDomain: "example.org", EnvGAMScopes: gUserRO}
	if _, e := ResolveScope("gam", envOf(map[string]string{})); e == nil || e.Code != gws.CodeMissingCredentials {
		t.Fatalf("no domain: %v", e)
	}
	if _, e := ResolveScope("gam", envOf(env)); e == nil || e.Code != gws.CodeMissingCredentials {
		t.Fatalf("no gws credentials must fail, got %v", e)
	}
	t.Setenv(gws.EnvAccount, "admin@example.org")
	t.Setenv(gws.EnvCredentialsFile, f)
	if _, e := ResolveScope("gam", envOf(env)); e == nil || e.Code != gws.CodeUnsupported {
		t.Fatalf("oauth credentials are unmapped: %v", e)
	}
	t.Setenv(gws.EnvCredentialsFile, "")
	t.Setenv(gws.EnvServiceAccountFile, f)
	s, e := ResolveScope("gam", envOf(env))
	if e != nil || s.Account != "admin@example.org" || s.CredentialFile != f {
		t.Fatalf("scope: %+v %v", s, e)
	}
	t.Setenv(gws.EnvCredentialsFile, f)
	if _, e := ResolveScope("gam", envOf(env)); e == nil || e.Code != gws.CodeInvalidInput {
		t.Fatalf("ambiguous gws credentials must fail: %v", e)
	}
	t.Setenv(gws.EnvCredentialsFile, "")
	env[EnvGAMDomain] = "not a domain"
	if _, e := ResolveScope("gam", envOf(env)); e == nil || e.Code != gws.CodeInvalidInput {
		t.Fatalf("bad domain: %v", e)
	}
	env[EnvGAMDomain] = "example.org"
	delete(env, EnvGAMScopes)
	if _, e := ResolveScope("gam", envOf(env)); e == nil {
		t.Fatal("scopes are required")
	}
	ce := strings.Join(ChildEnv(Scope{Client: "gam", CredentialFile: f}, "/scratch", nil), "\n")
	if !strings.Contains(ce, "OAUTHSERVICEFILE="+f) || !strings.Contains(ce, "GAMCFGDIR=/scratch") {
		t.Errorf("child env: %s", ce)
	}
}

func TestValidateLeastScopeAndSandbox(t *testing.T) {
	cases := []struct {
		name  string
		scope string
		sand  bool
		op    string
		code  gws.Code
	}{
		{"ok read", "User.Read.All", false, "user-list", ""},
		{"missing permission", "Group.Read.All", false, "user-list", gws.CodePermissionDenied},
		{"read with write permission", "User.Read.All,User.ReadWrite.All", true, "user-list", gws.CodePermissionDenied},
		{"mutation on non-test tenant", "User.ReadWrite.All", false, "user-add", gws.CodePermissionDenied},
		{"mutation on test tenant", "User.ReadWrite.All", true, "user-add", ""},
		{"unknown op", "User.Read.All", true, "tenant-delete", gws.CodeUnsupported},
	}
	for _, c := range cases {
		_, e := Request{Scope: m365Scope(t, c.scope, c.sand), Op: c.op}.Validate()
		switch {
		case c.code == "" && e != nil, c.code != "" && (e == nil || e.Code != c.code):
			t.Errorf("%s: got %v", c.name, e)
		}
	}
	big := Request{Scope: m365Scope(t, "User.ReadWrite.All", true), Op: "user-add", Payload: make([]byte, gws.MaxPayloadBytes+1)}
	if _, e := big.Validate(); e == nil || e.Code != gws.CodeOutputLimit {
		t.Errorf("payload bound: %v", e)
	}
	s := m365Scope(t, "User.Read.All", false)
	a, _ := Request{Scope: s, Op: "user-list", Payload: []byte("a")}.Validate()
	b, _ := Request{Scope: s, Op: "user-list", Payload: []byte("a")}.Validate()
	if a.Key != b.Key || !strings.HasPrefix(a.Key, "bashy-") {
		t.Error("derived key must be deterministic")
	}
}

func TestModeAdmission(t *testing.T) {
	refused := map[string][][]string{
		"m365": {{"login"}, {"request", "--url", "x"}, {"cli", "config", "set"}},
		"gam":  {{"csv", "u.csv", "gam", "delete", "user"}, {"loop", "x"}, {"oauth", "create"}, {"print", "filelist"}, {"user", "a@b.c", "print", "messages"}, {"calendar", "x", "info"}},
	}
	for c, list := range refused {
		for _, argv := range list {
			if _, e := AdmitArgs(c, argv); e == nil || e.Code != gws.CodeUnsupported || e.Exit() != 2 {
				t.Errorf("%s %v must be refused unsupported: %v", c, argv, e)
			}
		}
	}
	if _, e := AdmitArgs("gam", []string{"print", "users", "--domain", "x"}); e != nil {
		t.Errorf("gam print users: %v", e)
	}
	if _, e := AdmitArgs("m365", []string{"entra", "user", "list", "-o", "json"}); e != nil {
		t.Errorf("m365 entra user list: %v", e)
	}
	if _, e := AdmitArgs("gam", []string{"print", "filelist"}); e == nil || !strings.Contains(e.Message, "use gws") {
		t.Errorf("overlap refusal must point at gws: %v", e)
	}
}

func TestParseAndPagination(t *testing.T) {
	m := []byte(`[{"id":"1","n":9007199254740993,"x":null},{"id":"2"},{"id":"3"}]`)
	s := m365Scope(t, "User.Read.All", false)
	p, e := ListPage("m365", s, "user-list", "", m, "", 2)
	if e != nil || len(p.Items) != 2 || p.Complete || p.NextToken == "" {
		t.Fatalf("page1: %+v %v", p, e)
	}
	if !strings.Contains(string(p.Items[0]), "9007199254740993") || !strings.Contains(string(p.Items[0]), "null") {
		t.Errorf("precision/null not preserved: %s", p.Items[0])
	}
	p2, e := ListPage("m365", s, "user-list", "", m, p.NextToken, 2)
	if e != nil || !p2.Complete || len(p2.Items) != 1 {
		t.Fatalf("page2: %+v %v", p2, e)
	}
	if _, e := ListPage("m365", s, "group-list", "", m, p.NextToken, 2); e == nil || e.Code != gws.CodeCursorMismatch {
		t.Errorf("cross-op token must mismatch: %v", e)
	}
	if _, e := ListPage("m365", s, "user-list", "", m, p.NextToken+"x", 2); e == nil {
		t.Error("tampered token must fail")
	}
	g, e := ListPage("gam", Scope{Target: "example.org"}, "user-list", "", []byte("primaryEmail,name\na@example.org,A\nb@example.org,\"B, C\"\n"), "", 10)
	if e != nil || !g.Complete || len(g.Items) != 2 {
		t.Fatalf("gam csv: %+v %v", g, e)
	}
	var rec map[string]string
	_ = json.Unmarshal(g.Items[1], &rec)
	if rec["name"] != "B, C" {
		t.Errorf("csv quoting: %v", rec)
	}
	if _, e := ParseRecords("m365", []byte("not json")); e == nil || e.Code != gws.CodeProviderFailure {
		t.Errorf("garbage output: %v", e)
	}
	var buf strings.Builder
	if rc := gws.EmitPage(&buf, "m365 user-list", p); rc != 0 || !strings.Contains(buf.String(), `"schema_version":"loom-v2"`) || !strings.Contains(buf.String(), `"status":"partial"`) {
		t.Errorf("envelope: %s", buf.String())
	}
}

// helper process: a fake CLI that is the test binary itself (no third-party
// binary, no network).
func TestHelperCLI(t *testing.T) {
	mode := os.Getenv("ADMIN_FAKE_MODE")
	if mode == "" {
		return
	}
	switch mode {
	case "ok":
		fmt.Print(`[{"id":"1"}]`)
	case "big":
		b := make([]byte, MaxOutputBytes+10)
		for i := range b {
			b[i] = 'x'
		}
		os.Stdout.Write(b)
	case "fail":
		fmt.Fprint(os.Stderr, "Insufficient privileges for token "+os.Getenv("ADMIN_FAKE_SECRET"))
		os.Exit(1)
	}
	os.Exit(0)
}

func runFake(t *testing.T, mode string, secrets ...string) ([]byte, *gws.Error) {
	t.Helper()
	env := append(os.Environ(), "ADMIN_FAKE_MODE="+mode, "ADMIN_FAKE_SECRET="+canary)
	return Run(context.Background(), os.Args[0], []string{"-test.run=TestHelperCLI"}, env, secrets...)
}

func TestRunBoundsAndRedaction(t *testing.T) {
	out, e := runFake(t, "ok")
	if e != nil || !strings.Contains(string(out), `"id":"1"`) {
		t.Fatalf("ok: %q %v", out, e)
	}
	if _, e := runFake(t, "big"); e == nil || e.Code != gws.CodeOutputLimit {
		t.Errorf("big output: %v", e)
	}
	_, e = runFake(t, "fail", canary)
	if e == nil || e.Code != gws.CodePermissionDenied {
		t.Fatalf("fail: %v", e)
	}
	if strings.Contains(e.Message, canary) {
		t.Errorf("secret leaked in typed error: %s", e.Message)
	}
	if _, e := Run(context.Background(), filepath.Join(t.TempDir(), "missing"), nil, nil); e == nil {
		t.Error("missing binary must fail")
	}
}

// fakeDirectory is an in-memory tenant: create then reverse leaves it empty.
type fakeDirectory struct{ users map[string]bool }

func (d *fakeDirectory) apply(op, id string) {
	switch op {
	case "user-add", "user-create":
		d.users[id] = true
	case "user-remove", "user-delete":
		delete(d.users, id)
	}
}

func TestReversibleRoundTripWithLedger(t *testing.T) {
	dir := &fakeDirectory{users: map[string]bool{}}
	var led Ledger
	s := m365Scope(t, "User.ReadWrite.All", true)
	do := func(op, id string) *gws.Error {
		r := Request{Scope: s, Op: op, Payload: []byte(id)}
		plan, e := r.Validate()
		if e != nil {
			return e
		}
		if _, replay := led.Begin(plan.Key); replay {
			return gws.NewError(gws.CodePartialMutation, "replayed key — not applying again", nil)
		}
		dir.apply(op, id)
		led.Finish(plan.Key, "confirmed")
		return nil
	}
	if e := do("user-add", "u1"); e != nil || !dir.users["u1"] {
		t.Fatal(e)
	}
	if e := do("user-add", "u1"); e == nil || e.Code != gws.CodePartialMutation {
		t.Errorf("replay must be refused: %v", e)
	}
	rev, _ := LookupOp("m365", "user-add")
	if e := do(rev.Reverse, "u1"); e != nil || len(dir.users) != 0 {
		t.Fatalf("cleanup failed: %v %v", e, dir.users)
	}
	led.Finish("k", "failed")
}

func TestUnsupportedAdminDocumented(t *testing.T) {
	for k, v := range UnsupportedAdmin {
		if v == "" || !strings.Contains(k, ":") {
			t.Errorf("bad row %q", k)
		}
	}
	for _, k := range []string{"gam:mobile", "gam:chromeos"} {
		if UnsupportedAdmin[k] == "" {
			t.Errorf("%s must be documented unsupported", k)
		}
	}
}
