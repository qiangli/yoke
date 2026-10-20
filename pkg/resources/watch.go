package resources

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Level is the alarm vocabulary of the resource watcher.
type Level int

const (
	LevelNormal Level = iota
	LevelWarning
	LevelCritical
)

func (l Level) String() string {
	switch l {
	case LevelWarning:
		return "warning"
	case LevelCritical:
		return "critical"
	default:
		return "normal"
	}
}

func (l Level) MarshalJSON() ([]byte, error) { return []byte(`"` + l.String() + `"`), nil }

func (l *Level) UnmarshalJSON(b []byte) error {
	switch strings.Trim(strings.ToLower(string(b)), `"`) {
	case "warning":
		*l = LevelWarning
	case "critical":
		*l = LevelCritical
	default:
		*l = LevelNormal
	}
	return nil
}

// WatchConfig carries every threshold the watcher evaluates. Zero-value
// fields mean "not evaluated" (e.g. LinkMbps 0 disables the throughput
// metric); sensible values come from DefaultWatchConfig and are then
// narrowed by the host record and per-resource overrides.
type WatchConfig struct {
	DiskWarnUsed      float64 `json:"disk_warn_used"`
	DiskCritUsed      float64 `json:"disk_crit_used"`
	DiskFreeWarnBytes uint64  `json:"disk_free_warn_bytes"`
	DiskFreeCritBytes uint64  `json:"disk_free_crit_bytes"`

	MemWarn  float64 `json:"mem_warn"`
	MemCrit  float64 `json:"mem_crit"`
	SwapWarn float64 `json:"swap_warn"`
	SwapCrit float64 `json:"swap_crit"`

	CPUWarn    float64       `json:"cpu_warn"`
	CPUCrit    float64       `json:"cpu_crit"`
	CPUSustain time.Duration `json:"cpu_sustain"`

	GPUWarn    float64       `json:"gpu_warn"`
	GPUCrit    float64       `json:"gpu_crit"`
	GPUSustain time.Duration `json:"gpu_sustain"`
	GPUMemWarn float64       `json:"gpu_mem_warn"`
	GPUMemCrit float64       `json:"gpu_mem_crit"`

	NetWarn    float64       `json:"net_warn"`
	NetCrit    float64       `json:"net_crit"`
	NetSustain time.Duration `json:"net_sustain"`
	NetErrWarn float64       `json:"net_err_warn"`
	NetErrCrit float64       `json:"net_err_crit"`
	LinkMbps   float64       `json:"link_mbps,omitempty"`

	Hysteresis int           `json:"hysteresis"`
	Renotify   time.Duration `json:"renotify"`
	NotifyTo   string        `json:"notify_to,omitempty"`
	Enabled    bool          `json:"enabled"`
}

// DefaultWatchConfig is the operator-specified starting set (S480):
// disk 85/95% used with a 20/10 GiB free floor, memory 85/95, swap 50/80,
// CPU 85/95 sustained 5m, GPU util 90/98 sustained 5m, GPU memory 90/97,
// network errors above 1% of packets and 80/95% of link speed sustained 5m.
// The only value the operator spec left open is the network-error critical
// level: 5% is recorded here as the chosen default (docs/resource-watch.md).
func DefaultWatchConfig() WatchConfig {
	return WatchConfig{
		DiskWarnUsed:      85,
		DiskCritUsed:      95,
		DiskFreeWarnBytes: 20 << 30,
		DiskFreeCritBytes: 10 << 30,

		MemWarn:  85,
		MemCrit:  95,
		SwapWarn: 50,
		SwapCrit: 80,

		CPUWarn:    85,
		CPUCrit:    95,
		CPUSustain: 5 * time.Minute,

		GPUWarn:    90,
		GPUCrit:    98,
		GPUSustain: 5 * time.Minute,
		GPUMemWarn: 90,
		GPUMemCrit: 97,

		NetWarn:    80,
		NetCrit:    95,
		NetSustain: 5 * time.Minute,
		NetErrWarn: 1,
		NetErrCrit: 5,

		Hysteresis: 3,
		Renotify:   30 * time.Minute,
		Enabled:    true,
	}
}

