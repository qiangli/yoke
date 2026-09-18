package dag

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmbudget"
)

// CapacityPolicy is explicit transport authority, independent of reach aliases.
// The receiving host applies its own policy and owns its admission ledger.
// Workspace is an existing checkout; this path never provisions or transfers it.
type CapacityPolicy struct {
	Version     int              `json:"version"`
	LocalWorker string           `json:"local_worker"`
	Targets     []CapacityTarget `json:"targets"`
}
type CapacityTarget struct {
	Executables []CapacityExecutable `json:"executables,omitempty"`
	Name        string               `json:"name"`
	Worker      string               `json:"worker"`
	Observe     bool                 `json:"observe"`
	Dispatch    bool                 `json:"dispatch"`
	DataClasses []string             `json:"data_classes"`
	Workspace   string               `json:"workspace"`
	Revision    string               `json:"revision"`
	Toolchain   string               `json:"toolchain"`
	Slots       int                  `json:"slots"`
	MemoryBytes uint64               `json:"memory_bytes"`
}
type CapacityRequest struct {
	Executables []CapacityExecutable `json:"executables,omitempty"`
	Version     int                  `json:"version"`
	ID          string               `json:"id"`
	Target      string               `json:"target,omitempty"`
	Spec        TaskSpec             `json:"spec"`
	Body        string               `json:"body"`
	DataClass   string               `json:"data_class"`
	Revision    string               `json:"revision"`
	Toolchain   string               `json:"toolchain"`
}
type CapacityObservation struct {
	Facts         HostFacts `json:"facts"`
	FreeCPU       float64   `json:"free_cpu"`
	FreeMemory    uint64    `json:"free_memory"`
	HeadroomKnown bool      `json:"headroom_known"`
	Revision      string    `json:"revision"`
	Toolchain     string    `json:"toolchain"`
}
type CapacityResult struct {
	Executables   []CapacityExecutable `json:"executables,omitempty"`
	Record        RunRecord            `json:"record"`
	Revision      string               `json:"revision"`
	Toolchain     string               `json:"toolchain"`
	RequestSHA256 string               `json:"request_sha256"`
	Output        string               `json:"output"`
	OutputSHA256  string               `json:"output_sha256"`
}
type CapacityReply struct {
	CapacityHeld bool                 `json:"capacity_held,omitempty"`
	Version      int                  `json:"version"`
	Decision     string               `json:"decision"`
	Reason       string               `json:"reason,omitempty"`
	Worker       string               `json:"worker,omitempty"`
	Refusals     []string             `json:"refusals,omitempty"`
	Observation  *CapacityObservation `json:"observation,omitempty"`
	Monitor      json.RawMessage      `json:"monitor,omitempty"`
	Result       *CapacityResult      `json:"result,omitempty"`
}
type capacityWire struct {
	Operation string          `json:"operation"`
	Worker    string          `json:"worker"`
	Request   CapacityRequest `json:"request"`
}
type CapacityServices struct {
	Identity func(context.Context, int) (string, error)
	Probe    func(context.Context, string) (CapacityObservation, error)
	Observe  func(context.Context) (json.RawMessage, error)
}
type CapacityClient struct {
	Policy    *CapacityPolicy
	Resolve   func(string) (fleet.Host, bool)
	Transport func(fleet.Host) Transport
}

