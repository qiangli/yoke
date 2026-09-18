// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// chunkedSuite is a corpus of four chunks plus the phony aggregator the matrix
// expansion creates. Each chunk writes the case list it was handed, so a run's
// output is a direct record of what membership reached the workers.
const chunkedSuite = "## Tasks\n\n" +
	"### suite\nMatrix: shard=1,2,3,4\nArtifacts: chunk-*.out\n" +
	"```bash\n" +
	"printf '%s' \"$DAG_CHUNK_MEMBERS\" > chunk-$DAG_CHUNK_ID.out\n" +
	"```\n"

var chunkedManifest = &ChunkManifest{SchemaVersion: 1, Suite: "demo", ChunkCount: 4, Chunks: []Chunk{
	{ID: 1, Fixtures: []Fixture{{Name: "jobs"}}},
	{ID: 2, Fixtures: []Fixture{{Name: "trap"}}},
	{ID: 3, Fixtures: []Fixture{{Name: "func"}, {Name: "heredoc"}}},
	{ID: 4, Fixtures: []Fixture{{Name: "array"}, {Name: "glob"}, {Name: "errors"}}},
}}

// chunkedEngine parses the suite, expands the matrix, binds the committed
// membership, and returns an engine over the resulting graph.
func chunkedEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	d := doc(t, chunkedSuite)
	d.expandMatrix()
	if err := BindChunks(d, chunkedManifest); err != nil {
		t.Fatalf("BindChunks: %v", err)
	}
	g, err := BuildGraph(d)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return &Engine{
		Graph: g, Dir: dir, Env: os.Environ(),
		Concurrency: 1, FailFast: true, Capture: true,
		Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer),
	}
}

// runOutcome is the comparable shape of a run: everything a caller can observe
// except wall-clock time, which is the one thing a fleet is supposed to change.
type runOutcome struct {
	Tasks     []string
	Artifacts map[string]string
}

func outcomeOf(t *testing.T, e *Engine, dir string, target string) runOutcome {
	t.Helper()
	report, err := e.Run(context.Background(), target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Fatalf("run failed: %+v", report.Results)
	}

	out := runOutcome{Artifacts: map[string]string{}}
	for _, r := range report.Results {
		out.Tasks = append(out.Tasks, fmt.Sprintf("%s=%s/%d", r.Name, r.Status, r.ExitCode))
	}
	entries, err := filepath.Glob(filepath.Join(dir, "chunk-*.out"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out.Artifacts[filepath.Base(path)] = string(data)
	}
	return out
}

// The gate: a local Pool running the chunks must produce exactly what the serial
// run produces. Same targets, same statuses, same per-chunk case lists. The pool
// changes who runs the work and how much of it runs at once — never what the
// work is or what it concludes.
func TestLocalPoolRunEqualsSerialRun(t *testing.T) {
	serialDir := t.TempDir()
	serial := chunkedEngine(t, serialDir)
	want := outcomeOf(t, serial, serialDir, "suite")

	if len(want.Artifacts) != 4 {
		t.Fatalf("serial run produced %d chunk artifacts, want 4", len(want.Artifacts))
	}

	// Every fleet size must agree with the serial run, including N=1: one host
	// is a valid fleet, and one worker is an ordinary value of the parameter.
	for _, workers := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			dir := t.TempDir()
			e := chunkedEngine(t, dir)
			e.Fleet = true
			e.Pool = LocalPool(workers)

			got := outcomeOf(t, e, dir, "suite")
			if !equalOutcome(got, want) {
				t.Errorf("fleet(%d) outcome differs from serial:\n got %+v\nwant %+v", workers, got, want)
			}
		})
	}
}

// Degrade-to-today: --fleet with no pool configured is LocalPool(Concurrency) —
// one in-process worker with N slots — and must not change what -j N already did.
func TestFleetWithNoPoolConfiguredMatchesDashJ(t *testing.T) {
	jDir := t.TempDir()
	j := chunkedEngine(t, jDir)
	j.Concurrency = 4
	want := outcomeOf(t, j, jDir, "suite")

	fleetDir := t.TempDir()
	f := chunkedEngine(t, fleetDir)
	f.Concurrency = 4
	f.Fleet = true // Pool stays nil

	got := outcomeOf(t, f, fleetDir, "suite")
	if !equalOutcome(got, want) {
		t.Errorf("--fleet outcome differs from -j 4:\n got %+v\nwant %+v", got, want)
	}
	if f.pool != nil {
		t.Error("run-scoped pool leaked past Run")
	}
}

