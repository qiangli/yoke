package craft

// ROLES — what an argument MEANS, so knowledge can move between commands.
//
// The point of this file is TRANSFER, not recall. Recording "ssh with these
// flags worked" only ever helps re-running ssh. Recording "this host answers on
// port 2222 as user xuser" helps ssh, scp, sftp, rsync, git-over-ssh — anything
// that targets the same machine.
//
// That requires knowing what an argument IS, because the same fact renders
// differently everywhere:
//
//	ssh    -p 2222
//	scp    -P 2222          <- capital
//	sftp   -P 2222
//	rsync  -e 'ssh -p 2222'
//
// A human gets that wrong regularly and an agent gets it wrong more. Learning
// the port once and rendering it correctly per command is the whole payoff, and
// it is not reachable by matching argv shapes.
//
// # Realms: the prior that stops a wrong transfer
//
// "user" is not one concept. An ssh login and a PostgreSQL role are different
// namespaces that happen to share a word, and transferring between them
// produces a confident, wrong suggestion.
//
// So every command declares an auth REALM, and a fact only transfers within
// one. That is a hand-written prior — correct on day one with zero observations,
// which a learned model cannot be. Evidence can later refine it per
// (role, from, to) triple; it cannot bootstrap it.
//
// # Secrets are refused at extraction, not filtered downstream
//
// argv is exactly where passwords appear (`mysql -pSECRET`). A deny-list is
// declared per command and the value after such a flag is never read into a
// variable that could be stored or logged. Filtering later would mean the
// secret existed in the pipeline first.

import (
	"strings"
)

// Role is what an argument means. A closed vocabulary: an open one would let
// two commands invent different names for the same thing, which is precisely
// what prevents transfer.
type Role string

const (
	RoleUser     Role = "user"          // the login/account to authenticate as
	RolePort     Role = "port"          // the TCP port
	RoleIdentity Role = "identity_file" // a private key or credential FILE (a path, never a secret)
	RoleJump     Role = "jump_host"     // an intermediate host to route through
	RoleDatabase Role = "database"      // the database/namespace to select

	// RoleHost is the target itself, named by a flag rather than a positional
	// (`psql -h`, `docker -H`). It is consumed INTO the entity and never stored
	// as a fact about it — "this host's host is itself" says nothing.
	RoleHost Role = "host"
	// RoleContext names a cluster/config context. Like RoleHost it identifies
	// the entity rather than describing it.
	RoleContext Role = "context"
	// RoleNamespace is the namespace to work in. Unlike a port it is genuinely
	// multi-valued in the world, and the store holds one value per slot — so
	// this is the most-recent namespace, which is a useful default and not a
	// complete answer. See the kubectl spec.
	RoleNamespace Role = "namespace"
)

// Realm is an authentication namespace. A fact transfers only within one.
type Realm string

const (
	RealmSSH      Realm = "ssh"      // ssh, scp, sftp, rsync, sshfs, git-over-ssh — one credential world
	RealmPostgres Realm = "postgres" // psql — `-U` is a DB role, NOT a login
	RealmMySQL    Realm = "mysql"
	RealmRedis    Realm = "redis"
	RealmDocker   Realm = "docker" // a daemon endpoint, not a login
	RealmKube     Realm = "kube"   // a cluster context; kubeconfig holds the credential
)

