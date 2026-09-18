package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/spf13/cobra"
)

func TestCapacityReceiverChild(t *testing.T) {
	if os.Getenv("CAPACITY_TEST_CHILD") != "1" {
		t.Skip("subprocess endpoint")
	}
	root := &cobra.Command{Use: "dag"}
	AddCapacityCommands(root, CapacityServices{Identity: func(_ context.Context, pid int) (string, error) {
		boot := os.Getenv("CAPACITY_TEST_BOOT")
		if boot == "" {
			boot = "boot-a"
		}
		return fmt.Sprintf("%s:%d", boot, pid), nil
	}, Probe: func(ctx context.Context, worker string) (CapacityObservation, error) {
		freeCPU := 4.0
		if value := os.Getenv("CAPACITY_TEST_FREE_CPU"); value != "" {
			freeCPU, _ = strconv.ParseFloat(value, 64)
		}
		return CapacityObservation{Facts: HostFacts{SchemaVersion: 1, Worker: worker, OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: 4, MemBytes: 4096, Venues: []string{VenueUserland}, ObservedAt: time.Now()}, FreeCPU: freeCPU, FreeMemory: 4096, HeadroomKnown: true}, nil
	}, Observe: func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"schema_version":"fixture-monitor","host":"logical"}`), nil
	}})
	root.SetArgs([]string{"capacity", "receive"})
	if e := root.Execute(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	os.Exit(0)
}
func capacityFixture(t *testing.T) (*CapacityClient, CapacityRequest) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SSH shell fixture")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CAPACITY_TEST_CHILD", "1")
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0700)
	for _, args := range [][]string{{"init", "-q", work}, {"-C", work, "-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "--allow-empty", "-qm", "fixture"}} {
		if out, e := exec.Command("git", args...).CombinedOutput(); e != nil {
			t.Fatalf("fixture git: %s %v", out, e)
		}
	}
	revision, e := capacityCheckoutRevision(context.Background(), work)
	if e != nil {
		t.Fatal(e)
	}
	shellPath, e := exec.LookPath("sh")
	if e != nil {
		t.Fatal(e)
	}
	// Preserve the authorized executable alias: on Linux sh commonly resolves
	// to dash, while the request deliberately calls the declared sh name.
	shellBytes, e := os.ReadFile(shellPath)
	if e != nil {
		t.Fatal(e)
	}
	target := CapacityTarget{Executables: []CapacityExecutable{{Path: shellPath, SHA256: capacityOutputDigest(string(shellBytes))}}, Name: "fixture", Worker: "worker-fixture", Observe: true, Dispatch: true, DataClasses: []string{"fixture-public"}, Workspace: work, Revision: revision, Toolchain: capacityRuntimeToolchain(), Slots: 1, MemoryBytes: 4096}
	policy := &CapacityPolicy{Version: 1, LocalWorker: target.Worker, Targets: []CapacityTarget{target}}
	p := filepath.Join(dir, "policy.json")
	b, _ := json.Marshal(policy)
	os.WriteFile(p, b, 0600)
	t.Setenv("BASHY_REMOTE_CAPACITY_POLICY", p)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0700)
	os.WriteFile(filepath.Join(bin, "bashy"), []byte("#!/bin/sh\nexec "+shellQuote(os.Args[0])+" -test.run '^TestCapacityReceiverChild$'\n"), 0700)
	ssh := filepath.Join(bin, "fixture-ssh")
	os.WriteFile(ssh, []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := &CapacityClient{Policy: policy, Resolve: func(name string) (fleet.Host, bool) { return fleet.Host{Name: name}, name == "fixture" }, Transport: func(h fleet.Host) Transport { x := NewSSHTransport(h); x.Command = ssh; return x }}
	request := CapacityRequest{Version: 1, ID: "run-a", Spec: TaskSpec{SchemaVersion: 1, Task: "fixture-task", Venue: VenueUserland, Distribution: DistributionSingle, CPUPerTask: 1, MemPerTask: 128, Timeout: 5 * time.Second, Match: map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH}}, Body: "printf 'verified fixture output\\n'", DataClass: "fixture-public", Revision: revision, Toolchain: capacityRuntimeToolchain()}
	return client, request
}
func TestCapacityAuthorizedSSHDispatchVerifiesResult(t *testing.T) {
	c, r := capacityFixture(t)
	monitor, e := c.Observe(context.Background(), "fixture")
	if e != nil || !bytes.Contains(monitor, []byte("fixture-monitor")) {
		t.Fatalf("remote inspection: %s %v", monitor, e)
	}
	plan, e := c.Plan(context.Background(), r)
	if e != nil || plan.Decision != "remote-ready" {
		t.Fatalf("plan: %+v %v", plan, e)
	}
	result, e := c.Dispatch(context.Background(), r)
	if e != nil || result.Decision != "completed" || result.Result.Record.Status != RunPassed || result.Result.Output != "verified fixture output\n" {
		t.Fatalf("dispatch: %+v %v", result, e)
	}
	result.Result.Output += "tamper"
	if e := verifyCapacityResult(c.Policy.Targets[0], r, result.Result); e == nil {
		t.Fatal("artifact mismatch accepted")
	}
	result.Result.Output = "verified fixture output\n"
	result.Result.Revision = "wrong"
	if e := verifyCapacityResult(c.Policy.Targets[0], r, result.Result); e == nil {
		t.Fatal("wrong revision accepted")
	}
}
func TestCapacityDenialIncompatibilityAndUnreachableRemainQueued(t *testing.T) {
	c, r := capacityFixture(t)
	for _, mode := range []string{"denied", "data", "platform", "toolchain", "unregistered", "unreachable"} {
		t.Run(mode, func(t *testing.T) {
			clone := *c
			policy := *c.Policy
			policy.Targets = append([]CapacityTarget(nil), c.Policy.Targets...)
			clone.Policy = &policy
			request := r
			request.Spec.Match = map[string]string{"os": runtime.GOOS}
			switch mode {
			case "denied":
				policy.Targets[0].Dispatch = false
			case "data":
				request.DataClass = "private-disallowed"
			case "platform":
				request.Spec.Match["os"] = "unavailable-platform"
			case "toolchain":
				request.Toolchain = "wrong"
			case "unregistered":
				clone.Resolve = func(string) (fleet.Host, bool) { return fleet.Host{}, false }
			case "unreachable":
				clone.Transport = func(h fleet.Host) Transport {
					x := NewSSHTransport(h)
					x.Command = filepath.Join(t.TempDir(), "missing-ssh")
					return x
				}
			}
			plan, e := clone.Plan(context.Background(), request)
			if e != nil || plan.Decision != "queued-local" || len(plan.Refusals) == 0 {
				t.Fatalf("%s unexpectedly dispatched: %+v %v", mode, plan, e)
			}
		})
	}
	c.Policy.Targets[0].Observe = false
	if _, e := c.Observe(context.Background(), "fixture"); e == nil {
		t.Fatal("dispatch permission authorized observation")
	}
}
func TestCapacityTargetSideRacingClaimsCannotOverbook(t *testing.T) {
	c, r := capacityFixture(t)
	c.Policy.Targets[0].Slots = 8
	b, _ := json.Marshal(c.Policy)
	if e := os.WriteFile(CapacityPolicyPath(), b, 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("CAPACITY_TEST_FREE_CPU", "1")
	r.Body = "printf start; sleep 0.3; printf done"
	var wg sync.WaitGroup
	out := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := r
			request.ID = fmt.Sprintf("race-%d", i)
			target := c.Policy.Targets[0]
			reply, e := c.exchange(context.Background(), target, capacityWire{Operation: "execute", Worker: target.Worker, Request: request})
			if e != nil {
				errs <- e
				return
			}
			out <- reply.Decision
		}(i)
	}
	wg.Wait()
	close(out)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	completed, queued := 0, 0
	for decision := range out {
		if decision == "completed" {
			completed++
		}
		if decision == "queued-local" {
			queued++
		}
	}
	if completed != 1 || queued != 1 {
		t.Fatalf("target admitted races: completed=%d queued=%d", completed, queued)
	}
}
func TestCapacityFreshnessAndOutputBounds(t *testing.T) {
	c, r := capacityFixture(t)
	_ = c
	o := CapacityObservation{Facts: HostFacts{SchemaVersion: 1, Worker: "w", OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: 4, MemBytes: 4096, Venues: []string{VenueUserland}, ObservedAt: time.Now().Add(time.Minute)}, FreeCPU: 4, FreeMemory: 4096, HeadroomKnown: true, Revision: r.Revision, Toolchain: r.Toolchain}
	if capacityCompatible(&o, r, time.Now()) {
		t.Fatal("future facts accepted")
	}
	o.Facts.ObservedAt = time.Now().Add(-time.Minute)
	if capacityCompatible(&o, r, time.Now()) {
		t.Fatal("stale facts accepted")
	}
	b := &limitedCapacityBuffer{limit: 3}
	if _, e := io.WriteString(b, "four"); e == nil || b.Len() != 0 {
		t.Fatal("output budget not enforced")
	}
	if decodeCapacity(strings.NewReader(`{"version":1} {}`), new(CapacityPolicy)) == nil {
		t.Fatal("trailing policy accepted")
	}
}

func TestCapacityConstrainedProofRejectsEscapeSyntax(t *testing.T) {
	for _, body := range []string{"eval 'sleep 1 &'", "source ./script", "printf '%s' $(sh -c true)", "printf '%s' <(sh -c true)", "f(){ sleep 1; }; f", "sleep 1 &", "X=x printf hi", "printf '%s' $PATH"} {
		if capacityBoundedBuiltinBody(body) {
			t.Fatalf("escape syntax accepted: %s", body)
		}
	}
	if !capacityBoundedBuiltinBody("printf hello; sleep 0.01; echo done") {
		t.Fatal("constrained builtins refused")
	}
}
func TestCapacityExternalDescendantsRetainUntilVerifiedBootReconciliation(t *testing.T) {
	c, r := capacityFixture(t)
	r.ID = "retained"
	r.Executables = append([]CapacityExecutable(nil), c.Policy.Targets[0].Executables...)
	r.Body = "sh -c 'sleep 0.1 >/dev/null 2>&1 &'"
	result, e := c.Dispatch(context.Background(), r)
	if e != nil || result.Decision != "completed" || !result.CapacityHeld || !strings.Contains(result.Reason, "reconcile") {
		t.Fatalf("escaped descendants falsely released: %+v %v", result, e)
	}
	reply, e := c.Reconcile(context.Background(), "fixture", r.ID)
	if e != nil || reply.Decision != "retained" {
		t.Fatalf("unverified reconciliation: %+v %v", reply, e)
	}
	time.Sleep(150 * time.Millisecond)
	// Deterministic receiver boot-identity fixture, not a real host reboot.
	t.Setenv("CAPACITY_TEST_BOOT", "boot-b")
	reply, e = c.Reconcile(context.Background(), "fixture", r.ID)
	if e != nil || reply.Decision != "reconciled" {
		t.Fatalf("verified boot did not release: %+v %v", reply, e)
	}
	reply, e = c.Reconcile(context.Background(), "fixture", r.ID)
	if e != nil || reply.Decision != "reconciled" {
		t.Fatalf("reconcile replay failed: %+v %v", reply, e)
	}
}
func TestCapacityUntrackedCheckoutAndRuntimeArtifactMismatchRefused(t *testing.T) {
	c, r := capacityFixture(t)
	work := c.Policy.Targets[0].Workspace
	os.WriteFile(filepath.Join(work, "untracked"), []byte("override"), 0600)
	plan, e := c.Plan(context.Background(), r)
	if e != nil || plan.Decision != "queued-local" {
		t.Fatalf("untracked checkout eligible: %+v %v", plan, e)
	}
	os.Remove(filepath.Join(work, "untracked"))
	r.Body = "printf changed > untracked"
	result, e := c.Dispatch(context.Background(), r)
	if e != nil || result.Decision != "result-unverified" {
		t.Fatalf("post-execution mismatch lost structured result: %+v %v", result, e)
	}
}

func TestCapacityTimeoutKeepsStructuredOutcomeAndQueueFallbackPersists(t *testing.T) {
	c, r := capacityFixture(t)
	r.Spec.Timeout = 50 * time.Millisecond
	r.Body = "sleep 1"
	result, e := c.Dispatch(context.Background(), r)
	if e != nil || result.Decision != "completed" || result.Result.Record.Status == RunPassed {
		t.Fatalf("timeout lost result envelope: %+v %v", result, e)
	}
	r.ID = "queued"
	r.DataClass = "not-authorized"
	result, e = c.Dispatch(context.Background(), r)
	if e != nil || result.Decision != "queued-local" {
		t.Fatalf("fallback: %+v %v", result, e)
	}
	path := filepath.Join(filepath.Dir(CapacityPolicyPath()), "remote-capacity", "pending", capacityOutputDigest(r.ID)+".json")
	f, e := os.Open(path)
	if e != nil {
		t.Fatal("fallback did not persist local queue", e)
	}
	defer f.Close()
	var row capacityQueuedRequest
	if e = decodeCapacity(f, &row); e != nil || row.Request.ID != r.ID {
		t.Fatal("bad queued request", e)
	}
}

func TestCapacityDeclaredExecutableProofPinsPathAndDetectsChangedInventory(t *testing.T) {
	c, r := capacityFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "compiler")
	script := "#!/bin/sh\nprintf verified-compiler\n"
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	r.Executables = []CapacityExecutable{{Path: path, SHA256: capacityOutputDigest(script)}}
	r.Body = "compiler"
	r.ID = "declared-compiler"
	c.Policy.Targets[0].Executables = append([]CapacityExecutable(nil), r.Executables...)
	b, _ := json.Marshal(c.Policy)
	os.WriteFile(CapacityPolicyPath(), b, 0600)
	shadowDir := t.TempDir()
	os.WriteFile(filepath.Join(shadowDir, "compiler"), []byte("#!/bin/sh\nprintf wrong-PATH-compiler\n"), 0700)
	t.Setenv("PATH", shadowDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, e := c.Dispatch(context.Background(), r)
	if e != nil || result.Decision != "completed" || result.Result.Output != "verified-compiler" || !capacitySameInventory(r.Executables, result.Result.Executables) {
		t.Fatalf("declared direct compiler not verified/pinned: %+v %v", result, e)
	}
	// Receiver fingerprints the current file again before accepting another job.
	os.WriteFile(path, []byte(script+"# changed\n"), 0700)
	r.ID = "changed-compiler"
	plan, e := c.Plan(context.Background(), r)
	if e != nil || plan.Decision != "queued-local" {
		t.Fatalf("changed declared compiler remained eligible: %+v %v", plan, e)
	}
}
func TestCapacityExternalWithoutDeclaredInventoryQueuesAndDynamicCallsRefuse(t *testing.T) {
	c, r := capacityFixture(t)
	r.Body = "go version"
	r.Executables = nil
	plan, e := c.Plan(context.Background(), r)
	if e != nil || plan.Decision != "queued-local" || !strings.Contains(plan.Reason, "executable") {
		t.Fatalf("runtime-only external toolchain accepted: %+v %v", plan, e)
	}
	inventory := c.Policy.Targets[0].Executables
	for _, body := range []string{"$SHELL -c true", "eval 'sh -c true'", "unlisted-tool"} {
		if _, e := capacityPinnedBody(body, inventory); e == nil {
			t.Fatalf("unverified direct call accepted: %s", body)
		}
	}
}

func TestCapacityVerifiedDispatchConsumesMatchingLocalPendingRequest(t *testing.T) {
	c, r := capacityFixture(t)
	resolve := c.Resolve
	c.Resolve = func(string) (fleet.Host, bool) { return fleet.Host{}, false }
	plan, e := c.Dispatch(context.Background(), r)
	if e != nil || plan.Decision != "queued-local" {
		t.Fatalf("fallback: %+v %v", plan, e)
	}
	path := filepath.Join(filepath.Dir(CapacityPolicyPath()), "remote-capacity", "pending", capacityOutputDigest(r.ID)+".json")
	if _, e = os.Stat(path); e != nil {
		t.Fatal(e)
	}
	c.Resolve = resolve
	result, e := c.Dispatch(context.Background(), r)
	if e != nil || result.Decision != "completed" {
		t.Fatalf("retry: %+v %v", result, e)
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("completed request remained falsely pending", e)
	}
}

func TestCapacityDeclaredShellAliasPreservesPolicyNameAndDigest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell alias fixture; guarded execution is unsupported on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dash")
	alias := filepath.Join(dir, "sh")
	body := []byte("#!/bin/sh\nprintf fixture\n")
	if e := os.WriteFile(target, body, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, alias); e != nil {
		t.Fatal(e)
	}
	declared := []CapacityExecutable{{Path: alias, SHA256: capacityOutputDigest(string(body))}}
	verified, e := VerifyCapacityExecutables(context.Background(), declared)
	if e != nil || verified[0].Path != alias {
		t.Fatalf("declared alias identity changed: %+v %v", verified, e)
	}
	pinned, e := capacityPinnedBody("sh -c true", declared)
	if e != nil || !strings.Contains(pinned, alias) {
		t.Fatalf("declared sh alias was lost to dash basename: %s %v", pinned, e)
	}
}
