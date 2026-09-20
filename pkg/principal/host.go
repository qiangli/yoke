package principal

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// The host reach ladder, standalone and in precedence order:
//
//  1. a static alias entry the operator wrote
//  2. mDNS / .local — the definitive same-network observation
//  3. ~/.ssh/config — the convention users already maintain
//  4. the bare name, handed to the OS resolver
//
// Pairing adds contacts above none of these; it only appends relay methods
// and an authoritative online flag. Resolving a host never needs a network
// round-trip to a control plane.

// lookupTimeout bounds every name lookup. A resolver that blocks turns a
// `whois` into a hang, and an unreachable host is an answer, not an error.
const lookupTimeout = 700 * time.Millisecond

// Env is the ambient host configuration a resolver reads. Tests replace it;
// nothing else should.
type Env struct {
	// LookupHost resolves a name to addresses. Defaults to the OS resolver,
	// which is what answers .local through mDNS.
	LookupHost func(ctx context.Context, name string) ([]string, error)
	// SSHConfig is the path to the user's ssh client config.
	SSHConfig string
	// LocalUser is this process's account name — the last-resort ssh user,
	// and never a confident one.
	LocalUser string
	// Hostname is this machine's own name.
	Hostname string
	// Paired reports whether this host is joined to a control plane, which
	// is what makes relay contacts possible.
	Paired bool
	// PairedName is the name the control plane knows this host by.
	PairedName string

	// The observation stores — where agents on this host leave traces
	// whether or not anyone cataloged them. Read-only here; an empty path
	// disables that source, which is what keeps tests hermetic.
	//
	// BoardDir is the message board (~/.bashy/mb): seen/<name> cursors and
	// posts.jsonl authors.
	BoardDir string
	// MeetDir is the meet session store (~/.bashy/meet): each session's
	// state.json names its participants, secretary, and human.
	MeetDir string
	// RoomDir is the bus room (~/.bashy/room): subs/<name>.json is a
	// standing subscription held by that name.
	RoomDir string
	// LoginDB is the bashy-owned login db (~/.bashy/who/sessions): text
	// utmp-style rows written by the agent-session writer, one per live
	// session. Absent on most hosts; an empty path disables the source.
	LoginDB string
}