// CommandSpec is what bashy knows about one command's argument semantics.
type CommandSpec struct {
	Realm Realm
	// Flags maps a flag to the role of its VALUE (the next argv word).
	Flags map[string]Role
	// BoolFlags take no value; listing them prevents the following word being
	// mistaken for one — without this, `ssh -v host` would record the host as
	// the value of -v.
	BoolFlags map[string]bool
	// ValueFlags take a value that means nothing here (`sshfs -o reconnect`,
	// `ssh-keyscan -t rsa`). They must still be declared, because the reason to
	// list them is not what they hold but what follows them: an undeclared
	// value-taking flag leaves its value looking like a positional, and the
	// operand after it looking like nothing.
	ValueFlags map[string]bool
	// Render maps a role back to this command's flag, which is where the
	// ssh/scp capitalisation trap is actually solved.
	//
	// An empty map is a meaningful declaration, not an omission: it says the
	// command CONTRIBUTES facts but cannot receive them. git is the clearest
	// case — a clone over ssh:// teaches the port, and nothing in git's own
	// surface can express that port back as a flag.
	Render map[Role]string
	// SecretFlags are flags whose value must never be captured.
	SecretFlags map[string]bool
	// AttachedSecret marks flags that carry the secret in the SAME word
	// (`mysql -pSECRET`), which a next-word deny-list would miss entirely.
	AttachedSecret []string
	// HostPositional says the target host appears as a positional argument.
	HostPositional bool
	// HostHasPath marks the `host:path` operand form (scp, sftp, rsync). It is
	// the discriminator between a local file and a remote target: without it,
	// `scp file.txt remote-host:/tmp` records "file.txt" as the host, because a
	// bare filename parses as a hostname perfectly well.
	HostHasPath bool
	// HostRequiresURL demands a `scheme://` or a `user@` in the operand before
	// it is read as a target.
	//
	// This is a STRICTER test than HostHasPath, and git is why it exists: a
	// colon means "remote" for scp but means almost anything for git.
	// `git push origin HEAD:main` is a refspec, `git commit -m "fix: thing"` is
	// a message, and both would parse as `host:path` perfectly well. Demanding a
	// scheme or a login costs the bare `git clone host:repo` form — a real miss,
	// accepted deliberately, because a missed fact is recoverable on the next
	// invocation and a fabricated host named HEAD is not.
	HostRequiresURL bool
	// Schemes limits which URL schemes yield facts. Empty means any.
	//
	// The case is git again, and it is about REALMS rather than parsing: git
	// speaks ssh and https through one command, but a fact learned over https
	// belongs to a different credential world than one learned over ssh.
	// Restricting the scheme is how a multi-transport command still declares a
	// single honest realm.
	Schemes map[string]bool
	// Subcommands, when non-empty, requires argv[1] to be a member before
	// anything is extracted.
	//
	// A command with subcommands has a different grammar per subcommand, so
	// applying one flag table across all of them is how `-m` in `git commit`
	// gets read as something it is not.
	Subcommands map[string]bool
	// EntityFrom names the role whose value IDENTIFIES the target, for commands
	// that take it as a flag rather than a positional. Its value is consumed
	// into the entity and never kept as a fact.
	EntityFrom Role
	// ExitModel is this tool's exit-status convention, and TransportExits the
	// statuses meaning it never established the session. Together they separate
	// "the connection failed" from "the thing it carried failed" — see
	// outcome.go for why that distinction earns a table.
	ExitModel      ExitModel
	TransportExits map[int]bool
	// EntityKind is what that target is. It defaults to a host, and the default
	// is not cosmetic: a host has a grammar (`user@host:port`) that gets parsed,
	// while every other kind is an OPAQUE TOKEN taken verbatim. Parsing a
	// cluster context named `user@cluster` as a host would silently truncate it
	// to `cluster` and bind every fact to a name that does not exist.
	EntityKind EntityKind
}

