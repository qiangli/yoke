package resources

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func rss(v uint64) *uint64 { return &v }

func sample(pid, ppid int, r uint64) processSample {
	return processSample{Identity: ProcessIdentity{PID: pid}, PPID: ppid, RSS: rss(r)}
}

func TestTreeUsageSumsDescendantsOnly(t *testing.T) {
	samples := []processSample{
		sample(1, 0, 1000),
		sample(10, 1, 100), // root
		sample(11, 10, 20),
		sample(12, 11, 3),
		sample(13, 1, 5000), // sibling, not ours
		sample(14, 13, 7000),
		{Identity: ProcessIdentity{PID: 15}, PPID: 12}, // RSS unknown still counts as a process
	}
	u := treeUsage(samples, 10)
	if u.Processes != 4 || u.RSSBytes != 123 {
		t.Fatalf("usage = %+v, want 4 processes / 123 bytes", u)
	}
	if len(u.PIDs) != 4 || u.PIDs[0] != 10 {
		t.Fatalf("pids = %v, want root first then descendants", u.PIDs)
	}
	if got := treeUsage(samples, 99); got.Processes != 0 {
		t.Fatalf("absent root = %+v, want empty", got)
	}
	// A PPID cycle (pid reuse) must not loop.
	cyc := []processSample{sample(2, 3, 1), sample(3, 2, 1)}
	if got := treeUsage(cyc, 2); got.Processes != 2 {
		t.Fatalf("cycle usage = %+v", got)
	}
}

func TestCoveringDiskPicksLongestMountPrefix(t *testing.T) {
	disks := []Disk{
		{Mount: "/", FreeBytes: 1, TotalBytes: 10},
		{Mount: "/data", FreeBytes: 2, TotalBytes: 20},
		{Mount: "/data/deep", FreeBytes: 3, TotalBytes: 30},
		{Mount: `C:\`, FreeBytes: 4, TotalBytes: 40},
	}
	for path, want := range map[string]uint64{
		"/":            1,
		"/usr/bin":     1,
		"/data":        2,
		"/data/x":      2,
		"/database":    1, // not a path-boundary match
		"/data/deep/y": 3,
		`C:\Users\me`:  4,
		`c:\users`:     4,
	} {
		d, ok := coveringDisk(disks, path)
		if !ok || d.FreeBytes != want {
			t.Errorf("coveringDisk(%q) = %+v %v, want free %d", path, d, ok, want)
		}
	}
	if _, ok := coveringDisk(disks, `D:\x`); ok {
		t.Error("an uncovered path must report not found")
	}
}

func TestParseQuantity(t *testing.T) {
	for in, want := range map[string]uint64{
		"0":     0,
		"512":   512,
		"1Ki":   1024,
		"64Mi":  64 << 20,
		"4Gi":   4 << 30,
		"1Ti":   1 << 40,
		"1K":    1000,
		"2M":    2e6,
		"3G":    3e9,
		"1T":    1e12,
		"2g":    2 << 30,
		"16m":   16 << 20,
		"8k":    8 << 10,
		"1.5Gi": 3 << 29,
	} {
		got, err := ParseQuantity(in)
		if err != nil || got != want {
			t.Errorf("ParseQuantity(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Gi", "-1Gi", "4GB", "1Xi", "abc"} {
		if _, err := ParseQuantity(bad); err == nil {
			t.Errorf("ParseQuantity(%q) accepted", bad)
		}
	}
}

func TestProcessTreeLiveChild(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no native process source")
	}
	sleeper := []string{"sleep", "5"}
	if runtime.GOOS == "windows" {
		sleeper = []string{"ping", "-n", "6", "127.0.0.1"}
	}
	cmd := exec.Command(sleeper[0], sleeper[1:]...)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start %v: %v", sleeper, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var u TreeUsage
	var err error
	for range 20 { // the child may take a beat to appear in the table
		if u, err = ProcessTree(t.Context(), cmd.Process.Pid); err == nil && u.Processes >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || u.Processes < 1 || u.RSSBytes == 0 {
		t.Fatalf("ProcessTree(child) = %+v, %v", u, err)
	}
}

func TestMemoryNowAndFreeDiskAtLiveHost(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no native probes")
	}
	m, err := MemoryNow()
	if err != nil || m.TotalBytes == 0 || m.AvailableBytes == 0 {
		t.Fatalf("MemoryNow = %+v, %v", m, err)
	}
	dir := t.TempDir()
	free, total, err := FreeDiskAt(dir)
	if err != nil || total == 0 || free > total {
		t.Fatalf("FreeDiskAt(%s) = %d/%d, %v", dir, free, total, err)
	}
	wd, _ := os.Getwd()
	if _, _, err := FreeDiskAt(filepath.Base(wd)); err != nil {
		t.Fatalf("a relative path must resolve against cwd: %v", err)
	}
}
