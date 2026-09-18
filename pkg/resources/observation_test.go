package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func isolateObservation(t testing.TB) string {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BASHY_") {
			t.Setenv(key, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return filepath.Join(home, "resources")
}
func fixtureObservationSources(at *time.Time, calls *int) hostObservationSources {
	return hostObservationSources{now: func() time.Time { return *at }, system: func(context.Context) (*System, hostCPUSample, error) {
		*calls++
		return &System{At: *at, CPU: CPU{Source: "ticks-boot"}, Memory: Memory{TotalBytes: 100, UsedBytes: 50, AvailableBytes: 50}}, hostCPUSample{At: *at, Total: uint64(at.Unix()) * 100, Idle: uint64(at.Unix()) * 50, HasCPU: true}, nil
	}, processes: func(context.Context) ([]processSample, ObservationCoverage, error) {
		cpu := float64(at.Unix())
		rss := uint64(2048)
		return []processSample{{Identity: ProcessIdentity{PID: 1, StartID: "fixture:1"}, Name: "worker", CPUSeconds: &cpu, RSS: &rss, Source: "fixture"}}, ObservationCoverage{Complete: true, Seen: 1, Included: 1}, nil
	}, scan: scanObservationDirectory}
}
func TestHostObservationSharesRefreshAndIndependentScanTTL(t *testing.T) {
	dir := isolateObservation(t)
	at := time.Unix(1000, 0)
	calls := 0
	source := fixtureObservationSources(&at, &calls)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := HostObserveOptions{CacheDir: dir, ScanRoots: []ScanRoot{{Path: root}}}
	first, err := observeHost(context.Background(), opts, source)
	if err != nil {
		t.Fatal(err)
	}
	if first.Processes[0].CPU.Value != nil {
		t.Fatal("cold CPU fabricated")
	}
	second, err := observeHost(context.Background(), opts, source)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.ID != second.ID {
		t.Fatalf("duplicate refresh: calls=%d", calls)
	}
	at = at.Add(6 * time.Second)
	third, err := observeHost(context.Background(), opts, source)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || third.Processes[0].CPU.Value == nil || *third.Processes[0].CPU.Value != 100 {
		t.Fatalf("sample CPU missing: %+v", third.Processes)
	}
	cached, err := loadHostObservationCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cached.DirectoryScans != 1 {
		t.Fatal("5-second host refresh repeated 60-second scan")
	}
	source.system = func(context.Context) (*System, hostCPUSample, error) {
		return nil, hostCPUSample{}, errors.New("fixture source failed")
	}
	at = at.Add(6 * time.Second)
	failed, err := observeHost(context.Background(), opts, source)
	if err != nil {
		t.Fatal(err)
	}
	if failed.System == nil || !failed.Sections["memory"].Stale {
		t.Fatal("failed source erased healthy prior observation")
	}
}
func TestHostObservationContentionReturnsStaleAndCancellation(t *testing.T) {
	dir := isolateObservation(t)
	at := time.Unix(1000, 0)
	calls := 0
	source := fixtureObservationSources(&at, &calls)
	opts := HostObserveOptions{CacheDir: dir}
	if _, err := observeHost(context.Background(), opts, source); err != nil {
		t.Fatal(err)
	}
	at = at.Add(6 * time.Second)
	lock, err := lockfile.Acquire(filepath.Join(dir, ".host-refresh.lock"), lockfile.Holder{})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	got, err := observeHost(context.Background(), opts, source)
	if err != nil || !got.Refreshing || !got.Sections["cpu"].Stale || calls != 1 {
		t.Fatalf("contention %+v %v calls=%d", got, err, calls)
	}
	cold := t.TempDir()
	other, err := lockfile.Acquire(filepath.Join(cold, ".host-refresh.lock"), lockfile.Holder{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	got, err = observeHost(ctx, HostObserveOptions{CacheDir: cold}, source)
	if err != nil || got.Sections["cpu"].Kind != "unknown" || got.Sections["cache"].Reason == "" {
		t.Fatalf("cold timeout lost unknown state: %+v %v", got, err)
	}
}
func TestHostObservationAttributionRejectsPIDReuseAndDoubleCounting(t *testing.T) {
	isolateObservation(t)
	at := time.Now()
	p := func(pid, parent int, start string) ProcessObservation {
		return ProcessObservation{Identity: ProcessIdentity{PID: pid, StartID: start}, PPID: parent, CPU: observedValue(50.0, "fixture", at, time.Second), RSS: observedValue(uint64(10), "fixture", at, time.Second)}
	}
	rows := []ProcessObservation{p(1, 0, "a"), p(2, 1, "b"), p(3, 2, "c")}
	refs := []WorkloadRef{{ID: "parent", Root: rows[0].Identity}, {ID: "child", Root: rows[1].Identity}}
	attributed, work := AttributeProcesses(rows, refs)
	if attributed[2].WorkloadID != "child" || work[0].ProcessCount != 1 || work[1].ProcessCount != 2 {
		t.Fatalf("nested attribution %+v %+v", attributed, work)
	}
	refs[0].Root.StartID = "reused"
	refs = refs[:1]
	attributed, work = AttributeProcesses(rows, refs)
	if work[0].ProcessCount != 0 || attributed[0].Attribution != "unverified" {
		t.Fatal("PID reuse attributed")
	}
	refs = []WorkloadRef{{ID: "one", Root: rows[0].Identity}, {ID: "two", Root: rows[0].Identity}}
	attributed, _ = AttributeProcesses(rows, refs)
	if attributed[1].Attribution != "ambiguous" {
		t.Fatal("competing roots double-counted")
	}
}
func TestHostObservationScanBoundsSymlinksAndGrowth(t *testing.T) {
	isolateObservation(t)
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), make([]byte, 1000), 0600)
	os.WriteFile(filepath.Join(root, "data"), []byte("123"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	at := time.Now()
	scan := scanObservationDirectory(context.Background(), root, at, &directoryScanBudget{remaining: 100})
	if scan.Bytes.Value == nil || *scan.Bytes.Value != 3 || !scan.Coverage.Complete {
		t.Fatalf("scan followed link or lost data: %+v", scan)
	}
	partial := scanObservationDirectory(context.Background(), root, at, &directoryScanBudget{remaining: 1})
	if partial.Coverage.Complete {
		t.Fatal("entry budget not enforced")
	}
	os.WriteFile(filepath.Join(root, "data"), []byte("123456"), 0600)
	next := scanObservationDirectory(context.Background(), root, at.Add(time.Second), &directoryScanBudget{remaining: 100})
	directoryGrowth(&next, scan)
	if next.GrowthBytesPerSecond.Value == nil || *next.GrowthBytesPerSecond.Value != 3 {
		t.Fatal("growth unavailable")
	}
	next.GrowthBytesPerSecond.Value = nil
	directoryGrowth(&next, partial)
	if next.GrowthBytesPerSecond.Value != nil {
		t.Fatal("partial scan became valid growth baseline")
	}
}
func TestResourceAlertTransactionRollbackAndBounds(t *testing.T) {
	dir := isolateObservation(t)
	ctx := context.Background()
	ledger, err := UpdateAlertState(ctx, dir, func(s *AlertLedger) error { s.Entries["one"] = json.RawMessage(`{"pending":"stable-id"}`); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateAlertState(ctx, dir, func(s *AlertLedger) error { delete(s.Entries, "one"); return errors.New("abort") }); err == nil {
		t.Fatal("callback error accepted")
	}
	got, err := ReadAlertState(ctx, dir)
	if err != nil || got.Revision != ledger.Revision || len(got.Entries) != 1 {
		t.Fatalf("rollback failed: %+v %v", got, err)
	}
	if _, err := UpdateAlertState(ctx, dir, func(s *AlertLedger) error {
		s.Entries["huge"] = json.RawMessage(strconv.Quote(strings.Repeat("x", 8192)))
		return nil
	}); err == nil {
		t.Fatal("oversized entry published")
	}
}
func TestResourcesObservationChild(t *testing.T) {
	mode := os.Getenv("RESOURCE_OBSERVATION_CHILD")
	if mode == "" {
		t.Skip("subprocess fixture")
	}
	dir := os.Getenv("RESOURCE_OBSERVATION_DIR")
	if mode == "alert" {
		for i := 0; i < 10; i++ {
			started := time.Now()
			_, err := UpdateAlertState(context.Background(), dir, func(s *AlertLedger) error {
				var n int
				if b := s.Entries["n"]; len(b) > 0 {
					if err := json.Unmarshal(b, &n); err != nil {
						return err
					}
				}
				s.Entries["n"] = json.RawMessage(strconv.Itoa(n + 1))
				return nil
			})
			if err != nil {
				holder, held := lockfile.Owner(filepath.Join(dir, ".alerts.lock"))
				t.Fatalf("alert transaction %d/10 after %s: %v (lock held=%v holder=%+v)", i+1, time.Since(started), err, held, holder)
			}
		}
		return
	}
	at := time.Now()
	calls := 0
	source := fixtureObservationSources(&at, &calls)
	original := source.system
	source.system = func(ctx context.Context) (*System, hostCPUSample, error) {
		f, err := os.OpenFile(filepath.Join(dir, "refresh-count"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, hostCPUSample{}, err
		}
		_, err = f.WriteString("refresh\n")
		f.Close()
		if err != nil {
			return nil, hostCPUSample{}, err
		}
		time.Sleep(50 * time.Millisecond)
		return original(ctx)
	}
	if _, err := observeHost(context.Background(), HostObserveOptions{CacheDir: dir}, source); err != nil {
		t.Fatal(err)
	}
}
func TestHostObservationSeparateProcessesShareRefreshAndAlerts(t *testing.T) {
	dir := isolateObservation(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"host", "alert"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var cmds []*exec.Cmd
			var outputs []*bytes.Buffer
			defer func() {
				cancel()
				for _, cmd := range cmds {
					if cmd.ProcessState == nil {
						_ = cmd.Wait()
					}
				}
			}()
			for i := 0; i < 8; i++ {
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResourcesObservationChild$")
				cmd.Env = append(os.Environ(), "RESOURCE_OBSERVATION_CHILD="+mode, "RESOURCE_OBSERVATION_DIR="+dir)
				output := new(bytes.Buffer)
				cmd.Stdout, cmd.Stderr = output, output
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				cmds = append(cmds, cmd)
				outputs = append(outputs, output)
			}
			var failures []string
			for i, cmd := range cmds {
				if err := cmd.Wait(); err != nil {
					failures = append(failures, fmt.Sprintf("child %d failed: %v\n%s", i, err, outputs[i]))
				}
			}
			if len(failures) > 0 {
				t.Fatal(strings.Join(failures, "\n"))
			}
			if mode == "host" {
				b, err := os.ReadFile(filepath.Join(dir, "refresh-count"))
				if err != nil || strings.Count(string(b), "refresh") != 1 {
					t.Fatalf("duplicate native refresh: %q %v", b, err)
				}
			} else {
				state, err := ReadAlertState(context.Background(), dir)
				if err != nil || string(state.Entries["n"]) != "80" {
					t.Fatalf("lost concurrent transactions: %+v %v", state, err)
				}
			}
		})
	}
}
func TestHostObservationCountersRejectReset(t *testing.T) {
	isolateObservation(t)
	oldCPU, newCPU := 10.0, 1.0
	old := []processSample{{Identity: ProcessIdentity{PID: 1, StartID: "a"}, CPUSeconds: &oldCPU}}
	current := []processSample{{Identity: old[0].Identity, CPUSeconds: &newCPU}}
	processRates(current, old, 5)
	if current[0].CPUPercent != nil {
		t.Fatal("counter rollback invented CPU")
	}
	newCPU = 20
	current[0].Identity.StartID = "b"
	processRates(current, old, 5)
	if current[0].CPUPercent != nil {
		t.Fatal("PID reuse joined CPU samples")
	}
}

func TestHostObservationNativeIODeltasAndUnknownReset(t *testing.T) {
	isolateObservation(t)
	at := time.Unix(1000, 0)
	sys := &System{Disks: []Disk{{Mount: "/", Device: "/dev/sda"}}}
	sections := observationSections(sys, at)
	prior := hostCPUSample{At: at, Net: []observationNetCounter{{"eth0", 100, 200}}, Disk: []observationIOCounter{{"sda", 1000, 2000}}}
	current := hostCPUSample{At: at.Add(5 * time.Second), Net: []observationNetCounter{{"eth0", 150, 300}, {"new", 1, 1}}, Disk: []observationIOCounter{{"sda", 1500, 3000}}}
	applyObservationIO(sys, prior, current, sections, current.At)
	if sys.Network[0].RxBytesPerSec != 10 || sys.Disks[0].WriteBytesPerSec != 200 || sections["network.new.rates"].Kind != "unknown" || sections["disks.rates"].Kind != "actual" {
		t.Fatalf("bad native IO deltas: %+v %+v", sys, sections)
	}
	current.Net[0].RX = 1
	current.Disk[0].Read = 1
	sections = observationSections(sys, current.At)
	applyObservationIO(sys, prior, current, sections, current.At)
	if sections["network.eth0.rates"].Kind != "unknown" || sections["disks./.rates"].Kind != "unknown" {
		t.Fatal("reset counters were treated as healthy zero IO")
	}
}
func TestHostObservationFailedProcessSourceMarksEveryMetricStale(t *testing.T) {
	dir := isolateObservation(t)
	at := time.Unix(1000, 0)
	calls := 0
	source := fixtureObservationSources(&at, &calls)
	opts := HostObserveOptions{CacheDir: dir, Workloads: []WorkloadRef{{ID: "w", Root: ProcessIdentity{PID: 1, StartID: "fixture:1"}}}}
	if _, err := observeHost(context.Background(), opts, source); err != nil {
		t.Fatal(err)
	}
	at = at.Add(6 * time.Second)
	if _, err := observeHost(context.Background(), opts, source); err != nil {
		t.Fatal(err)
	}
	// Force explicit failure state while the saved sample is otherwise fresh.
	cache, err := loadHostObservationCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	markSectionFailures(cache, at, "processes", errors.New("permission changed"))
	view := projectHostObservation(cache, opts, at, false)
	if !view.Processes[0].CPU.Status.Stale || !view.Processes[0].RSS.Status.Stale || view.Workloads[0].CPU.Value != nil {
		t.Fatal("stale process facts remained healthy in aggregate")
	}
}
func TestHostObservationPartialAggregateUsesKnownContributorStatus(t *testing.T) {
	isolateObservation(t)
	at := time.Unix(1000, 0)
	rows := []ProcessObservation{{Identity: ProcessIdentity{PID: 1, StartID: "a"}, CPU: observedValue(50.0, "known", at, time.Second), RSS: observedValue(uint64(10), "known", at, time.Second)}, {Identity: ProcessIdentity{PID: 2, StartID: "b"}, PPID: 1, CPU: unknownValue[float64]("missing", "denied", at.Add(time.Second)), RSS: unknownValue[uint64]("missing", "denied", at.Add(time.Second))}}
	_, work := AttributeProcesses(rows, []WorkloadRef{{ID: "w", Root: rows[0].Identity}})
	if work[0].CPU.Value == nil || *work[0].CPU.Value != 50 || !work[0].CPU.Status.At.Equal(at) || work[0].CPU.Status.Kind != "estimated" || work[0].Coverage.Complete {
		t.Fatalf("unknown last row corrupted contributing metadata: %+v", work)
	}
}

func TestHostObservationProcStatUnitsAndIdentity(t *testing.T) {
	isolateObservation(t)
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[1] = "7"
	fields[11] = "200"
	fields[12] = "100"
	fields[19] = "999"
	fields[21] = "4"
	sample, err := parseObservationProcStat(42, []byte("42 (name with ) paren) "+strings.Join(fields, " ")), "boot-id", 100, 4096)
	if err != nil || sample.PPID != 7 || sample.Identity.StartID != "linux:boot-id:999" || sample.Name != "name with ) paren" || sample.CPUSeconds == nil || *sample.CPUSeconds != 3 || sample.RSS == nil || *sample.RSS != 16384 {
		t.Fatalf("native stat parsing: %+v %v", sample, err)
	}
	if _, err := parseObservationProcStat(1, []byte("malformed"), "boot", 100, 4096); err == nil {
		t.Fatal("malformed proc record accepted")
	}
}
func TestHostObservationCorruptCacheRepairsWithEvidence(t *testing.T) {
	dir := isolateObservation(t)
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "host.json"), []byte("incomplete json"), 0600)
	at := time.Unix(1000, 0)
	calls := 0
	view, err := observeHost(context.Background(), HostObserveOptions{CacheDir: dir}, fixtureObservationSources(&at, &calls))
	if err != nil || calls != 1 || view.Sections["cache"].Reason == "" {
		t.Fatalf("corruption was silently healthy: %+v %v", view, err)
	}
}

func TestHostObservationParentPIDReuseCannotCaptureOlderChild(t *testing.T) {
	isolateObservation(t)
	rows := []ProcessObservation{{Identity: ProcessIdentity{PID: 1, StartID: "darwin:200:0"}}, {Identity: ProcessIdentity{PID: 2, StartID: "darwin:100:0"}, PPID: 1}}
	got, work := AttributeProcesses(rows, []WorkloadRef{{ID: "new-parent", Root: rows[0].Identity}})
	if got[1].Attribution != "unverified" || got[1].WorkloadID != "" || work[0].ProcessCount != 1 {
		t.Fatalf("reused parent captured old child: %+v %+v", got, work)
	}
}

func TestHostObservationProcessIdentityLookup(t *testing.T) {
	isolateObservation(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("native identity unsupported")
	}
	identity, err := LookupProcessIdentity(context.Background(), os.Getpid())
	if err != nil || identity.PID != os.Getpid() || identity.StartID == "" {
		t.Fatalf("self identity: %+v %v", identity, err)
	}
	samples, _, err := collectProcessSamples(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sample := range samples {
		if sample.Identity.PID == os.Getpid() {
			found = true
			if sample.Identity != identity {
				t.Fatalf("launch/read identity formats differ: %+v %+v", identity, sample.Identity)
			}
		}
	}
	if !found {
		t.Fatal("self absent from bounded native snapshot")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LookupProcessIdentity(ctx, os.Getpid()); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
	if _, err := LookupProcessIdentity(context.Background(), -1); err == nil {
		t.Fatal("invalid PID accepted")
	}
}
