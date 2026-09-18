package resources

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

const maxHostCacheBytes = 4 << 20
const hostCacheSchema = "bashy-host-cache-v1"

type hostCPUSample struct {
	At          time.Time `json:"at"`
	Total, Idle uint64
	HasCPU      bool
	Net         []observationNetCounter
	Disk        []observationIOCounter
}
type observationNetCounter struct {
	Name   string
	RX, TX uint64
}
type observationIOCounter struct {
	Device      string
	Read, Write uint64
}
type hostObservationCache struct {
	Schema            string                          `json:"schema"`
	Revision          uint64                          `json:"revision"`
	At                time.Time                       `json:"at"`
	RetryAt           time.Time                       `json:"retry_at"`
	System            *System                         `json:"system"`
	CPU               hostCPUSample                   `json:"cpu_sample"`
	Sections          map[string]ObservationStatus    `json:"sections"`
	ProcessAt         time.Time                       `json:"process_at"`
	PreviousProcessAt time.Time                       `json:"previous_process_at"`
	Processes         []processSample                 `json:"processes"`
	ProcessCoverage   ObservationCoverage             `json:"process_coverage"`
	Scans             map[string]DirectoryObservation `json:"scans"`
	HostRefreshes     uint64                          `json:"host_refreshes"`
	DirectoryScans    uint64                          `json:"directory_scans"`
}
type hostObservationSources struct {
	now       func() time.Time
	system    func(context.Context) (*System, hostCPUSample, error)
	processes func(context.Context) ([]processSample, ObservationCoverage, error)
	scan      func(context.Context, string, time.Time, *directoryScanBudget) DirectoryObservation
}

func defaultHostObservationSources() hostObservationSources {
	return hostObservationSources{now: time.Now, system: collectObservationSystem, processes: collectProcessSamples, scan: scanObservationDirectory}
}
func collectObservationSystem(ctx context.Context) (*System, hostCPUSample, error) {
	// Collect reuses the established native memory/disk/network/GPU collectors.
	// One adjacent raw tick read supplies a persisted counter, avoiding a sleep
	// and a second full collection for each refresh/client.
	sys, err := Collect(ctx, Options{})
	if err != nil {
		return nil, hostCPUSample{}, err
	}
	raw, rawErr := sampleCounters()
	sample := hostCPUSample{At: raw.at, Total: raw.cpu.total, Idle: raw.cpu.idle, HasCPU: raw.hasCPU && rawErr == nil}
	for _, n := range raw.net {
		sample.Net = append(sample.Net, observationNetCounter{n.name, n.rxBytes, n.txBytes})
	}
	for _, d := range raw.diskIO {
		sample.Disk = append(sample.Disk, observationIOCounter{d.device, d.readBytes, d.writeBytes})
	}
	if err := ctx.Err(); err != nil {
		return nil, hostCPUSample{}, err
	}
	return sys, sample, nil
}
func newHostObservationCache() *hostObservationCache {
	return &hostObservationCache{Schema: hostCacheSchema, Sections: map[string]ObservationStatus{}, Scans: map[string]DirectoryObservation{}}
}
func loadHostObservationCache(dir string) (*hostObservationCache, error) {
	c := newHostObservationCache()
	err := readObservationJSON(filepath.Join(dir, "host.json"), maxHostCacheBytes, c)
	if err != nil {
		return nil, err
	}
	if c.Schema != hostCacheSchema || len(c.Processes) > maxObservedProcesses || len(c.Scans) > 128 {
		return nil, errors.New("resources: invalid host cache schema or bounds")
	}
	if c.Scans == nil {
		c.Scans = map[string]DirectoryObservation{}
	}
	if c.Sections == nil {
		c.Sections = map[string]ObservationStatus{}
	}
	return c, nil
}

