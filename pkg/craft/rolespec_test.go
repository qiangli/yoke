package craft

import (
	"testing"
)

// THE CROSS-COMMAND PAYOFF, in its widest form. A clone over ssh:// teaches the
// same port and login `ssh` needs — so the fact enters through git and leaves
// through ssh, which is the whole reason roles exist rather than argv shapes.
func TestExtract_GitOverSSH(t *testing.T) {
	x, ok := Extract([]string{"git", "clone", "ssh://xuser@remote-host:2222/srv/repo.git"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Name != "remote-host" {
		t.Errorf("entity = %q, want remote-host", x.Entity.Name)
	}
	if x.Roles[RolePort] != "2222" {
		t.Errorf("port = %q, want 2222 — the URL colon introduces a PORT, not a path", x.Roles[RolePort])
	}
	if x.Roles[RoleUser] != "xuser" {
		t.Errorf("user = %q, want xuser", x.Roles[RoleUser])
	}
	if x.Realm != RealmSSH {
		t.Errorf("realm = %s, want ssh — git over ssh shares ssh's credential world", x.Realm)
	}
	// And the fact it contributed renders for a command that CAN express it.
	if got, ok := RenderRole("scp", RolePort, x.Roles[RolePort]); !ok || got != "-P 2222" {
		t.Errorf("scp render = %q ok=%v", got, ok)
	}
}

// The scp-form git URL, which has no scheme but does have a login.
func TestExtract_GitSCPForm(t *testing.T) {
	x, ok := Extract([]string{"git", "clone", "xuser@remote-host:repos/thing.git"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Name != "remote-host" || x.Roles[RoleUser] != "xuser" {
		t.Errorf("got entity=%q roles=%+v", x.Entity.Name, x.Roles)
	}
	if _, hasPort := x.Roles[RolePort]; hasPort {
		t.Error("the scp-form colon introduces a PATH; no port may be invented from it")
	}
}

// THE TRAP THAT MAKES git DIFFERENT FROM scp. A colon reliably separates host
// from path for scp; for git it separates refspecs, commit messages, and
// anything else. Every one of these would parse as `host:path` perfectly well,
// and each would write a machine that does not exist.
func TestExtract_GitColonsThatAreNotHosts(t *testing.T) {
	for _, argv := range [][]string{
		{"git", "push", "origin", "HEAD:main"},
		{"git", "push", "origin", "refs/heads/x:refs/heads/y"},
		{"git", "commit", "-m", "fix: a thing"},
		{"git", "clone", "https://example.test/repo.git"}, // different realm
		{"git", "log", "--oneline"},
		{"git", "fetch", "origin"},
	} {
		if x, ok := Extract(argv); ok {
			t.Errorf("%v extracted %+v; expected nothing", argv, x)
		}
	}
}

// https is a token, ssh is a key. One command spanning two credential worlds
// must not fold them into one realm — so the scheme restriction is what lets
// git declare a single honest realm.
func TestExtract_GitHTTPSIsRefused(t *testing.T) {
	if x, ok := Extract([]string{"git", "clone", "https://xuser@example.test/repo.git"}); ok {
		t.Errorf("https clone extracted %+v; its credentials are not ssh's", x)
	}
}

// git contributes facts and receives none, and says so rather than inventing a
// suggestion nobody asked for.
func TestRenderRole_GitReceivesNothing(t *testing.T) {
	if got, ok := RenderRole("git", RolePort, "2222"); ok {
		t.Errorf("git rendered a port as %q; it has no flag that expresses one", got)
	}
	if !Transfers(RolePort, "ssh", "git") {
		t.Error("the fact still TRANSFERS semantically — only rendering is one-way")
	}
}

// The ssh realm is the one that pays, so every member of it must actually be in
// it. A command silently landing in the wrong realm is invisible: it simply
// stops sharing facts.
func TestSpecs_SSHRealmMembership(t *testing.T) {
	for _, name := range []string{"ssh", "scp", "sftp", "rsync", "sshfs", "ssh-copy-id", "ssh-keyscan", "git"} {
		spec, ok := SpecFor(name)
		if !ok {
			t.Fatalf("%s has no spec", name)
		}
		if spec.Realm != RealmSSH {
			t.Errorf("%s realm = %s, want ssh", name, spec.Realm)
		}
	}
}

func TestExtract_SSHFamilyExtras(t *testing.T) {
	cases := []struct {
		argv []string
		host string
		port string
		user string
	}{
		{[]string{"ssh-copy-id", "-p", "2222", "-i", "/k/id_ed25519", "xuser@remote-host"}, "remote-host", "2222", "xuser"},
		{[]string{"ssh-keyscan", "-p", "2222", "remote-host"}, "remote-host", "2222", ""},
		{[]string{"sshfs", "-p", "2222", "xuser@remote-host:/srv", "/mnt/srv"}, "remote-host", "2222", "xuser"},
	}
	for _, c := range cases {
		x, ok := Extract(c.argv)
		if !ok {
			t.Errorf("%v: extraction failed (%+v)", c.argv, x)
			continue
		}
		if x.Entity.Name != c.host || x.Roles[RolePort] != c.port || x.Roles[RoleUser] != c.user {
			t.Errorf("%v: got host=%q roles=%+v", c.argv, x.Entity.Name, x.Roles)
		}
	}
}

// ssh-keyscan's -H hashes output; it is not a header flag. Read as one it would
// swallow the host operand and the invocation would teach nothing — a silent
// loss, which is the failure mode worth a test.
func TestExtract_KeyscanDashHIsBoolean(t *testing.T) {
	x, ok := Extract([]string{"ssh-keyscan", "-H", "-p", "2222", "remote-host"})
	if !ok || x.Entity.Name != "remote-host" {
		t.Errorf("got ok=%v entity=%q roles=%+v", ok, x.Entity.Name, x.Roles)
	}
}

// A value-taking flag that is NOT a role still has to be declared, or its value
// looks like an operand.
func TestExtract_ValueFlagsConsumeTheirValue(t *testing.T) {
	x, ok := Extract([]string{"sshfs", "-o", "reconnect", "-p", "2222", "xuser@remote-host:/srv", "/mnt"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Name != "remote-host" {
		t.Errorf("entity = %q — `-o reconnect` must not shadow the target", x.Entity.Name)
	}
	// git's global options appear BEFORE the subcommand, so consuming them
	// wrongly makes the subcommand gate reject a legitimate clone.
	g, ok := Extract([]string{"git", "-C", "/work/repo", "clone", "ssh://remote-host:2222/r.git"})
	if !ok || g.Entity.Name != "remote-host" {
		t.Errorf("git -C: ok=%v entity=%q", ok, g.Entity.Name)
	}
}

// A PASSWORD IN argv IS THE HAZARD. The deny-list refuses the value at
// extraction rather than filtering it downstream, because filtering later would
// mean the secret had already been read into something that could store it.
func TestExtract_SecretsNeverCaptured(t *testing.T) {
	cases := [][]string{
		{"redis-cli", "-h", "cache-host", "-p", "6380", "-a", "hunter2"},
		{"redis-cli", "-u", "redis://user:hunter2@cache-host:6380"},
		{"mysql", "-h", "db-host", "--password=hunter2"},
		{"git", "clone", "ssh://xuser:hunter2@remote-host:2222/r.git"},
	}
	for _, argv := range cases {
		x, _ := Extract(argv)
		for role, v := range x.Roles {
			if v == "hunter2" {
				t.Errorf("%v: role %s captured the password", argv, role)
			}
		}
		if x.Entity.Name == "hunter2" {
			t.Errorf("%v: password captured as the entity", argv)
		}
	}
}

// The `--flag=value` spelling must reach the SAME deny-list as the separate
// form. Filtering it later would mean the secret had already been read.
func TestExtract_AttachedLongFormSecretIsCounted(t *testing.T) {
	x, _ := Extract([]string{"redis-cli", "-h", "cache-host", "--pass=hunter2"})
	if x.Redacted == 0 {
		t.Error("--flag=value secret was not refused by the deny-list")
	}
}

func TestExtract_RedisCLI(t *testing.T) {
	x, ok := Extract([]string{"redis-cli", "-h", "cache-host", "-p", "6380", "-n", "3", "--user", "svc"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Name != "cache-host" {
		t.Errorf("entity = %q, want cache-host", x.Entity.Name)
	}
	if x.Roles[RolePort] != "6380" || x.Roles[RoleDatabase] != "3" || x.Roles[RoleUser] != "svc" {
		t.Errorf("roles = %+v", x.Roles)
	}
	// A redis account is not an ssh login, and nothing in this table may imply
	// it is.
	if Transfers(RoleUser, "redis-cli", "ssh") || Transfers(RoleUser, "ssh", "redis-cli") {
		t.Error("redis and ssh are different credential worlds")
	}
}

// An explicit flag is the more deliberate statement, so it wins over the same
// role stated inline in a URL.
func TestExtract_FlagBeatsInlineURLValue(t *testing.T) {
	x, ok := Extract([]string{"sshfs", "-p", "2200", "xuser@remote-host:/srv", "/mnt"})
	if !ok || x.Roles[RolePort] != "2200" {
		t.Errorf("roles = %+v, want the flag's 2200", x.Roles)
	}
}

func TestSplitTarget_Forms(t *testing.T) {
	cases := []struct {
		in               string
		host, user, port string
	}{
		{"remote-host", "remote-host", "", ""},
		{"xuser@remote-host", "remote-host", "xuser", ""},
		{"xuser@remote-host:/srv", "remote-host", "xuser", ""},
		{"ssh://remote-host/r.git", "remote-host", "", ""},
		{"ssh://xuser@remote-host:2222/r.git", "remote-host", "xuser", "2222"},
		{"ssh://[fd00::1]:2222/r.git", "fd00::1", "", "2222"},
		{"ssh://[fd00::1]/r.git", "fd00::1", "", ""},
		// A non-numeric authority tail is not a port. Recording it would put a
		// string where every consumer expects a number.
		{"ssh://remote-host:branch/r.git", "remote-host", "", ""},
		{"/local/path", "", "", ""},
		{"./rel", "", "", ""},
	}
	for _, c := range cases {
		h, u, p := splitTarget(c.in)
		if h != c.host || u != c.user || p != c.port {
			t.Errorf("splitTarget(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, h, u, p, c.host, c.user, c.port)
		}
	}
}

// A fact read from DECLARED config has no command behind it, so its realm has
// to come from somewhere else. Without that, an imported ssh config lands in
// the store and is then never suggested for anything — which is indistinguishable
// from the import having silently failed.
func TestTransfersTo_DeclaredProvenance(t *testing.T) {
	if !TransfersTo("ssh-config", "ssh") {
		t.Error("an ssh-config fact must reach ssh")
	}
	if !TransfersTo("ssh-config", "scp") {
		t.Error("an ssh-config fact must reach the rest of the ssh realm")
	}
	if TransfersTo("ssh-config", "psql") {
		t.Error("an ssh-config fact must NOT reach a database realm")
	}
	if !TransfersTo("exec:ssh", "scp") {
		t.Error("watched provenance must still work")
	}
	// An unresolvable realm is a reason to stay quiet, not to guess one.
	if TransfersTo("exec:some-unknown-binary", "ssh") {
		t.Error("an unknown command's realm must not default to anything")
	}
	if TransfersTo("hand-written-note", "ssh") {
		t.Error("an unrecognised declared source must not default to a realm")
	}
	if TransfersTo("", "ssh") {
		t.Error("empty provenance must not transfer")
	}
}

func TestSourceRealm(t *testing.T) {
	cases := []struct {
		source string
		realm  Realm
		ok     bool
	}{
		{"exec:ssh", RealmSSH, true},
		{"exec:git", RealmSSH, true},
		{"exec:psql", RealmPostgres, true},
		{"exec:redis-cli", RealmRedis, true},
		{"ssh-config", RealmSSH, true},
		{"exec:nope", "", false},
		{"nope", "", false},
	}
	for _, c := range cases {
		r, ok := SourceRealm(c.source)
		if r != c.realm || ok != c.ok {
			t.Errorf("SourceRealm(%q) = (%q,%v), want (%q,%v)", c.source, r, ok, c.realm, c.ok)
		}
	}
}

// Every declared source must name a realm that a real command also uses, or the
// facts it writes have nowhere to go.
func TestDeclaredSourceRealms_HaveConsumers(t *testing.T) {
	for source, realm := range declaredSourceRealms {
		found := false
		for _, spec := range commandSpecs {
			if spec.Realm == realm {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("source %q declares realm %q, which no command shares", source, realm)
		}
	}
}

// THE DESIGN DECISION THIS SPEC ENCODES. kubectl's target is a CONTEXT, not a
// host: a cluster is reached through kubeconfig and no hostname appears in a
// normal invocation. So the entity is a service, and the fact worth keeping is
// the namespace — the thing that actually gets forgotten.
func TestExtract_Kubectl(t *testing.T) {
	x, ok := Extract([]string{"kubectl", "--context", "prod-cluster", "-n", "payments", "get", "pods"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Kind != EntityService || x.Entity.Name != "prod-cluster" {
		t.Errorf("entity = %+v, want service:prod-cluster", x.Entity)
	}
	if x.Roles[RoleNamespace] != "payments" {
		t.Errorf("namespace = %q", x.Roles[RoleNamespace])
	}
	// The context NAMED the entity; keeping it as a fact would record that a
	// thing is itself.
	if _, kept := x.Roles[RoleContext]; kept {
		t.Error("the context was kept as a fact about itself")
	}
	if got, ok := RenderRole("kubectl", RoleNamespace, "payments"); !ok || got != "-n payments" {
		t.Errorf("render = %q ok=%v", got, ok)
	}
}

// A cluster context is an OPAQUE TOKEN. Real ones look like `gke_proj_zone_x`
// and `user@cluster` — applying host grammar would truncate the second at the
// `@` and bind every fact to a name nobody can look up.
func TestExtract_KubectlContextIsNotParsedAsAHost(t *testing.T) {
	for _, ctx := range []string{"user@cluster.local", "gke_proj_us-east1_prod", "arn:aws:eks:us-east-1:1234:cluster/x"} {
		x, ok := Extract([]string{"kubectl", "--context", ctx, "-n", "app", "get", "pods"})
		if !ok {
			t.Fatalf("%q: extraction failed", ctx)
		}
		if x.Entity.Name != ctx {
			t.Errorf("context %q was rewritten to %q", ctx, x.Entity.Name)
		}
	}
}

// THE HONEST LIMIT, pinned so nobody later reads silence as a bug. Without
// --context there is no entity: the current context could only be found by
// running kubectl, which this package must not do.
func TestExtract_KubectlWithoutContextLearnsNothing(t *testing.T) {
	if x, ok := Extract([]string{"kubectl", "-n", "payments", "get", "pods"}); ok {
		t.Errorf("extracted %+v with no context to bind it to", x)
	}
}

// kubectl's grammar changes per subcommand — `-f` is a filename for apply and a
// follow switch for logs. Leaving such flags undeclared is safer than guessing,
// because an undeclared flag is skipped and its value becomes an ignored
// positional, which is correct under BOTH readings.
func TestExtract_KubectlAmbiguousFlagsDoNotShadowRoles(t *testing.T) {
	for _, argv := range [][]string{
		{"kubectl", "apply", "-f", "deploy.yaml", "--context", "prod-cluster", "-n", "app"},
		{"kubectl", "logs", "-f", "mypod", "--context", "prod-cluster", "-n", "app"},
		{"kubectl", "--context", "prod-cluster", "get", "pods", "-o", "wide", "-n", "app"},
	} {
		x, ok := Extract(argv)
		if !ok || x.Entity.Name != "prod-cluster" || x.Roles[RoleNamespace] != "app" {
			t.Errorf("%v: ok=%v entity=%q roles=%+v", argv, ok, x.Entity.Name, x.Roles)
		}
	}
}

func TestExtract_DockerEndpoint(t *testing.T) {
	x, ok := Extract([]string{"docker", "-H", "tcp://build-host:2375", "ps"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Entity.Kind != EntityHost || x.Entity.Name != "build-host" {
		t.Errorf("entity = %+v, want host:build-host", x.Entity)
	}
	if x.Roles[RolePort] != "2375" {
		t.Errorf("port = %q — the flag value carried it and it should not be thrown away", x.Roles[RolePort])
	}
	// docker cannot express a port apart from rebuilding the whole -H URL, so
	// it gives facts and takes none.
	if _, ok := RenderRole("docker", RolePort, "2375"); ok {
		t.Error("docker rendered a port it cannot express")
	}
}

// `docker -H ssh://user@host` really does use the ssh credential, but docker
// declares ONE realm — folding ssh into it would let a docker fact answer an
// ssh question.
func TestExtract_DockerSSHSchemeIsRefused(t *testing.T) {
	if x, ok := Extract([]string{"docker", "-H", "ssh://xuser@build-host", "ps"}); ok {
		t.Errorf("extracted %+v across a realm boundary", x)
	}
	if Transfers(RolePort, "docker", "ssh") || Transfers(RolePort, "ssh", "docker") {
		t.Error("docker and ssh are different credential worlds")
	}
	if Transfers(RoleNamespace, "kubectl", "docker") {
		t.Error("kube and docker are different credential worlds")
	}
}

// psql's -W forces a password PROMPT and takes no value. Declared as a secret
// flag it ate the next argument, so `psql -W -U bob` quietly lost the role —
// the kind of silent loss that looks like the feature simply not working.
func TestExtract_PsqlDashWTakesNoValue(t *testing.T) {
	x, ok := Extract([]string{"psql", "-h", "db-host", "-W", "-U", "bob", "-d", "sales"})
	if !ok {
		t.Fatalf("extraction failed: %+v", x)
	}
	if x.Roles[RoleUser] != "bob" || x.Roles[RoleDatabase] != "sales" {
		t.Errorf("roles = %+v — -W must not consume the next flag", x.Roles)
	}
}

// RATCHET. Every spec must be internally consistent, because each of these
// mistakes fails SILENTLY — a role that cannot render, or a flag counted twice,
// simply stops teaching, and nothing reports it.
func TestSpecs_Consistent(t *testing.T) {
	for name, spec := range commandSpecs {
		if spec.Realm == "" {
			t.Errorf("%s declares no realm; a fact with no realm transfers nowhere", name)
		}
		for flag, role := range spec.Flags {
			if role == "" {
				t.Errorf("%s: flag %s maps to the empty role — use ValueFlags to consume a value that means nothing", name, flag)
			}
			if spec.BoolFlags[flag] || spec.ValueFlags[flag] || spec.SecretFlags[flag] {
				t.Errorf("%s: flag %s is declared twice with different meanings", name, flag)
			}
		}
		for flag := range spec.BoolFlags {
			if spec.ValueFlags[flag] || spec.SecretFlags[flag] {
				t.Errorf("%s: flag %s is both boolean and value-taking", name, flag)
			}
		}
		for role, flag := range spec.Render {
			if flag == "" {
				t.Errorf("%s: role %s renders to an empty flag", name, role)
			}
		}
		if spec.HostRequiresURL && !spec.HostPositional {
			t.Errorf("%s: HostRequiresURL is meaningless without HostPositional", name)
		}
		// Schemes applies wherever a HOST is parsed — as an operand or as a flag
		// value. It is meaningless for an opaque-token entity, which is never
		// parsed at all.
		hostFromFlag := spec.EntityFrom != "" && (spec.EntityKind == "" || spec.EntityKind == EntityHost)
		if len(spec.Schemes) > 0 && !spec.HostPositional && !hostFromFlag {
			t.Errorf("%s: Schemes is meaningless where no host is parsed", name)
		}
		if spec.EntityFrom != "" && spec.HostPositional {
			t.Errorf("%s: the target cannot arrive as both a flag and an operand", name)
		}
		if !spec.HostPositional && spec.EntityFrom == "" {
			t.Errorf("%s: names no target — every fact it extracts would have nothing to bind to", name)
		}
		if _, isRole := spec.Flags[""]; isRole {
			t.Errorf("%s: has an empty flag", name)
		}
		if spec.EntityFrom != "" {
			found := false
			for _, role := range spec.Flags {
				if role == spec.EntityFrom {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: EntityFrom is %s, which no flag produces — the entity would never be set", name, spec.EntityFrom)
			}
		}
	}
}
