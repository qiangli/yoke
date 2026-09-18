package craft

import (
	"testing"
)

// THE MOTIVATING CASE. One ssh invocation teaches the host's port and login,
// bound to the HOST rather than to ssh — which is what lets any other command
// use them.
func TestExtract_SSH(t *testing.T) {
	x, ok := Extract([]string{"ssh", "-p", "2222", "-l", "xuser", "remote-host"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Kind != EntityHost || x.Entity.Name != "remote-host" {
		t.Errorf("entity = %+v, want host:remote-host", x.Entity)
	}
	if x.Roles[RolePort] != "2222" || x.Roles[RoleUser] != "xuser" {
		t.Errorf("roles = %+v", x.Roles)
	}
	if x.Realm != RealmSSH {
		t.Errorf("realm = %s", x.Realm)
	}
}

// THE PAYOFF. The port learned from ssh renders as scp's -P, not ssh's -p. This
// difference is forgotten constantly, and it is the clearest thing the role
// table buys.
func TestRenderRole_SSHPortBecomesScpCapitalP(t *testing.T) {
	got, ok := RenderRole("scp", RolePort, "2222")
	if !ok {
		t.Fatal("scp cannot render a port")
	}
	if got != "-P 2222" {
		t.Errorf("scp port = %q, want %q — scp uses CAPITAL P", got, "-P 2222")
	}
	if got, _ := RenderRole("ssh", RolePort, "2222"); got != "-p 2222" {
		t.Errorf("ssh port = %q, want lowercase", got)
	}
}

// THE PRIOR THAT PREVENTS A CONFIDENT WRONG ANSWER. An ssh login and a psql
// role share a word and nothing else; suggesting one for the other would be
// wrong before any evidence exists to correct it.
func TestTransfers_RealmBoundary(t *testing.T) {
	if !Transfers(RoleUser, "ssh", "scp") {
		t.Error("user should transfer within the ssh realm")
	}
	if !Transfers(RolePort, "ssh", "sftp") {
		t.Error("port should transfer within the ssh realm")
	}
	if Transfers(RoleUser, "ssh", "psql") {
		t.Error("an ssh login must NOT transfer to a psql role — different namespaces")
	}
	if Transfers(RoleUser, "psql", "mysql") {
		t.Error("database realms are distinct from each other too")
	}
	if Transfers(RoleUser, "ssh", "unknown-binary") {
		t.Error("an unknown command has no declared semantics; nothing transfers to it")
	}
}

// A role the target cannot express returns false rather than a plausible guess.
func TestRenderRole_RefusesWhatItCannotExpress(t *testing.T) {
	if _, ok := RenderRole("rsync", RolePort, "2222"); ok {
		t.Error("rsync carries the port inside its transport string; it must not claim a flag")
	}
	if _, ok := RenderRole("scp", RoleDatabase, "x"); ok {
		t.Error("scp has no database concept")
	}
	if _, ok := RenderRole("ssh", RolePort, ""); ok {
		t.Error("an empty value rendered a flag")
	}
}

// SECRETS ARE REFUSED AT EXTRACTION. argv is exactly where a password appears,
// and filtering later would mean it was in the pipeline first.
func TestExtract_RefusesSecrets(t *testing.T) {
	t.Run("attached", func(t *testing.T) {
		// `mysql -pSECRET` is ONE word; a next-word deny-list never sees it.
		x, _ := Extract([]string{"mysql", "-h", "db1", "-u", "admin", "-phunter2"})
		if x.Redacted == 0 {
			t.Error("an attached secret was not refused")
		}
		for role, v := range x.Roles {
			if v == "hunter2" || v == "-phunter2" {
				t.Fatalf("a password was captured as %s", role)
			}
		}
	})

	t.Run("next word", func(t *testing.T) {
		x, _ := Extract([]string{"redis-cli", "-h", "cache-host", "-a", "hunter2"})
		if x.Redacted == 0 {
			t.Error("a credential flag's value was not refused")
		}
		for _, v := range x.Roles {
			if v == "hunter2" {
				t.Fatal("a credential was captured")
			}
		}
	})
}

// Without a bool-flag table, `ssh -v host` would record the host as the value
// of -v and then find no entity at all.
func TestExtract_BoolFlagsDoNotEatTheNextWord(t *testing.T) {
	x, ok := Extract([]string{"ssh", "-v", "-4", "-p", "2222", "remote-host"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Name != "remote-host" {
		t.Errorf("entity = %q; a boolean flag consumed the host", x.Entity.Name)
	}
	if x.Roles[RolePort] != "2222" {
		t.Errorf("roles = %+v", x.Roles)
	}
}

func TestExtract_TargetForms(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		wantHost   string
		wantUser   string
		wantNoFact bool
	}{
		{name: "user@host", argv: []string{"ssh", "xuser@remote-host"}, wantHost: "remote-host", wantUser: "xuser"},
		{name: "scp host:path", argv: []string{"scp", "-P", "2222", "file.txt", "remote-host:/tmp/"}, wantHost: "remote-host"},
		{name: "scp user@host:path", argv: []string{"scp", "-P", "22", "f", "xuser@remote-host:/tmp"}, wantHost: "remote-host", wantUser: "xuser"},
		{name: "local path only", argv: []string{"scp", "-P", "22", "/a/b", "/c/d"}, wantNoFact: true},
		{name: "unknown binary", argv: []string{"frobnicate", "-p", "1"}, wantNoFact: true},
		{name: "empty", argv: nil, wantNoFact: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, ok := Extract(tc.argv)
			if tc.wantNoFact {
				if ok {
					t.Errorf("expected nothing usable, got %+v", x)
				}
				return
			}
			if !ok {
				t.Fatalf("extraction failed: %+v", x)
			}
			if x.Entity.Name != tc.wantHost {
				t.Errorf("host = %q, want %q", x.Entity.Name, tc.wantHost)
			}
			if tc.wantUser != "" && x.Roles[RoleUser] != tc.wantUser {
				t.Errorf("user = %q, want %q", x.Roles[RoleUser], tc.wantUser)
			}
		})
	}
}

// An explicit flag beats the inline form, because it is the more specific
// statement of intent.
func TestExtract_FlagUserBeatsInlineUser(t *testing.T) {
	x, _ := Extract([]string{"ssh", "-l", "flaguser", "inline@remote-host"})
	if x.Roles[RoleUser] != "flaguser" {
		t.Errorf("user = %q, want the -l value", x.Roles[RoleUser])
	}
}

// An unknown binary yields nothing rather than a guess: inventing roles from
// flag shapes is how `-p` becomes "port" for a command where it means
// "preserve".
func TestExtract_UnknownBinaryYieldsNothing(t *testing.T) {
	if _, ok := Extract([]string{"cp", "-p", "a", "b"}); ok {
		t.Error("cp -p (preserve) was interpreted as a port")
	}
}

// The facts are keyed by ROLE, not by one command's flag spelling — which is
// the difference between knowledge that moves and knowledge that does not.
func TestFactsFrom_KeyedByRole(t *testing.T) {
	x, _ := Extract([]string{"ssh", "-p", "2222", "-l", "xuser", "remote-host"})
	facts := FactsFrom(x, "ssh")

	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2: %+v", len(facts), facts)
	}
	byKey := map[string]string{}
	for _, f := range facts {
		if f.Entity.Name != "remote-host" {
			t.Errorf("fact not bound to the host: %+v", f)
		}
		byKey[f.Key] = f.Value
	}
	if byKey["port"] != "2222" || byKey["user"] != "xuser" {
		t.Errorf("facts = %+v; keys should be roles, not flags", byKey)
	}
	if _, isFlag := byKey["-p"]; isFlag {
		t.Error("a fact was keyed by a flag; it would not transfer")
	}
}

func TestFactsFrom_NoEntityYieldsNothing(t *testing.T) {
	if got := FactsFrom(Extraction{Roles: map[Role]string{RolePort: "22"}}, "ssh"); len(got) != 0 {
		t.Errorf("facts with nothing to bind to: %+v", got)
	}
}

// END TO END: learn from ssh, suggest for scp — the transfer this file exists
// for.
func TestTransfer_SSHToSCP(t *testing.T) {
	// Agent A connects with ssh.
	x, ok := Extract([]string{"ssh", "-p", "2222", "-l", "xuser", "remote-host"})
	if !ok {
		t.Fatal("extraction failed")
	}
	dir := t.TempDir()
	store := OpenFacts(dir)
	for _, f := range FactsFrom(x, "ssh") {
		if err := store.Record(f); err != nil {
			t.Fatal(err)
		}
	}

	// Agent B reaches for scp against the same host and needs the flags.
	var suggestions []string
	for _, f := range store.For(Entity{Kind: EntityHost, Name: "remote-host"}) {
		role := Role(f.Key)
		if !Transfers(role, "ssh", "scp") {
			continue
		}
		if s, ok := RenderRole("scp", role, f.Value); ok {
			suggestions = append(suggestions, s)
		}
	}

	// The port transfers and renders with scp's spelling. The login does NOT
	// come back as a flag, because scp expresses it as user@host rather than
	// -l — and suggesting `-l` for scp would be wrong.
	found := false
	for _, s := range suggestions {
		if s == "-P 2222" {
			found = true
		}
		if s == "-l xuser" {
			t.Error("suggested `-l` for scp, which does not accept it")
		}
	}
	if !found {
		t.Errorf("suggestions = %v, want the port rendered as -P 2222", suggestions)
	}
}
