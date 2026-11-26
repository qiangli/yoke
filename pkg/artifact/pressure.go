// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	bolt "go.etcd.io/bbolt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/qiangli/yoke/pkg/prune"
)

const (
	PressureStateSchemaV1 = "bashy-pressure-state-v1"

	PressureNormal   = "normal"
	PressureWarning  = "warning"
	PressureCritical = "critical"

	AlertEventTransition       = "transition"
	AlertEventReclaim          = "reclaim"
	AlertEventModelUnload      = "model_unload"
	AlertEventUnsatisfiedFloor = "unsatisfied_floor"
	AlertEventRecovery         = "recovery"
	AlertEventError            = "error"

	DefaultManagedOllamaURL = "http://127.0.0.1:11435"
	DefaultMaxAlertRecords  = 256

	defaultInodeWarnPct    = 85.0
	defaultInodeCritPct    = 95.0
	defaultInodeRecoverPct = 80.0

	pressureStateFile = "pressure-state.json"
	alertJournalFile  = "alerts.jsonl"

	otelInstrumentationScope = "github.com/qiangli/yoke/pkg/artifact"
)

// DiskObservation captures byte and optional inode accounting for one host,
// artifact-store, or VM/container-storage filesystem.
type DiskObservation struct {
	Path              string  `json:"path"`
	Mount             string  `json:"mount,omitempty"`
	TotalBytes        uint64  `json:"total_bytes"`
	UsedBytes         uint64  `json:"used_bytes"`
	FreeBytes         uint64  `json:"free_bytes"`
	UsedPercent       float64 `json:"used_percent"`
	InodesTotal       uint64  `json:"inodes_total,omitempty"`
	InodesUsed        uint64  `json:"inodes_used,omitempty"`
	InodesFree        uint64  `json:"inodes_free,omitempty"`
	InodesUsedPercent float64 `json:"inodes_used_percent,omitempty"`
	Role              string  `json:"role,omitempty"` // "host", "artifact", "vm", "extra"
}

// PressureObservation summarizes host + VM/artifact filesystems, inodes, memory, and CPU.
type PressureObservation struct {
	At                time.Time         `json:"at"`
	Host              string            `json:"host,omitempty"`
	CPUUsagePercent   float64           `json:"cpu_usage_percent"`
	CPUSource         string            `json:"cpu_source,omitempty"`
	MemTotalBytes     uint64            `json:"mem_total_bytes"`
	MemUsedBytes      uint64            `json:"mem_used_bytes"`
	MemAvailableBytes uint64            `json:"mem_available_bytes"`
	MemUsedPercent    float64           `json:"mem_used_percent"`
	Disks             []DiskObservation `json:"disks"`
}

// CPUSamplePoint stores one timestamped CPU utilization reading for sustained-window evaluation.
type CPUSamplePoint struct {
	At           time.Time `json:"at"`
	UsagePercent float64   `json:"usage_percent"`
}

// UnsatisfiedFloorDetail explains why critical reclamation could not restore disk headroom.
type UnsatisfiedFloorDetail struct {
	Path               string `json:"path,omitempty"`
	ProtectedBytes     int64  `json:"protected_bytes"`
	LeasedBytes        int64  `json:"leased_bytes"`
	FreeBytes          uint64 `json:"free_bytes"`
	RequiredFloorBytes uint64 `json:"required_floor_bytes"`
}

// PressureStateV1 is persisted atomically at <artifact_root>/pressure-state.json (0600).
type PressureStateV1 struct {
	SchemaVersion       string                  `json:"schema_version"`
	UpdatedAt           time.Time               `json:"updated_at"`
	Activated           bool                    `json:"activated"`
	Level               string                  `json:"level"`
	ObservedLevel       string                  `json:"observed_level"`
	Reasons             []string                `json:"reasons,omitempty"`
	GrowthBlocked       bool                    `json:"growth_blocked"`
	ThrottleConcurrency int                     `json:"throttle_concurrency,omitempty"`
	UnsatisfiedFloor    bool                    `json:"unsatisfied_floor"`
	FloorDetail         *UnsatisfiedFloorDetail `json:"floor_detail,omitempty"`
	HealthySamples      int                     `json:"healthy_samples"`
	HealthySince        *time.Time              `json:"healthy_since,omitempty"`
	CPUPoints           []CPUSamplePoint        `json:"cpu_points,omitempty"`
	LastReclaimedBytes  int64                   `json:"last_reclaimed_bytes,omitempty"`
	LastReclaimedItems  int                     `json:"last_reclaimed_items,omitempty"`
	LastUnloadedModels  []string                `json:"last_unloaded_models,omitempty"`
}

// AlertRecordV1 is one structured JSONL line in <artifact_root>/alerts.jsonl (schema bashy-offline-alert-v1).
type AlertRecordV1 struct {
	SchemaVersion    string                  `json:"schema_version"`
	At               time.Time               `json:"at"`
	EventType        string                  `json:"event_type"` // "transition" | "reclaim" | "model_unload" | "unsatisfied_floor" | "recovery" | "error"
	FromLevel        string                  `json:"from_level,omitempty"`
	ToLevel          string                  `json:"to_level,omitempty"`
	Reasons          []string                `json:"reasons,omitempty"`
	ReclaimedBytes   int64                   `json:"reclaimed_bytes,omitempty"`
	ReclaimedItems   int                     `json:"reclaimed_items,omitempty"`
	SkippedProtected int                     `json:"skipped_protected,omitempty"`
	SkippedLeased    int                     `json:"skipped_leased,omitempty"`
	UnloadedModels   []string                `json:"unloaded_models,omitempty"`
	GrowthBlocked    bool                    `json:"growth_blocked"`
	UnsatisfiedFloor bool                    `json:"unsatisfied_floor"`
	FloorDetail      *UnsatisfiedFloorDetail `json:"floor_detail,omitempty"`
}

// PressureResult is the structured verdict returned by PressureController.Evaluate.
type PressureResult struct {
	ArtifactRoot     string              `json:"artifact_root"`
	ProfileName      string              `json:"profile_name"`
	State            PressureStateV1     `json:"state"`
	Observation      PressureObservation `json:"observation"`
	ReclaimedBytes   int64               `json:"reclaimed_bytes"`
	ReclaimedItems   int                 `json:"reclaimed_items"`
	SkippedProtected int                 `json:"skipped_protected"`
	SkippedLeased    int                 `json:"skipped_leased"`
	ProtectedBytes   int64               `json:"protected_bytes"`
	LeasedBytes      int64               `json:"leased_bytes"`
	UnloadedModels   []string            `json:"unloaded_models,omitempty"`
	EmittedAlerts    []AlertRecordV1     `json:"emitted_alerts,omitempty"`
}

