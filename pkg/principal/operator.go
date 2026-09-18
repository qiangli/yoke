package principal

// The operator is the fallback relay when nothing else reaches an agent, and
// a resolver that cannot reach the operator has no fallback. Two gaps closed
// here (docs/agent-comms-synergy.md):
//
//   - The person at the keyboard usually is not in the people catalog. But
//     the OS already vouches for them: this process runs AS their account.
//     So the login name resolves as a person even with an empty catalog.
//   - Persons had no way to be REACHED. `bashy ask` is the channel built to
//     put a prompt in front of the human at this host's console, and the
//     login db (~/.bashy/who/sessions, written by the agent-session writer)
//     names the ttys `write` can reach.
//
// The login db is read with the same deliberately minimal parser stance as
// observe.go: its owner is cmds/internal/session, this package must not
// depend on it, and a malformed row is simply not evidence.

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// isLocalOperator reports whether name is the account this session runs as.
func isLocalOperator(env Env, name string) bool {
	return env.LocalUser != "" && strings.EqualFold(name, env.LocalUser)
}

// resolveOSUser answers for the login name when the people catalog does not.
// The evidence is direct — the process environment names the account — so the
// claim is observed, not assumed.
func (r *Resolver) resolveOSUser(name string) (Resolution, bool) {
	if !isLocalOperator(r.env, name) {
		return Resolution{}, false
	}
	res := Resolution{
		URN: URN(KindPerson, r.env.LocalUser, r.owner), Kind: KindPerson,
		Name: r.env.LocalUser, Owner: r.owner,
		Source: SourceHost, Confidence: Observed,
		Summary: "the OS account this session runs as — not in the people catalog",
		Facts:   [][2]string{{"account on " + r.env.Hostname, r.env.LocalUser}},
	}
	res.Contacts = operatorContacts(r.env, r.env.LocalUser)
	rankContacts(res.Contacts)
	return res, true
}

// operatorContacts is the reach ladder for a human who is at this host:
// `bashy ask` always (its own ladder ends in an out-of-band rendezvous, so it
// reaches an attended console whenever there is one), and `write` when the
// login db shows a session under the account.
func operatorContacts(env Env, account string) []Contact {
	cs := []Contact{{
		Method: "ask", Address: "bashy ask --prompt \"<question>\"",
		Source: SourceHost, Confidence: Declared, Live: true, Cost: 10,
		Why: "prompts the person at this host's console (ctty, GUI askpass, or rendezvous)",
	}}
	if s, ok := newestLogin(env.LoginDB, account); ok {
		c := Contact{
			Method: "write", Address: "write " + account + " " + s.tty,
			Source: SourceHost, Confidence: Observed, Live: true, Cost: 8,
		}
		if s.n > 1 {
			c.Why = "logged in on " + strconv.Itoa(s.n) + " ttys; newest shown"
		}
		cs = append(cs, c)
	}
	return cs
}

// loginSession is one row of the login db that matched.
type loginSession struct {
	tty string
	at  time.Time
	n   int // how many sessions the account holds
}

// newestLogin scans the text login db for account's sessions and returns the
// newest. Best-effort and read-only: no db, an unreadable file, or a malformed
// row is an empty answer — the db ships separately (the who/sessions writer)
// and most hosts will not have one yet.
//
// Row format is cmds/internal/session's text form: `user tty [epoch] [host]
// [type] [key=value...]`. Only the first three fields matter here.
func newestLogin(path, account string) (loginSession, bool) {
	if path == "" || account == "" {
		return loginSession{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return loginSession{}, false
	}
	defer f.Close()

	var out loginSession
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], account) {
			continue
		}
		var at time.Time
		if len(fields) > 2 {
			if sec, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
				at = time.Unix(sec, 0)
			}
		}
		out.n++
		if out.tty == "" || at.After(out.at) {
			out.tty, out.at = fields[1], at
		}
	}
	return out, out.n > 0
}