// commandSpecs is the per-command table. Small on purpose: a dozen commands
// cover nearly every connection an operator makes, and each entry is the one
// place per-command knowledge genuinely earns its cost.
//
// The ssh realm is deliberately the widest, because that is where transfer
// actually pays: eight commands share one credential world, so a port learned
// from any of them renders correctly for the rest.
//
// # A command with no target does not belong here
//
// Every entry must NAME A TARGET, as an operand or a flag. Facts bind to an
// entity, so a spec with no entity extracts nothing, suggests nothing, and
// changes no behaviour — it is inert.
//
// That is worth a rule rather than a shrug, because an inert entry is not
// merely useless: it reads as coverage while providing none. curl and wget were
// briefly listed here on the theory that a secret deny-list earned them a place
// even without a target. It does not. With no entity nothing was ever going to
// be stored, so there was nothing for the deny-list to prevent — and argv
// secrets for those commands are already handled where it counts, in
// pkg/telemetry and pkg/policy/audit, on the paths that actually write.
var commandSpecs = map[string]CommandSpec{
	"ssh": {
		// 255 is ssh's OWN failure; every other status is the remote command's,
		// which is why `ssh host make` returning 2 says nothing about the host.
		ExitModel:      ExitPassThrough,
		TransportExits: map[int]bool{255: true},
		Realm:          RealmSSH,
		Flags:          map[string]Role{"-p": RolePort, "-l": RoleUser, "-i": RoleIdentity, "-J": RoleJump},
		// Every ssh flag that CONSUMES A VALUE has to be listed, even the ones
		// carrying no role, or the value is read as an operand — and for a
		// HostPositional command the first operand IS the host.
		//
		// `ssh -o BatchMode=yes host true` recorded facts about a host named
		// "batchmode=yes". That is worse than learning nothing: an unknown flag
		// is skipped by design ("never guessed at"), so the flag vanished
		// quietly and its value was promoted to the one field the whole entity
		// binding hangs off. `-o` is the common case; the rest are here so the
		// same silent promotion cannot happen again.
		ValueFlags: map[string]bool{
			"-o": true, "-F": true, "-c": true, "-b": true, "-B": true,
			"-D": true, "-E": true, "-e": true, "-I": true, "-L": true,
			"-m": true, "-O": true, "-Q": true, "-R": true, "-S": true,
			"-W": true, "-w": true,
		},
		BoolFlags:      map[string]bool{"-v": true, "-vv": true, "-vvv": true, "-q": true, "-t": true, "-T": true, "-A": true, "-N": true, "-f": true, "-4": true, "-6": true, "-C": true, "-G": true, "-K": true, "-k": true, "-M": true, "-n": true, "-s": true, "-V": true, "-X": true, "-x": true, "-Y": true, "-y": true},
		Render:         map[Role]string{RolePort: "-p", RoleUser: "-l", RoleIdentity: "-i", RoleJump: "-J"},
		HostPositional: true,
	},
	"scp": {
		ExitModel:   ExitToolOnly,
		HostHasPath: true,
		Realm:       RealmSSH,
		// -P, capital. The single most-forgotten flag difference in this family,
		// and the clearest thing this table buys.
		Flags: map[string]Role{"-P": RolePort, "-i": RoleIdentity, "-J": RoleJump},
		// Same silent-promotion hazard as ssh's -o. Note `-l` here is a
		// BANDWIDTH LIMIT, not a login — listing it as a value flag is what
		// keeps it from ever being read as one.
		ValueFlags: map[string]bool{
			"-o": true, "-F": true, "-c": true, "-S": true, "-l": true, "-X": true,
		},
		BoolFlags:      map[string]bool{"-r": true, "-v": true, "-q": true, "-C": true, "-p": true, "-3": true, "-4": true, "-6": true, "-A": true, "-B": true, "-O": true, "-T": true},
		Render:         map[Role]string{RolePort: "-P", RoleIdentity: "-i", RoleJump: "-J"},
		HostPositional: true,
	},
	"sftp": {
		ExitModel:   ExitToolOnly,
		HostHasPath: true,
		Realm:       RealmSSH,
		Flags:       map[string]Role{"-P": RolePort, "-i": RoleIdentity, "-J": RoleJump},
		ValueFlags: map[string]bool{
			"-o": true, "-F": true, "-c": true, "-S": true, "-s": true, "-b": true,
			"-B": true, "-D": true, "-l": true, "-R": true, "-X": true,
		},
		BoolFlags:      map[string]bool{"-r": true, "-v": true, "-q": true, "-a": true, "-C": true, "-f": true, "-N": true, "-p": true, "-4": true, "-6": true},
		Render:         map[Role]string{RolePort: "-P", RoleIdentity: "-i", RoleJump: "-J"},
		HostPositional: true,
	},
	"rsync": {
		// rsync's own table: 10 socket I/O, 12 protocol stream, 30/35 timeouts,
		// 255 the ssh underneath. Its other codes are file-level (11 file I/O,
		// 23/24 partial transfer) — the link worked, the payload did not.
		ExitModel:      ExitPassThrough,
		TransportExits: map[int]bool{10: true, 12: true, 30: true, 35: true, 255: true},
		HostHasPath:    true,
		Realm:          RealmSSH,
		Flags:          map[string]Role{},
		BoolFlags:      map[string]bool{"-a": true, "-v": true, "-z": true, "-r": true, "-P": true, "-n": true},
		// rsync carries the port INSIDE its transport string, which is why the
		// render map is a direction of its own rather than a mirror of Flags.
		Render:         map[Role]string{},
		HostPositional: true,
	},
	"psql": {
		// psql documents 2 as "connection to the server went bad"; 1 is a fatal
		// SQL error and 3 a script error, both of which mean it connected.
		ExitModel:      ExitPassThrough,
		TransportExits: map[int]bool{2: true},
		// A DIFFERENT realm. `-U` is a database role; transferring an ssh login
		// here would be a confident wrong answer.
		Realm: RealmPostgres,
		Flags: map[string]Role{"-p": RolePort, "-U": RoleUser, "-h": RoleHost, "-d": RoleDatabase},
		// -W and -w force/suppress the password PROMPT; neither takes a value.
		// Listing -W as a secret flag made it eat the next argument, so
		// `psql -W -U bob` quietly lost the role. psql has no password flag at
		// all — the credential arrives via PGPASSWORD or .pgpass — so there is
		// nothing here for a deny-list to catch.
		BoolFlags:      map[string]bool{"-l": true, "-q": true, "-t": true, "-W": true, "-w": true},
		Render:         map[Role]string{RolePort: "-p", RoleUser: "-U", RoleDatabase: "-d"},
		HostPositional: false,
		EntityFrom:     RoleHost,
	},
	"mysql": {
		Realm:     RealmMySQL,
		Flags:     map[string]Role{"-P": RolePort, "-u": RoleUser, "-h": RoleHost, "-D": RoleDatabase},
		BoolFlags: map[string]bool{"-e": false},
		Render:    map[Role]string{RolePort: "-P", RoleUser: "-u", RoleDatabase: "-D"},
		// `mysql -pSECRET` attaches the password to the flag itself.
		AttachedSecret: []string{"-p", "--password="},
		HostPositional: false,
		EntityFrom:     RoleHost,
	},
	"sshfs": {
		ExitModel:      ExitToolOnly,
		HostHasPath:    true,
		Realm:          RealmSSH,
		Flags:          map[string]Role{"-p": RolePort},
		ValueFlags:     map[string]bool{"-o": true},
		BoolFlags:      map[string]bool{"-d": true, "-f": true, "-s": true, "-v": true},
		Render:         map[Role]string{RolePort: "-p"},
		HostPositional: true,
	},
	"ssh-copy-id": {
		ExitModel:      ExitToolOnly,
		Realm:          RealmSSH,
		Flags:          map[string]Role{"-p": RolePort, "-i": RoleIdentity},
		BoolFlags:      map[string]bool{"-f": true, "-n": true, "-x": true},
		Render:         map[Role]string{RolePort: "-p", RoleIdentity: "-i"},
		HostPositional: true,
	},
	"ssh-keyscan": {
		ExitModel:  ExitToolOnly,
		Realm:      RealmSSH,
		Flags:      map[string]Role{"-p": RolePort},
		ValueFlags: map[string]bool{"-t": true, "-T": true, "-f": true},
		// -H hashes the output. It is NOT a header flag, and mistaking it for
		// one would consume the host operand as its value.
		BoolFlags:      map[string]bool{"-H": true, "-v": true, "-4": true, "-6": true, "-D": true},
		Render:         map[Role]string{RolePort: "-p"},
		HostPositional: true,
	},
	"git": {
		// git over ssh IS the ssh realm — the port and login a clone uses are
		// the same ones ssh needs, which makes this the clearest cross-command
		// transfer in the table: `git clone ssh://user@host:2222/repo` teaches a
		// fact that `ssh host` can then be reminded of.
		//
		// The direction is one-way by design. Render is empty because git has
		// no flag that expresses a port, so git gives facts and never receives
		// them. Declaring that honestly is better than inventing a
		// `-c core.sshCommand=...` suggestion nobody asked for.
		Realm:       RealmSSH,
		Subcommands: map[string]bool{"clone": true, "fetch": true, "pull": true, "push": true, "ls-remote": true, "archive": true},
		Flags:       map[string]Role{},
		// -C and -c are git's global options and appear BEFORE the subcommand,
		// so they have to be consumed correctly or the subcommand gate reads
		// their value as the subcommand and refuses a legitimate clone.
		ValueFlags:      map[string]bool{"-C": true, "-c": true, "-b": true, "--branch": true, "--depth": true, "-o": true, "--origin": true, "--reference": true, "-u": true},
		BoolFlags:       map[string]bool{"--bare": true, "--mirror": true, "-v": true, "-q": true, "--all": true, "--tags": true, "--force": true, "-f": true, "--recurse-submodules": true},
		Render:          map[Role]string{},
		HostPositional:  true,
		HostRequiresURL: true,
		// https:// is a different credential world (a token, not a key), so it
		// is left out rather than folded into the ssh realm.
		Schemes: map[string]bool{"ssh": true, "git": true},
	},
	"redis-cli": {
		Realm: RealmRedis,
		Flags: map[string]Role{"-h": RoleHost, "-p": RolePort, "-n": RoleDatabase, "--user": RoleUser},
		BoolFlags: map[string]bool{
			"--askpass": true, "--tls": true, "--no-auth-warning": true, "-c": true,
		},
		Render: map[Role]string{RolePort: "-p", RoleDatabase: "-n", RoleUser: "--user"},
		// -a is the password and -u is a URI with the password embedded. Both
		// are exactly the argv-carries-a-secret case this deny-list exists for.
		SecretFlags:    map[string]bool{"-a": true, "--pass": true, "-u": true},
		HostPositional: false,
		EntityFrom:     RoleHost,
	},
	// podman is the name that ACTUALLY ARRIVES on a bashy shell: the `docker`
	// shim does not re-spell the command, it replaces it with `bashy podman`.
	// Both spellings are registered because both are real — `podman` is what the
	// shim produces, `docker` is what a directly-invoked binary is called.
	"podman":  dockerSpec,
	"docker":  dockerSpec,
	"kubectl": kubectlSpec,
}

