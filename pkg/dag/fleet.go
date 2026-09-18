// Copyright (c) 2025 qiangli

// See LICENSE for licensing information

package dag

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This is the FLEET half of fleet execution — how a chunk reaches a worker. It
// is the mirror of chunks.go and the split is the load-bearing invariant:
// nothing here decides what is *in* a chunk, and nothing in chunks.go consults
// a slot count. Capacity sets how many chunks run concurrently; the committed
// manifest sets how many chunks exist and what they contain.
//
// One host is a valid fleet. There is one scheduler and one code path; fleet
// size is a parameter and N=1 is an ordinary value of it. `pool == nil` degrades
// to LocalPool(Concurrency) — today's `-j N` reproduced by construction rather
// than by branching around the pool.
//
// P2 ships venue 1 (userland: same-host, in-process) only. Worker carries the
// fields the remote venues will need (Host, Labels, MemBytes) so that adding a
// transport is a new Transport implementation, not a reshaping of the Pool —
// but no remote transport exists here, by design.

// Execution venues. A venue answers "what isolation does this chunk get",
// separately from "how does the orchestrator reach it" — which is why one pool
// can eventually serve all six. Only VenueUserland is implemented in P2.
const (
	VenueUserland  = "userland"  // venue 1 — in-process on this host
	VenueWorkspace = "workspace" // venue 2 — private HOME/TMPDIR clone (not in P2)
	VenueSandbox   = "sandbox"   // venue 3 — container (not in P2)
	VenueNative    = "native"    // venue 4 — native-process virtual node
	VenueCluster   = "cluster"   // venue 5 — scheduler-selected cluster worker
	VenueCloud     = "cloud"     // venue 6 — provisioned cloud worker
)

// Distribution is the dhnt.pipeline/v1 closed vocabulary describing how a
// task may be expanded by a pipeline executor. The DAG runner preserves this
// intent but does not itself invent an execution or lowering strategy.
type Distribution string

const (
	DistributionSingle          Distribution = "single"
	DistributionShardable       Distribution = "shardable"
	DistributionReplicated      Distribution = "replicated"
	DistributionTopologyCoupled Distribution = "topology-coupled"
)

// LocalWorkerID is the logical name of the same-host worker. It is deliberately
// a venue name and not a hostname: a worker identity that leaked host facts
// would carry them into every result and artifact the fleet produces.
const LocalWorkerID = "local-userland"

// Worker is one capacity bucket in the fleet.
type Worker struct {
	ID   string // logical identity — never a hostname
	Host string // "" => in-process on this box; set only by remote transports

	Venues []string          // which venues this worker can offer
	Labels map[string]string // os/arch/libc/... for a future Requires-host: match

	CPU      int    // slot ceiling
	MemBytes uint64 // the other half of the capacity formula

	// Facts is a timestamped observation of this worker. When present it wins
	// over static configuration; stale facts make the worker ineligible rather
	// than allowing scheduling from inventory that is no longer trustworthy.
	Facts       *HostFacts
	MaxFactsAge time.Duration

	Transport Transport // nil => the pool's default transport
}

// PlacementRefusal explains why one worker cannot accept a task. It is kept
// deliberately small and machine-readable so callers can report every worker
// deterministically instead of flattening an unknown capability into a vague
// "no worker" string.
type PlacementRefusal struct {
	Worker      string `json:"worker"`
	Code        string `json:"code"`
	Requirement string `json:"requirement"`
	Missing     string `json:"missing,omitempty"`
	Available   string `json:"available,omitempty"`
}

// PlacementError carries the complete, ordered set of per-worker refusals.
type PlacementError struct {
	Constraints Constraints
	Refusals    []PlacementRefusal
}

func (e *PlacementError) Error() string {
	if len(e.Refusals) == 0 {
		return "no eligible worker"
	}
	parts := make([]string, 0, len(e.Refusals))
	for _, r := range e.Refusals {
		parts = append(parts, r.Worker+": "+r.Code+" "+r.Requirement)
	}
	return "no worker offers " + describeConstraints(e.Constraints) + ": " + strings.Join(parts, "; ")
}

func (e *PlacementError) FleetFailure() (RunStatus, FailureReason) {
	return RunInfraFailed, FailureReason{Code: FailNoWorker}
}

const defaultFactsMaxAge = 5 * time.Minute

