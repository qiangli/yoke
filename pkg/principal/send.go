package principal

// The cheap half of resolution, for the hot send path.
//
// `bashy ping X "..."` must consult the SAME name authority `whois X` answers
// from — one resolver, per docs/agent-comms-synergy.md — but it must not pay
// what a full Resolve pays. Measured 2026-08-25: whois takes ~0.8s, almost
// all of it liveness probes and mDNS/DNS lookups with 700ms timeouts. None of
// that answers the send-time question, which is only "does this name resolve,
// and to which canonical address". So LookupSend:
//
//   - reads the same sources Resolve reads — the role table, the fleet
//     catalog, the OS login, the observation stores — with the same
//     precedence (catalogs first, observation only when they name nothing);
//   - builds no contact ladders and probes nothing: no network, no PATH
//     checks, no verification;
//   - memoizes the catalog load in-process, because the fleet catalog
//     re-reads its files on every query and a shell sends many messages.
//
// Sending a one-line notification must not cost the better part of a second.

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// SendTarget is one thing a send target resolved to: the kind, and the
// canonical name mail should be addressed to (for a role, the seat topic).
type SendTarget struct {
	Kind Kind
	Name string
}

// LookupSend answers what mail addressed to name would reach, using only
// cheap local evidence. Multiple entries mean the name is ambiguous and the
// caller must fail with choices rather than guess — Yoke's rule, the same
// one whois enforces with exit 3.
func LookupSend(name string) []SendTarget {
	return lookupSend(catalogSnap(), DefaultEnv(), name)
}

func lookupSend(snap *snapshot, env Env, name string) []SendTarget {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	var out []SendTarget

	// Role first — the addresser's precedence. The address is the topic,
	// which is what makes the mail survive a handover.
	for _, hr := range hostRoles() {
		if strings.EqualFold(hr.Label, name) || strings.EqualFold(hr.Topic, name) {
			out = append(out, SendTarget{Kind: KindRole, Name: hr.Topic})
			break
		}
	}
	if a, ok := agentIn(snap.agents, name); ok {
		out = append(out, SendTarget{Kind: KindAgent, Name: a.Name})
	}
	if p, ok := personIn(snap.people, name); ok {
		out = append(out, SendTarget{Kind: KindPerson, Name: p.Handle})
	} else if isLocalOperator(env, name) {
		out = append(out, SendTarget{Kind: KindPerson, Name: env.LocalUser})
	}

	// Observation answers only when the declared sources name nothing — the
	// same subordination rule Resolve applies, so the two paths cannot
	// disagree about a declared name.
	if len(out) == 0 {
		t := observe(env, name)
		if len(t.agent) > 0 {
			out = append(out, SendTarget{Kind: KindAgent, Name: name})
		}
		if len(t.person) > 0 {
			out = append(out, SendTarget{Kind: KindPerson, Name: name})
		}
	}
	return out
}

// agentIn matches the catalog's own resolution ladder — exact against every
// declared name, then case-insensitive — minus the tool:model binding form,
// which is a launch spelling and not a mail address.
func agentIn(agents []fleet.Agent, name string) (fleet.Agent, bool) {
	for _, a := range agents {
		for _, n := range a.Names() {
			if n == name {
				return a, true
			}
		}
	}
	for _, a := range agents {
		for _, n := range a.Names() {
			if strings.EqualFold(n, name) {
				return a, true
			}
		}
	}
	return fleet.Agent{}, false
}

func personIn(people []fleet.Person, name string) (fleet.Person, bool) {
	for _, p := range people {
		for _, n := range p.Names() {
			if strings.EqualFold(n, name) {
				return p, true
			}
		}
	}
	return fleet.Person{}, false
}

// --- the in-process catalog memo ------------------------------------------

// snapshot is one loaded view of the rows LookupSend needs.
type snapshot struct {
	agents []fleet.Agent
	people []fleet.Person
}

// snapTTL bounds staleness: an entry added by `bashy agent add` becomes
// visible to a long-running shell within this window, while a burst of sends
// pays for one catalog load.
const snapTTL = 5 * time.Second

var snapCache struct {
	sync.Mutex
	key  string
	at   time.Time
	snap *snapshot
}

// snapKey fingerprints everything that redirects where the catalog reads
// from, so a test (or a shell) that repoints the store never sees another
// root's cached rows.
func snapKey() string {
	return strings.Join([]string{
		os.Getenv("BASHY_FLEET_DIR"), os.Getenv("HOME"),
		os.Getenv("BASHY_AGENTS_DIR"), os.Getenv("BASHY_PEOPLE_DIR"),
		os.Getenv("BASHY_AGENTS_PATH"), os.Getenv("BASHY_PEOPLE_PATH"),
	}, "\x00")
}

func catalogSnap() *snapshot {
	key := snapKey()
	now := time.Now()
	snapCache.Lock()
	defer snapCache.Unlock()
	if snapCache.snap != nil && snapCache.key == key && now.Sub(snapCache.at) < snapTTL {
		return snapCache.snap
	}
	cat := fleet.New()
	s := &snapshot{}
	s.agents, _ = cat.Agents()
	s.people, _ = cat.People()
	snapCache.key, snapCache.at, snapCache.snap = key, now, s
	return s
}