// dockerSpec is shared by the two names above.
var dockerSpec = CommandSpec{
	// A daemon endpoint, not a login: the credential is a TLS cert or an
	// ssh key held elsewhere, so nothing here shares a realm with anything
	// else. What it contributes is the endpoint's port, learned from the
	// URL — and, more usefully, the fact that a machine runs a daemon at all.
	Realm: RealmDocker,
	Flags: map[string]Role{"-H": RoleHost, "--host": RoleHost},
	// `docker -H ssh://user@build-host` really does use the ssh credential,
	// but docker declares ONE realm and folding ssh into it would let a
	// docker fact answer an ssh question. Restricting to tcp keeps the
	// realm honest; the same invocation's ssh facts are better learned from
	// ssh itself, which is where they will render correctly anyway.
	Schemes:    map[string]bool{"tcp": true},
	ValueFlags: map[string]bool{"--context": true, "--config": true, "-c": true, "--log-level": true},
	BoolFlags:  map[string]bool{"--debug": true, "-D": true, "--tls": true, "--tlsverify": true},
	// docker cannot express a port apart from rebuilding the whole -H URL,
	// so it gives facts and takes none.
	Render:         map[Role]string{},
	HostPositional: false,
	EntityFrom:     RoleHost,
}

var kubectlSpec = CommandSpec{
	// THE ENTITY IS THE CONTEXT, NOT A HOST, and that is the whole reason
	// this entry needed a design decision rather than another table row. A
	// cluster is reached through kubeconfig; no hostname appears in a normal
	// invocation, and the credential belongs to the context. So the target
	// is a SERVICE named by --context, and the useful fact about it is the
	// namespace — the thing agents forget most.
	//
	// Two honest limits, both worth knowing before trusting this:
	//
	//   - With no --context there is no entity and nothing is learned. The
	//     current context could only be found by running kubectl, which
	//     this package must not do.
	//   - A context has MANY namespaces and the store holds one value per
	//     slot, so what comes back is the most recent. That is a useful
	//     default and not a complete answer, and it is why the suggestion
	//     only ever fires on a failure that did not name one.
	Realm: RealmKube,
	Flags: map[string]Role{
		"-n": RoleNamespace, "--namespace": RoleNamespace,
		"--context": RoleContext,
	},
	// Deliberately no ValueFlags. kubectl's grammar changes per subcommand
	// — `-f` is a filename for apply and a follow switch for logs — and
	// guessing wrong there consumes the next word. Since positionals are
	// discarded anyway (the entity comes from a flag), leaving them
	// undeclared is strictly safer: an undeclared flag is skipped and its
	// value becomes an ignored positional, which is correct under both
	// readings.
	Render:         map[Role]string{RoleNamespace: "-n"},
	HostPositional: false,
	EntityFrom:     RoleContext,
	EntityKind:     EntityService,
}