// OverrideOnly returns base with the given watch overrides applied; entries
// that fail ValidateWatchOverride are skipped (the registry already refuses
// them at `resource set` time, so runtime input here is trusted-but-checked).
func OverrideOnly(base WatchConfig, kv map[string]string) WatchConfig {
	for _, k := range sortedKeys(kv) {
		v := kv[k]
		if err := ValidateWatchOverride(k, v); err != nil {
			continue
		}
		applyWatchOverride(&base, k, v)
	}
	return base
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ValidateWatchOverride checks one key=value pair accepted by
// `resource set --watch k=v`.
func ValidateWatchOverride(key, value string) error {
	if value == "" {
		return fmt.Errorf("watch: %s: empty value", key)
	}
	pct := func() error {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f <= 0 || f > 100 {
			return fmt.Errorf("watch: %s: %q is not a percentage in (0,100]", key, value)
		}
		return nil
	}
	switch key {
	case "disk-warn-used", "disk-crit-used", "mem-warn", "mem-crit",
		"swap-warn", "swap-crit", "cpu-warn", "cpu-crit",
		"gpu-warn", "gpu-crit", "gpu-mem-warn", "gpu-mem-crit",
		"net-warn", "net-crit", "net-err-warn", "net-err-crit":
		return pct()
	case "disk-free-warn", "disk-free-crit":
		if _, err := parseByteSize(value); err != nil {
			return fmt.Errorf("watch: %s: %v", key, err)
		}
		return nil
	case "cpu-sustain", "gpu-sustain", "net-sustain", "renotify":
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 {
			return fmt.Errorf("watch: %s: %q is not a duration", key, value)
		}
		return nil
	case "hysteresis":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return fmt.Errorf("watch: %s: %q is not a count in [1,100]", key, value)
		}
		return nil
	case "link-mbps":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f <= 0 || f > 1_000_000 {
			return fmt.Errorf("watch: %s: %q is not a link speed", key, value)
		}
		return nil
	case "enabled":
		if value != "true" && value != "false" {
			return fmt.Errorf("watch: %s: want true or false", key)
		}
		return nil
	case "notify-to":
		if strings.ContainsAny(value, " \t\r\n") {
			return fmt.Errorf("watch: %s: no whitespace", key)
		}
		return nil
	default:
		return fmt.Errorf("watch: unknown override key %q", key)
	}
}

func applyWatchOverride(cfg *WatchConfig, key, value string) {
	f := func() float64 {
		v, _ := strconv.ParseFloat(value, 64)
		return v
	}
	b := func() uint64 {
		v, _ := parseByteSize(value)
		return v
	}
	d := func() time.Duration {
		v, _ := time.ParseDuration(value)
		return v
	}
	switch key {
	case "disk-warn-used":
		cfg.DiskWarnUsed = f()
	case "disk-crit-used":
		cfg.DiskCritUsed = f()
	case "disk-free-warn":
		cfg.DiskFreeWarnBytes = b()
	case "disk-free-crit":
		cfg.DiskFreeCritBytes = b()
	case "mem-warn":
		cfg.MemWarn = f()
	case "mem-crit":
		cfg.MemCrit = f()
	case "swap-warn":
		cfg.SwapWarn = f()
	case "swap-crit":
		cfg.SwapCrit = f()
	case "cpu-warn":
		cfg.CPUWarn = f()
	case "cpu-crit":
		cfg.CPUCrit = f()
	case "cpu-sustain":
		cfg.CPUSustain = d()
	case "gpu-warn":
		cfg.GPUWarn = f()
	case "gpu-crit":
		cfg.GPUCrit = f()
	case "gpu-sustain":
		cfg.GPUSustain = d()
	case "gpu-mem-warn":
		cfg.GPUMemWarn = f()
	case "gpu-mem-crit":
		cfg.GPUMemCrit = f()
	case "net-warn":
		cfg.NetWarn = f()
	case "net-crit":
		cfg.NetCrit = f()
	case "net-sustain":
		cfg.NetSustain = d()
	case "net-err-warn":
		cfg.NetErrWarn = f()
	case "net-err-crit":
		cfg.NetErrCrit = f()
	case "link-mbps":
		cfg.LinkMbps = f()
	case "hysteresis":
		n, _ := strconv.Atoi(value)
		cfg.Hysteresis = n
	case "renotify":
		cfg.Renotify = d()
	case "notify-to":
		cfg.NotifyTo = value
	case "enabled":
		cfg.Enabled = value == "true"
	}
}

// ClassifyLevel maps a value onto the three-level vocabulary against the
// warning and critical thresholds (inclusive at the boundary).
func ClassifyLevel(v, warn, crit float64) Level {
	switch {
	case crit > 0 && v >= crit:
		return LevelCritical
	case warn > 0 && v >= warn:
		return LevelWarning
	default:
		return LevelNormal
	}
}

// WatchNamePrefix sanitizes a hostname into the prefix of the resource names
// the watcher registers and addresses (cpu, mem, disk-*, net-*, gpu-*).
func WatchNamePrefix(host string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(host) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = r == '-'
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// watchSlug flattens one resource-local identity (mount point, interface
// name, GPU name) into a name fragment.
func watchSlug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "root"
	}
	return out
}
