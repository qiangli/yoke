package resources

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// TreeUsage is one process tree's footprint: the root plus every descendant
// by PPID closure. PIDs lists the members, root first, so a limit guard can
// signal the ones that left the root's process group.
type TreeUsage struct {
	Processes int    `json:"processes"`
	RSSBytes  uint64 `json:"rss_bytes"`
	PIDs      []int  `json:"pids,omitempty"`
}

// ProcessTree samples the host process table once and sums the tree rooted at
// rootPID. An exited root yields an empty usage, not an error.
func ProcessTree(ctx context.Context, rootPID int) (TreeUsage, error) {
	samples, _, err := collectProcessSamples(ctx)
	if err != nil {
		return TreeUsage{}, err
	}
	return treeUsage(samples, rootPID), nil
}

func treeUsage(samples []processSample, root int) TreeUsage {
	children := make(map[int][]int, len(samples))
	byPID := make(map[int]processSample, len(samples))
	for _, p := range samples {
		byPID[p.Identity.PID] = p
		children[p.PPID] = append(children[p.PPID], p.Identity.PID)
	}
	var u TreeUsage
	if _, ok := byPID[root]; !ok {
		return u
	}
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		p := byPID[pid]
		u.Processes++
		u.PIDs = append(u.PIDs, pid)
		if p.RSS != nil {
			u.RSSBytes += *p.RSS
		}
		for _, c := range children[pid] {
			if !seen[c] { // PID reuse can close a PPID cycle
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	return u
}

// MemoryNow is the host memory reading alone, without the CPU sample window
// Collect waits for.
func MemoryNow() (Memory, error) { return memoryStats() }

// FreeDiskAt reports the caller-available and total bytes of the filesystem
// holding path: the mount with the longest prefix covering the absolute path.
func FreeDiskAt(path string) (free, total uint64, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, 0, err
	}
	disks, err := diskStats()
	if err != nil {
		return 0, 0, err
	}
	d, ok := coveringDisk(disks, abs)
	if !ok {
		return 0, 0, fmt.Errorf("resources: no mounted filesystem covers %s", abs)
	}
	return d.FreeBytes, d.TotalBytes, nil
}

func coveringDisk(disks []Disk, path string) (Disk, bool) {
	best, found := Disk{}, false
	for _, d := range disks {
		if mountCovers(d.Mount, path) && (!found || len(d.Mount) > len(best.Mount)) {
			best, found = d, true
		}
	}
	return best, found
}

func mountCovers(mount, path string) bool {
	sep := "/"
	if strings.Contains(mount, `\`) || (len(mount) >= 2 && mount[1] == ':') {
		// A Windows volume: drive letters and paths are case-insensitive.
		sep, mount, path = `\`, strings.ToLower(mount), strings.ToLower(path)
	}
	mount = strings.TrimSuffix(mount, sep)
	return path == mount || strings.HasPrefix(path, mount+sep)
}

// ParseQuantity reads a Kubernetes-style byte quantity: binary Ki/Mi/Gi/Ti,
// decimal K/M/G/T, and docker's lowercase k/m/g/t as binary. A plain number
// is bytes; a fraction is allowed ("1.5Gi").
func ParseQuantity(s string) (uint64, error) {
	num, mult := s, 1.0
	for _, u := range []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
	} {
		if strings.HasSuffix(s, u.suffix) {
			num, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || num == "" || strings.ContainsAny(num, "eE+-") || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("invalid quantity %q (want e.g. 512Mi, 4Gi, 2G, 1g)", s)
	}
	return uint64(v * mult), nil
}