// SpecFor returns what is known about a command's arguments.
func SpecFor(binary string) (CommandSpec, bool) {
	s, ok := commandSpecs[strings.TrimSpace(binary)]
	return s, ok
}

// TransferableRoles is the closed vocabulary a fact KEY must be spelled in to
// travel between commands.
//
// It is exported because the hand-written path cannot otherwise know it. A fact
// learned by watching is keyed by role automatically; one typed at the prompt is
// keyed by whatever the operator wrote, and `remote_user` — the spelling this
// command's own example used to suggest — is not `user`, so nothing would ever
// offer it. The fact is still worth storing; the point is to SAY so, because a
// key that silently never transfers is indistinguishable from one that does
// until the day it is needed.
func TransferableRoles() []Role {
	return []Role{RoleUser, RolePort, RoleIdentity, RoleJump, RoleDatabase, RoleNamespace}
}

// IsTransferableRole reports whether a fact key is spelled as a role.
func IsTransferableRole(key string) bool {
	for _, r := range TransferableRoles() {
		if Role(strings.TrimSpace(key)) == r {
			return true
		}
	}
	return false
}

// Extraction is what one invocation taught.
type Extraction struct {
	// Entity is what the facts are ABOUT — the target host. Zero when the
	// command named no entity, in which case the facts have nothing to bind to
	// and are dropped.
	Entity Entity
	Realm  Realm
	Roles  map[Role]string
	// Redacted counts values refused because their flag carries a secret.
	// Surfaced so a caller can see the deny-list firing rather than assume it.
	Redacted int
}

