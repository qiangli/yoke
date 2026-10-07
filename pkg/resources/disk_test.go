package resources

import (
	"strings"
	"testing"
)

// apfsFixtureCandidates models a Getfsstat enumeration captured with
// the false-100% report (bsize 4096 throughout; byte counts abbreviated).
// /dev/disk3s1s1 (/) is the sealed read-only system snapshot sharing the
// APFS container with Data; /dev/disk5s2 is a mounted read-only installer
// disk image at 99.6% that the old pipeline surfaced as host disk pressure.
func apfsFixtureCandidates() []diskCandidate {
	return []diskCandidate{
		{mount: "/", device: "/dev/disk3s1s1", fstype: "apfs", readOnly: true, blockSize: 4096, blocks: 120699413, bfree: 14957901, bavail: 14957901},
		{mount: "/System/Volumes/VM", device: "/dev/disk3s6", fstype: "apfs", blockSize: 4096, blocks: 120699413, bfree: 14957989, bavail: 14957989},
		{mount: "/System/Volumes/Preboot", device: "/dev/disk3s2", fstype: "apfs", blockSize: 4096, blocks: 120699413, bfree: 14697222, bavail: 14697222},
		{mount: "/System/Volumes/Update", device: "/dev/disk3s4", fstype: "apfs", blockSize: 4096, blocks: 120699413, bfree: 14841204, bavail: 14841204},
		{mount: "/System/Volumes/xarts", device: "/dev/disk1s2", fstype: "apfs", blockSize: 4096, blocks: 128000, bfree: 123206, bavail: 123206},
		{mount: "/System/Volumes/iSCPreboot", device: "/dev/disk1s1", fstype: "apfs", blockSize: 4096, blocks: 128000, bfree: 123206, bavail: 123206},
		{mount: "/System/Volumes/Hardware", device: "/dev/disk1s3", fstype: "apfs", blockSize: 4096, blocks: 128000, bfree: 123206, bavail: 123206},
		{mount: "/System/Volumes/Data", device: "/dev/disk3s5", fstype: "apfs", blockSize: 4096, blocks: 120699413, bfree: 14957124, bavail: 14957124},
		{mount: "/Volumes/ZoomLauncher", device: "/dev/disk4s1", fstype: "hfs", readOnly: true, blockSize: 4096, blocks: 51190, bfree: 49742, bavail: 49742},
		{mount: "/Volumes/ZCode 3.14.4-arm64", device: "/dev/disk5s2", fstype: "hfs", readOnly: true, blockSize: 4096, blocks: 189726, bfree: 736, bavail: 736},
	}
}

// TestCandidateDisksDropsReadOnlyMedia is the false-100% regression: the
// mounted installer image at 99.6% must not survive collection, so the
// worst surviving volume stays the APFS container (~88%, elevated) rather
// than a read-only DMG (~100%, critical).
func TestCandidateDisksDropsReadOnlyMedia(t *testing.T) {
	got := dedupeByKey(candidateDisks(apfsFixtureCandidates()), apfsContainerKey)
	for _, d := range got {
		if strings.HasPrefix(d.Mount, "/Volumes/") {
			t.Errorf("read-only media survived collection: %+v", d)
		}
	}
	worst := 0.0
	for _, d := range got {
		worst = max(worst, d.UsedPercent)
	}
	if worst >= 90 {
		t.Errorf("worst disk = %.1f%%, want below the 90%% alert threshold; disks: %+v", worst, got)
	}
	if len(got) != 2 {
		t.Errorf("got %d disks %v, want [/ /System/Volumes/xarts] after APFS dedupe", len(got), got)
	}
}

// TestCandidateDisksKeepsReadOnlyRoot pins the exception: the sealed APFS
// system volume is read-only yet stays, as the covering fallback and the
// container's canonical representative.
func TestCandidateDisksKeepsReadOnlyRoot(t *testing.T) {
	got := candidateDisks([]diskCandidate{
		{mount: "/", device: "/dev/disk3s1s1", fstype: "apfs", readOnly: true, blockSize: 4096, blocks: 120699413, bfree: 14957901, bavail: 14957901},
	})
	if len(got) != 1 || got[0].Mount != "/" {
		t.Fatalf("read-only root dropped: %+v", got)
	}
	root := got[0]
	// Actual filesystem accounting: free is caller-available bytes and the
	// percentage is taken over used+free, matching df's Avail/Capacity
	// columns rather than root-reserved blocks.
	if want := uint64(14957901) * 4096; root.FreeBytes != want {
		t.Errorf("root free = %d, want bavail*bsize = %d", root.FreeBytes, want)
	}
	if got := root.UsedPercent; got < 87 || got > 89 {
		t.Errorf("root used = %.1f%%, want the container-wide ~88%% statfs reports (df shows 18%% for the snapshot alone)", got)
	}
}

// TestCandidateDisksSkipsZeroSized pseudo mounts divide by nothing.
func TestCandidateDisksSkipsZeroSized(t *testing.T) {
	got := candidateDisks([]diskCandidate{
		{mount: "/System/Volumes/Data/home", device: "map auto_home", fstype: "autofs", blockSize: 1024, blocks: 0, bfree: 0, bavail: 0},
		{mount: "/", device: "/dev/disk3s1s1", fstype: "apfs", readOnly: true, blockSize: 4096, blocks: 120699413, bfree: 14957901, bavail: 14957901},
	})
	if len(got) != 1 || got[0].Mount != "/" {
		t.Errorf("zero-sized pseudo mount survived: %+v", got)
	}
}

// apfsContainerKey is the test-local spelling of the darwin container rule
// (/dev/disk3s1s1 -> /dev/disk3); sys_darwin.go owns the production copy.
func apfsContainerKey(d Disk) string {
	dev := d.Device
	if !strings.HasPrefix(dev, "/dev/disk") {
		return dev
	}
	if i := strings.IndexByte(dev[len("/dev/disk"):], 's'); i >= 0 {
		return dev[:len("/dev/disk")+i]
	}
	return dev
}
