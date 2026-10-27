package loom

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationsAllowedDomains(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"unset", "", ""},
		{"blank", "   ", ""},
		{"one host", "replica.local", "replica.local,github.com,*.github.com"},
		// An operator reads host:port off a replica URL; Gitea matches the
		// hostname only, so the port has to come off or the pattern is dead.
		{"port stripped", "replica.local:33400", "replica.local,github.com,*.github.com"},
		{"loopback with port", "127.0.0.1:33400", "127.0.0.1,github.com,*.github.com"},
		{"ipv6 literal with port", "[::1]:33400", "::1,github.com,*.github.com"},
		{"bare ipv6 literal", "::1", "::1,github.com,*.github.com"},
		{"several, spaced", " a.local:1 , 10.0.0.4 ,, b.local ", "a.local,10.0.0.4,b.local,github.com,*.github.com"},
		{"github already named", "github.com,replica.local", "github.com,replica.local,*.github.com"},
		{"duplicates collapse", "r.local,R.LOCAL:9,r.local", "r.local,github.com,*.github.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := migrationsAllowedDomains(c.raw); got != c.want {
				t.Fatalf("migrationsAllowedDomains(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestEnsureConfig_MirrorAllowListUnsetWritesNothing: opting in loosens an SSRF
// guard, so a loom that did not ask keeps Gitea's defaults — no [migrations]
// section at all, seeded or converged.
func TestEnsureConfig_MirrorAllowListUnsetWritesNothing(t *testing.T) {
	t.Setenv(MirrorAllowedDomainsEnv, "")
	dir := t.TempDir()
	cfg, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	if s := string(b); strings.Contains(s, "[migrations]") || strings.Contains(s, "ALLOW_LOCALNETWORKS") {
		t.Fatalf("unset env still wrote the mirror allow-list:\n%s", s)
	}
}

func TestEnsureConfig_SeedsMirrorAllowList(t *testing.T) {
	t.Setenv(MirrorAllowedDomainsEnv, "replica.local:33400")
	dir := t.TempDir()
	cfg, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	s := string(b)
	if got := sectionValue(s, "migrations", "ALLOW_LOCALNETWORKS"); got != "true" {
		t.Fatalf("ALLOW_LOCALNETWORKS = %q, want true:\n%s", got, s)
	}
	// github.com must ride along: a non-empty ALLOWED_DOMAINS drops Gitea's
	// built-in "external hosts are allowed" rule, which would strand S515.5's
	// GitHub push mirror.
	if got := sectionValue(s, "migrations", "ALLOWED_DOMAINS"); got != "replica.local,github.com,*.github.com" {
		t.Fatalf("ALLOWED_DOMAINS = %q:\n%s", got, s)
	}
}

// TestEnsureConfig_ConvergesMirrorAllowList: an app.ini written before this
// story (or before the operator opted in) has to pick the keys up on the next
// start, and a changed replica list has to be rewritten in place — the round-4
// gap was that the only way to get them there was hand-editing app.ini.
func TestEnsureConfig_ConvergesMirrorAllowList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MirrorAllowedDomainsEnv, "")
	cfg, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cfg)
	if strings.Contains(string(before), "[migrations]") {
		t.Fatal("precondition: seeded config should carry no [migrations]")
	}

	t.Setenv(MirrorAllowedDomainsEnv, "a.local:1,b.local")
	if _, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	s := string(b)
	if got := sectionValue(s, "migrations", "ALLOWED_DOMAINS"); got != "a.local,b.local,github.com,*.github.com" {
		t.Fatalf("converged ALLOWED_DOMAINS = %q:\n%s", got, s)
	}
	if got := sectionValue(s, "migrations", "ALLOW_LOCALNETWORKS"); got != "true" {
		t.Fatalf("converged ALLOW_LOCALNETWORKS = %q:\n%s", got, s)
	}
	// Converging must not cost the rest of the file.
	if !strings.Contains(s, "INSTALL_LOCK = true") || !strings.Contains(s, "DB_TYPE = sqlite3") {
		t.Fatalf("converge damaged the config:\n%s", s)
	}
	if strings.Count(s, "[migrations]") != 1 {
		t.Fatalf("duplicate [migrations] section:\n%s", s)
	}

	// A narrowed list is rewritten in place, not appended to.
	t.Setenv(MirrorAllowedDomainsEnv, "b.local")
	if _, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(cfg)
	s = string(b)
	if got := sectionValue(s, "migrations", "ALLOWED_DOMAINS"); got != "b.local,github.com,*.github.com" {
		t.Fatalf("rewritten ALLOWED_DOMAINS = %q:\n%s", got, s)
	}
	if strings.Count(s, "ALLOWED_DOMAINS") != 1 {
		t.Fatalf("ALLOWED_DOMAINS duplicated:\n%s", s)
	}

	// Opting back out leaves what is there: ensureConfig converges app.ini
	// forward, it does not police a key the owner may have set by hand.
	t.Setenv(MirrorAllowedDomainsEnv, "")
	if _, err := ensureConfig(dir, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(cfg)
	if got := sectionValue(string(b), "migrations", "ALLOWED_DOMAINS"); got != "b.local,github.com,*.github.com" {
		t.Fatalf("unsetting the env dropped the keys: %q\n%s", got, string(b))
	}
}