// ObserveHost shares native observations across processes and applies attribution
// only after the raw cache read. Workloads must include all active host work.
func ObserveHost(ctx context.Context, opts HostObserveOptions) (*HostObservation, error) {
	return observeHost(ctx, opts, defaultHostObservationSources())
}
func observeHost(ctx context.Context, opts HostObserveOptions, source hostObservationSources) (*HostObservation, error) {
	if err := validateHostObserveOptions(&opts); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := observationDir(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	now := source.now().UTC()
	cache, loadErr := loadHostObservationCache(dir)
	if cache == nil {
		cache = newHostObservationCache()
	}
	needsHost := cache.At.IsZero() || now.Before(cache.At) || (!now.Before(cache.At.Add(HostObservationTTL)) && !now.Before(cache.RetryAt))
	if !needsHost && !needsDirectoryScan(cache, opts.ScanRoots, now) {
		return projectHostObservation(cache, opts, now, false), nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return degradedHostObservation(cache, opts, now, err), nil
	}
	lock, lockErr := lockfile.TryAcquire(filepath.Join(dir, ".host-refresh.lock"), lockfile.Holder{Name: "resources", Intent: "host refresh"})
	if lockErr != nil {
		if !errors.Is(lockErr, lockfile.ErrHeld) {
			return degradedHostObservation(cache, opts, now, lockErr), nil
		}
		if !cache.At.IsZero() {
			return projectHostObservation(cache, opts, now, true), nil
		}
		// Cold readers wait for the current winner, without doing duplicate work.
		for {
			if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
				return degradedHostObservation(cache, opts, now, err), nil
			}
			if fresh, err := loadHostObservationCache(dir); err == nil && !fresh.At.IsZero() {
				return projectHostObservation(fresh, opts, source.now(), true), nil
			}
		}
	}
	defer lock.Release()
	// The winner may have completed between our initial read and lock acquisition.
	if latest, err := loadHostObservationCache(dir); err == nil {
		cache = latest
		loadErr = nil
	}
	now = source.now().UTC()
	needsHost = cache.At.IsZero() || now.Before(cache.At) || (!now.Before(cache.At.Add(HostObservationTTL)) && !now.Before(cache.RetryAt))
	if needsHost {
		collectCtx, done := context.WithTimeout(ctx, time.Second)
		sys, cpu, sysErr := source.system(collectCtx)
		if sysErr == nil && sys != nil {
			previousCPU := cache.CPU
			applyHostCPUDelta(sys, previousCPU, cpu)
			cache.System = sys
			cache.CPU = cpu
			cache.Sections = observationSections(sys, now)
			applyObservationIO(sys, previousCPU, cpu, cache.Sections, now)
		} else {
			markSectionFailures(cache, now, "system", sysErr)
		}
		samples, coverage, procErr := source.processes(collectCtx)
		if procErr == nil {
			processRates(samples, cache.Processes, now.Sub(cache.ProcessAt).Seconds())
			cache.PreviousProcessAt = cache.ProcessAt
			cache.ProcessAt = now
			cache.Processes = samples
			cache.ProcessCoverage = coverage
			cache.Sections["processes"] = observationStatus("actual", "native:processes", coverage.Reason, now, HostObservationTTL)
		} else {
			markSectionFailures(cache, now, "processes", procErr)
		}
		done()
		cache.At = now
		cache.RetryAt = now.Add(HostObservationTTL)
		cache.HostRefreshes++
	}
	scanCtx, done := context.WithTimeout(ctx, 100*time.Millisecond)
	budget := &directoryScanBudget{remaining: maxObservationScanEntries}
	seen := map[string]bool{}
	for _, root := range opts.ScanRoots {
		if seen[root.Path] {
			continue
		}
		seen[root.Path] = true
		prior, exists := cache.Scans[root.Path]
		if exists && !now.Before(prior.Bytes.Status.At) && now.Before(prior.Bytes.Status.ExpiresAt) {
			continue
		}
		scanned := source.scan(scanCtx, root.Path, now, budget)
		directoryGrowth(&scanned, prior)
		cache.Scans[root.Path] = scanned
		cache.DirectoryScans++
	}
	done()
	evictOldDirectoryScans(cache)
	cache.Revision++
	if err := ctx.Err(); err != nil {
		return degradedHostObservation(cache, opts, now, err), nil
	}
	if err := writeObservationJSON(filepath.Join(dir, "host.json"), maxHostCacheBytes, cache); err != nil {
		return degradedHostObservation(cache, opts, now, err), nil
	}
	result := projectHostObservation(cache, opts, now, false)
	if loadErr != nil && !os.IsNotExist(loadErr) {
		result.Sections["cache"] = observationStatus("actual", "resources:atomic-cache", "recovered invalid prior cache: "+loadErr.Error(), now, HostObservationTTL)
	}
	return result, nil
}
func validateHostObserveOptions(opts *HostObserveOptions) error {
	opts.ScanRoots = append([]ScanRoot(nil), opts.ScanRoots...)
	if len(opts.Workloads) > 4096 || len(opts.ScanRoots) > maxObservationScanRoots {
		return errors.New("resources: workload or scan-root count exceeds bound")
	}
	seen := map[string]bool{}
	for _, w := range opts.Workloads {
		if w.ID == "" || seen[w.ID] || len(w.ID) > 512 || len(w.Workspace) > 4096 || len(w.Agent) > 256 || len(w.Sprint) > 64 || len(w.Run) > 512 || len(w.Root.StartID) > 256 {
			return errors.New("resources: invalid workload identity or field bounds")
		}
		seen[w.ID] = true
	}
	for i, r := range opts.ScanRoots {
		if r.Path == "" || len(r.Path) > 4096 || len(r.WorkloadID) > 512 || len(r.Kind) > 64 {
			return errors.New("resources: invalid scan root")
		}
		absolute, err := filepath.Abs(r.Path)
		if err != nil {
			return err
		}
		opts.ScanRoots[i].Path = filepath.Clean(absolute)
	}
	return nil
}
func needsDirectoryScan(c *hostObservationCache, roots []ScanRoot, now time.Time) bool {
	for _, root := range roots {
		s, ok := c.Scans[root.Path]
		if !ok || now.Before(s.Bytes.Status.At) || !now.Before(s.Bytes.Status.ExpiresAt) {
			return true
		}
	}
	return false
}
func evictOldDirectoryScans(c *hostObservationCache) {
	if len(c.Scans) <= 128 {
		return
	}
	keys := make([]string, 0, len(c.Scans))
	for key := range c.Scans {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return c.Scans[keys[i]].Bytes.Status.At.Before(c.Scans[keys[j]].Bytes.Status.At) })
	for _, key := range keys[:len(keys)-128] {
		delete(c.Scans, key)
	}
}
func applyHostCPUDelta(sys *System, prior, current hostCPUSample) {
	elapsed := current.At.Sub(prior.At).Seconds()
	if prior.HasCPU && current.HasCPU && elapsed > 0 && elapsed <= 300 && current.Total > prior.Total && current.Idle >= prior.Idle {
		sys.CPU.UsagePercent = tickUsage(cpuTicks{prior.Total, prior.Idle}, cpuTicks{current.Total, current.Idle})
		sys.CPU.Source = "ticks"
		sys.CPU.PerCorePercent = nil
		sys.CPU.Pressure = classify(sys.CPU.UsagePercent)
		sys.SampleSeconds = elapsed
	}
}
func observationSections(sys *System, now time.Time) map[string]ObservationStatus {
	out := map[string]ObservationStatus{}
	for _, section := range []string{"cpu", "memory", "disks", "network", "gpu"} {
		out[section] = observationStatus("actual", "native:"+section, "", now, HostObservationTTL)
	}
	cpu := out["cpu"]
	cpu.Source = sys.CPU.Source
	switch sys.CPU.Source {
	case "loadavg":
		cpu.Kind = "estimated"
		cpu.Reason = "load pressure proxy; not sampled CPU utilization"
	case "ticks-boot":
		cpu.Reason = "cumulative since boot; not sampled utilization"
	case "unavailable", "":
		cpu.Kind = "unknown"
		cpu.Reason = "native CPU source unavailable"
	}
	if sys.CPU.Source == "ticks" {
		cpu.WindowEnd = now
		cpu.WindowStart = now.Add(-time.Duration(sys.SampleSeconds * float64(time.Second)))
	}
	out["cpu"] = cpu
	if sys.Memory.Source == "sysctl:page_free_count" {
		out["memory"] = observationStatus("estimated", sys.Memory.Source, "available memory estimated from free/speculative pages", now, HostObservationTTL)
	}
	if sys.Memory.TotalBytes == 0 {
		out["memory"] = observationStatus("unknown", "native:memory", "memory source unavailable", now, HostObservationTTL)
	}
	if len(sys.Disks) == 0 {
		out["disks"] = observationStatus("unknown", "native:disks", "no readable disk observations", now, HostObservationTTL)
	}
	for _, warning := range sys.Warnings {
		for _, section := range []string{"memory", "disk", "network", "gpu"} {
			if strings.HasPrefix(warning, section+":") {
				key := section
				if key == "disk" {
					key = "disks"
				}
				out[key] = observationStatus("unknown", "native:"+section, warning, now, HostObservationTTL)
			}
		}
	}
	// Existing zero-interval network/disk rates are omitted/unknown, not zero IO.
	out["network.rates"] = observationStatus("unknown", "native:network", "shared byte-rate sampling unavailable", now, HostObservationTTL)
	out["disks.rates"] = observationStatus("unknown", "native:disks", "shared IO-rate sampling unavailable", now, HostObservationTTL)
	return out
}
func markSectionFailures(c *hostObservationCache, now time.Time, section string, err error) {
	reason := "source returned no data"
	if err != nil {
		reason = err.Error()
	}
	keys := []string{section}
	if section == "system" {
		keys = []string{"cpu", "memory", "disks", "network", "gpu"}
	}
	for _, key := range keys {
		s, ok := c.Sections[key]
		if !ok {
			s = observationStatus("unknown", "native:"+key, reason, now, HostObservationTTL)
		}
		s.Stale = true
		s.Reason = reason
		c.Sections[key] = s
	}
}
func projectHostObservation(c *hostObservationCache, opts HostObserveOptions, now time.Time, refreshing bool) *HostObservation {
	out := &HostObservation{SchemaVersion: HostObservationSchema, ID: fmt.Sprintf("host-%d-%d", c.At.UnixNano(), c.Revision), At: c.At, ExpiresAt: c.At.Add(HostObservationTTL), System: c.System, Sections: map[string]ObservationStatus{}, ProcessCoverage: c.ProcessCoverage, DirectoryCoverage: ObservationCoverage{Complete: true, Limit: maxObservationScanEntries}, Refreshing: refreshing}
	for key, value := range c.Sections {
		value.Stale = value.Stale || now.Before(value.At) || !now.Before(value.ExpiresAt)
		out.Sections[key] = value
	}
	for _, key := range []string{"cpu", "memory", "disks", "network", "gpu", "processes"} {
		if _, ok := out.Sections[key]; !ok {
			out.Sections[key] = observationStatus("unknown", "native:"+key, "no observation available", now, HostObservationTTL)
		}
	}
	processes := processObservations(c.Processes, c.ProcessAt, c.PreviousProcessAt, now)
	if status := out.Sections["processes"]; status.Stale || status.Kind == "unknown" {
		for i := range processes {
			processes[i].CPU.Status.Stale = true
			processes[i].RSS.Status.Stale = true
			processes[i].CPU.Status.Reason = status.Reason
			processes[i].RSS.Status.Reason = status.Reason
		}
	}
	out.Processes, out.Workloads = attributeProcessesInPlace(processes, opts.Workloads)
	if !out.ProcessCoverage.Complete {
		for i := range out.Workloads {
			out.Workloads[i].Coverage.Complete = false
			out.Workloads[i].Coverage.Reason = "host process enumeration incomplete"
		}
	}
	unique := map[string]bool{}
	for _, root := range opts.ScanRoots {
		scan, ok := c.Scans[root.Path]
		if !ok {
			scan = DirectoryObservation{Path: root.Path, Bytes: unknownValue[uint64]("directory", "refresh pending", now), GrowthBytesPerSecond: unknownValue[float64]("directory", "refresh pending", now), Coverage: ObservationCoverage{Reason: "refresh pending"}}
		}
		scan.WorkloadID = root.WorkloadID
		scan.Kind = root.Kind
		scan.Bytes.Status.Stale = now.Before(scan.Bytes.Status.At) || !now.Before(scan.Bytes.Status.ExpiresAt)
		scan.GrowthBytesPerSecond.Status.Stale = scan.Bytes.Status.Stale
		out.Directories = append(out.Directories, scan)
		if unique[root.Path] {
			continue
		}
		unique[root.Path] = true
		out.DirectoryCoverage.Seen += scan.Entries
		out.DirectoryCoverage.Included += scan.Entries
		if !scan.Coverage.Complete {
			out.DirectoryCoverage.Complete = false
			out.DirectoryCoverage.Reason = "one or more scans incomplete"
		}
	}
	return out
}
func degradedHostObservation(c *hostObservationCache, opts HostObserveOptions, now time.Time, err error) *HostObservation {
	out := projectHostObservation(c, opts, now, true)
	out.Sections["cache"] = observationStatus("unknown", "resources:atomic-cache", err.Error(), now, HostObservationTTL)
	return out
}

