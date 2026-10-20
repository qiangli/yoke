package resources

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// DiskFloorConfig defines the required floor for disk free space.
type DiskFloorConfig struct {
	MinFreeBytes   uint64
	MinFreePercent float64
	Mount          string
}

// ParseDiskFloor parses a floor specification string such as "100G", "10%",
// or comma-separated combinations like "100G,10%".
func ParseDiskFloor(s string) (DiskFloorConfig, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DiskFloorConfig{}, errors.New("empty disk floor specification")
	}

	var cfg DiskFloorConfig
	parts := strings.Split(s, ",")
	for _, p := range parts {
		part := strings.TrimSpace(p)
		if part == "" {
			continue
		}
		if strings.HasSuffix(part, "%") {
			numStr := strings.TrimSpace(strings.TrimSuffix(part, "%"))
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil || val <= 0 || val > 100 {
				return DiskFloorConfig{}, fmt.Errorf("invalid disk floor percent %q", part)
			}
			cfg.MinFreePercent = val
		} else {
			bytes, err := parseByteSize(part)
			if err != nil {
				return DiskFloorConfig{}, err
			}
			cfg.MinFreeBytes = bytes
		}
	}

	if cfg.MinFreeBytes == 0 && cfg.MinFreePercent <= 0 {
		return DiskFloorConfig{}, fmt.Errorf("invalid disk floor specification %q", s)
	}
	return cfg, nil
}

func parseByteSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty byte size")
	}

	// Separate numeric value and unit suffix
	idx := 0
	for idx < len(s) && (unicode.IsDigit(rune(s[idx])) || s[idx] == '.') {
		idx++
	}
	numStr := s[:idx]
	unitStr := strings.TrimSpace(s[idx:])

	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil || val <= 0 {
		return 0, fmt.Errorf("invalid disk size number %q", s)
	}

	multiplier := float64(1)
	switch strings.ToUpper(unitStr) {
	case "", "B":
		multiplier = 1
	case "K", "KB", "KIB":
		multiplier = 1024
	case "M", "MB", "MIB":
		multiplier = 1024 * 1024
	case "G", "GB", "GIB":
		multiplier = 1024 * 1024 * 1024
	case "T", "TB", "TIB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown size unit %q in %q", unitStr, s)
	}

	total := val * multiplier
	if total <= 0 || total > 1e18 {
		return 0, fmt.Errorf("size out of range: %q", s)
	}
	return uint64(total), nil
}

// CheckDiskFloor checks whether the given disks satisfy the floor config.
func CheckDiskFloor(disks []Disk, cfg DiskFloorConfig) error {
	if cfg.MinFreeBytes == 0 && cfg.MinFreePercent <= 0 {
		return nil
	}
	if len(disks) == 0 {
		return errors.New("disk floor alarm: no disk mounts available to check")
	}

	var targets []Disk
	if cfg.Mount != "" {
		for _, d := range disks {
			if d.Mount == cfg.Mount {
				targets = append(targets, d)
				break
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("disk floor alarm: mount %q not found", cfg.Mount)
		}
	} else {
		for _, d := range disks {
			// When evaluating all mounts against a byte floor, skip small mounts
			// whose total capacity is less than the required floor (e.g. boot/firmware partitions).
			if cfg.MinFreeBytes > 0 && d.TotalBytes < cfg.MinFreeBytes {
				continue
			}
			targets = append(targets, d)
		}
		if len(targets) == 0 {
			// Fallback to checking root "/" or the largest mount if all mounts are smaller than floor.
			var fallback Disk
			for _, d := range disks {
				if d.Mount == "/" {
					fallback = d
					break
				}
				if d.TotalBytes > fallback.TotalBytes {
					fallback = d
				}
			}
			targets = append(targets, fallback)
		}
	}

	for _, d := range targets {
		freePct := 0.0
		if d.TotalBytes > 0 {
			freePct = float64(d.FreeBytes) / float64(d.TotalBytes) * 100.0
		}

		breachedBytes := cfg.MinFreeBytes > 0 && d.FreeBytes < cfg.MinFreeBytes
		breachedPercent := cfg.MinFreePercent > 0 && freePct < cfg.MinFreePercent

		if breachedBytes || breachedPercent {
			return formatDiskAlarm(d, cfg, freePct, breachedBytes, breachedPercent)
		}
	}
	return nil
}

func formatFloorBytes(v uint64) string {
	s := HumanBytes(v)
	return strings.Replace(s, ".0", "", 1)
}

func formatDiskAlarm(d Disk, cfg DiskFloorConfig, freePct float64, breachedBytes, breachedPercent bool) error {
	freeStr := HumanBytes(d.FreeBytes)
	freePctStr := fmt.Sprintf("%.1f%%", freePct)

	var floorDesc string
	if breachedBytes && breachedPercent {
		floorDesc = fmt.Sprintf("%s and %s%%", formatFloorBytes(cfg.MinFreeBytes), trimFloat(cfg.MinFreePercent))
	} else if breachedBytes {
		floorDesc = formatFloorBytes(cfg.MinFreeBytes)
	} else {
		floorDesc = fmt.Sprintf("%s%%", trimFloat(cfg.MinFreePercent))
	}

	return fmt.Errorf("disk floor alarm: mount %q has %s free (%s free), below %s floor: run safe reclaim (weave prune, go clean -modcache)",
		d.Mount, freeStr, freePctStr, floorDesc)
}