// Artifacts must not carry the identity of the machine that produced them. A
// verdict stamped with a hostname cannot be compared against one produced
// anywhere else, and the worker identity a chunk sees is a logical venue name
// precisely so that no host fact has a path into the result.
func TestFleetArtifactsCarryNoHostFacts(t *testing.T) {
	dir := t.TempDir()
	md := "## Tasks\n\n### chunk\nArtifacts: verdict.json\n" +
		"```bash\n" +
		"printf '{\"worker\":\"%s\",\"venue\":\"%s\",\"members\":\"%s\"}' " +
		"\"$DAG_FLEET_WORKER\" \"$DAG_FLEET_VENUE\" \"$DAG_CHUNK_MEMBERS\" > verdict.json\n" +
		"```\n"

	e := engineFor(t, dir, md)
	e.Capture = true
	e.Fleet = true
	e.Pool = LocalPool(2)

	report, err := e.Run(context.Background(), "chunk")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Failed {
		t.Fatalf("run failed: %+v", report.Results)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "verdict.json"))
	if err != nil {
		t.Fatal(err)
	}
	var verdict struct {
		Worker string `json:"worker"`
		Venue  string `json:"venue"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		t.Fatalf("verdict is not valid JSON: %v\n%s", err, raw)
	}

	// The worker identity is a venue, not a machine.
	if verdict.Worker != LocalWorkerID || verdict.Venue != VenueUserland {
		t.Errorf("worker/venue = %q/%q, want %q/%q", verdict.Worker, verdict.Venue, LocalWorkerID, VenueUserland)
	}
	if strings.Contains(verdict.Worker, runtime.GOOS) || strings.Contains(verdict.Worker, runtime.GOARCH) {
		t.Errorf("worker id %q carries host facts", verdict.Worker)
	}

	// Nothing the engine hands back may carry them either.
	assertNoHostFacts(t, "artifact", string(raw))
	for _, r := range report.Results {
		assertNoHostFacts(t, "result stdout", r.Stdout)
		assertNoHostFacts(t, "result stderr", r.Stderr)
		assertNoHostFacts(t, "result host field", r.Host)
	}
}

// assertNoHostFacts fails if s carries this machine's identity. Short probes are
// skipped: a 3-character hostname would match half the alphabet and make the
// check flaky rather than strict.
func assertNoHostFacts(t *testing.T, what, s string) {
	t.Helper()
	if s == "" {
		return
	}
	hay := strings.ToLower(s)

	probes := map[string]string{}
	if h, err := os.Hostname(); err == nil {
		for _, part := range strings.Split(h, ".") { // "box.example.com" and "box.local"
			if len(part) >= 5 && !strings.EqualFold(part, "local") {
				probes["hostname"] = part
			}
		}
	}
	// Ephemeral CI accounts (GitHub Actions runs as "runner", Windows runners as
	// "runneradmin") are not a personal or machine identity — every runner shares
	// the name, so it fingerprints nothing, and it collides with the common word
	// (a manifest's "runner": "<harness>" field, a "test runner", …). Treating it
	// as a host fact turns every CI run into a false positive; skip it. Real host
	// paths still surface through the hostname and home-directory probes.
	genericCIUser := map[string]bool{"runner": true, "runneradmin": true}
	if u, err := user.Current(); err == nil && len(u.Username) >= 4 && !genericCIUser[strings.ToLower(u.Username)] {
		probes["username"] = u.Username
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) >= 5 {
		probes["home directory"] = home
	}

	for kind, val := range probes {
		if strings.Contains(hay, strings.ToLower(val)) {
			t.Errorf("%s leaked %s %q:\n%s", what, kind, val, s)
		}
	}
}

// The anti-hang guard. A task no worker could ever host must fail fast with a
// reason, exactly as a missing tool does — not wait forever for a slot that will
// never qualify. A one-worker pool is not a special case here.
func TestPoolFailsFastWhenNoWorkerOffersTheVenue(t *testing.T) {
	pool := NewPool(localTransport{}, &Worker{
		ID:     "container-only",
		Venues: []string{VenueSandbox},
		CPU:    4,
	})

	if pool.Eligible(Constraints{Venue: VenueUserland}) {
		t.Fatal("Eligible said yes for a venue no worker offers")
	}
	if !pool.Eligible(Constraints{Venue: VenueSandbox}) {
		t.Fatal("Eligible said no for the venue the worker does offer")
	}

	// If this hangs instead of failing, the test times out — which is the bug
	// the guard exists to prevent, caught in the most direct way available.
	done := make(chan TaskResult, 1)
	go func() {
		done <- pool.Exec(context.Background(), Constraints{Venue: VenueUserland},
			&Task{Name: "chunk"}, TaskIO{Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	}()

	select {
	case res := <-done:
		if res.Status != StatusFailed {
			t.Fatalf("status = %v, want failed", res.Status)
		}
		if res.Err == nil || !strings.Contains(res.Err.Error(), "no worker offers venue=userland") {
			t.Fatalf("err = %v, want it to name the unsatisfiable venue", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec hung on an unplaceable task instead of failing fast")
	}
}

// The engine surfaces the same failure rather than deadlocking the scheduler.
func TestEngineFailsFastOnUnplaceableTask(t *testing.T) {
	dir := t.TempDir()
	e := engineFor(t, dir, "## Tasks\n\n### chunk\n"+block("bash", "echo hi"))
	e.Capture = true
	e.Fleet = true
	e.Pool = NewPool(localTransport{}, &Worker{ID: "container-only", Venues: []string{VenueSandbox}, CPU: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	report, err := e.Run(ctx, "chunk")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Failed {
		t.Fatal("run succeeded on a pool that cannot host the task")
	}
	if got := report.Results[0].Err; got == nil || !strings.Contains(got.Error(), "no worker offers") {
		t.Fatalf("err = %v, want a no-eligible-worker failure", got)
	}
}

// Slots are gated by memory as often as by cores: a 16 GB / 10-core host running
// a suite with a 4 GB/task watchdog offers 4 slots, not 10.
func TestPoolSlotsAreMemoryGated(t *testing.T) {
	w := &Worker{ID: "big", Venues: []string{VenueUserland}, CPU: 10, MemBytes: 16 << 30}
	pool := NewPool(localTransport{}, w)

	if got := pool.Slots(w, 4<<30); got != 4 {
		t.Errorf("slots at 4GB/task = %d, want 4 (memory-gated, not core-gated)", got)
	}
	if got := pool.Slots(w, 1<<30); got != 10 {
		t.Errorf("slots at 1GB/task = %d, want 10 (core-gated)", got)
	}
	if got := pool.Slots(w, 0); got != 10 {
		t.Errorf("slots with no memory declaration = %d, want 10", got)
	}
	// A task needing more memory than the worker has can never be placed there.
	if pool.Eligible(Constraints{Venue: VenueUserland, MemPerTask: 32 << 30}) {
		t.Error("Eligible said yes for a task larger than the worker's memory")
	}
}

// TryAcquire is non-blocking on purpose: when the longest ready chunk cannot be
// placed, the scheduler must be free to try the next-longest instead of
// committing to a task it cannot start.
func TestTryAcquireIsNonBlockingWhenFull(t *testing.T) {
	pool := LocalPool(1)
	c := Constraints{Venue: VenueUserland}

	w, release := pool.TryAcquire(c)
	if w == nil {
		t.Fatal("first TryAcquire found no slot")
	}
	if got, _ := pool.TryAcquire(c); got != nil {
		t.Fatal("TryAcquire handed out a second slot from a 1-slot pool")
	}
	release()
	if got, _ := pool.TryAcquire(c); got == nil {
		t.Fatal("TryAcquire found no slot after release")
	}
}

// An exclusive task never shares its worker, even when slots are free —
// co-scheduling perturbs a timing measurement and invalidates a certification.
func TestExclusiveTaskDrainsItsWorker(t *testing.T) {
	pool := LocalPool(4)
	shared := Constraints{Venue: VenueUserland}
	alone := Constraints{Venue: VenueUserland, Exclusive: true}

	w, release := pool.TryAcquire(alone)
	if w == nil {
		t.Fatal("exclusive TryAcquire found no worker")
	}
	if got, _ := pool.TryAcquire(shared); got != nil {
		t.Error("a shared task co-scheduled onto a drained worker")
	}
	release()

	// And the reverse: one ordinary task in flight blocks an exclusive one.
	w, release = pool.TryAcquire(shared)
	if w == nil {
		t.Fatal("shared TryAcquire found no worker")
	}
	if got, _ := pool.TryAcquire(alone); got != nil {
		t.Error("an exclusive task landed on a worker that was already busy")
	}
	release()
	if got, _ := pool.TryAcquire(alone); got == nil {
		t.Error("exclusive task could not acquire an idle worker")
	}
}

// Capacity is the sum of the workers' slots, and it is what the engine sizes its
// admission gate from. Two workers of different widths is the case a single
// global semaphore cannot express — which is why the pool exists.
func TestPoolCapacitySpansWorkers(t *testing.T) {
	pool := NewPool(localTransport{},
		&Worker{ID: "a", Venues: []string{VenueUserland}, CPU: 4},
		&Worker{ID: "b", Venues: []string{VenueUserland}, CPU: 8},
	)
	if got := pool.Capacity(); got != 12 {
		t.Errorf("capacity = %d, want 12", got)
	}

	// All 12 slots are real: acquire them all, then confirm the 13th is refused.
	var releases []func()
	for i := 0; i < 12; i++ {
		w, release := pool.TryAcquire(Constraints{Venue: VenueUserland})
		if w == nil {
			t.Fatalf("slot %d of 12 was refused", i+1)
		}
		releases = append(releases, release)
	}
	if w, _ := pool.TryAcquire(Constraints{Venue: VenueUserland}); w != nil {
		t.Error("pool handed out a 13th slot")
	}
	for _, release := range releases {
		release()
	}
}

// The pool must not admit more work at once than it has slots. This is the
// property the WIP's per-task pool construction silently lost: a pool rebuilt on
// every dispatch hands out a fresh set of slots each time and gates nothing.
func TestPoolNeverExceedsItsSlotCount(t *testing.T) {
	const slots = 3
	pool := LocalPool(slots)

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	pool.SetDefaultTransport(countingTransport{
		enter: func() {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
		},
		leave: func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.Exec(context.Background(), Constraints{Venue: VenueUserland},
				&Task{Name: "chunk"}, TaskIO{})
		}()
	}
	wg.Wait()

	if peak > slots {
		t.Errorf("peak concurrency = %d, want at most %d slots", peak, slots)
	}
	if peak == 0 {
		t.Fatal("no task ran")
	}
}

// The engine must hold ONE pool for the whole run. A pool rebuilt per dispatch
// would hand every task a fresh, fully-idle set of workers — each worker would
// then accept unbounded concurrent work while still reporting the right total
// capacity, so nothing but per-worker accounting can catch it.
func TestEngineHoldsOnePoolAcrossTheRun(t *testing.T) {
	var (
		mu    sync.Mutex
		live  = map[string]int{}
		peak  = map[string]int{}
		tasks = "## Tasks\n\n### fan\nRequires: " + strings.Join(chunkNames(8), ", ") + "\n"
	)
	for _, name := range chunkNames(8) {
		tasks += "### " + name + "\n" + block("bash", "true")
	}

	track := countingTransport{}
	pool := NewPool(track, // two single-slot workers: 2 slots total, 1 each
		&Worker{ID: "worker-a", Venues: []string{VenueUserland}, CPU: 1},
		&Worker{ID: "worker-b", Venues: []string{VenueUserland}, CPU: 1},
	)
	track.onWorker = func(id string, delta int) {
		mu.Lock()
		live[id] += delta
		if live[id] > peak[id] {
			peak[id] = live[id]
		}
		mu.Unlock()
	}
	pool.transport = track // replace: SetDefaultTransport only fills a nil one

	dir := t.TempDir()
	e := engineFor(t, dir, tasks)
	e.Capture = true
	e.Concurrency = 8 // deliberately wider than the pool
	e.Fleet = true
	e.Pool = pool

	if _, err := e.Run(context.Background(), "fan"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(peak) == 0 {
		t.Fatal("no task reached a worker")
	}
	for id, n := range peak {
		if n > 1 {
			t.Errorf("worker %s ran %d tasks at once, but offers 1 slot "+
				"(the engine is not holding a single pool across the run)", id, n)
		}
	}
}

// The same property on the default path, where the engine builds the pool
// itself (`--fleet` with no pool configured — what a real invocation does). The
// pool must be built ONCE per run: rebuilt per dispatch it hands every task a
// fresh, fully-idle worker, and since it is now the only gate, concurrency goes
// unbounded.
func TestEngineBuiltPoolGatesConcurrency(t *testing.T) {
	const slots = 2

	var (
		mu   sync.Mutex
		live int
		peak int
	)
	counting := executorFunc(func(ctx context.Context, task *Task, io TaskIO) TaskResult {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()

		time.Sleep(3 * time.Millisecond)

		mu.Lock()
		live--
		mu.Unlock()
		return TaskResult{Name: task.Name, Status: StatusDone}
	})

	md := "## Tasks\n\n### fan\nRequires: " + strings.Join(chunkNames(12), ", ") + "\n"
	for _, name := range chunkNames(12) {
		md += "### " + name + "\n" + block("bash", "true")
	}

	dir := t.TempDir()
	e := engineFor(t, dir, md)
	e.Capture = true
	e.Concurrency = slots
	e.Fleet = true
	e.Executor = counting // reached through the pool's default local transport
	// e.Pool stays nil: the engine builds LocalPool(Concurrency).

	if _, err := e.Run(context.Background(), "fan"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak == 0 {
		t.Fatal("no task reached the executor: the pool is not using the engine's executor")
	}
	if peak > slots {
		t.Errorf("peak concurrency = %d, want at most %d — the pool is not gating "+
			"(is it being rebuilt per dispatch?)", peak, slots)
	}
}

// executorFunc adapts a func to the Executor seam.
type executorFunc func(context.Context, *Task, TaskIO) TaskResult

func (f executorFunc) Execute(ctx context.Context, t *Task, io TaskIO) TaskResult {
	return f(ctx, t, io)
}

func chunkNames(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("chunk-%d", i))
	}
	return out
}

// countingTransport records how many tasks are in flight at once, overall and
// per worker.
type countingTransport struct {
	enter, leave func()
	onWorker     func(id string, delta int)
}

func (x countingTransport) Exec(ctx context.Context, w *Worker, t *Task, io TaskIO) TaskResult {
	if x.enter != nil {
		x.enter()
	}
	if x.onWorker != nil {
		x.onWorker(w.ID, 1)
	}
	time.Sleep(2 * time.Millisecond) // hold the slot long enough to overlap
	if x.onWorker != nil {
		x.onWorker(w.ID, -1)
	}
	if x.leave != nil {
		x.leave()
	}
	return TaskResult{Name: t.Name, Status: StatusDone}
}

func (x countingTransport) Close() error { return nil }

// End to end through the CLI: --fleet runs the chunks the committed manifest
// pins, and each one receives its own case list.
func TestCommandFleetRunsCommittedChunks(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // bodies run in the invoking cwd (make parity)
	dagPath := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(dagPath, []byte(chunkedSuite), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(chunkedManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chunks.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--fleet", "-j", "4", "--json", "--file", dagPath, "suite"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}

	var env struct {
		Status string `json:"status"`
		Result struct {
			Tasks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if env.Status != "ok" {
		t.Fatalf("status = %q\n%s", env.Status, out.String())
	}
	// Four chunks plus the phony aggregator.
	if len(env.Result.Tasks) != 5 {
		t.Fatalf("ran %d targets, want 5 (4 chunks + aggregator): %+v", len(env.Result.Tasks), env.Result.Tasks)
	}

	// The committed membership is what reached the workers.
	want := map[string]string{
		"chunk-1.out": "jobs",
		"chunk-2.out": "trap",
		"chunk-3.out": "func heredoc",
		"chunk-4.out": "array glob errors",
	}
	for name, members := range want {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("chunk artifact %s: %v", name, err)
		}
		if string(data) != members {
			t.Errorf("%s = %q, want %q", name, data, members)
		}
	}
	assertNoHostFacts(t, "--json envelope", out.String())
}

// A --chunks manifest that reaches no target is a silent truncation of the
// corpus, so the CLI must refuse it rather than run a subset.
func TestCommandRejectsManifestThatBindsNothing(t *testing.T) {
	dir := t.TempDir()
	dagPath := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(dagPath, []byte("## Tasks\n\n### build\n"+block("bash", "echo hi")), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "chunks.json")
	raw, err := json.Marshal(chunkedManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--fleet", "--json", "--file", dagPath, "--chunks", manifest, "build"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error for a manifest that binds no target")
	}
	if !strings.Contains(out.String()+errOut.String(), "no target declares a shard") {
		t.Errorf("error did not explain the mismatch:\n%s%s", out.String(), errOut.String())
	}
}

// An unchunked DAG file in a repo that happens to commit a chunks.json must keep
// working: a discovered manifest binds only when the document is actually
// sharded (an explicitly-named one still must bind, per the test above).
func TestDiscoveredManifestIgnoredForUnshardedFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // bodies run in the invoking cwd (make parity)
	dagPath := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(dagPath, []byte("## Tasks\n\n### build\n"+block("bash", "echo hi > built.txt")), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(chunkedManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chunks.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewDagCmd()
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--json", "--file", dagPath, "build"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "built.txt")); err != nil {
		t.Fatalf("unsharded build did not run: %v", err)
	}
}

func equalOutcome(a, b runOutcome) bool {
	if len(a.Tasks) != len(b.Tasks) || len(a.Artifacts) != len(b.Artifacts) {
		return false
	}
	for i := range a.Tasks {
		if a.Tasks[i] != b.Tasks[i] {
			return false
		}
	}
	for k, v := range a.Artifacts {
		if b.Artifacts[k] != v {
			return false
		}
	}
	return true
}