// Preserve native interface/disk counters across refreshes. Each individual
// rate gets its own availability metadata so new/reset devices are not zero IO.
func applyObservationIO(sys *System, prior, current hostCPUSample, sections map[string]ObservationStatus, now time.Time) {
	elapsed := current.At.Sub(prior.At).Seconds()
	if elapsed <= 0 || elapsed > 300 {
		return
	}
	oldNet := map[string]observationNetCounter{}
	for _, n := range prior.Net {
		oldNet[n.Name] = n
	}
	if len(current.Net) > 0 {
		sys.Network = nil
	}
	known := 0
	for _, n := range current.Net {
		row := Interface{Name: n.Name, RxBytes: n.RX, TxBytes: n.TX}
		status := observationStatus("unknown", "native:network-delta", "no comparable interface counter", now, HostObservationTTL)
		if old, ok := oldNet[n.Name]; ok && n.RX >= old.RX && n.TX >= old.TX {
			row.RxBytesPerSec = rate(old.RX, n.RX, elapsed)
			row.TxBytesPerSec = rate(old.TX, n.TX, elapsed)
			status.Kind = "actual"
			status.Reason = ""
			status.WindowStart = prior.At
			status.WindowEnd = current.At
			known++
		}
		sections["network."+n.Name+".rates"] = status
		sys.Network = append(sys.Network, row)
	}
	if known > 0 {
		status := observationStatus("actual", "native:network-delta", "", now, HostObservationTTL)
		status.WindowStart = prior.At
		status.WindowEnd = current.At
		if known < len(current.Net) {
			status.Kind = "estimated"
			status.Reason = "some interface counters unavailable"
		}
		sections["network.rates"] = status
	}
	oldDisk := map[string]observationIOCounter{}
	for _, d := range prior.Disk {
		oldDisk[d.Device] = d
	}
	newDisk := map[string]observationIOCounter{}
	for _, d := range current.Disk {
		newDisk[d.Device] = d
	}
	known = 0
	for i := range sys.Disks {
		disk := &sys.Disks[i]
		disk.ReadBytesPerSec = 0
		disk.WriteBytesPerSec = 0
		key := deviceKey(disk.Device)
		cur, ok := newDisk[key]
		old, previous := oldDisk[key]
		status := observationStatus("unknown", "native:disk-delta", "native device counters unavailable or reset", now, HostObservationTTL)
		if ok && previous && cur.Read >= old.Read && cur.Write >= old.Write {
			disk.ReadBytesPerSec = rate(old.Read, cur.Read, elapsed)
			disk.WriteBytesPerSec = rate(old.Write, cur.Write, elapsed)
			status.Kind = "actual"
			status.Reason = ""
			status.WindowStart = prior.At
			status.WindowEnd = current.At
			known++
		}
		sections["disks."+disk.Mount+".rates"] = status
	}
	if known > 0 {
		status := observationStatus("actual", "native:disk-delta", "", now, HostObservationTTL)
		status.WindowStart = prior.At
		status.WindowEnd = current.At
		if known < len(sys.Disks) {
			status.Kind = "estimated"
			status.Reason = "some disk counters unavailable"
		}
		sections["disks.rates"] = status
	}
}