// DefaultEnv reads the real host.
func DefaultEnv() Env {
	e := Env{
		LookupHost: func(ctx context.Context, name string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, name)
		},
		LocalUser: firstEnv("USER", "LOGNAME", "USERNAME"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		e.SSHConfig = filepath.Join(home, ".ssh", "config")
		// The outpost daemon records the name the control plane knows this
		// machine by; its presence is also the pairing signal.
		// outpost's own path first; then the pre-rename `matrix` path, which
		// hosts paired before the outpost rename still carry (the MATRIX_*
		// wire identifiers were kept on purpose, and so was the config dir).
		// A host read only at the new path reported UNPAIRED while paired.
		for _, dir := range []string{"outpost", "matrix"} {
			if name, ok := readOutpostAgentName(filepath.Join(home, ".config", dir, "agent.json")); ok {
				e.Paired, e.PairedName = true, name
				break
			}
		}
		e.BoardDir = filepath.Join(home, ".bashy", "mb")
		e.MeetDir = filepath.Join(home, ".bashy", "meet")
		e.RoomDir = filepath.Join(home, ".bashy", "room")
		e.LoginDB = filepath.Join(home, ".bashy", "who", "sessions")
	}
	// The owning packages honor these overrides when they write; reading the
	// stores they redirected means following the same variables.
	if d := strings.TrimSpace(os.Getenv("BASHY_MB_DIR")); d != "" {
		e.BoardDir = d
	}
	if d := strings.TrimSpace(os.Getenv("BASHY_MEET_DIR")); d != "" {
		e.MeetDir = d
	}
	if d := strings.TrimSpace(os.Getenv("BASHY_ROOM_DIR")); d != "" {
		e.RoomDir = d
	}
	e.Hostname, _ = os.Hostname()
	return e
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// readOutpostAgentName pulls agent_name out of the daemon's config without
// importing it. A missing or unreadable file simply means "not paired".
func readOutpostAgentName(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	// Deliberately not a JSON decode into a shared struct: this package must
	// not depend on the daemon's schema to answer "am I paired".
	const key = `"agent_name"`
	i := strings.Index(string(data), key)
	if i < 0 {
		return "", true // paired, name unknown
	}
	rest := string(data)[i+len(key):]
	if _, rest, ok := strings.Cut(rest, `"`); ok {
		if name, _, ok := strings.Cut(rest, `"`); ok && name != "" {
			return name, true
		}
	}
	return "", true
}

// sshHostConfig is the subset of an ssh_config Host stanza that affects reach.
type sshHostConfig struct {
	HostName string
	User     string
	Port     int
	Found    bool
	// Exact records whether the matching Host pattern named this host
	// literally rather than by wildcard. Only an exact stanza is evidence
	// that the host EXISTS: a `Host *` catch-all matches every string, and
	// treating it as existence would make every typo resolve to a machine.
	// A wildcard still supplies User/Port/HostName once the host is known.
	Exact bool
}

// readSSHConfig finds the first Host stanza whose patterns match name and
// returns its reach-relevant keys.
//
// This is deliberately a small parser, not a full ssh_config implementation:
// it reads exact and simple wildcard Host patterns and the three keys that
// determine where a connection goes and as whom. Include directives, Match
// blocks, and canonicalization are out of scope — when they matter, the user
// writes a static alias entry.
func readSSHConfig(path, name string) sshHostConfig {
	f, err := os.Open(path)
	if err != nil {
		return sshHostConfig{}
	}
	defer f.Close()

	var out sshHostConfig
	inMatch := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitSSHLine(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			if out.Found {
				return out // first matching stanza wins, as ssh does for these keys
			}
			inMatch = false
			for _, pat := range strings.Fields(val) {
				if !matchSSHPattern(pat, name) {
					continue
				}
				inMatch, out.Found = true, true
				if !strings.ContainsAny(pat, "*?") {
					out.Exact = true
				}
				break
			}
		case "match":
			// A Match block ends the applicability of the previous Host stanza
			// for our purposes; stop rather than mis-attribute its keys.
			if out.Found {
				return out
			}
			inMatch = false
		case "hostname":
			if inMatch && out.HostName == "" {
				out.HostName = val
			}
		case "user":
			if inMatch && out.User == "" {
				out.User = val
			}
		case "port":
			if inMatch && out.Port == 0 {
				out.Port, _ = strconv.Atoi(val)
			}
		}
	}
	return out
}

// splitSSHLine handles both "Key value" and "Key=value".
func splitSSHLine(line string) (key, val string, ok bool) {
	if k, v, found := strings.Cut(line, "="); found && !strings.ContainsAny(strings.TrimSpace(k), " \t") {
		return strings.TrimSpace(k), strings.TrimSpace(v), true
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], strings.Join(fields[1:], " "), true
}

// matchSSHPattern supports exact names and `*`/`?` wildcards, which covers
// how Host patterns are written in practice.
func matchSSHPattern(pat, name string) bool {
	if pat == name {
		return true
	}
	if !strings.ContainsAny(pat, "*?") {
		return false
	}
	ok, err := filepath.Match(pat, name)
	return err == nil && ok
}

// sshUser resolves the account name to use on host, best evidence first.
//
// The local $USER is the LAST rung and is reported as a guess. Agents
// routinely assume their own account exists on the remote box; it usually
// does not, and a silent assumption turns into a confusing auth failure.
func sshUser(host string, alias fleet.Host, cfg sshHostConfig, who fleet.Person, env Env) (string, Confidence) {
	if alias.SSHUser != "" {
		return alias.SSHUser, Declared
	}
	if cfg.User != "" {
		return cfg.User, Declared
	}
	if u, known := who.OSUserFor(host); known {
		return u, Declared
	}
	if env.LocalUser != "" {
		return env.LocalUser, Assumed
	}
	return "", Assumed
}

// locality is what direct observation says about where a host is.
type locality struct {
	sameLAN bool
	addrs   []string
	name    string // the .local name that answered
}