// PressureControllerOptions configures PressureController.
type PressureControllerOptions struct {
	Root          string
	Profile       *ProfileV1
	ProfileSpec   string
	Backend       Backend
	ExtraPruners  []prune.Noun
	ExtraPaths    []string
	OllamaBaseURL string
	HTTPClient    *http.Client

	InodeWarnPct    float64
	InodeCritPct    float64
	InodeRecoverPct float64
	MaxAlertRecords int

	Now              func() time.Time
	ObserveFunc      func(ctx context.Context) (PressureObservation, error)
	DiskFreeFunc     func(path string) (uint64, error)
	UnloadModelsFunc func(ctx context.Context, baseURL string) ([]string, error)

	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// PressureController evaluates host/VM resource pressure, triggers policy-gated
// retention-aware reclamation and model memory unload, enforces recovery hysteresis,
// and journals bounded local alerts independently of any external OTel collector.
type PressureController struct {
	opts PressureControllerOptions
}

// NewPressureController creates a PressureController with opts.
func NewPressureController(opts PressureControllerOptions) *PressureController {
	return &PressureController{opts: opts}
}

// LoadPressureState reads <root>/pressure-state.json if present.
func LoadPressureState(root string) (PressureStateV1, bool, error) {
	p := filepath.Join(resolveRoot(root), pressureStateFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PressureStateV1{}, false, nil
		}
		return PressureStateV1{}, false, err
	}
	var st PressureStateV1
	if err := json.Unmarshal(raw, &st); err != nil {
		return PressureStateV1{}, false, fmt.Errorf("artifact: decode %s: %w", p, err)
	}
	if st.SchemaVersion != "" && st.SchemaVersion != PressureStateSchemaV1 {
		return PressureStateV1{}, false, fmt.Errorf("artifact: unsupported pressure state schema %q", st.SchemaVersion)
	}
	if st.SchemaVersion == "" {
		st.SchemaVersion = PressureStateSchemaV1
	}
	if st.Level == "" {
		st.Level = PressureNormal
	}
	if st.ObservedLevel == "" {
		st.ObservedLevel = st.Level
	}
	return st, true, nil
}

