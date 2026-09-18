package craft

// ~/.ssh/config — knowledge that is DECLARED rather than observed.
//
// Every other path into the fact store learns by watching: a command runs, it
// succeeds, and what it used is recorded. That is patient and it works, but it
// only ever knows about hosts somebody has already reached.
//
// An ssh config is the opposite — a curated, authoritative map of hosts to the
// port, login and key each one needs, written deliberately by the operator. It
// is the same "enumerate rather than guess" principle the lexicon's system
// inventory follows: every fact here is real by construction, because a person
// declared it.
//
// So this fills the store instantly instead of waiting for invocations, and it
// covers hosts that have never been contacted from this machine.
//
// # Aliases are the interesting part
//
// `Host dev-box` with `HostName 10.0.0.41` means "dev-box" is a name that
// exists nowhere but here. That is exactly the local jargon `bashy define`
// should be able to answer for, and it arrives with its meaning attached.
//
// # What is deliberately not read
//
// Wildcard blocks (`Host *`) declare defaults for everything rather than facts
// about anything, and recording them against a literal entity named "*" would
// invent a machine. `Include` directives are not followed: they can escape the
// ssh directory, and a config that pulls in a system-wide file would attribute
// an operator's declarations to hosts they never chose.

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// sshConfigKeys maps the ssh_config keyword to the role it declares. The
// comparison is case-insensitive because ssh_config keywords are.
var sshConfigKeys = map[string]Role{
	"port":         RolePort,
	"user":         RoleUser,
	"identityfile": RoleIdentity,
	"proxyjump":    RoleJump,
}

// SSHConfigEntry is one Host block, resolved.
type SSHConfigEntry struct {
	// Alias is the name typed on the command line — `ssh dev-box`. It is local
	// jargon: it exists on this machine and nowhere else.
	Alias string
	// HostName is what the alias resolves to, when declared. Empty means the
	// alias IS the hostname.
	HostName string
	Roles    map[Role]string
}

// DefaultSSHConfigPath is the operator's own config. The system-wide file is
// deliberately not read: it describes the machine's defaults rather than this
// person's deliberate choices, and attributing it to them would overstate what
// was actually declared.
func DefaultSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

// ParseSSHConfig reads Host blocks from an ssh config.
//
// A missing file is normal and yields nothing. Malformed lines are skipped
// rather than guessed at: this reads declarations, it does not interpret ssh.
func ParseSSHConfig(path string) []SSHConfigEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []SSHConfigEntry
	var cur *SSHConfigEntry

	flush := func() {
		// A block with no roles and no HostName declares nothing worth
		// recording — it is a name with no content behind it.
		if cur != nil && (len(cur.Roles) > 0 || cur.HostName != "") {
			out = append(out, *cur)
		}
		cur = nil
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// ssh_config accepts `Key Value` and `Key=Value`.
		key, val := splitConfigLine(line)
		if key == "" {
			continue
		}

		switch strings.ToLower(key) {
		case "host":
			flush()
			alias := firstPattern(val)
			if alias == "" {
				continue // a wildcard-only block declares defaults, not a host
			}
			cur = &SSHConfigEntry{Alias: alias, Roles: map[Role]string{}}
		case "hostname":
			if cur != nil {
				cur.HostName = val
			}
		case "include":
			// Not followed. See the file comment: an include can escape the ssh
			// directory, and a system-wide file's declarations are not this
			// operator's choices.
			continue
		default:
			if cur == nil {
				continue
			}
			if role, isRole := sshConfigKeys[strings.ToLower(key)]; isRole && val != "" {
				if _, already := cur.Roles[role]; !already {
					// ssh takes the FIRST value for a keyword, so later
					// declarations do not override earlier ones.
					cur.Roles[role] = val
				}
			}
		}
	}
	flush()
	return out
}

// splitConfigLine handles both `Key Value` and `Key=Value`.
func splitConfigLine(line string) (key, val string) {
	if i := strings.IndexAny(line, " \t="); i > 0 {
		key = line[:i]
		val = strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
		return key, val
	}
	return "", ""
}

// firstPattern returns the first non-wildcard alias in a Host line.
//
// `Host a b c` declares one block for several names; the first concrete one is
// used, because recording the same facts under every alias would inflate the
// entity count with duplicates of one machine.
func firstPattern(val string) string {
	for _, p := range strings.Fields(val) {
		if strings.ContainsAny(p, "*?!") {
			continue
		}
		return p
	}
	return ""
}

// SSHConfigFacts converts parsed entries into entity-bound facts.
//
// Facts bind to the ALIAS rather than the resolved HostName, because the alias
// is what a person types and therefore what a later command will name. The
// resolved address is recorded as a fact ABOUT the alias, which keeps both
// answerable: `define dev-box` can say what it is, and the store can say where
// it points.
func SSHConfigFacts(entries []SSHConfigEntry, source string) []Fact {
	var out []Fact
	for _, e := range entries {
		ent := Entity{Kind: EntityHost, Name: e.Alias}
		if !ent.Valid() {
			continue
		}
		if e.HostName != "" && !strings.EqualFold(e.HostName, e.Alias) {
			out = append(out, Fact{Entity: ent, Key: "hostname", Value: e.HostName, Source: source})
		}
		for role, value := range e.Roles {
			out = append(out, Fact{Entity: ent, Key: string(role), Value: value, Source: source})
		}
	}
	return out
}
