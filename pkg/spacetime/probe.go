package spacetime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	busevent "github.com/qiangli/yoke/pkg/bus/event"
)

// ErrNotApplicable marks a probe that has no meaningful value on this
// host (e.g. libc outside linux). Such probes are omitted from
// snapshots — never reported as an empty value.
var ErrNotApplicable = errors.New("spacetime: probe not applicable on this host")

// A Probe measures one fact about this host's space-time coordinate.
// Values are normalized lowercase [a-z0-9.-].
type Probe struct {
	Name string
	Eval func() (string, error)
	// Volatile marks a fact that can change while the process runs, so
	// it is re-evaluated on every read instead of memoized. Network
	// locality is the motivating case.
	Volatile bool
}

// A Resolver owns a lazy dotted namespace ("tool", "engine", "mesh")
// whose keys are open-ended and evaluated only on reference.
type Resolver interface {
	Namespace() string               // "tool" answers "tool.<key>"
	Eval(key string) (string, error) // "git" → "2.49.0" | "present" | "absent"
}

// A VolatileResolver's namespace is never served from the persistent
// Cache. Implement it on any resolver whose answers expire faster than
// the cache TTL — `net.*` and `peer.*` above all. Within a single
// process the value is still memoized once, so one command sees one
// consistent coordinate.
type VolatileResolver interface {
	Resolver
	Volatile() bool
}

// ProbeSet is the evaluation surface: core probes computed once per
// process plus namespace resolvers cached through Cache.
type ProbeSet struct {
	mu        sync.Mutex
	core      map[string]Probe
	coreVal   map[string]string
	coreDone  map[string]bool
	resolvers map[string]Resolver
	volatile  map[string]bool   // namespaces that bypass the persistent cache
	memo      map[string]string // in-process memo for volatile namespaces
	memoDone  map[string]bool
	cache     Cache
	pathHash  string

	movementOnce     sync.Once
	movementStop     chan struct{}
	movementDone     chan struct{}
	movementPoll     time.Duration
	movementDebounce time.Duration
	publishMovement  func([]string) error

	localityMu        sync.Mutex
	localityBaseline  string
	localityCandidate string
	localitySet       bool
	localityTimer     *time.Timer
}

// DefaultProbes returns the pinned probe set: the always-on core
// (os, arch, os.release, libc, container, tty, elevated, time.*, place.id)
// plus the lazy
// tool.* / engine.* / mesh.* resolvers. cache may be nil (no
// persistence). Static host facts (e.g. the bashy version) are added by
// the caller via SetStatic.
func DefaultProbes(cache Cache) *ProbeSet {
	if cache == nil {
		cache = NopCache()
	}
	ps := &ProbeSet{
		core:             map[string]Probe{},
		coreVal:          map[string]string{},
		coreDone:         map[string]bool{},
		resolvers:        map[string]Resolver{},
		volatile:         map[string]bool{},
		memo:             map[string]string{},
		memoDone:         map[string]bool{},
		cache:            cache,
		pathHash:         hashOf(os.Getenv("PATH")),
		movementStop:     make(chan struct{}),
		movementDone:     make(chan struct{}),
		movementPoll:     2 * time.Second,
		movementDebounce: 10 * time.Second,
		publishMovement:  busevent.PublishSpacetimeMovement,
	}
	for _, p := range coreProbeList() {
		ps.core[p.Name] = p
	}
	ps.Register(&toolResolver{})
	ps.Register(&engineResolver{})
	ps.Register(meshResolver{})
	return ps
}

// Register adds (or replaces) a namespace resolver. A resolver that
// implements VolatileResolver and reports true bypasses the persistent
// cache for its whole namespace.
func (ps *ProbeSet) Register(r Resolver) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ns := r.Namespace()
	ps.resolvers[ns] = r
	if vr, ok := r.(VolatileResolver); ok && vr.Volatile() {
		ps.volatile[ns] = true
	} else {
		delete(ps.volatile, ns)
	}
}

// SetStatic pins a core probe to a fixed value ("" removes it) — used by
// the host binary to inject facts only it knows (its own version).
func (ps *ProbeSet) SetStatic(name, value string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if value == "" {
		delete(ps.core, name)
		delete(ps.coreVal, name)
		delete(ps.coreDone, name)
		return
	}
	v := value
	ps.core[name] = Probe{Name: name, Eval: func() (string, error) { return v, nil }}
	ps.coreVal[name] = v
	ps.coreDone[name] = true
}

// SetProbe registers (or replaces) a core probe. Use it to add a
// volatile fact — a probe re-evaluated on every read.
func (ps *ProbeSet) SetProbe(p Probe) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.core[p.Name] = p
	delete(ps.coreVal, p.Name)
	delete(ps.coreDone, p.Name)
}