// Extract reads roles and a target entity out of an invocation.
//
// It reads only what a command DECLARES it means. An unknown binary yields
// nothing rather than a guess: inventing roles from flag shapes is how `-p`
// becomes "port" for a command where it means "preserve".
func Extract(argv []string) (Extraction, bool) {
	if len(argv) == 0 {
		return Extraction{}, false
	}
	spec, ok := SpecFor(argv[0])
	if !ok {
		return Extraction{}, false
	}

	out := Extraction{Realm: spec.Realm, Roles: map[Role]string{}}
	var positionals []string
	needSub := len(spec.Subcommands) > 0

	for i := 1; i < len(argv); i++ {
		a := argv[i]

		// Attached secrets first: `-pSECRET` is one word, so a next-word check
		// would never see it.
		if hasAttachedSecret(spec, a) {
			out.Redacted++
			continue
		}
		// A `--flag=value` word is split before anything looks at it, so a
		// secret written that way is refused by the same deny-list rather than
		// slipping through as an unrecognised flag.
		name, attached, isAttached := strings.Cut(a, "=")
		if !isAttached || !strings.HasPrefix(a, "-") {
			name, attached = a, ""
		}

		if spec.SecretFlags[name] {
			out.Redacted++
			if !isAttached {
				i++ // consume the separate value without reading it
			}
			continue
		}
		if role, isRole := spec.Flags[name]; isRole {
			switch {
			case isAttached:
				out.Roles[role] = attached
			case i+1 < len(argv):
				out.Roles[role] = argv[i+1]
				i++
			}
			continue
		}
		if spec.ValueFlags[name] {
			if !isAttached {
				i++ // consumed so the value is not mistaken for an operand
			}
			continue
		}
		if spec.BoolFlags[name] {
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue // an unknown flag is skipped, never guessed at
		}
		if needSub {
			// The first bare word is the subcommand. A command with
			// subcommands has a different grammar under each, so an unknown one
			// yields nothing rather than being parsed with the wrong table.
			needSub = false
			if !spec.Subcommands[a] {
				return Extraction{}, false
			}
			continue
		}
		positionals = append(positionals, a)
	}

	if spec.HostPositional {
		positionals = remoteOperands(spec, positionals)
		for _, p := range positionals {
			host, user, port := splitTarget(p)
			if host == "" {
				continue
			}
			if !schemeAllowed(spec, p) {
				continue
			}
			out.Entity = Entity{Kind: EntityHost, Name: host}
			// `user@host` and `ssh://host:2222` state the login and port
			// inline, and they should not be lost just because they were not
			// given as flags. An explicit flag still wins: it is the more
			// deliberate statement of the two.
			if user != "" {
				if _, already := out.Roles[RoleUser]; !already {
					out.Roles[RoleUser] = user
				}
			}
			if port != "" {
				if _, already := out.Roles[RolePort]; !already {
					out.Roles[RolePort] = port
				}
			}
			break
		}
	} else if spec.EntityFrom != "" {
		if v, named := out.Roles[spec.EntityFrom]; named {
			out.Entity = entityFromValue(spec, v)
			// Whatever named the target is consumed into it. Keeping it as a
			// fact would record that a thing is itself.
			delete(out.Roles, spec.EntityFrom)
			// A flag value can carry more than the name (`-H tcp://host:2375`),
			// and the extra should not be thrown away just because it arrived
			// inside a URL.
			if _, already := out.Roles[RolePort]; !already && out.Entity.Kind == EntityHost {
				if _, _, port := splitTarget(v); port != "" {
					out.Roles[RolePort] = port
				}
			}
		}
	}

	return out, out.Entity.Valid() && len(out.Roles) > 0
}

