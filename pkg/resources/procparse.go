package resources

import (
	"bufio"
	"io"
	"path"
	"strconv"
	"strings"
)

// This file holds the pure parsers for Linux's /proc text interfaces. They
// live outside the linux build tag on purpose: the parsing is where the
// bugs are, and keeping it platform-neutral means the table tests exercise
// it on every CI leg, not just the Linux one.

// parseProcStat reads /proc/stat's cpu lines. The first ("cpu") is the
// aggregate; "cpuN" lines are the cores, returned in N order.
func parseProcStat(r io.Reader) (agg cpuTicks, cores []cpuTicks, ok bool) {
	sc := bufio.NewScanner(r)
	indexed := map[int]cpuTicks{}
	maxIdx := -1
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		var t cpuTicks
		for i, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				break
			}
			t.total += v
			// user nice system IDLE IOWAIT irq softirq steal ...
			// idle and iowait are both time the CPU had nothing to run.
			if i == 3 || i == 4 {
				t.idle += v
			}
		}
		if fields[0] == "cpu" {
			agg, ok = t, true
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu"))
		if err != nil {
			continue
		}
		indexed[n] = t
		if n > maxIdx {
			maxIdx = n
		}
	}
	for i := 0; i <= maxIdx; i++ {
		cores = append(cores, indexed[i])
	}
	return agg, cores, ok
}

// parseMeminfo returns /proc/meminfo keys in bytes (the file reports kB).
func parseMeminfo(r io.Reader) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			v *= 1024
		}
		out[key] = v
	}
	return out
}

// parseNetDev reads /proc/net/dev. Loopback is kept: a steward comparing
// mesh chatter against local IPC wants both.
func parseNetDev(r io.Reader) []netCounter {
	var out []netCounter
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if name == "" || len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, netCounter{name: name, rxBytes: rx, txBytes: tx})
	}
	return out
}

// parseDiskstats reads /proc/diskstats. Fields 3 and 7 (0-based, after the
// device name) are sectors read and written; the kernel always reports
// those in 512-byte units regardless of the hardware sector size.
func parseDiskstats(r io.Reader) []ioCounter {
	const sectorBytes = 512
	var out []ioCounter
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		read, err1 := strconv.ParseUint(fields[5], 10, 64)
		written, err2 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, ioCounter{device: fields[2], readBytes: read * sectorBytes, writeBytes: written * sectorBytes})
	}
	return out
}

// parseLoadavg reads /proc/loadavg's leading three averages.
func parseLoadavg(s string) ([]float64, bool) {
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return nil, false
	}
	out := make([]float64, 0, 3)
	for _, f := range fields[:3] {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// parseCPUModel pulls the model name out of /proc/cpuinfo. The key differs
// by architecture ("model name" on x86, "Model" or "Hardware" on arm).
func parseCPUModel(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, value, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "model name", "hardware", "cpu model":
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// mountLine is one entry of /proc/self/mountinfo.
type mountLine struct {
	device string
	point  string
	fstype string
}

// parseMountinfo reads /proc/self/mountinfo, keeping only real block-backed
// filesystems: the pseudo/virtual mounts (proc, sysfs, cgroup, the dozens of
// overlay and tmpfs entries a container host carries) are noise in a
// capacity view, not disks.
func parseMountinfo(r io.Reader) []mountLine {
	var out []mountLine
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		pre, post, found := strings.Cut(sc.Text(), " - ")
		if !found {
			continue
		}
		head := strings.Fields(pre)
		tail := strings.Fields(post)
		if len(head) < 5 || len(tail) < 2 {
			continue
		}
		m := mountLine{point: unescapeOctal(head[4]), fstype: tail[0], device: unescapeOctal(tail[1])}
		if !realFSType(m.fstype) || !strings.HasPrefix(m.device, "/") || seen[m.point] {
			continue
		}
		seen[m.point] = true
		out = append(out, m)
	}
	return out
}

// realFSType reports whether a filesystem type represents durable storage
// the steward can run out of.
func realFSType(t string) bool {
	switch t {
	case "ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "f2fs", "jfs", "reiserfs",
		"vfat", "exfat", "ntfs", "ntfs3", "fuseblk", "bcachefs":
		return true
	}
	return false
}

// unescapeOctal expands the \040-style escapes the kernel writes for
// spaces and other separators in mount paths.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// deviceKey reduces a device path to the diskstats name (/dev/nvme0n1p2 ->
// nvme0n1p2) so mount entries and IO counters line up.
func deviceKey(device string) string {
	return path.Base(strings.TrimSpace(device))
}