// offers reports whether the worker can host the given venue. An empty venue
// means "unconstrained", which resolves to userland.
func (w *Worker) offers(venue string) bool {
	if venue == "" {
		venue = VenueUserland
	}
	venues := w.Venues
	if facts := w.observedFacts(time.Now()); facts != nil && len(facts.Venues) > 0 {
		venues = facts.Venues
	}
	for _, v := range venues {
		if v == venue {
			return true
		}
	}
	return false
}

// matches reports whether the worker satisfies a label constraint.
func (w *Worker) matches(want map[string]string) bool {
	for k, v := range want {
		if w.Labels[k] != v {
			return false
		}
	}
	return true
}

func (w *Worker) observedFacts(now time.Time) *HostFacts {
	if w.Facts != nil && !w.Facts.Stale(now, w.maxFactsAgeOrDefault()) {
		return w.Facts
	}
	return nil
}

func (w *Worker) maxFactsAgeOrDefault() time.Duration {
	if w.MaxFactsAge == 0 {
		return defaultFactsMaxAge
	}
	return w.MaxFactsAge
}

func (w *Worker) cpuCount() int {
	if facts := w.observedFacts(time.Now()); facts != nil && facts.CPU > 0 {
		return facts.CPU
	}
	return w.CPU
}

func (w *Worker) memoryBytes() uint64 {
	if facts := w.observedFacts(time.Now()); facts != nil {
		return facts.MemBytes
	}
	return w.MemBytes
}

func (w *Worker) staleFacts(now time.Time) bool {
	if w.Facts == nil {
		return false
	}
	return w.Facts.Stale(now, w.maxFactsAgeOrDefault())
}

// fact looks up a capability from observed facts first, then static labels.
// OS and architecture are typed observed facts, but labels remain the fallback
// for workers configured before host observation was introduced.
func (w *Worker) fact(key string) string {
	if facts := w.observedFacts(time.Now()); facts != nil {
		switch strings.ToLower(key) {
		case "os":
			if facts.OS != "" {
				return facts.OS
			}
		case "arch":
			if facts.Arch != "" {
				return facts.Arch
			}
		default:
			if v := facts.Labels[key]; v != "" {
				return v
			}
		}
	}
	return w.Labels[key]
}

func (w *Worker) refusal(c Constraints, now time.Time) *PlacementRefusal {
	if c.CPUPerTask < 0 {
		return &PlacementRefusal{Worker: w.ID, Code: "invalid-constraint", Requirement: "cpu_per_task>=0"}
	}
	if err := c.Accelerator.validate(); err != nil {
		return &PlacementRefusal{Worker: w.ID, Code: "invalid-constraint", Requirement: "valid accelerator request", Available: err.Error()}
	}
	for resource, minimum := range c.MinimumCapacity {
		if strings.TrimSpace(resource) == "" || minimum == 0 {
			return &PlacementRefusal{Worker: w.ID, Code: "invalid-constraint", Requirement: "positive named minimum capacity"}
		}
	}
	if w.staleFacts(now) {
		return &PlacementRefusal{Worker: w.ID, Code: "unknown-capability", Requirement: "fresh host facts", Missing: "observed_at"}
	}
	venue := c.Venue
	if venue == "" {
		venue = VenueUserland
	}
	if !w.offers(venue) {
		return &PlacementRefusal{Worker: w.ID, Code: "missing-capability", Requirement: "venue=" + venue, Missing: "venue"}
	}
	keys := make([]string, 0, len(c.Match))
	for k := range c.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want, got := c.Match[k], w.fact(k)
		if got == "" {
			return &PlacementRefusal{Worker: w.ID, Code: "unknown-capability", Requirement: k + "=" + want, Missing: k}
		}
		if got != want {
			return &PlacementRefusal{Worker: w.ID, Code: "capability-mismatch", Requirement: k + "=" + want, Available: got}
		}
	}
	if c.MemPerTask > 0 {
		mem := w.memoryBytes()
		if mem == 0 {
			return &PlacementRefusal{Worker: w.ID, Code: "unknown-capability", Requirement: "mem_bytes>=" + strconv.FormatUint(c.MemPerTask, 10), Missing: "mem_bytes"}
		}
		if mem < c.MemPerTask {
			return &PlacementRefusal{Worker: w.ID, Code: "insufficient-capacity", Requirement: "mem_bytes>=" + strconv.FormatUint(c.MemPerTask, 10), Available: strconv.FormatUint(mem, 10)}
		}
	}
	if c.CPUPerTask > 0 && w.cpuCount() < c.CPUPerTask {
		return &PlacementRefusal{Worker: w.ID, Code: "insufficient-capacity", Requirement: "cpu>=" + strconv.Itoa(c.CPUPerTask), Available: strconv.Itoa(w.cpuCount())}
	}
	facts := w.observedFacts(now)
	if c.Accelerator.Kind != "" {
		if facts == nil {
			return &PlacementRefusal{Worker: w.ID, Code: "unknown-capability", Requirement: "accelerator=" + c.Accelerator.Kind, Missing: "accelerators"}
		}
		if !acceleratorSatisfies(facts.Accelerators, c.Accelerator) {
			return &PlacementRefusal{Worker: w.ID, Code: "insufficient-capacity", Requirement: describeAccelerator(c.Accelerator)}
		}
	}
	if c.TopologyClass != "" {
		if facts == nil || facts.Topology.Class == "" {
			return &PlacementRefusal{Worker: w.ID, Code: "unknown-capability", Requirement: "topology=" + c.TopologyClass, Missing: "topology"}
		}
		if facts.Topology.Class != c.TopologyClass {
			return &PlacementRefusal{Worker: w.ID, Code: "capability-mismatch", Requirement: "topology=" + c.TopologyClass, Available: facts.Topology.Class}
		}
	}
	for resource, minimum := range c.MinimumCapacity {
		if facts == nil {
			return &PlacementRefusal{Worker: w.ID, Code: "unknown-capability", Requirement: resource + ">=" + strconv.FormatUint(minimum, 10), Missing: resource}
		}
		available, ok := facts.Capacities[resource]
		if !ok {
			return &PlacementRefusal{Worker: w.ID, Code: "unknown-capability", Requirement: resource + ">=" + strconv.FormatUint(minimum, 10), Missing: resource}
		}
		if available < minimum {
			return &PlacementRefusal{Worker: w.ID, Code: "insufficient-capacity", Requirement: resource + ">=" + strconv.FormatUint(minimum, 10), Available: strconv.FormatUint(available, 10)}
		}
	}
	return nil
}

