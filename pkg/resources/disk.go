package resources

import "sort"

// dedupeByKey collapses mounts that are views of the SAME storage pool,
// keeping the one with the shortest mount path so the canonical mount
// ("/" over "/System/Volumes/Data") survives. Bind mounts, btrfs
// subvolumes, and APFS's eight-volumes-per-container layout all report the
// same capacity from several mount points; listing each one makes one
// 460GB disk look like eight and buries the number that matters.
//
// The key is deliberately caller-supplied: what counts as "the same pool"
// is platform knowledge (the device for a bind mount, the APFS container
// on darwin), not something this function should guess.
func dedupeByKey(disks []Disk, key func(Disk) string) []Disk {
	sorted := append([]Disk(nil), disks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if len(sorted[i].Mount) != len(sorted[j].Mount) {
			return len(sorted[i].Mount) < len(sorted[j].Mount)
		}
		return sorted[i].Mount < sorted[j].Mount
	})
	seen := map[string]bool{}
	out := make([]Disk, 0, len(sorted))
	for _, d := range sorted {
		k := key(d)
		if k != "" && seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}

// byDevice is the portable pool key: one entry per backing device, which
// is exactly what collapses bind mounts.
func byDevice(d Disk) string { return d.Device }

// diskCandidate is one OS-reported mount before capacity filtering. The
// platform collectors translate their native mount enumeration into these;
// candidateDisks applies the portable keep/drop rules.
type diskCandidate struct {
	mount, device, fstype            string
	readOnly                         bool
	blockSize, blocks, bfree, bavail uint64
}

// candidateDisks converts candidates to Disks, dropping mounts that carry
// no actionable capacity:
//
//   - Read-only media (installer disk images, mounted ISOs/DMGs) cannot
//     receive new bytes, so a nearly-full read-only volume is normal, not
//     pressure. Keeping one made a mounted installer report host disk at
//     ~100% while the writable container had tens of GiB free.
//   - Zero-sized pseudo mounts (autofs triggers and the like) divide by
//     nothing and display as 0 or 100% trivia.
//
// The root mount is always kept even when read-only: it is the covering
// fallback for destination-filesystem lookups, and on APFS the sealed
// system volume is the container's canonical representative.
func candidateDisks(in []diskCandidate) []Disk {
	out := make([]Disk, 0, len(in))
	for _, c := range in {
		if c.readOnly && c.mount != "/" {
			continue
		}
		d := diskFromStatfs(c.mount, c.device, c.fstype, c.blockSize, c.blocks, c.bfree, c.bavail)
		if d.TotalBytes == 0 {
			continue
		}
		out = append(out, d)
	}
	return out
}

// diskFromStatfs builds a Disk from the statfs quintet every unix reports.
// "Free" is the caller-available figure (bavail), not bfree: the
// root-reserved blocks are not space a fleet of agents can use, and
// reporting them as free is how a host runs out of disk while the board
// says it has room.
func diskFromStatfs(point, device, fstype string, blockSize, blocks, bfree, bavail uint64) Disk {
	d := Disk{Mount: point, Device: device, FSType: fstype}
	d.TotalBytes = blocks * blockSize
	d.FreeBytes = bavail * blockSize
	if blocks > bfree {
		d.UsedBytes = (blocks - bfree) * blockSize
	}
	d.UsedPercent = pct(d.UsedBytes, d.UsedBytes+d.FreeBytes)
	return d
}
