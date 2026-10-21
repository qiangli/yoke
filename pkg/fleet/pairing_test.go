package fleet

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pairedHost builds a hermetic host: a temp HOME/XDG config, an outpost stub
// on PATH that prints the PAIRING token, and an agent.json naming server.
// An empty server writes no agent.json at all.
func pairedHost(t *testing.T, server string) (cfg string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	home := t.TempDir()
	cfg = filepath.Join(home, ".config")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("BASHY_CLOUDBOX_URL", "")

	bindir := t.TempDir()
	stub := "#!/bin/sh\n[ \"$1\" = token ] && [ \"$2\" = print ] || exit 9\necho pairing-token\n"
	if err := os.WriteFile(filepath.Join(bindir, "outpost"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir)

	if server != "" {
		dir := filepath.Join(cfg, "outpost")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"agent_name":"h","server_addr":"` + server + `","server_port":443,"access_token":"pairing-token"}`
		if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func writeMaster(t *testing.T, cfg, body string) {
	t.Helper()
	dir := filepath.Join(cfg, "bashy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "master-token"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// (a) Still paired to the coordination base: the pairing token, as before.
func TestResolveCloudPairedToBaseUsesPairingToken(t *testing.T) {
	cfg := pairedHost(t, "ai.dhnt.io")
	writeMaster(t, cfg, "master-token\n") // present but not needed
	base, tok := ResolveCloud("", "")
	if base != "https://ai.dhnt.io" || tok != "pairing-token" {
		t.Fatalf("got %q %q, want the pairing token against the base", base, tok)
	}
}

// No agent.json at all: the pairing target is unknown, so today's behavior holds.
func TestResolveCloudUnknownPairingKeepsLegacyLadder(t *testing.T) {
	pairedHost(t, "")
	if _, tok := ResolveCloud("", ""); tok != "pairing-token" {
		t.Fatalf("tok = %q", tok)
	}
}

// The legacy matrix path counts as the pairing record too.
func TestResolveCloudReadsLegacyMatrixAgentJSON(t *testing.T) {
	cfg := pairedHost(t, "")
	dir := filepath.Join(cfg, "matrix")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"server_addr":"sat.example"}`), 0o600)
	if _, tok := ResolveCloud("", ""); tok != "" {
		t.Fatalf("tok = %q — a satellite pairing token went to the base", tok)
	}
}

// (b) Migrated to a satellite with the master credential saved: master token.
func TestResolveCloudSatelliteUsesMasterToken(t *testing.T) {
	cfg := pairedHost(t, "sat1.dhnt.io")
	writeMaster(t, cfg, "master-token\nhttps://ai.dhnt.io\n")
	if _, tok := ResolveCloud("", ""); tok != "master-token" {
		t.Fatalf("tok = %q, want master-token", tok)
	}
	// A bare one-line file is also accepted.
	writeMaster(t, cfg, "master-token\n")
	if _, tok := ResolveCloud("", ""); tok != "master-token" {
		t.Fatalf("tok = %q, want master-token", tok)
	}
}

// A master credential labeled for a DIFFERENT base is not sent.
func TestResolveCloudMasterTokenForOtherBaseIsIgnored(t *testing.T) {
	cfg := pairedHost(t, "sat1.dhnt.io")
	writeMaster(t, cfg, "master-token\nhttps://other.example\n")
	if _, tok := ResolveCloud("", ""); tok != "" {
		t.Fatalf("tok = %q, want empty", tok)
	}
}

// (c) Satellite, no master credential: empty — never the satellite's token.
func TestResolveCloudSatelliteWithoutMasterIsEmpty(t *testing.T) {
	pairedHost(t, "sat1.dhnt.io:443")
	if _, tok := ResolveCloud("", ""); tok != "" {
		t.Fatalf("tok = %q — a satellite pairing token went to the base", tok)
	}
}

// When the coordination base IS the satellite, its pairing token is right.
func TestResolveCloudBaseOverrideMatchingSatellite(t *testing.T) {
	pairedHost(t, "sat1.dhnt.io")
	t.Setenv("BASHY_CLOUDBOX_URL", "https://SAT1.dhnt.io/")
	if _, tok := ResolveCloud("", ""); tok != "pairing-token" {
		t.Fatalf("tok = %q", tok)
	}
}

// (d) Override and env still win over everything.
func TestResolveCloudEnvAndOverrideStillWin(t *testing.T) {
	cfg := pairedHost(t, "sat1.dhnt.io")
	writeMaster(t, cfg, "master-token\n")
	t.Setenv("BASHY_API_KEY", "api-key")
	if _, tok := ResolveCloud("", ""); tok != "api-key" {
		t.Fatalf("tok = %q", tok)
	}
	t.Setenv("BASHY_FLEET_TOKEN", "fleet-tok")
	if _, tok := ResolveCloud("", ""); tok != "fleet-tok" {
		t.Fatalf("tok = %q", tok)
	}
	if _, tok := ResolveCloud("", "flag-tok"); tok != "flag-tok" {
		t.Fatalf("tok = %q", tok)
	}
}

func TestSaveMasterTokenFromAgentJSON(t *testing.T) {
	cfg := pairedHost(t, "sat1.dhnt.io")
	src := filepath.Join(t.TempDir(), "agent.json.pre-migration")
	os.WriteFile(src, []byte(`{"server_addr":"ai.dhnt.io","access_token":"old-master"}`), 0o600)

	path, err := SaveMasterToken(src)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(cfg, "bashy", "master-token") {
		t.Fatalf("path = %q", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "old-master\nhttps://ai.dhnt.io\n" {
		t.Fatalf("file = %q", data)
	}
	if _, tok := ResolveCloud("", ""); tok != "old-master" {
		t.Fatalf("tok = %q", tok)
	}

	// No access_token: refuse, and the message never echoes file content.
	os.WriteFile(src, []byte(`{"server_addr":"ai.dhnt.io"}`), 0o600)
	if _, err := SaveMasterToken(src); err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("err = %v", err)
	}
}

func TestMasterTokenCmdNeverPrintsToken(t *testing.T) {
	pairedHost(t, "sat1.dhnt.io")
	src := filepath.Join(t.TempDir(), "agent.json")
	os.WriteFile(src, []byte(`{"server_addr":"ai.dhnt.io","access_token":"s3cret-xyz"}`), 0o600)
	cmd := NewMasterTokenCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"save", "--from", src})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "s3cret") {
		t.Fatalf("the token was printed: %q", out.String())
	}
}