// TestReplicaAdd_LocalNetworkHint: Gitea reports "this replica is a private
// address" as 401 Permission denied, which sends an operator after tokens. The
// error must name the env var instead.
func TestReplicaAdd_LocalNetworkHint(t *testing.T) {
	prim, ps := newFakeGitea("acme/app")
	defer ps.Close()
	prim.denyMirrorCreate = true
	_, rs := newFakeGitea()
	defer rs.Close()

	_, err := ReplicaAdd(context.Background(), ReplicaOptions{
		Primary: ps.URL, Replica: rs.URL, Token: "t", ReplicaToken: "r",
	})
	if err == nil {
		t.Fatal("expected the 401 to fail replica add")
	}
	msg := err.Error()
	for _, want := range []string{
		"401",
		"Permission denied",
		MirrorAllowedDomainsEnv,
		"ALLOW_LOCALNETWORKS",
		"not about credentials",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q:\n%s", want, msg)
		}
	}
	// The hint has to be actionable: it names the replica host to allow.
	host := strings.TrimPrefix(rs.URL, "http://")
	if !strings.Contains(msg, MirrorAllowedDomainsEnv+"="+host) {
		t.Fatalf("hint does not name the replica host %q:\n%s", host, msg)
	}
}

// A bad token is a real 401 too; the hint is printed either way, so it must say
// which to check first rather than asserting the cause.
func TestLocalNetworkHintNamesCredentialsFirst(t *testing.T) {
	h := localNetworkHint("http://10.1.2.3:33400/acme/app.git")
	if !strings.Contains(h, "10.1.2.3:33400") {
		t.Fatalf("hint lost the host:\n%s", h)
	}
	if !strings.Contains(h, "token is good") {
		t.Fatalf("hint does not rule the token out first:\n%s", h)
	}
}

func TestEnsureMigrationsSectionEmptyDomainsIsNoop(t *testing.T) {
	in := "APP_NAME = loom\n"
	if got := ensureMigrationsSection(in, ""); got != in {
		t.Fatalf("empty domains mutated the ini: %q", got)
	}
}

// The seeded app.ini and a converged one must agree — a divergence here is how
// "works on a fresh data dir, not on mine" starts.
func TestSeedAndConvergeAgreeOnMirrorAllowList(t *testing.T) {
	t.Setenv(MirrorAllowedDomainsEnv, "r.local:33400")
	seeded := t.TempDir()
	cfgA, err := ensureConfig(seeded, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(cfgA)

	converged := t.TempDir()
	t.Setenv(MirrorAllowedDomainsEnv, "")
	if _, err := ensureConfig(converged, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv(MirrorAllowedDomainsEnv, "r.local:33400")
	if _, err := ensureConfig(converged, "127.0.0.1", 3000, "http://127.0.0.1:3000/", false, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(converged, "app.ini"))

	for _, key := range []string{"ALLOW_LOCALNETWORKS", "ALLOWED_DOMAINS"} {
		if x, y := sectionValue(string(a), "migrations", key), sectionValue(string(b), "migrations", key); x != y {
			t.Fatalf("%s: seeded %q != converged %q", key, x, y)
		}
	}
}

// TestReplicaAddCmd_HintIsNotBuriedInUsage: round 4 measured the hint live and
// saw cobra's usage block instead — a runtime refusal from the forge is not a
// usage error, and the usage text is long enough to scroll the explanation off
// the screen.
func TestReplicaAddCmd_HintIsNotBuriedInUsage(t *testing.T) {
	prim, ps := newFakeGitea("acme/app")
	defer ps.Close()
	prim.denyMirrorCreate = true
	_, rs := newFakeGitea()
	defer rs.Close()

	cmd := newReplicaCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"add", rs.URL, "--primary", ps.URL, "--token", "t", "--replica-token", "r"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected the 401 to fail `replica add`")
	}
	if s := out.String(); strings.Contains(s, "Usage:") {
		t.Fatalf("usage block printed for a runtime error:\n%s", s)
	}
}