// observeLAN asks the OS resolver for <host>.local. An answer is the
// definitive same-network signal: mDNS does not cross a router. A control
// plane's location hint compares last-seen external IPs and false-positives
// whenever two machines share a CGNAT egress, so it never overrides this.
func observeLAN(env Env, host string) locality {
	if env.LookupHost == nil || !looksLikeHostname(host) {
		return locality{}
	}
	name := host
	if !strings.HasSuffix(name, ".local") {
		name += ".local"
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	addrs, err := env.LookupHost(ctx, name)
	if err != nil || len(addrs) == 0 {
		return locality{}
	}
	return locality{sameLAN: true, addrs: addrs, name: name}
}

// looksLikeHostname rejects names the system resolver would accept as legacy
// numeric addresses.
//
// getaddrinfo parses `007` as the octal address 0.0.0.7 and `42` as 0.0.0.42,
// so a plain lookup "succeeds" for them. Without this guard the nickname 007
// resolves as a host, and `whois 007` reports an ambiguity between the agent
// and a machine that does not exist. A hostname contains a letter.
func looksLikeHostname(name string) bool {
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

// resolvable reports whether the bare name resolves at all. This is the
// weakest rung of the ladder, so it is also the most careful.
func resolvable(env Env, name string) bool {
	if env.LookupHost == nil || !looksLikeHostname(name) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	addrs, err := env.LookupHost(ctx, name)
	return err == nil && len(addrs) > 0
}

// hostContacts builds the ranked reach ladder for one host. The caller has
// already observed the network and read the ssh config; both are passed in so
// a single `whois` never probes the same fact twice.
func hostContacts(host string, alias fleet.Host, cfg sshHostConfig, loc locality, who fleet.Person, env Env) []Contact {
	user, userConf := sshUser(host, alias, cfg, who, env)
	target := firstNonEmpty(alias.Address, cfg.HostName, loc.name, host)
	port := alias.SSHPort
	if port == 0 {
		port = cfg.Port
	}

	var cs []Contact

	// ssh — the workhorse. Its address is always fully qualified with the
	// account name, and a guessed account says so.
	addr := "ssh://"
	if user != "" {
		addr += user + "@"
	}
	addr += target
	if port != 0 {
		addr += ":" + strconv.Itoa(port)
	}
	ssh := Contact{Method: "ssh", Address: addr, Cost: 10}
	switch {
	case loc.sameLAN:
		ssh.Live, ssh.Source, ssh.Confidence = true, "mdns", Observed
	case cfg.Exact:
		ssh.Live, ssh.Source, ssh.Confidence = true, "ssh_config", Declared
	case alias.Address != "":
		ssh.Live, ssh.Source, ssh.Confidence = true, "alias", Declared
	case resolvable(env, host):
		ssh.Live, ssh.Source, ssh.Confidence = true, "dns", Observed
	default:
		ssh.Source, ssh.Confidence, ssh.Why = "none", Assumed, "name does not resolve from here"
	}
	if user == "" {
		ssh.Why = appendWhy(ssh.Why, "no account name known for this host")
	} else if userConf == Assumed {
		ssh.Why = appendWhy(ssh.Why, "account name assumed from the local $USER — it may not exist there")
		if ssh.Confidence.rank() < Assumed.rank() {
			ssh.Confidence = Assumed
		}
	}
	cs = append(cs, ssh)

	// mdns — the direct LAN endpoint.
	if loc.sameLAN {
		// The address is the name a client dials, not a URL: `ssh $(… --reach)`
		// and `curl $(… --reach)` both have to work on it verbatim.
		cs = append(cs, Contact{
			Method: "mdns", Address: loc.name,
			Source: "mdns", Confidence: Observed, Live: true, Cost: 5,
		})
	}

	// a declared LAN service endpoint only works from the same network.
	if alias.LANEndpoint != "" {
		c := Contact{
			Method: "lan", Address: alias.LANEndpoint,
			Source: "alias", Confidence: Declared, Live: loc.sameLAN, Cost: 1,
		}
		if !loc.sameLAN {
			c.Why = "same-network endpoint; this host is not on that network"
		}
		cs = append(cs, c)
	}

	// relay — only when this machine is paired. It survives a roam, which is
	// exactly why the ladder keeps it even when the LAN path is live.
	relay := Contact{
		Method: "relay", Address: "matrix:/matrix/h/" + host + "/app/shell/",
		Source: "pairing", Confidence: Inferred, Cost: 50,
	}
	if env.Paired {
		relay.Live = true
		relay.Why = "reaches the host wherever it is, at relay latency"
	} else {
		relay.Why = "this machine is not paired with a control plane"
	}
	cs = append(cs, relay)

	rankContacts(cs)
	return cs
}

func appendWhy(cur, add string) string {
	if cur == "" {
		return add
	}
	return cur + "; " + add
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