// SavePressureState atomically writes <root>/pressure-state.json with 0600 permissions.
func SavePressureState(root string, st PressureStateV1) error {
	r := resolveRoot(root)
	if err := os.MkdirAll(r, 0o700); err != nil {
		return err
	}
	if st.SchemaVersion == "" {
		st.SchemaVersion = PressureStateSchemaV1
	}
	if st.Level == "" {
		st.Level = PressureNormal
	}
	if st.ObservedLevel == "" {
		st.ObservedLevel = st.Level
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(filepath.Join(r, pressureStateFile), data, 0o600)
}

// LoadAlertRecords reads structured alert records from <root>/alerts.jsonl (oldest first;
// when limit > 0, returns at most the newest limit records).
func LoadAlertRecords(root string, limit int) ([]AlertRecordV1, error) {
	p := filepath.Join(resolveRoot(root), alertJournalFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []AlertRecordV1{}, nil
		}
		return nil, err
	}
	var out []AlertRecordV1
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 2<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec AlertRecordV1
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// AppendAlertRecords appends alerts to <root>/alerts.jsonl, bounding the file to the
// newest maxRecords entries via an atomic 0600 rewrite so local alert logging never
// grows unbounded or depends on an external collector.
func AppendAlertRecords(root string, maxRecords int, newAlerts ...AlertRecordV1) error {
	if len(newAlerts) == 0 {
		return nil
	}
	if maxRecords <= 0 {
		maxRecords = DefaultMaxAlertRecords
	}
	r := resolveRoot(root)
	if err := os.MkdirAll(r, 0o700); err != nil {
		return err
	}
	existing, _ := LoadAlertRecords(r, 0)
	for i := range newAlerts {
		if newAlerts[i].SchemaVersion == "" {
			newAlerts[i].SchemaVersion = AlertJournalSchemaV1
		}
		if newAlerts[i].At.IsZero() {
			newAlerts[i].At = time.Now().UTC()
		} else {
			newAlerts[i].At = newAlerts[i].At.UTC()
		}
	}
	combined := append(existing, newAlerts...)
	if len(combined) > maxRecords {
		combined = combined[len(combined)-maxRecords:]
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, rec := range combined {
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return writeFileAtomic(filepath.Join(r, alertJournalFile), buf.Bytes(), 0o600)
}

func normalizeResourcePolicy(in ResourcePolicy) ResourcePolicy {
	def := DefaultResourcePolicy()
	out := in
	if out.DiskWarnUsedPct <= 0 {
		out.DiskWarnUsedPct = def.DiskWarnUsedPct
	}
	if out.DiskCritUsedPct <= 0 {
		out.DiskCritUsedPct = def.DiskCritUsedPct
	}
	if out.DiskFreeWarnBytes == 0 {
		out.DiskFreeWarnBytes = def.DiskFreeWarnBytes
	}
	if out.DiskFreeCritBytes == 0 {
		out.DiskFreeCritBytes = def.DiskFreeCritBytes
	}
	if out.DiskRecoverUsedPct <= 0 {
		out.DiskRecoverUsedPct = def.DiskRecoverUsedPct
	}
	if out.DiskRecoverFreeBytes == 0 {
		out.DiskRecoverFreeBytes = def.DiskRecoverFreeBytes
	}
	if out.MemWarnPct <= 0 {
		out.MemWarnPct = def.MemWarnPct
	}
	if out.MemCritPct <= 0 {
		out.MemCritPct = def.MemCritPct
	}
	if out.CPUWarnPct <= 0 {
		out.CPUWarnPct = def.CPUWarnPct
	}
	if out.CPUCritPct <= 0 {
		out.CPUCritPct = def.CPUCritPct
	}
	if out.CPUSustainSeconds <= 0 {
		out.CPUSustainSeconds = def.CPUSustainSeconds
	}
	if out.RecoverHealthySamples <= 0 {
		out.RecoverHealthySamples = def.RecoverHealthySamples
	}
	if out.RecoverMinSeconds <= 0 {
		out.RecoverMinSeconds = def.RecoverMinSeconds
	}
	return out
}

func detectVMStoragePaths(artifactRoot string) []string {
	var candidates []string
	if v := strings.TrimSpace(os.Getenv("CONTAINERS_STORAGE_DIR")); v != "" {
		candidates = append(candidates, v)
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "containers"))
	}
	if bh := strings.TrimSpace(os.Getenv("BASHY_HOME")); bh != "" {
		candidates = append(candidates,
			filepath.Join(bh, "containers"),
			filepath.Join(bh, "podman"),
		)
	}
	if h, err := os.UserHomeDir(); err == nil && strings.TrimSpace(h) != "" {
		candidates = append(candidates,
			filepath.Join(h, ".local", "share", "containers"),
			filepath.Join(h, ".config", "containers", "podman", "machine"),
		)
	}
	if artifactRoot != "" {
		candidates = append(candidates,
			filepath.Join(artifactRoot, "podman"),
			filepath.Join(artifactRoot, "oci"),
		)
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil || seen[abs] {
			continue
		}
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	return out
}

// StatFilesystemPath queries disk space and inode usage for path (or its nearest existing ancestor).
func StatFilesystemPath(ctx context.Context, path, role string) (DiskObservation, error) {
	target, err := nearestExistingDir(path)
	if err != nil {
		return DiskObservation{}, err
	}
	u, err := disk.UsageWithContext(ctx, target)
	if err != nil {
		return DiskObservation{}, err
	}
	return DiskObservation{
		Path:              path,
		Mount:             u.Path,
		TotalBytes:        u.Total,
		UsedBytes:         u.Used,
		FreeBytes:         u.Free,
		UsedPercent:       u.UsedPercent,
		InodesTotal:       u.InodesTotal,
		InodesUsed:        u.InodesUsed,
		InodesFree:        u.InodesFree,
		InodesUsedPercent: u.InodesUsedPercent,
		Role:              role,
	}, nil
}

// DefaultObservePressure inspects host memory, CPU, root mount, artifact root,
// detected Podman/container VM storage directories, and extraPaths.
func DefaultObservePressure(ctx context.Context, root string, extraPaths []string) (PressureObservation, error) {
	absRoot := resolveRoot(root)
	host, _ := os.Hostname()
	obs := PressureObservation{
		At:   time.Now().UTC(),
		Host: host,
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil && vm != nil {
		obs.MemTotalBytes = vm.Total
		obs.MemUsedBytes = vm.Used
		obs.MemAvailableBytes = vm.Available
		obs.MemUsedPercent = vm.UsedPercent
	}
	if pcts, err := cpu.PercentWithContext(ctx, 50*time.Millisecond, false); err == nil && len(pcts) > 0 {
		obs.CPUUsagePercent = pcts[0]
		obs.CPUSource = "sampled"
	}

	seenTarget := map[string]bool{}
	addPath := func(p, role string, mustExist bool) {
		if strings.TrimSpace(p) == "" {
			return
		}
		if mustExist {
			if _, err := os.Stat(p); err != nil {
				return
			}
		}
		target, err := nearestExistingDir(p)
		if err != nil || seenTarget[target] {
			return
		}
		d, err := StatFilesystemPath(ctx, p, role)
		if err != nil {
			return
		}
		seenTarget[target] = true
		obs.Disks = append(obs.Disks, d)
	}

	addPath(absRoot, "artifact", false)
	for _, vmPath := range detectVMStoragePaths(absRoot) {
		addPath(vmPath, "vm", true)
	}
	for _, ep := range extraPaths {
		addPath(ep, "extra", false)
	}
	hostRoot := "/"
	if runtime.GOOS == "windows" {
		if vol := filepath.VolumeName(absRoot); vol != "" {
			hostRoot = vol + `\`
		} else {
			hostRoot = `C:\`
		}
	}
	addPath(hostRoot, "host", true)

	return obs, nil
}

func levelRank(lvl string) int {
	switch lvl {
	case PressureCritical:
		return 2
	case PressureWarning:
		return 1
	default:
		return 0
	}
}

func maxPressureLevel(a, b string) string {
	if levelRank(b) > levelRank(a) {
		return b
	}
	return a
}

func effectiveDiskUsedPct(d DiskObservation) float64 {
	if d.UsedPercent > 0 {
		return d.UsedPercent
	}
	if d.TotalBytes > 0 && d.UsedBytes > 0 {
		return float64(d.UsedBytes) / float64(d.TotalBytes) * 100.0
	}
	if d.TotalBytes > 0 && d.TotalBytes > d.FreeBytes {
		return float64(d.TotalBytes-d.FreeBytes) / float64(d.TotalBytes) * 100.0
	}
	return 0
}

func effectiveInodeUsedPct(d DiskObservation) float64 {
	if d.InodesTotal == 0 {
		return 0
	}
	if d.InodesUsedPercent > 0 {
		return d.InodesUsedPercent
	}
	if d.InodesUsed > 0 {
		return float64(d.InodesUsed) / float64(d.InodesTotal) * 100.0
	}
	if d.InodesTotal > d.InodesFree {
		return float64(d.InodesTotal-d.InodesFree) / float64(d.InodesTotal) * 100.0
	}
	return 0
}

func effectiveMemUsedPct(obs PressureObservation) float64 {
	if obs.MemUsedPercent > 0 {
		return obs.MemUsedPercent
	}
	if obs.MemTotalBytes > 0 && obs.MemUsedBytes > 0 {
		return float64(obs.MemUsedBytes) / float64(obs.MemTotalBytes) * 100.0
	}
	if obs.MemTotalBytes > 0 && obs.MemAvailableBytes > 0 && obs.MemTotalBytes >= obs.MemAvailableBytes {
		return float64(obs.MemTotalBytes-obs.MemAvailableBytes) / float64(obs.MemTotalBytes) * 100.0
	}
	return 0
}

func appendCPUPoint(points []CPUSamplePoint, at time.Time, usage float64, sustain time.Duration) []CPUSamplePoint {
	keepWindow := max(2*sustain, 15*time.Minute)
	cutoff := at.Add(-keepWindow)
	out := make([]CPUSamplePoint, 0, len(points)+1)
	for _, p := range points {
		if !p.At.Before(cutoff) {
			out = append(out, p)
		}
	}
	out = append(out, CPUSamplePoint{At: at, UsagePercent: usage})
	if len(out) > 256 {
		out = out[len(out)-256:]
	}
	return out
}

func sustainedCPUExceeds(points []CPUSamplePoint, at time.Time, threshold float64, sustain time.Duration) bool {
	if threshold <= 0 || len(points) == 0 {
		return false
	}
	if sustain <= 0 {
		return points[len(points)-1].UsagePercent >= threshold
	}
	startIdx := -1
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].UsagePercent >= threshold {
			startIdx = i
		} else {
			break
		}
	}
	if startIdx < 0 || len(points)-startIdx < 2 {
		return false
	}
	return at.Sub(points[startIdx].At) >= sustain
}

func diskLabel(d DiskObservation) string {
	if strings.TrimSpace(d.Path) != "" {
		return d.Path
	}
	if strings.TrimSpace(d.Mount) != "" {
		return d.Mount
	}
	return "/"
}

func classifyPressureObservation(
	obs PressureObservation,
	cpuPoints []CPUSamplePoint,
	at time.Time,
	policy ResourcePolicy,
	rebuildReserveBytes uint64,
	inodeWarnPct, inodeCritPct float64,
) (string, []string) {
	lvl := PressureNormal
	var reasons []string

	for _, d := range obs.Disks {
		lbl := diskLabel(d)
		usedPct := effectiveDiskUsedPct(d)
		if policy.DiskCritUsedPct > 0 && usedPct >= policy.DiskCritUsedPct {
			lvl = maxPressureLevel(lvl, PressureCritical)
			reasons = append(reasons, fmt.Sprintf("disk %s %.1f%% used >= %.1f%% critical", lbl, usedPct, policy.DiskCritUsedPct))
		} else if policy.DiskWarnUsedPct > 0 && usedPct >= policy.DiskWarnUsedPct {
			lvl = maxPressureLevel(lvl, PressureWarning)
			reasons = append(reasons, fmt.Sprintf("disk %s %.1f%% used >= %.1f%% warning", lbl, usedPct, policy.DiskWarnUsedPct))
		}

		if policy.DiskFreeCritBytes > 0 && (d.TotalBytes == 0 || d.TotalBytes >= policy.DiskFreeCritBytes) && d.FreeBytes < policy.DiskFreeCritBytes {
			lvl = maxPressureLevel(lvl, PressureCritical)
			reasons = append(reasons, fmt.Sprintf("disk %s free %d bytes < %d bytes critical floor", lbl, d.FreeBytes, policy.DiskFreeCritBytes))
		} else if policy.DiskFreeWarnBytes > 0 && (d.TotalBytes == 0 || d.TotalBytes >= policy.DiskFreeWarnBytes) && d.FreeBytes < policy.DiskFreeWarnBytes {
			lvl = maxPressureLevel(lvl, PressureWarning)
			reasons = append(reasons, fmt.Sprintf("disk %s free %d bytes < %d bytes warning floor", lbl, d.FreeBytes, policy.DiskFreeWarnBytes))
		}

		checkReserve := rebuildReserveBytes > 0 && (d.Role == "artifact" || d.Role == "vm" || d.Role == "extra" || d.TotalBytes == 0 || d.TotalBytes >= rebuildReserveBytes)
		if checkReserve && d.FreeBytes < rebuildReserveBytes {
			lvl = maxPressureLevel(lvl, PressureCritical)
			reasons = append(reasons, fmt.Sprintf("disk %s free %d bytes < rebuild_reserve_bytes %d", lbl, d.FreeBytes, rebuildReserveBytes))
		}

		if d.InodesTotal > 0 {
			inPct := effectiveInodeUsedPct(d)
			if inodeCritPct > 0 && inPct >= inodeCritPct {
				lvl = maxPressureLevel(lvl, PressureCritical)
				reasons = append(reasons, fmt.Sprintf("disk %s inodes %.1f%% used >= %.1f%% critical", lbl, inPct, inodeCritPct))
			} else if inodeWarnPct > 0 && inPct >= inodeWarnPct {
				lvl = maxPressureLevel(lvl, PressureWarning)
				reasons = append(reasons, fmt.Sprintf("disk %s inodes %.1f%% used >= %.1f%% warning", lbl, inPct, inodeWarnPct))
			}
		}
	}

	memPct := effectiveMemUsedPct(obs)
	if memPct > 0 {
		if policy.MemCritPct > 0 && memPct >= policy.MemCritPct {
			lvl = maxPressureLevel(lvl, PressureCritical)
			reasons = append(reasons, fmt.Sprintf("memory %.1f%% used >= %.1f%% critical", memPct, policy.MemCritPct))
		} else if policy.MemWarnPct > 0 && memPct >= policy.MemWarnPct {
			lvl = maxPressureLevel(lvl, PressureWarning)
			reasons = append(reasons, fmt.Sprintf("memory %.1f%% used >= %.1f%% warning", memPct, policy.MemWarnPct))
		}
	}

	sustain := time.Duration(policy.CPUSustainSeconds) * time.Second
	if sustainedCPUExceeds(cpuPoints, at, policy.CPUCritPct, sustain) {
		lvl = maxPressureLevel(lvl, PressureCritical)
		reasons = append(reasons, fmt.Sprintf("cpu %.1f%% >= %.1f%% sustained %s critical", obs.CPUUsagePercent, policy.CPUCritPct, sustain))
	} else if sustainedCPUExceeds(cpuPoints, at, policy.CPUWarnPct, sustain) {
		lvl = maxPressureLevel(lvl, PressureWarning)
		reasons = append(reasons, fmt.Sprintf("cpu %.1f%% >= %.1f%% sustained %s warning", obs.CPUUsagePercent, policy.CPUWarnPct, sustain))
	}

	return lvl, reasons
}

func meetsRecoveryTarget(
	obs PressureObservation,
	cpuPoints []CPUSamplePoint,
	at time.Time,
	policy ResourcePolicy,
	rebuildReserveBytes uint64,
	inodeRecoverPct float64,
) bool {
	for _, d := range obs.Disks {
		usedPct := effectiveDiskUsedPct(d)
		if policy.DiskRecoverUsedPct > 0 && usedPct > policy.DiskRecoverUsedPct {
			return false
		}
		if policy.DiskRecoverFreeBytes > 0 && (d.TotalBytes == 0 || d.TotalBytes >= policy.DiskRecoverFreeBytes) {
			if d.FreeBytes < policy.DiskRecoverFreeBytes {
				return false
			}
		}
		checkReserve := rebuildReserveBytes > 0 && (d.Role == "artifact" || d.Role == "vm" || d.Role == "extra" || d.TotalBytes == 0 || d.TotalBytes >= rebuildReserveBytes)
		if checkReserve && d.FreeBytes < rebuildReserveBytes {
			return false
		}
		if d.InodesTotal > 0 && inodeRecoverPct > 0 && effectiveInodeUsedPct(d) > inodeRecoverPct {
			return false
		}
	}

	if memPct := effectiveMemUsedPct(obs); memPct > 0 && policy.MemWarnPct > 0 && memPct >= policy.MemWarnPct {
		return false
	}

	sustain := time.Duration(policy.CPUSustainSeconds) * time.Second
	if policy.CPUWarnPct > 0 && (obs.CPUUsagePercent >= policy.CPUWarnPct || sustainedCPUExceeds(cpuPoints, at, policy.CPUWarnPct, sustain)) {
		return false
	}
	return true
}

type catalogReclaimSummary struct {
	skippedProtected int
	skippedLeased    int
	protectedBytes   int64
	leasedBytes      int64
	accessOrder      map[string]reclaimSortKey
}

type reclaimSortKey struct {
	retention RetentionClass
	refTime   time.Time
}

func inspectStoreCatalogSummary(s *Store, now time.Time) (catalogReclaimSummary, error) {
	out := catalogReclaimSummary{
		accessOrder: make(map[string]reclaimSortKey),
	}
	if s == nil || s.db == nil {
		return out, nil
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		snap, err := s.readCatalogSnapshotTx(tx, now)
		if err != nil {
			return err
		}
		for dgst, st := range snap.records {
			if !st.PayloadPresent {
				continue
			}
			ref := st.Record.LastAccessedAt
			if ref.IsZero() {
				ref = st.Record.CreatedAt
			}
			out.accessOrder[dgst] = reclaimSortKey{
				retention: st.Record.Retention,
				refTime:   ref,
			}
			if st.Record.Retention == RetentionProtectedRoot || len(st.Record.RetentionRoots) > 0 || snap.protectedClosure[dgst] {
				out.skippedProtected++
				out.protectedBytes += st.Record.Size
				continue
			}
			if snap.hasActiveLease(dgst) {
				out.skippedLeased++
				out.leasedBytes += st.Record.Size
			}
		}
		return nil
	})
	return out, err
}

func inspectRootCatalogSummary(ctx context.Context, root string, backend Backend, now time.Time) catalogReclaimSummary {
	if st, ok := backend.(*Store); ok {
		if sum, err := inspectStoreCatalogSummary(st, now); err == nil {
			return sum
		}
	}
	dbPath := filepath.Join(root, "catalog.db")
	if fi, err := os.Stat(dbPath); err != nil || fi.IsDir() {
		return catalogReclaimSummary{accessOrder: map[string]reclaimSortKey{}}
	}
	ro, err := OpenStore(ctx, StoreOptions{Root: root, ReadOnly: true, Now: func() time.Time { return now }})
	if err != nil {
		return catalogReclaimSummary{accessOrder: map[string]reclaimSortKey{}}
	}
	defer ro.Close()
	sum, _ := inspectStoreCatalogSummary(ro, now)
	return sum
}

func sortPruneItemsForReclaim(items []prune.Item, order map[string]reclaimSortKey) {
	if len(items) <= 1 || len(order) == 0 {
		return
	}
	rankClass := func(r RetentionClass) int {
		if r == RetentionScratch {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(items, func(a, b prune.Item) int {
		ka, okA := order[a.ID]
		kb, okB := order[b.ID]
		if okA && okB {
			if ra, rb := rankClass(ka.retention), rankClass(kb.retention); ra != rb {
				return ra - rb
			}
			if !ka.refTime.Equal(kb.refTime) {
				if ka.refTime.Before(kb.refTime) {
					return -1
				}
				return 1
			}
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func isBenignConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		msg := strings.ToLower(netErr.Error())
		if strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused") {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused")
}

func requiredDiskFloorBytes(d DiskObservation, policy ResourcePolicy, rebuildReserveBytes uint64) uint64 {
	var floor uint64
	checkReserve := rebuildReserveBytes > 0 && (d.Role == "artifact" || d.Role == "vm" || d.Role == "extra" || d.TotalBytes == 0 || d.TotalBytes >= rebuildReserveBytes)
	if checkReserve {
		floor = max(floor, rebuildReserveBytes)
	}
	if policy.DiskFreeWarnBytes > 0 && (d.TotalBytes == 0 || d.TotalBytes >= policy.DiskFreeWarnBytes) {
		floor = max(floor, policy.DiskFreeWarnBytes)
	} else if policy.DiskFreeCritBytes > 0 && (d.TotalBytes == 0 || d.TotalBytes >= policy.DiskFreeCritBytes) {
		floor = max(floor, policy.DiskFreeCritBytes)
	}
	if d.TotalBytes > 0 && policy.DiskWarnUsedPct > 0 && policy.DiskWarnUsedPct < 100 {
		pctFloor := uint64(math.Ceil(float64(d.TotalBytes) * (100.0 - policy.DiskWarnUsedPct) / 100.0))
		floor = max(floor, pctFloor)
	}
	return floor
}

func selectConstrainedDisk(disks []DiskObservation, policy ResourcePolicy, rebuildReserveBytes uint64) (DiskObservation, uint64, bool) {
	if len(disks) == 0 {
		return DiskObservation{}, 0, false
	}
	bestIdx := 0
	var bestFloor uint64
	bestDeficit := int64(0)
	foundDeficit := false

	for i, d := range disks {
		req := requiredDiskFloorBytes(d, policy, rebuildReserveBytes)
		if req > d.FreeBytes {
			def := int64(req - d.FreeBytes)
			if !foundDeficit || def > bestDeficit {
				foundDeficit = true
				bestDeficit = def
				bestIdx = i
				bestFloor = req
			}
		} else if !foundDeficit && (d.Role == "artifact" || i == 0) {
			bestIdx = i
			bestFloor = req
		}
	}
	return disks[bestIdx], bestFloor, true
}

func (c *PressureController) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *PressureController) resolveOllamaURL() (string, bool) {
	if u := strings.TrimSpace(c.opts.OllamaBaseURL); u != "" {
		return u, true
	}
	if u := strings.TrimSpace(os.Getenv("BASHY_OLLAMA_HOST")); u != "" {
		if !strings.Contains(u, "://") {
			u = "http://" + u
		}
		return u, true
	}
	return DefaultManagedOllamaURL, false
}

func (c *PressureController) emitOTel(ctx context.Context, res PressureResult) {
	tp := c.opts.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	mp := c.opts.MeterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}

	tracer := tp.Tracer(otelInstrumentationScope)
	_, span := tracer.Start(ctx, "artifact.pressure.evaluate",
		trace.WithAttributes(
			attribute.Bool("activated", res.State.Activated),
			attribute.String("level", res.State.Level),
			attribute.String("observed_level", res.State.ObservedLevel),
			attribute.Bool("growth_blocked", res.State.GrowthBlocked),
			attribute.Bool("unsatisfied_floor", res.State.UnsatisfiedFloor),
		),
	)
	for _, al := range res.EmittedAlerts {
		attrs := []attribute.KeyValue{
			attribute.String("event_type", al.EventType),
			attribute.Bool("growth_blocked", al.GrowthBlocked),
			attribute.Bool("unsatisfied_floor", al.UnsatisfiedFloor),
		}
		if al.FromLevel != "" {
			attrs = append(attrs, attribute.String("from_level", al.FromLevel))
		}
		if al.ToLevel != "" {
			attrs = append(attrs, attribute.String("to_level", al.ToLevel))
		}
		if al.ReclaimedBytes > 0 {
			attrs = append(attrs, attribute.Int64("reclaimed_bytes", al.ReclaimedBytes))
		}
		if len(al.UnloadedModels) > 0 {
			attrs = append(attrs, attribute.StringSlice("unloaded_models", al.UnloadedModels))
		}
		if al.FloorDetail != nil {
			attrs = append(attrs,
				attribute.Int64("protected_bytes", al.FloorDetail.ProtectedBytes),
				attribute.Int64("leased_bytes", al.FloorDetail.LeasedBytes),
				attribute.Int64("free_bytes", int64(min(al.FloorDetail.FreeBytes, uint64(math.MaxInt64)))),
				attribute.Int64("required_floor_bytes", int64(min(al.FloorDetail.RequiredFloorBytes, uint64(math.MaxInt64)))),
			)
		}
		span.AddEvent(al.EventType, trace.WithAttributes(attrs...))
	}
	span.End()

	meter := mp.Meter(otelInstrumentationScope)
	if res.ReclaimedBytes > 0 {
		if ctr, err := meter.Int64Counter("bashy.offline.reclaimed_bytes"); err == nil {
			ctr.Add(ctx, res.ReclaimedBytes, metric.WithAttributes(attribute.String("level", res.State.Level)))
		}
	}
	for _, al := range res.EmittedAlerts {
		if al.EventType == AlertEventTransition || al.EventType == AlertEventRecovery {
			if ctr, err := meter.Int64Counter("bashy.offline.transitions"); err == nil {
				ctr.Add(ctx, 1, metric.WithAttributes(
					attribute.String("event_type", al.EventType),
					attribute.String("from_level", al.FromLevel),
					attribute.String("to_level", al.ToLevel),
				))
			}
		}
	}
	if gauge, err := meter.Int64Gauge("bashy.offline.unsatisfied_floor"); err == nil {
		val := int64(0)
		if res.State.UnsatisfiedFloor {
			val = 1
		}
		gauge.Record(ctx, val, metric.WithAttributes(attribute.String("level", res.State.Level)))
	}
}

func (c *PressureController) runGuardedReclaim(ctx context.Context, root string, now time.Time) (prune.Counts, catalogReclaimSummary, error) {
	backend := c.opts.Backend
	closeBackend := false
	if backend == nil {
		_, svcOK := TryConnectService(ctx, root)
		dbExists := false
		if fi, err := os.Stat(filepath.Join(root, "catalog.db")); err == nil && !fi.IsDir() {
			dbExists = true
		}
		if svcOK || dbExists {
			opened, err := ConnectOrOpen(ctx, root)
			if err != nil {
				return prune.Counts{}, catalogReclaimSummary{}, err
			}
			backend = opened
			closeBackend = true
		}
	}
	if closeBackend {
		defer backend.Close()
	}

	preSummary := inspectRootCatalogSummary(ctx, root, backend, now)
	var total prune.Counts

	if backend != nil {
		items, err := backend.DryRunPrune(ctx, 0)
		if err != nil {
			return total, preSummary, err
		}
		sortPruneItemsForReclaim(items, preSummary.accessOrder)
		if len(items) > 0 {
			counts, err := backend.ApplyPrune(ctx, items)
			total.Removed += counts.Removed
			total.Bytes += counts.Bytes
			if err != nil {
				return total, preSummary, err
			}
		}
	}

	for _, noun := range c.opts.ExtraPruners {
		if noun == nil {
			continue
		}
		items, err := noun.DryRun(ctx, prune.Options{})
		if err != nil {
			return total, preSummary, err
		}
		if len(items) == 0 {
			continue
		}
		counts, err := noun.Apply(ctx, items)
		total.Removed += counts.Removed
		total.Bytes += counts.Bytes
		if err != nil {
			return total, preSummary, err
		}
	}

	return total, preSummary, nil
}

// Evaluate runs one resource observation and policy-gated pressure control pass.
func (c *PressureController) Evaluate(ctx context.Context) (PressureResult, error) {
	if err := ctx.Err(); err != nil {
		return PressureResult{}, err
	}
	root := resolveRoot(c.opts.Root)
	now := c.opts.Now
	nowTS := c.now()
	_ = now

	var sealPtr *SealStateV1
	if st, ok, err := LoadSealState(root); err == nil && ok {
		sealPtr = &st
	}
	var manifestPtr *ManifestV1
	if c.opts.Backend != nil {
		if manifests, err := c.opts.Backend.ListManifests(ctx); err == nil && len(manifests) > 0 {
			manifestPtr = &manifests[len(manifests)-1]
		}
	} else if ro, ok, err := openReadBackendIfPresent(ctx, root); err == nil && ok {
		if manifests, mErr := ro.ListManifests(ctx); mErr == nil && len(manifests) > 0 {
			manifestPtr = &manifests[len(manifests)-1]
		}
		_ = ro.Close()
	}

	prof, err := resolveEffectiveProfile(root, c.opts.Profile, c.opts.ProfileSpec, manifestPtr, sealPtr)
	if err != nil {
		prof = DefaultProfileV1(CurrentPlatform())
		if c.opts.Profile != nil {
			prof.Resources = c.opts.Profile.Resources
			if c.opts.Profile.Capacity.RebuildReserveBytes > 0 {
				prof.Capacity = c.opts.Profile.Capacity
			}
		}
	}
	policy := normalizeResourcePolicy(prof.Resources)
	rebuildReserveBytes := prof.Capacity.RebuildReserveBytes
	if c.opts.Profile != nil && c.opts.Profile.Capacity.RebuildReserveBytes == 0 && c.opts.Profile.SchemaVersion == "" {
		// Allow minimal test profiles that omit Capacity unless explicitly set.
		rebuildReserveBytes = 0
	}
	if manifestPtr != nil && manifestPtr.RebuildReserveBytes > rebuildReserveBytes {
		rebuildReserveBytes = manifestPtr.RebuildReserveBytes
	}

	inodeWarn := c.opts.InodeWarnPct
	if inodeWarn <= 0 {
		inodeWarn = defaultInodeWarnPct
	}
	inodeCrit := c.opts.InodeCritPct
	if inodeCrit <= 0 {
		inodeCrit = defaultInodeCritPct
	}
	inodeRecover := c.opts.InodeRecoverPct
	if inodeRecover <= 0 {
		inodeRecover = defaultInodeRecoverPct
	}

	var obs PressureObservation
	if c.opts.ObserveFunc != nil {
		obs, err = c.opts.ObserveFunc(ctx)
		if err != nil {
			return PressureResult{}, err
		}
	} else {
		obs, err = DefaultObservePressure(ctx, root, c.opts.ExtraPaths)
		if err != nil {
			return PressureResult{}, err
		}
	}
	if obs.At.IsZero() {
		obs.At = nowTS
	} else if c.opts.Now != nil {
		obs.At = nowTS
	} else {
		nowTS = obs.At.UTC()
	}

	prevState, hasPrev, _ := LoadPressureState(root)
	sustainDur := time.Duration(policy.CPUSustainSeconds) * time.Second
	cpuPoints := appendCPUPoint(prevState.CPUPoints, nowTS, obs.CPUUsagePercent, sustainDur)

	observedLevel, reasons := classifyPressureObservation(obs, cpuPoints, nowTS, policy, rebuildReserveBytes, inodeWarn, inodeCrit)

	res := PressureResult{
		ArtifactRoot: root,
		ProfileName:  prof.Name,
		Observation:  obs,
	}

	// Policy Activation Gate: when Activated == false, stay strictly advisory-only!
	if !policy.Activated {
		advisoryState := PressureStateV1{
			SchemaVersion:       PressureStateSchemaV1,
			UpdatedAt:           nowTS,
			Activated:           false,
			Level:               observedLevel,
			ObservedLevel:       observedLevel,
			Reasons:             reasons,
			GrowthBlocked:       false,
			ThrottleConcurrency: 0,
			UnsatisfiedFloor:    false,
			CPUPoints:           cpuPoints,
		}
		_ = SavePressureState(root, advisoryState)
		res.State = advisoryState
		return res, nil
	}

	prevLevel := PressureNormal
	prevBlocked := false
	prevThrottle := 0
	prevUnsatisfied := false
	var prevFloorDetail *UnsatisfiedFloorDetail
	healthySamples := 0
	var healthySince *time.Time

	if hasPrev && prevState.Activated {
		if prevState.Level != "" {
			prevLevel = prevState.Level
		}
		prevBlocked = prevState.GrowthBlocked
		prevThrottle = prevState.ThrottleConcurrency
		prevUnsatisfied = prevState.UnsatisfiedFloor
		prevFloorDetail = prevState.FloorDetail
		healthySamples = prevState.HealthySamples
		healthySince = prevState.HealthySince
	}

	nextState := PressureStateV1{
		SchemaVersion: PressureStateSchemaV1,
		UpdatedAt:     nowTS,
		Activated:     true,
		ObservedLevel: observedLevel,
		Reasons:       slices.Clone(reasons),
		CPUPoints:     cpuPoints,
	}

	var alerts []AlertRecordV1

	switch observedLevel {
	case PressureWarning:
		nextState.Level = PressureWarning
		nextState.GrowthBlocked = prevBlocked
		nextState.ThrottleConcurrency = prevThrottle
		nextState.UnsatisfiedFloor = prevUnsatisfied
		nextState.FloorDetail = prevFloorDetail
		nextState.HealthySamples = 0
		nextState.HealthySince = nil

		if prevLevel != PressureWarning {
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion:    AlertJournalSchemaV1,
				At:               nowTS,
				EventType:        AlertEventTransition,
				FromLevel:        prevLevel,
				ToLevel:          PressureWarning,
				Reasons:          slices.Clone(reasons),
				GrowthBlocked:    nextState.GrowthBlocked,
				UnsatisfiedFloor: nextState.UnsatisfiedFloor,
			})
		}

		counts, summary, reclaimErr := c.runGuardedReclaim(ctx, root, nowTS)
		res.ReclaimedBytes = counts.Bytes
		res.ReclaimedItems = counts.Removed
		res.SkippedProtected = summary.skippedProtected
		res.SkippedLeased = summary.skippedLeased
		res.ProtectedBytes = summary.protectedBytes
		res.LeasedBytes = summary.leasedBytes
		nextState.LastReclaimedBytes = counts.Bytes
		nextState.LastReclaimedItems = counts.Removed

		if reclaimErr != nil {
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion:    AlertJournalSchemaV1,
				At:               nowTS,
				EventType:        AlertEventError,
				FromLevel:        prevLevel,
				ToLevel:          PressureWarning,
				Reasons:          []string{"reclaim error: " + reclaimErr.Error()},
				GrowthBlocked:    nextState.GrowthBlocked,
				UnsatisfiedFloor: nextState.UnsatisfiedFloor,
			})
		}
		if counts.Removed > 0 || prevLevel != PressureWarning {
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion:    AlertJournalSchemaV1,
				At:               nowTS,
				EventType:        AlertEventReclaim,
				FromLevel:        prevLevel,
				ToLevel:          PressureWarning,
				Reasons:          slices.Clone(reasons),
				ReclaimedBytes:   counts.Bytes,
				ReclaimedItems:   counts.Removed,
				SkippedProtected: summary.skippedProtected,
				SkippedLeased:    summary.skippedLeased,
				GrowthBlocked:    nextState.GrowthBlocked,
				UnsatisfiedFloor: nextState.UnsatisfiedFloor,
			})
		}

	case PressureCritical:
		nextState.Level = PressureCritical
		nextState.GrowthBlocked = true
		nextState.ThrottleConcurrency = 1
		nextState.HealthySamples = 0
		nextState.HealthySince = nil

		if prevLevel != PressureCritical {
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion: AlertJournalSchemaV1,
				At:            nowTS,
				EventType:     AlertEventTransition,
				FromLevel:     prevLevel,
				ToLevel:       PressureCritical,
				Reasons:       slices.Clone(reasons),
				GrowthBlocked: true,
			})
		}

		// 1. Guarded retention-aware reclamation.
		counts, summary, reclaimErr := c.runGuardedReclaim(ctx, root, nowTS)
		res.ReclaimedBytes = counts.Bytes
		res.ReclaimedItems = counts.Removed
		res.SkippedProtected = summary.skippedProtected
		res.SkippedLeased = summary.skippedLeased
		nextState.LastReclaimedBytes = counts.Bytes
		nextState.LastReclaimedItems = counts.Removed

		if reclaimErr != nil {
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion: AlertJournalSchemaV1,
				At:            nowTS,
				EventType:     AlertEventError,
				FromLevel:     prevLevel,
				ToLevel:       PressureCritical,
				Reasons:       []string{"reclaim error: " + reclaimErr.Error()},
				GrowthBlocked: true,
			})
		}
		alerts = append(alerts, AlertRecordV1{
			SchemaVersion:    AlertJournalSchemaV1,
			At:               nowTS,
			EventType:        AlertEventReclaim,
			FromLevel:        prevLevel,
			ToLevel:          PressureCritical,
			Reasons:          slices.Clone(reasons),
			ReclaimedBytes:   counts.Bytes,
			ReclaimedItems:   counts.Removed,
			SkippedProtected: summary.skippedProtected,
			SkippedLeased:    summary.skippedLeased,
			GrowthBlocked:    true,
		})

		// 2. Unload idle resident models from RAM/VRAM while keeping CAS/disk weights intact.
		ollamaURL, explicitOllama := c.resolveOllamaURL()
		unloadFn := c.opts.UnloadModelsFunc
		if unloadFn == nil {
			unloadFn = func(uctx context.Context, baseURL string) ([]string, error) {
				return UnloadAllLoadedModels(uctx, baseURL, c.opts.HTTPClient)
			}
		}
		unloaded, unloadErr := unloadFn(ctx, ollamaURL)
		if len(unloaded) > 0 {
			res.UnloadedModels = slices.Clone(unloaded)
			nextState.LastUnloadedModels = slices.Clone(unloaded)
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion:  AlertJournalSchemaV1,
				At:             nowTS,
				EventType:      AlertEventModelUnload,
				FromLevel:      prevLevel,
				ToLevel:        PressureCritical,
				Reasons:        slices.Clone(reasons),
				UnloadedModels: slices.Clone(unloaded),
				GrowthBlocked:  true,
			})
		}
		if unloadErr != nil && (explicitOllama || c.opts.UnloadModelsFunc != nil || !isBenignConnectionRefused(unloadErr)) {
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion: AlertJournalSchemaV1,
				At:            nowTS,
				EventType:     AlertEventError,
				FromLevel:     prevLevel,
				ToLevel:       PressureCritical,
				Reasons:       []string{"model unload error: " + unloadErr.Error()},
				GrowthBlocked: true,
			})
		}

		// 3. Re-sample disk headroom after reclamation and check if floor remains unsatisfied.
		postObs := obs
		if c.opts.DiskFreeFunc != nil {
			if freeAfter, fErr := c.opts.DiskFreeFunc(root); fErr == nil {
				updated := slices.Clone(postObs.Disks)
				if len(updated) == 0 {
					updated = []DiskObservation{{Path: root, FreeBytes: freeAfter, Role: "artifact"}}
				} else {
					for i := range updated {
						if updated[i].Role == "artifact" || i == 0 {
							updated[i].FreeBytes = freeAfter
							if updated[i].TotalBytes >= freeAfter {
								updated[i].UsedBytes = updated[i].TotalBytes - freeAfter
								updated[i].UsedPercent = float64(updated[i].UsedBytes) / float64(updated[i].TotalBytes) * 100.0
							}
						}
					}
				}
				postObs.Disks = updated
			}
		} else if c.opts.ObserveFunc != nil {
			if refreshed, oErr := c.opts.ObserveFunc(ctx); oErr == nil {
				postObs = refreshed
			}
		} else if refreshed, oErr := DefaultObservePressure(ctx, root, c.opts.ExtraPaths); oErr == nil {
			postObs = refreshed
		}

		postSummary := inspectRootCatalogSummary(ctx, root, c.opts.Backend, nowTS)
		res.ProtectedBytes = postSummary.protectedBytes
		res.LeasedBytes = postSummary.leasedBytes
		res.SkippedProtected = postSummary.skippedProtected
		res.SkippedLeased = postSummary.skippedLeased

		postLevel, postReasons := classifyPressureObservation(postObs, cpuPoints, nowTS, policy, rebuildReserveBytes, inodeWarn, inodeCrit)
		targetDisk, reqFloorBytes, hasDisk := selectConstrainedDisk(postObs.Disks, policy, rebuildReserveBytes)

		floorUnsatisfied := postLevel == PressureCritical || (hasDisk && reqFloorBytes > 0 && targetDisk.FreeBytes < reqFloorBytes)
		if floorUnsatisfied {
			detail := &UnsatisfiedFloorDetail{
				Path:               diskLabel(targetDisk),
				ProtectedBytes:     postSummary.protectedBytes,
				LeasedBytes:        postSummary.leasedBytes,
				FreeBytes:          targetDisk.FreeBytes,
				RequiredFloorBytes: reqFloorBytes,
			}
			nextState.UnsatisfiedFloor = true
			nextState.FloorDetail = detail
			floorMsg := fmt.Sprintf(
				"unsatisfied_floor: path=%s free_bytes=%d required_floor_bytes=%d protected_bytes=%d leased_bytes=%d",
				detail.Path, detail.FreeBytes, detail.RequiredFloorBytes, detail.ProtectedBytes, detail.LeasedBytes,
			)
			alertReasons := append(slices.Clone(postReasons), floorMsg)
			nextState.Reasons = alertReasons
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion:    AlertJournalSchemaV1,
				At:               nowTS,
				EventType:        AlertEventUnsatisfiedFloor,
				FromLevel:        prevLevel,
				ToLevel:          PressureCritical,
				Reasons:          alertReasons,
				ReclaimedBytes:   counts.Bytes,
				ReclaimedItems:   counts.Removed,
				SkippedProtected: postSummary.skippedProtected,
				SkippedLeased:    postSummary.skippedLeased,
				UnloadedModels:   slices.Clone(res.UnloadedModels),
				GrowthBlocked:    true,
				UnsatisfiedFloor: true,
				FloorDetail:      detail,
			})
		}

	default: // observedLevel == PressureNormal
		if prevLevel == PressureNormal && !prevBlocked {
			nextState.Level = PressureNormal
			nextState.GrowthBlocked = false
			nextState.ThrottleConcurrency = 0
			nextState.UnsatisfiedFloor = false
			nextState.HealthySamples = 0
			nextState.HealthySince = nil
			break
		}

		// Recovery hysteresis from warning/critical -> normal:
		// Must satisfy recovery targets (<=80% disk used AND >=25 GiB free, plus healthy
		// mem/inode/reserve/CPU) across >= RecoverHealthySamples AND >= RecoverMinSeconds.
		if !meetsRecoveryTarget(obs, cpuPoints, nowTS, policy, rebuildReserveBytes, inodeRecover) {
			nextState.Level = prevLevel
			nextState.GrowthBlocked = prevBlocked
			nextState.ThrottleConcurrency = prevThrottle
			nextState.UnsatisfiedFloor = prevUnsatisfied
			nextState.FloorDetail = prevFloorDetail
			nextState.HealthySamples = 0
			nextState.HealthySince = nil
			nextState.Reasons = []string{"awaiting recovery target (<=80% disk used and >=25 GiB free across consecutive samples)"}
			break
		}

		if healthySamples <= 0 || healthySince == nil {
			healthySamples = 1
			tCopy := nowTS
			healthySince = &tCopy
		} else {
			healthySamples++
		}

		elapsed := nowTS.Sub(*healthySince)
		minDur := time.Duration(policy.RecoverMinSeconds) * time.Second
		if healthySamples >= policy.RecoverHealthySamples && elapsed >= minDur {
			nextState.Level = PressureNormal
			nextState.GrowthBlocked = false
			nextState.ThrottleConcurrency = 0
			nextState.UnsatisfiedFloor = false
			nextState.FloorDetail = nil
			nextState.HealthySamples = 0
			nextState.HealthySince = nil
			nextState.Reasons = nil
			alerts = append(alerts, AlertRecordV1{
				SchemaVersion:    AlertJournalSchemaV1,
				At:               nowTS,
				EventType:        AlertEventRecovery,
				FromLevel:        prevLevel,
				ToLevel:          PressureNormal,
				Reasons:          []string{fmt.Sprintf("recovered after %d consecutive healthy samples over %s", healthySamples, elapsed)},
				GrowthBlocked:    false,
				UnsatisfiedFloor: false,
			})
		} else {
			nextState.Level = prevLevel
			nextState.GrowthBlocked = prevBlocked
			nextState.ThrottleConcurrency = prevThrottle
			nextState.UnsatisfiedFloor = prevUnsatisfied
			nextState.FloorDetail = prevFloorDetail
			nextState.HealthySamples = healthySamples
			nextState.HealthySince = healthySince
			nextState.Reasons = []string{fmt.Sprintf(
				"recovery hysteresis in progress: %d/%d healthy samples, %s/%s elapsed",
				healthySamples, policy.RecoverHealthySamples, elapsed, minDur,
			)}
		}
	}

	if err := AppendAlertRecords(root, c.opts.MaxAlertRecords, alerts...); err != nil {
		return PressureResult{}, err
	}
	if err := SavePressureState(root, nextState); err != nil {
		return PressureResult{}, err
	}

	res.State = nextState
	res.EmittedAlerts = alerts
	c.emitOTel(ctx, res)
	return res, nil
}