// entityFromValue turns a flag value into the entity it names.
//
// The kind decides how much grammar to assume. A HOST has one and it is worth
// parsing: `-H tcp://build-host:2375` names a machine and a port. Everything
// else is an opaque token — a cluster context named `user@cluster` or
// `gke_proj_zone_cluster` is one string, and applying host grammar to it would
// truncate it at the `@` and bind facts to a name nobody can look up.
func entityFromValue(spec CommandSpec, value string) Entity {
	kind := spec.EntityKind
	if kind == "" {
		kind = EntityHost
	}
	if kind != EntityHost {
		return Entity{Kind: kind, Name: strings.TrimSpace(value)}
	}
	if !schemeAllowed(spec, value) {
		return Entity{}
	}
	host, _, _ := splitTarget(value)
	return Entity{Kind: kind, Name: host}
}

// remoteOperands narrows positionals to the ones that can name a remote target.
//
// Two strictnesses, because a colon means different things in different
// grammars. For scp and friends it reliably separates host from path. For git
// it separates almost anything from anything else — refspecs, commit messages,
// URLs — so a scheme or a login is demanded instead.
func remoteOperands(spec CommandSpec, positionals []string) []string {
	if spec.HostRequiresURL {
		var out []string
		for _, p := range positionals {
			if strings.Contains(p, "://") || strings.Contains(p, "@") {
				out = append(out, p)
			}
		}
		return out
	}
	if !spec.HostHasPath {
		return positionals
	}
	var out []string
	for _, p := range positionals {
		if strings.Contains(p, ":") {
			out = append(out, p)
		}
	}
	return out
}

// schemeAllowed enforces a spec's scheme restriction.
//
// A scheme-less operand passes: `user@host:path` is the scp form, which for
// every command in this table means the command's declared realm.
func schemeAllowed(spec CommandSpec, operand string) bool {
	if len(spec.Schemes) == 0 {
		return true
	}
	i := strings.Index(operand, "://")
	if i < 0 {
		return true
	}
	return spec.Schemes[operand[:i]]
}

// splitTarget pulls host, optional user and optional port out of a target
// operand, handling `user@host`, `host:/path`, `user@host:/path`, and the URL
// form `scheme://user@host:port/path`.
//
// The scheme is what discriminates the two colon meanings: in `host:/path` the
// colon introduces a PATH, in `ssh://host:2222/repo` it introduces a PORT.
// Reading one as the other would either invent a port named "/tmp" or throw
// away a real one.
func splitTarget(p string) (host, user, port string) {
	if i := strings.Index(p, "://"); i >= 0 {
		return splitURLTarget(p[i+3:])
	}
	// A local path is not a host. Checking the separator rather than the
	// filesystem keeps this a pure function.
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, ".") {
		return "", "", ""
	}
	if i := strings.Index(p, ":"); i >= 0 {
		p = p[:i]
	}
	if i := strings.Index(p, "@"); i >= 0 {
		user, p = p[:i], p[i+1:]
	}
	if p == "" || strings.Contains(p, "/") {
		return "", user, ""
	}
	return p, user, ""
}

// splitURLTarget parses the authority of a URL: `[user[:pass]@]host[:port]`.
func splitURLTarget(rest string) (host, user, port string) {
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		user, rest = rest[:i], rest[i+1:]
		// `user:password@host` puts a password in argv. It is dropped here
		// rather than downstream, so it never reaches a variable that could be
		// stored: this function's callers write what it returns.
		if j := strings.Index(user, ":"); j >= 0 {
			user = user[:j]
		}
	}
	if strings.HasPrefix(rest, "[") { // IPv6 literal
		j := strings.Index(rest, "]")
		if j < 0 {
			return "", user, ""
		}
		host = rest[1:j]
		if tail := rest[j+1:]; strings.HasPrefix(tail, ":") {
			port = numericPort(tail[1:])
		}
		return host, user, port
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		host, port = rest[:i], numericPort(rest[i+1:])
	} else {
		host = rest
	}
	return host, user, port
}

