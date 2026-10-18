package dbstore

import (
	"sort"
	"strings"
)

// Caps is one engine's capability facts in the field names of the contract's
// `bashy-db-caps-v1` entry (§7). It is data only: the caps story assembles the
// envelope. Every value reports what the adapter actually delivers.
//
// Sharing and exclusion differ by mode — a persistent file may be natively
// shared while a memory database is reachable only through its owner — so
// each supported ModeCaps carries its own, and the engine-level Sharing and
// Exclusion are the conservative summary Summarize derives from them.
type Caps struct {
	Engine       string            `json:"engine"`
	Categories   []string          `json:"categories"`
	Modes        map[Mode]ModeCaps `json:"modes"`
	Sharing      Sharing           `json:"sharing"`
	Transactions Transactions      `json:"transactions"`
	Exclusion    Exclusion         `json:"exclusion"`
	// Notes record limitations an agent must know (process-global state,
	// lock behaviour, pre-v1 engines). Stable, short sentences.
	Notes []string `json:"notes,omitempty"`
}

// ModeCaps reports one storage mode. Via is "native" or
// "supplemental-adapter:<name>". An unsupported mode carries its Reason.
type ModeCaps struct {
	Supported bool       `json:"supported"`
	Via       string     `json:"via,omitempty"`
	Lifetimes []string   `json:"lifetimes,omitempty"`
	Sharing   *Sharing   `json:"sharing,omitempty"`
	Exclusion *Exclusion `json:"exclusion,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

type Sharing struct {
	NativeMultiprocess           bool `json:"native_multiprocess"`
	OwnerRequiredForMultiprocess bool `json:"owner_required_for_multiprocess"`
}

type Transactions struct {
	AtomicBatch bool   `json:"atomic_batch"`
	Isolation   string `json:"isolation"`
}

type Exclusion struct {
	Scopes          []string `json:"scopes"`
	EnforcedBy      string   `json:"enforced_by"`
	NonBashyClients string   `json:"non_bashy_clients"`
}

// Enforcement points and non-Bashy-client outcomes (§9.5).
const (
	EnforcedByOwner      = "owner"
	EnforcedByNativeLock = "native-lock"
	NonBashyBlocked      = "blocked"
	NonBashyCooperative  = "cooperative"
)

// MemoryModeCaps is the memory mode every adapter delivers through a
// MemSpace: data lives in the owner process, so other processes reach it only
// through that owner, and non-Bashy clients cannot reach it at all.
func MemoryModeCaps(via string) ModeCaps {
	return ModeCaps{
		Supported: true, Via: via, Lifetimes: MemoryLifetimes,
		Sharing:   &Sharing{NativeMultiprocess: false, OwnerRequiredForMultiprocess: true},
		Exclusion: &Exclusion{Scopes: []string{"database"}, EnforcedBy: EnforcedByOwner, NonBashyClients: NonBashyBlocked},
	}
}

// Summarize fills the engine-level Sharing and Exclusion from the supported
// modes, conservatively: native multi-process sharing only if every mode has
// it, an owner required if any mode needs one, the scopes every mode
// declares, every enforcement point ("owner" first, joined by "+"), and
// cooperative if any mode is cooperative.
func Summarize(c Caps) Caps {
	first := true
	var scopes []string
	points := map[string]bool{}
	c.Sharing = Sharing{NativeMultiprocess: true}
	c.Exclusion = Exclusion{NonBashyClients: NonBashyBlocked}
	for _, m := range []Mode{Persistent, Memory} {
		mc, ok := c.Modes[m]
		if !ok || !mc.Supported || mc.Sharing == nil || mc.Exclusion == nil {
			continue
		}
		c.Sharing.NativeMultiprocess = c.Sharing.NativeMultiprocess && mc.Sharing.NativeMultiprocess
		c.Sharing.OwnerRequiredForMultiprocess = c.Sharing.OwnerRequiredForMultiprocess || mc.Sharing.OwnerRequiredForMultiprocess
		if first {
			scopes = append([]string(nil), mc.Exclusion.Scopes...)
			first = false
		} else {
			scopes = intersect(scopes, mc.Exclusion.Scopes)
		}
		for _, p := range strings.Split(mc.Exclusion.EnforcedBy, "+") {
			points[p] = true
		}
		if mc.Exclusion.NonBashyClients == NonBashyCooperative {
			c.Exclusion.NonBashyClients = NonBashyCooperative
		}
	}
	if first {
		c.Sharing = Sharing{}
		c.Exclusion = Exclusion{}
		return c
	}
	var order []string
	if points[EnforcedByOwner] {
		order = append(order, EnforcedByOwner)
		delete(points, EnforcedByOwner)
	}
	rest := make([]string, 0, len(points))
	for p := range points {
		rest = append(rest, p)
	}
	sort.Strings(rest)
	c.Exclusion.Scopes = scopes
	c.Exclusion.EnforcedBy = strings.Join(append(order, rest...), "+")
	return c
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

// MemoryLifetimes are the lifetimes a MemSpace can declare (§3.3).
var MemoryLifetimes = []string{LifetimeInvocation, LifetimeMCPSession, LifetimeOwner}