func CapacityPolicyPath() string {
	if p := os.Getenv("BASHY_REMOTE_CAPACITY_POLICY"); p != "" {
		return p
	}
	return filepath.Join(fleet.DefaultRoot(), "remote-capacity-policy.json")
}
func LoadCapacityPolicy() (*CapacityPolicy, error) {
	f, err := os.Open(CapacityPolicyPath())
	if err != nil {
		return nil, fmt.Errorf("remote capacity policy unavailable: %w", err)
	}
	defer f.Close()
	var p CapacityPolicy
	if err = decodeCapacity(f, &p); err != nil {
		return nil, err
	}
	if p.Version != 1 || len(p.Targets) > 64 {
		return nil, errors.New("invalid remote capacity policy version or target count")
	}
	validID := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	seen := map[string]bool{}
	workers := map[string]bool{}
	for _, t := range p.Targets {
		if !validID.MatchString(t.Name) || !validID.MatchString(t.Worker) || seen[t.Name] || workers[t.Worker] || t.Slots < 0 || len(t.Name) > 128 || len(t.Worker) > 128 {
			return nil, errors.New("invalid or duplicate capacity target")
		}
		seen[t.Name] = true
		workers[t.Worker] = true
		if !capacityInventoryAllowed(t.Executables, t.Executables) {
			return nil, errors.New("invalid declared executable inventory")
		}
		if t.Dispatch && (t.Slots == 0 || t.MemoryBytes == 0 || t.Workspace == "" || !filepath.IsAbs(t.Workspace) || t.Revision == "" || t.Toolchain == "" || len(t.DataClasses) == 0) {
			return nil, errors.New("dispatch policy requires workspace, revision, toolchain, data classes and nonzero capacity")
		}
	}
	return &p, nil
}
func decodeCapacity(r io.Reader, out any) error {
	b, e := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return errors.New("capacity envelope exceeds 1MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		return e
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return errors.New("capacity envelope has trailing content")
	}
	return nil
}
func NewCapacityClient() (*CapacityClient, error) {
	p, e := LoadCapacityPolicy()
	if e != nil {
		return nil, e
	}
	return &CapacityClient{Policy: p, Resolve: fleet.New().Host, Transport: func(h fleet.Host) Transport { return NewSSHTransport(h) }}, nil
}
func (c *CapacityClient) target(name string) (CapacityTarget, error) {
	for _, t := range c.Policy.Targets {
		if t.Name == name {
			return t, nil
		}
	}
	return CapacityTarget{}, errors.New("target has no capacity permission")
}
func (c *CapacityClient) exchange(ctx context.Context, t CapacityTarget, wire capacityWire) (*CapacityReply, error) {
	host, ok := c.Resolve(t.Name)
	if !ok {
		return nil, errors.New("target is not registered in fleet")
	}
	transport := c.Transport(host)
	defer transport.Close()
	payload, e := json.Marshal(wire)
	if e != nil {
		return nil, e
	}
	body := "printf '%s' " + shellQuote(string(payload)) + " | bashy dag capacity receive\n"
	var out limitedCapacityBuffer
	out.limit = 1 << 20
	var stderr limitedCapacityBuffer
	stderr.limit = 8192
	res := transport.Exec(ctx, &Worker{ID: t.Worker}, &Task{Name: "capacity-" + wire.Operation, Body: body}, TaskIO{Env: os.Environ(), Stdout: &out, Stderr: &stderr})
	if res.Err != nil || res.ExitCode != 0 {
		return nil, errors.New("registered target unreachable or capacity endpoint unavailable")
	}
	var reply CapacityReply
	if e = decodeCapacity(bytes.NewReader(out.Bytes()), &reply); e != nil {
		return nil, e
	}
	if reply.Version != 1 || reply.Worker != t.Worker {
		return nil, errors.New("remote capacity identity mismatch")
	}
	return &reply, nil
}
func (c *CapacityClient) Observe(ctx context.Context, name string) (json.RawMessage, error) {
	t, e := c.target(name)
	if e != nil {
		return nil, e
	}
	if !t.Observe {
		return nil, errors.New("target observation is not authorized")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, e := c.exchange(ctx, t, capacityWire{Operation: "observe", Worker: t.Worker})
	if e != nil {
		return nil, e
	}
	if r.Decision != "observed" || len(r.Monitor) == 0 {
		return nil, fmt.Errorf("remote observation refused: %s", r.Reason)
	}
	return r.Monitor, nil
}
func validateCapacityRequest(r CapacityRequest) error {
	if r.Version != 1 || r.ID == "" || len(r.ID) > 128 || r.Body == "" || len(r.Body) > 65536 || r.Revision == "" || r.Toolchain == "" || r.DataClass == "" {
		return errors.New("capacity request requires version, id, bounded body, revision, toolchain and data class")
	}
	if e := r.Spec.ValidateForPlacement(); e != nil {
		return e
	}
	if r.Spec.Distribution != DistributionSingle || r.Spec.CPUPerTask < 1 || r.Spec.MemPerTask == 0 || r.Spec.Timeout <= 0 || r.Spec.Timeout > 10*time.Minute {
		return errors.New("capacity dispatch requires single distribution, explicit CPU/memory and timeout <=10m")
	}
	return nil
}
func permitsCapacity(t CapacityTarget, r CapacityRequest) bool {
	if !capacityInventoryAllowed(t.Executables, r.Executables) {
		return false
	}
	if !t.Dispatch || t.Revision != r.Revision || t.Toolchain != r.Toolchain || t.Slots < r.Spec.CPUPerTask || t.MemoryBytes < r.Spec.MemPerTask {
		return false
	}
	for _, class := range t.DataClasses {
		if class == r.DataClass {
			return true
		}
	}
	return false
}
func capacityCompatible(o *CapacityObservation, r CapacityRequest, now time.Time) bool {
	return o != nil && o.Facts.Validate() == nil && !o.Facts.ObservedAt.After(now) && !o.Facts.Stale(now, 10*time.Second) && o.Facts.Satisfies(r.Spec) && o.HeadroomKnown && o.FreeCPU >= float64(r.Spec.CPUPerTask) && o.FreeMemory >= r.Spec.MemPerTask && o.Revision == r.Revision && o.Toolchain == r.Toolchain
}
func (c *CapacityClient) Plan(ctx context.Context, r CapacityRequest) (*CapacityReply, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e := validateCapacityRequest(r); e != nil {
		return nil, e
	}
	plan := &CapacityReply{Version: 1, Decision: "queued-local", Reason: "no authorized compatible remote capacity; dispatch retains a local pending request"}
	if _, e := capacityPinnedBody(r.Body, r.Executables); e != nil {
		plan.Reason = e.Error()
		plan.Refusals = []string{e.Error()}
		return plan, nil
	}
	targets := append([]CapacityTarget(nil), c.Policy.Targets...)
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	for _, t := range targets {
		if ctx.Err() != nil {
			plan.Refusals = append(plan.Refusals, "placement observation deadline reached; uninspected targets omitted")
			return plan, nil
		}
		if r.Target != "" && r.Target != t.Name {
			continue
		}
		if !permitsCapacity(t, r) {
			plan.Refusals = append(plan.Refusals, t.Worker+": dispatch/data/revision/toolchain permission denied")
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		// The task body is never sent during placement inspection.
		probeRequest := r
		probeRequest.Body = ""
		reply, e := c.exchange(probeCtx, t, capacityWire{Operation: "probe", Worker: t.Worker, Request: probeRequest})
		cancel()
		if e != nil {
			plan.Refusals = append(plan.Refusals, t.Worker+": unreachable")
			continue
		}
		if reply.Decision != "observed" {
			reason := reply.Reason
			if reason == "" {
				reason = "remote capacity refused"
			}
			if len(reason) > 256 {
				reason = reason[:256]
			}
			plan.Refusals = append(plan.Refusals, t.Worker+": "+reason)
			continue
		}
		if !capacityCompatible(reply.Observation, r, time.Now()) {
			plan.Refusals = append(plan.Refusals, t.Worker+": stale, unknown or incompatible headroom/platform/toolchain/data")
			continue
		}
		plan.Decision = "remote-ready"
		plan.Reason = "receiver must atomically admit before execution"
		plan.Worker = t.Worker
		plan.Observation = reply.Observation
		return plan, nil
	}
	return plan, nil
}
func (c *CapacityClient) Dispatch(ctx context.Context, r CapacityRequest) (*CapacityReply, error) {
	plan, e := c.Plan(ctx, r)
	if e != nil {
		return plan, e
	}
	if plan.Decision != "remote-ready" {
		if e = queueCapacityRequest(r, plan.Reason); e != nil {
			return nil, e
		}
		return plan, nil
	}
	for _, t := range c.Policy.Targets {
		if t.Worker == plan.Worker && (r.Target == "" || r.Target == t.Name) {
			ctx, cancel := context.WithTimeout(ctx, r.Spec.Timeout+10*time.Second)
			defer cancel()
			reply, e := c.exchange(ctx, t, capacityWire{Operation: "execute", Worker: t.Worker, Request: r})
			if e != nil {
				return nil, e
			}
			if reply.Decision == "queued-local" {
				if e = queueCapacityRequest(r, reply.Reason); e != nil {
					return nil, e
				}
			}
			if reply.Decision == "completed" {
				if e = verifyCapacityResult(t, r, reply.Result); e != nil {
					return nil, e
				}
				if e = clearCapacityQueuedRequest(r); e != nil {
					reply.Reason += "; local pending request cleanup: " + e.Error()
				}
			}
			return reply, nil
		}
	}
	return nil, errors.New("planned target authority disappeared")
}
func capacityDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func verifyCapacityResult(t CapacityTarget, r CapacityRequest, result *CapacityResult) error {
	if result == nil || result.Record.Validate() != nil || result.Record.Worker != t.Worker || result.Record.Task != r.Spec.Task || result.Revision != r.Revision || result.Toolchain != r.Toolchain || result.RequestSHA256 != capacityDigest(r) || result.OutputSHA256 != capacityOutputDigest(result.Output) || !capacitySameInventory(r.Executables, result.Executables) {
		return errors.New("remote result provenance or artifact digest mismatch")
	}
	return nil
}

func capacityOutputDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type limitedCapacityBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	limit  int
}

func (b *limitedCapacityBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buffer.Len()+len(p) > b.limit {
		return 0, errors.New("capacity output budget exceeded")
	}
	return b.buffer.Write(p)
}
func (b *limitedCapacityBuffer) Bytes() []byte                     { return b.buffer.Bytes() }
func (b *limitedCapacityBuffer) String() string                    { return b.buffer.String() }
func (b *limitedCapacityBuffer) Len() int                          { return b.buffer.Len() }
func (b *limitedCapacityBuffer) WriteString(s string) (int, error) { return b.Write([]byte(s)) }
func capacityRuntimeToolchain() string {
	return runtime.Version() + "/" + runtime.GOOS + "/" + runtime.GOARCH
}

func receiveCapacity(ctx context.Context, p *CapacityPolicy, s CapacityServices, w capacityWire) (*CapacityReply, error) {
	reply := &CapacityReply{Version: 1, Worker: w.Worker, Decision: "denied"}
	if p.LocalWorker == "" || w.Worker != p.LocalWorker {
		return nil, errors.New("receiver is not the requested logical worker")
	}
	var target *CapacityTarget
	for i := range p.Targets {
		if p.Targets[i].Worker == w.Worker {
			target = &p.Targets[i]
			break
		}
	}
	if target == nil {
		return nil, errors.New("receiver has no local target authority")
	}
	if w.Operation == "observe" {
		if !target.Observe || s.Observe == nil {
			return reply, nil
		}
		b, e := s.Observe(ctx)
		if e != nil {
			return nil, e
		}
		reply.Decision = "observed"
		reply.Monitor = b
		return reply, nil
	}
	if w.Operation == "reconcile" {
		return reconcileCapacity(ctx, *target, w.Request.ID, s)
	}
	if !permitsCapacity(*target, w.Request) {
		reply.Reason = "receiver dispatch or data permission denied"
		return reply, nil
	}
	if !capacityPlatformSupportsExecution(runtime.GOOS) {
		reply.Decision = "queued-local"
		reply.Reason = "guarded remote execution is unsupported on this receiver platform; observation remains available"
		return reply, nil
	}
	if s.Probe == nil {
		return nil, errors.New("native headroom observation unavailable")
	}
	if _, e := VerifyCapacityExecutables(ctx, w.Request.Executables); e != nil {
		reply.Decision = "queued-local"
		reply.Reason = "declared executable inventory unavailable or changed"
		return reply, nil
	}
	if w.Operation == "execute" {
		if _, e := capacityPinnedBody(w.Request.Body, w.Request.Executables); e != nil {
			reply.Decision = "queued-local"
			reply.Reason = e.Error()
			return reply, nil
		}
	}
	observation, e := s.Probe(ctx, w.Worker)
	if e != nil {
		return nil, e
	}
	revision, e := capacityCheckoutRevision(ctx, target.Workspace)
	if e != nil {
		return nil, e
	}
	observation.Revision = revision
	observation.Toolchain = capacityRuntimeToolchain()
	reply.Observation = &observation
	if w.Operation == "probe" {
		reply.Decision = "observed"
		return reply, nil
	}
	if w.Operation != "execute" {
		return nil, errors.New("unknown capacity operation")
	}
	if e = validateCapacityRequest(w.Request); e != nil {
		return nil, e
	}
	if !capacityCompatible(&observation, w.Request, time.Now()) {
		reply.Decision = "queued-local"
		reply.Reason = "receiver capacity changed or checkout/toolchain mismatch"
		return reply, nil
	}
	return executeCapacity(ctx, *target, w.Request, reply, s)
}
func executeCapacity(ctx context.Context, t CapacityTarget, r CapacityRequest, reply *CapacityReply, s CapacityServices) (*CapacityReply, error) {
	if s.Identity == nil {
		return nil, errors.New("receiver native process identity unavailable")
	}
	boot, e := s.Identity(ctx, 1)
	if e != nil || boot == "" {
		return nil, errors.New("receiver boot identity unavailable")
	}
	allocation := t
	allocation.Slots = min(t.Slots, int(math.Floor(reply.Observation.FreeCPU)))
	allocation.MemoryBytes = min(t.MemoryBytes, reply.Observation.FreeMemory)
	g := capacityGate(allocation)
	owner, e := g.NewOwner(ctx, "dag-capacity")
	if e != nil {
		return nil, e
	}
	defer owner.Close()
	request := llmbudget.Request{ID: r.ID, Owner: owner.ID(), Run: r.ID, Host: t.Worker, HostSlots: r.Spec.CPUPerTask, MemoryBytes: r.Spec.MemPerTask, TTL: r.Spec.Timeout + time.Minute}
	admitted, e := g.Reserve(ctx, request)
	if e != nil {
		return nil, e
	}
	if admitted.Reservation == nil {
		reply.Decision = "queued-local"
		reply.Reason = "receiver atomic capacity is occupied"
		return reply, nil
	}
	reply.CapacityHeld = true
	receipt := capacityReceipt{Version: 1, Run: r.ID, Owner: owner.ID(), Worker: t.Worker, BootID: boot, BoundedBody: capacityBoundedBuiltinBody(r.Body)}
	if e = writeCapacityReceipt(receipt); e != nil {
		_ = g.Release(ctx, r.ID, owner.ID())
		return nil, e
	}
	// Capacity remains reserved unless the owned execution boundary proves exit.
	terminated := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if terminated {
			receipt.ReleasePending = true
			if writeCapacityReceipt(receipt) == nil {
				if g.Release(cleanup, r.ID, owner.ID()) == nil {
					receipt.Released = true
					reply.CapacityHeld = false
					_ = writeCapacityReceipt(receipt)
				} else {
					reply.Reason = capacityRetentionReason(t, r.ID)
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, r.Spec.Timeout)
	defer cancel()
	var output limitedCapacityBuffer
	output.limit = 64 << 10
	pinnedBody, pinErr := capacityPinnedBody(r.Body, r.Executables)
	if pinErr != nil {
		terminated = true
		return nil, pinErr
	}
	task := &Task{Name: r.Spec.Task, Body: pinnedBody, Venue: r.Spec.Venue, Lang: "bash"}
	res, proved := runCapacityOwnedTask(ctx, task, TaskIO{Dir: t.Workspace, Env: os.Environ(), Stdout: &output, Stderr: &output}, func(pid int) error {
		receipt.PID = pid
		receipt.StartID, _ = s.Identity(ctx, pid)
		return writeCapacityReceipt(receipt)
	})
	terminated = proved
	if !proved {
		reply.Reason = capacityRetentionReason(t, r.ID)
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	after, verifyErr := capacityCheckoutRevision(verifyCtx, t.Workspace)
	executableProof, executableErr := VerifyCapacityExecutables(verifyCtx, r.Executables)
	verifyCancel()
	record := RecordAttempt(task, &Worker{ID: t.Worker}, 1, res)
	reply.Decision = "completed"
	reply.Observation = nil
	reply.Result = &CapacityResult{Executables: executableProof, Record: record, Revision: r.Revision, Toolchain: capacityRuntimeToolchain(), RequestSHA256: capacityDigest(r), Output: output.String(), OutputSHA256: capacityOutputDigest(output.String())}
	if verifyErr != nil || after != r.Revision || executableErr != nil {
		reply.Decision = "result-unverified"
		reply.Reason = "checkout provenance changed or could not be verified"
		reply.Result.Revision = after
	}
	return reply, nil
}