// Value evaluates one probe. ok is false when the probe is not
// applicable, unknown, or (for lazy namespaces) has no resolver.
func (ps *ProbeSet) Value(name string) (string, bool) {
	ps.mu.Lock()
	v, ok := ps.valueLocked(name)
	ps.mu.Unlock()
	if name == "net.same_lan" {
		ps.observeLocality(v, ok)
	}
	return v, ok
}

func (ps *ProbeSet) valueLocked(name string) (string, bool) {
	if p, ok := ps.core[name]; ok {
		if ps.coreDone[name] && !p.Volatile {
			v := ps.coreVal[name]
			return v, v != ""
		}
		ps.coreDone[name] = true
		v, err := p.Eval()
		if err != nil {
			ps.coreVal[name] = ""
			return "", false
		}
		v = normalize(v)
		ps.coreVal[name] = v
		return v, v != ""
	}
	ns, key, ok := strings.Cut(name, ".")
	if !ok {
		return "", false
	}
	r, ok := ps.resolvers[ns]
	if !ok {
		return "", false
	}
	if ps.volatile[ns] {
		// Never persisted: a roam invalidates it. Memoized once per
		// process so a single command sees one consistent coordinate.
		if ps.memoDone[name] {
			v := ps.memo[name]
			return v, v != ""
		}
		v, err := r.Eval(key)
		if err != nil {
			ps.memoDone[name] = true
			ps.memo[name] = ""
			return "", false
		}
		v = normalize(v)
		ps.memoDone[name] = true
		ps.memo[name] = v
		return v, v != ""
	}
	if v, ok := ps.cache.Get(name, ps.pathHash); ok {
		return v, v != ""
	}
	v, err := r.Eval(key)
	if err != nil {
		return "", false
	}
	v = normalize(v)
	ps.cache.Put(name, ps.pathHash, v)
	return v, v != ""
}

// Snapshot evaluates the named probes and returns the present ones.
func (ps *ProbeSet) Snapshot(names []string) map[string]string {
	ps.mu.Lock()
	out := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := ps.valueLocked(n); ok {
			out[n] = v
		}
	}
	ps.mu.Unlock()
	for _, n := range names {
		switch n {
		case "net.same_lan":
			value, ok := out[n]
			ps.observeLocality(value, ok)
		}
	}
	return out
}

// Core evaluates every always-on probe and returns the present ones.
func (ps *ProbeSet) Core() map[string]string {
	ps.mu.Lock()
	names := make([]string, 0, len(ps.core))
	for n := range ps.core {
		names = append(names, n)
	}
	ps.mu.Unlock()
	return ps.Snapshot(names)
}

// Forget drops the in-process memo for a volatile probe so the next
// read re-evaluates it. Static probes are unaffected.
func (ps *ProbeSet) Forget(name string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.memo, name)
	delete(ps.memoDone, name)
	if p, ok := ps.core[name]; ok && p.Volatile {
		delete(ps.coreVal, name)
		delete(ps.coreDone, name)
	}
}

// StartMovementMonitor begins active place observation. The bus sidecar calls
// this for its lifetime; embedding long-lived processes may do the same.
// Repeated calls are harmless.
func (ps *ProbeSet) StartMovementMonitor() {
	initial, _ := ps.Value("place.id")
	ps.movementOnce.Do(func() {
		go ps.watchMovement(initial)
	})
}

// Close stops the movement watcher. Short-lived commands may let process exit
// do this; long-lived owners should defer Close.
func (ps *ProbeSet) Close() {
	ps.movementOnce.Do(func() { close(ps.movementDone) })
	select {
	case <-ps.movementStop:
	default:
		close(ps.movementStop)
	}
	ps.localityMu.Lock()
	if ps.localityTimer != nil {
		ps.localityTimer.Stop()
	}
	ps.localityMu.Unlock()
	<-ps.movementDone
}

// PathHash identifies the PATH the lazy probes were resolved under.
func (ps *ProbeSet) PathHash() string { return ps.pathHash }

// EnvironmentCoordinate is this host's coordinate for knowledge that belongs to
// no particular skill: ContextKey over EnvironmentProbes, and nothing else.
//
// It is deliberately narrower than "the probes this host can see". Keying over
// everything observable produced a coordinate that moved with the clock, with
// which lazy probes happened to be cached, and with whether the caller had a
// terminal — so every observation landed at a fresh address and the store
// filled with singletons. Nothing errors when that happens, which is why the
// probe list is pinned here rather than assembled at each call site.
func (ps *ProbeSet) EnvironmentCoordinate() string {
	return ContextKey(ps.Snapshot(EnvironmentProbes()))
}

// ContextKey fingerprints a probe snapshot. It is byte-compatible with
// the dhnt skill-CNL runtime: sorted "name=value" lines joined by \n,
// sha256, "c" + hex.
func ContextKey(vals map[string]string) string {
	lines := make([]string, 0, len(vals))
	for k, v := range vals {
		lines = append(lines, k+"="+v)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "c" + hex.EncodeToString(sum[:])
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// normalize lowercases and strips anything outside [a-z0-9.-].
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