// numericPort returns s only if it is digits. A non-numeric authority tail is
// not a port, and recording it as one would put a string where every consumer
// expects a number.
func numericPort(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s
}

func hasAttachedSecret(spec CommandSpec, arg string) bool {
	for _, p := range spec.AttachedSecret {
		if len(arg) > len(p) && strings.HasPrefix(arg, p) {
			return true
		}
	}
	return false
}

// Transfers reports whether a role learned from one command may be suggested
// for another.
//
// The realm check is the prior, and it is what makes the system correct before
// it has any evidence: an ssh login and a psql role are different namespaces
// that share a word, and suggesting one for the other is a confident wrong
// answer. Observation can later refine this per (role, from, to); it cannot
// bootstrap it, because the first wrong suggestion has already been made by
// then.
//
// This answers the SEMANTIC question only — whether the fact is about the same
// thing in both commands. Whether the target can express it as a flag is a
// separate question, answered by RenderRole, and conflating the two was a real
// bug: scp accepts a login perfectly well, just as `user@host` rather than
// `-l`, so a realm check that demanded a flag reported "does not transfer" for
// a fact that plainly does.
func Transfers(role Role, from, to string) bool {
	a, okA := SpecFor(from)
	b, okB := SpecFor(to)
	if !okA || !okB {
		return false
	}
	return a.Realm == b.Realm
}

// declaredSourceRealms maps non-command provenance to a realm.
//
// A fact learned by WATCHING carries the command that taught it, and that
// command's spec names the realm. A fact read from DECLARED config has no
// command behind it, so the realm has to be stated here instead — without this
// an imported ssh config lands in the store and is then never suggested for
// anything, which looks exactly like the import having silently failed.
var declaredSourceRealms = map[string]Realm{
	"ssh-config": RealmSSH,
}

// SourceRealm resolves the credential realm a fact's provenance belongs to,
// for both `exec:<binary>` and declared sources.
//
// An unrecognised source yields false rather than a default. A fact whose realm
// is unknown must not be suggested anywhere: "we don't know which credential
// world this belongs to" is a reason to stay quiet, not a reason to guess.
func SourceRealm(source string) (Realm, bool) {
	s := strings.TrimSpace(source)
	if bin, isExec := strings.CutPrefix(s, "exec:"); isExec {
		spec, ok := SpecFor(bin)
		if !ok {
			return "", false
		}
		return spec.Realm, true
	}
	r, ok := declaredSourceRealms[s]
	return r, ok
}

// TransfersTo reports whether a fact with the given provenance may be suggested
// for a command. It is the provenance-aware form of Transfers, and the one
// callers holding a Fact should use.
func TransfersTo(source, to string) bool {
	from, ok := SourceRealm(source)
	if !ok {
		return false
	}
	spec, ok := SpecFor(to)
	return ok && spec.Realm == from
}

// RenderRole formats a known fact as the target command's flag.
//
// This is where the ssh/scp capitalisation trap is actually paid off: the same
// port renders `-p` for ssh and `-P` for scp, and a caller never has to know.
// A role the target cannot express returns false rather than a plausible guess.
func RenderRole(binary string, role Role, value string) (string, bool) {
	spec, ok := SpecFor(binary)
	if !ok {
		return "", false
	}
	flag, ok := spec.Render[role]
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}
	return flag + " " + value, true
}

// FactsFrom converts an extraction into entity-bound facts.
//
// The role is the key, so the fact is stated in transferable terms rather than
// in one command's flag spelling — which is the difference between knowledge
// that moves and knowledge that does not.
func FactsFrom(x Extraction, source string) []Fact {
	if !x.Entity.Valid() {
		return nil
	}
	out := make([]Fact, 0, len(x.Roles))
	for role, value := range x.Roles {
		out = append(out, Fact{
			Entity: x.Entity,
			Key:    string(role),
			Value:  value,
			Source: source,
		})
	}
	return out
}
