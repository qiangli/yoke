package craft

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSSHConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// THE POINT OF READING DECLARED CONFIG. One file fills the store with hosts
// this machine has never contacted — knowledge the watch-and-learn path could
// only acquire by someone first connecting successfully.
func TestParseSSHConfig_Declared(t *testing.T) {
	path := writeSSHConfig(t, `
# a comment
Host dev-box
    HostName 10.0.0.41
    Port 2222
    User svc-build
    IdentityFile ~/.ssh/id_dev

Host bastion
    HostName gw.example.test
    User jump-acct

Host inner
    HostName 10.0.0.9
    ProxyJump bastion
`)

	entries := ParseSSHConfig(path)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}

	byAlias := map[string]SSHConfigEntry{}
	for _, e := range entries {
		byAlias[e.Alias] = e
	}

	dev := byAlias["dev-box"]
	if dev.HostName != "10.0.0.41" {
		t.Errorf("HostName = %q", dev.HostName)
	}
	if dev.Roles[RolePort] != "2222" || dev.Roles[RoleUser] != "svc-build" {
		t.Errorf("roles = %+v", dev.Roles)
	}
	if dev.Roles[RoleIdentity] != "~/.ssh/id_dev" {
		t.Errorf("identity = %q", dev.Roles[RoleIdentity])
	}
	if byAlias["inner"].Roles[RoleJump] != "bastion" {
		t.Errorf("ProxyJump = %q", byAlias["inner"].Roles[RoleJump])
	}
}

// Facts bind to the ALIAS, because the alias is what a person types and
// therefore what a later command will name. The resolved address is recorded as
// a fact ABOUT the alias, so both questions stay answerable.
func TestSSHConfigFacts_BindToAlias(t *testing.T) {
	entries := []SSHConfigEntry{{
		Alias:    "dev-box",
		HostName: "10.0.0.41",
		Roles:    map[Role]string{RolePort: "2222", RoleUser: "svc-build"},
	}}
	facts := SSHConfigFacts(entries, "ssh-config")

	got := map[string]string{}
	for _, f := range facts {
		if f.Entity.Name != "dev-box" {
			t.Errorf("fact bound to %q, want the alias dev-box", f.Entity.Name)
		}
		if f.Source != "ssh-config" {
			t.Errorf("source = %q — provenance is how a bad fact is diagnosed", f.Source)
		}
		got[f.Key] = f.Value
	}
	if got["hostname"] != "10.0.0.41" {
		t.Errorf("hostname fact = %q", got["hostname"])
	}
	if got[string(RolePort)] != "2222" || got[string(RoleUser)] != "svc-build" {
		t.Errorf("facts = %+v", got)
	}
}

// A wildcard block declares defaults for EVERYTHING rather than facts about
// anything. Recording it against a literal entity named "*" would invent a
// machine — and then every suggestion for every host would cite it.
func TestParseSSHConfig_WildcardsDeclareNoHost(t *testing.T) {
	path := writeSSHConfig(t, `
Host *
    User default-acct
    ServerAliveInterval 60

Host *.internal
    Port 2222

Host real-box
    HostName 10.0.0.7
`)
	entries := ParseSSHConfig(path)
	if len(entries) != 1 || entries[0].Alias != "real-box" {
		t.Fatalf("got %+v, want only real-box", entries)
	}
}

// `Host a b c` is one block for several names. Recording the same facts under
// each would inflate the entity count with duplicates of one machine.
func TestParseSSHConfig_MultiNameBlockTakesOne(t *testing.T) {
	path := writeSSHConfig(t, "Host alpha beta\n    Port 2222\n")
	entries := ParseSSHConfig(path)
	if len(entries) != 1 || entries[0].Alias != "alpha" {
		t.Fatalf("got %+v", entries)
	}
}

// Include can escape the ssh directory, and a system-wide file's declarations
// are not this operator's choices.
func TestParseSSHConfig_IncludeNotFollowed(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("Host smuggled\n    Port 9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "config")
	if err := os.WriteFile(main, []byte("Include "+other+"\nHost real\n    Port 22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, e := range ParseSSHConfig(main) {
		if e.Alias == "smuggled" {
			t.Fatal("an Include was followed; declarations from another file are not this config's")
		}
	}
}

// ssh takes the FIRST value for a keyword. Taking the last would report a
// different port than ssh itself would use — a fact that is confidently wrong
// rather than merely missing.
func TestParseSSHConfig_FirstValueWins(t *testing.T) {
	path := writeSSHConfig(t, "Host box\n    Port 2222\n    Port 3333\n")
	entries := ParseSSHConfig(path)
	if len(entries) != 1 || entries[0].Roles[RolePort] != "2222" {
		t.Fatalf("got %+v, want the first Port", entries)
	}
}

func TestParseSSHConfig_EqualsFormAndCase(t *testing.T) {
	path := writeSSHConfig(t, "host box\n  hostname=10.0.0.3\n  PORT=2222\n")
	entries := ParseSSHConfig(path)
	if len(entries) != 1 {
		t.Fatalf("got %+v", entries)
	}
	if entries[0].HostName != "10.0.0.3" || entries[0].Roles[RolePort] != "2222" {
		t.Errorf("got %+v — ssh_config keywords are case-insensitive and accept Key=Value", entries[0])
	}
}

// A machine with no ssh config has declared nothing. That is a clean state, not
// a failure, and must not read as one.
func TestParseSSHConfig_MissingFileIsQuiet(t *testing.T) {
	if got := ParseSSHConfig(filepath.Join(t.TempDir(), "absent")); got != nil {
		t.Errorf("got %+v, want nothing", got)
	}
}