// Constraints is what a task demands of a worker. It is derived from task
// metadata, never from the pool.
type Constraints struct {
	Venue           string             // userland | workspace | sandbox | ... ("" => userland)
	Match           map[string]string  // worker capability labels
	Exclusive       bool               // drain the worker; nothing co-schedules
	CPUPerTask      int                // 0 => one scheduler slot
	MemPerTask      uint64             // 0 => not memory-gated
	Accelerator     AcceleratorRequest // zero value => no accelerator requirement
	MinimumCapacity map[string]uint64  // Kubernetes-style scalar/extended resources
	TopologyClass   string             // empty => unconstrained
}

// AcceleratorRequest is a quantitative device requirement. MemoryBytes is
// per-device, so aggregate memory can never make one undersized GPU appear
// eligible.
type AcceleratorRequest struct {
	Kind        string `json:"kind,omitempty"`
	Family      string `json:"family,omitempty"`
	Count       int    `json:"count,omitempty"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	invalid     bool
}

func (r AcceleratorRequest) validate() error {
	if r.invalid {
		return errors.New("accelerator quantity is invalid")
	}
	if r.Count < 0 {
		return errors.New("accelerator count must not be negative")
	}
	if r.Kind == "" && (r.Family != "" || r.Count != 0 || r.MemoryBytes != 0) {
		return errors.New("accelerator kind is required when accelerator capacity is requested")
	}
	if r.Kind != "" && r.Count < 1 {
		return errors.New("accelerator count must be positive")
	}
	return nil
}

func acceleratorSatisfies(have []AcceleratorFacts, want AcceleratorRequest) bool {
	if want.Kind == "" {
		return true
	}
	for _, accelerator := range have {
		if accelerator.Kind == want.Kind &&
			(want.Family == "" || accelerator.Family == want.Family) &&
			accelerator.Count >= want.Count &&
			(want.MemoryBytes == 0 || accelerator.MemoryBytes >= want.MemoryBytes) {
			return true
		}
	}
	return false
}

// Transport delivers one task to one worker and returns its result. This is the
// seam every venue plugs into: localTransport runs it in-process here, and a
// remote transport (ssh, sandbox exec, cluster job) is another implementation
// of this one method. P2 implements the local one only.
//
// # EXEC CONTRACT
//
// An implementation MUST distinguish failure to deliver from failure of a body
// that ran. Undeliverable attempts must return a TaskResult with an Err that
// wraps `ErrWorkerUnreachable` (errors.Is(err, ErrWorkerUnreachable)) or
// returns an error implementing `FleetFailure`:
//
//	result.Err = fmt.Errorf("worker %q: tls: no route to host: %w", w.ID, ErrWorkerUnreachable)
//
// If an implementation instead returns an unmarked StatusFailed result, RecordAttempt
// cannot separate it from a genuine body failure and records it as a conformance
// RunFailed verdict against code that never ran. See RecordAttempt and
// ErrWorkerUnreachable/FleetFailure.
type Transport interface {
	Exec(ctx context.Context, w *Worker, t *Task, io TaskIO) TaskResult
	// Close releases the transport's resources. It must be idempotent: a
	// transport shared by several workers may be closed once per worker.
	Close() error
}

// Pool is a capacity-aware set of workers behind one or more transports.
//
// Slots are per-worker, not global: a single global semaphore cannot express "4
// slots here, 12 there", which is the whole point of a fleet. The pool owns
// placement and slot accounting; the engine owns the graph.
type Pool struct {
	mu      sync.Mutex
	workers []*Worker
	free    []int // free slots, indexed like workers
	busy    []int // in-flight tasks per worker (for Exclusive)

	transport Transport     // default transport when Worker.Transport is nil
	freed     chan struct{} // buffered(1): "a slot released, re-scan"
}

// NewPool builds a pool over the given workers. transport is the default for
// workers that do not carry one of their own.
func NewPool(transport Transport, workers ...*Worker) *Pool {
	p := &Pool{
		transport: transport,
		freed:     make(chan struct{}, 1),
	}
	for _, w := range workers {
		if w == nil {
			continue
		}
		if w.ID == "" {
			w.ID = LocalWorkerID
		}
		p.workers = append(p.workers, w)
		p.free = append(p.free, max(1, w.cpuCount()))
		p.busy = append(p.busy, 0)
	}
	return p
}

// LocalPool is the degrade-to-today constructor: one in-process worker offering
// the userland venue with `concurrency` slots. `dag -j 8` with no fleet
// configured is exactly this — same dispatch, same results, no off-box reach.
//
// Its transport is left nil so that the engine can install one built from its
// own executor (which is what carries --sandbox wrapping); standalone callers
// get the plain in-process executor via Exec's fallback.
func LocalPool(concurrency int) *Pool {
	return NewPool(nil, &Worker{
		ID:     LocalWorkerID,
		Host:   "", // in-process
		Venues: []string{VenueUserland},
		CPU:    max(1, concurrency),
	})
}

// SetDefaultTransport installs tr as the pool's default transport if it has
// none, leaving an explicitly-configured transport alone. The engine calls this
// so a pool from LocalPool() executes through the engine's executor rather than
// silently dropping its sandbox wrapper.
func (p *Pool) SetDefaultTransport(tr Transport) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.transport == nil {
		p.transport = tr
	}
}

// Slots is the capacity formula: cores, gated by memory. A 16 GB / 10-core host
// running a suite with a 4 GB/task watchdog offers 4 slots, not 10 — slots are
// gated by memory as often as by cores.
func (p *Pool) Slots(w *Worker, memPerTask uint64) int {
	slots := max(1, w.cpuCount())
	if memPerTask > 0 && w.memoryBytes() > 0 {
		if byMem := int(w.memoryBytes() / memPerTask); byMem < slots {
			slots = byMem
		}
	}
	return max(0, slots)
}

func (p *Pool) slotsFor(w *Worker, c Constraints) int {
	slots := p.Slots(w, c.MemPerTask)
	if c.CPUPerTask > 0 {
		slots = min(slots, w.cpuCount()/c.CPUPerTask)
	}
	if c.Accelerator.Kind != "" {
		if c.Accelerator.Count <= 0 {
			return 0
		}
		facts := w.observedFacts(time.Now())
		if facts == nil {
			return 0
		}
		for _, accelerator := range facts.Accelerators {
			if accelerator.Kind == c.Accelerator.Kind &&
				(c.Accelerator.Family == "" || accelerator.Family == c.Accelerator.Family) {
				slots = min(slots, accelerator.Count/c.Accelerator.Count)
			}
		}
	}
	if facts := w.observedFacts(time.Now()); facts != nil {
		for resource, minimum := range c.MinimumCapacity {
			if minimum == 0 {
				return 0
			}
			slots = min(slots, int(facts.Capacities[resource]/minimum))
		}
	}
	return max(0, slots)
}

// Capacity is the pool's total slot count across all workers.
func (p *Pool) Capacity() int {
	if p == nil {
		return 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, w := range p.workers {
		total += max(1, w.cpuCount())
	}
	return max(1, total)
}

// Eligible reports whether ANY worker could ever satisfy these constraints,
// free or not. This is the anti-hang guard: a ready task that no worker could
// ever host must fail fast with a clear reason, exactly as a missing tool does,
// rather than waiting forever for a slot that will never qualify.
//
// It must not special-case a one-worker pool: a single local worker that cannot
// offer venue=sandbox fails the same way a fleet of twenty would.
func (p *Pool) Eligible(c Constraints) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.workers {
		if w.refusal(c, time.Now()) == nil && p.slotsFor(w, c) > 0 {
			return true
		}
	}
	return false
}

// Refusals returns one deterministic explanation per worker that cannot ever
// run c. A nil result means at least one worker is eligible.
func (p *Pool) Refusals(c Constraints) []PlacementRefusal {
	p.mu.Lock()
	defer p.mu.Unlock()
	refusals := make([]PlacementRefusal, 0, len(p.workers))
	now := time.Now()
	for _, w := range p.workers {
		if r := w.refusal(c, now); r != nil {
			refusals = append(refusals, *r)
			continue
		}
		if p.slotsFor(w, c) == 0 {
			refusals = append(refusals, PlacementRefusal{Worker: w.ID, Code: "insufficient-capacity", Requirement: "slots>0", Available: "0"})
		}
	}
	if len(refusals) == len(p.workers) {
		return refusals
	}
	return nil
}

// TryAcquire reserves one qualifying slot and returns the worker plus its
// release func, or (nil, nil) when every qualifying worker is currently full.
//
// It is non-blocking on purpose. When the longest ready chunk cannot be placed
// on any free qualifying worker, the scheduler must be free to try the
// *next*-longest; a blocking acquire commits to one task and cannot.
func (p *Pool) TryAcquire(c Constraints) (*Worker, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Least-loaded first, so work spreads instead of stacking on worker 0.
	idx := make([]int, len(p.workers))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return p.free[idx[a]] > p.free[idx[b]] })

	for _, i := range idx {
		w := p.workers[i]
		if w.refusal(c, time.Now()) != nil {
			continue
		}
		limit := p.slotsFor(w, c)
		if limit <= 0 {
			continue
		}
		if c.Exclusive && p.busy[i] > 0 {
			continue // an exclusive task never shares a worker (perfbench, cert)
		}
		if p.free[i] <= 0 || p.busy[i] >= limit {
			continue
		}
		take := 1
		if c.Exclusive {
			take = p.free[i] // drain the worker: nothing may co-schedule
		}
		p.free[i] -= take
		p.busy[i]++
		return w, func() { p.release(i, take) }
	}
	return nil, nil
}

// Acquire blocks until a qualifying slot frees up or ctx ends. It fails fast
// (without waiting) when no worker could ever qualify.
func (p *Pool) Acquire(ctx context.Context, c Constraints) (*Worker, func(), error) {
	for {
		if w, release := p.TryAcquire(c); w != nil {
			// Pass the baton: another waiter might be sleeping because freed drops
			// signals when full. If we woke up and took a slot, there might be
			// MORE slots free that we didn't take. Wake up the next waiter.
			select {
			case p.freed <- struct{}{}:
			default:
			}
			return w, release, nil
		}
		// Facts may become stale while this call is waiting for a slot. Recheck
		// before sleeping so stale inventory turns into a refusal, never a hang.
		if refusals := p.Refusals(c); refusals != nil {
			return nil, nil, &PlacementError{Constraints: c, Refusals: refusals}
		}
		select {
		case <-p.freed:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

func (p *Pool) release(i, n int) {
	p.mu.Lock()
	p.free[i] += n
	p.busy[i]--
	p.mu.Unlock()
	select { // buffered(1): a pending waiter re-scans; a full buffer already will
	case p.freed <- struct{}{}:
	default:
	}
}

// Exec places one task on a qualifying worker and runs it there. It is the
// self-contained form: acquire, run, release.
//
// The engine does not use it — its scheduler acquires the slot itself (so that
// the When: condition and the cache check run inside the slot too) and then
// calls ExecOn. Keeping both means the pool is usable on its own without
// reaching into the engine's scheduling loop.
func (p *Pool) Exec(ctx context.Context, c Constraints, t *Task, io TaskIO) TaskResult {
	w, release, err := p.Acquire(ctx, c)
	if err != nil {
		return TaskResult{Name: t.Name, Host: t.Host, Status: StatusFailed, ExitCode: 1, Err: err}
	}
	defer release()
	return p.ExecOn(ctx, w, t, io)
}

// ExecOn runs a task on a worker whose slot the caller already holds. The
// worker's own transport wins over the pool's default, so a heterogeneous fleet
// (local here, ssh there) is a matter of which transport each worker carries.
func (p *Pool) ExecOn(ctx context.Context, w *Worker, t *Task, io TaskIO) TaskResult {
	tr := w.Transport
	if tr == nil {
		p.mu.Lock()
		tr = p.transport
		p.mu.Unlock()
	}
	if tr == nil {
		tr = localTransport{}
	}
	return tr.Exec(ctx, w, t, io)
}

// Close releases the pool's transports.
//
// Transports are deduplicated so a transport shared by twenty workers is torn
// down once — but only when its dynamic type is comparable. An interface value
// cannot be used as a map key (or compared with ==) when its dynamic type is
// unhashable, which any transport holding a func, slice, or map field is; doing
// so panics at run time. Uncomparable transports are therefore closed once per
// worker that carries them, which is why Transport.Close must be idempotent.
func (p *Pool) Close() error {
	p.mu.Lock()
	transports := make([]Transport, 0, len(p.workers)+1)
	add := func(tr Transport) {
		if tr == nil {
			return
		}
		if reflect.TypeOf(tr).Comparable() {
			for _, seen := range transports {
				if seen == tr { // safe: differing dynamic types compare false
					return
				}
			}
		}
		transports = append(transports, tr)
	}
	for _, w := range p.workers {
		add(w.Transport)
	}
	add(p.transport)
	p.mu.Unlock()

	var firstErr error
	for _, tr := range transports {
		if err := tr.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func describeConstraints(c Constraints) string {
	venue := c.Venue
	if venue == "" {
		venue = VenueUserland
	}
	parts := []string{"venue=" + venue}
	keys := make([]string, 0, len(c.Match))
	for k := range c.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+c.Match[k])
	}
	if c.CPUPerTask > 0 {
		parts = append(parts, "cpu>="+strconv.Itoa(c.CPUPerTask))
	}
	if c.MemPerTask > 0 {
		parts = append(parts, "mem_bytes>="+strconv.FormatUint(c.MemPerTask, 10))
	}
	if c.Accelerator.Kind != "" {
		parts = append(parts, describeAccelerator(c.Accelerator))
	}
	if c.TopologyClass != "" {
		parts = append(parts, "topology="+c.TopologyClass)
	}
	resourceKeys := make([]string, 0, len(c.MinimumCapacity))
	for resource := range c.MinimumCapacity {
		resourceKeys = append(resourceKeys, resource)
	}
	sort.Strings(resourceKeys)
	for _, resource := range resourceKeys {
		parts = append(parts, resource+">="+strconv.FormatUint(c.MinimumCapacity[resource], 10))
	}
	return strings.Join(parts, " ")
}

func describeAccelerator(r AcceleratorRequest) string {
	parts := []string{"accelerator=" + r.Kind, "count>=" + strconv.Itoa(r.Count)}
	if r.Family != "" {
		parts = append(parts, "family="+r.Family)
	}
	if r.MemoryBytes > 0 {
		parts = append(parts, "device_memory_bytes>="+strconv.FormatUint(r.MemoryBytes, 10))
	}
	return strings.Join(parts, ",")
}

// localTransport is venue 1: the task runs in-process on this host, through the
// engine's own executor. It is the P2 transport, and the one every other venue
// degrades to when the fleet is a single box.
//
// It tags the task's env with the worker's LOGICAL identity (DAG_FLEET_WORKER /
// DAG_FLEET_VENUE), never a hostname, uname, or user. Those facts are what a
// verdict must not carry: an artifact stamped with the machine that produced it
// cannot be compared against one produced anywhere else, and the standing rule
// is that no host fact reaches a committed artifact.
type localTransport struct {
	exec Executor // nil => the default in-process executor
}

func (x localTransport) Exec(ctx context.Context, w *Worker, t *Task, io TaskIO) TaskResult {
	start := time.Now()

	io.Env = append(append([]string{}, io.Env...),
		"DAG_FLEET_WORKER="+w.ID,
		"DAG_FLEET_VENUE="+VenueUserland,
	)

	exec := x.exec
	if exec == nil {
		exec = localExecutor{}
	}
	res := exec.Execute(ctx, t, io)
	if res.Duration == 0 {
		res.Duration = time.Since(start)
	}
	return res
}

func (x localTransport) Close() error { return nil }
