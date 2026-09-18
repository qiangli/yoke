package resources

import (
	"reflect"
	"strings"
	"testing"
)

const procStatFixture = `cpu  100 20 30 800 50 0 0 0 0 0
cpu0 50 10 15 400 25 0 0 0 0 0
cpu1 50 10 15 400 25 0 0 0 0 0
intr 12345 0 0
ctxt 999
`

func TestParseProcStat(t *testing.T) {
	agg, cores, ok := parseProcStat(strings.NewReader(procStatFixture))
	if !ok {
		t.Fatal("aggregate cpu line not found")
	}
	// total is every column; idle is the idle+iowait pair.
	if agg.total != 1000 || agg.idle != 850 {
		t.Fatalf("aggregate = %+v, want {total:1000 idle:850}", agg)
	}
	if len(cores) != 2 {
		t.Fatalf("cores = %d, want 2", len(cores))
	}
	for i, c := range cores {
		if c.total != 500 || c.idle != 425 {
			t.Errorf("core %d = %+v, want {total:500 idle:425}", i, c)
		}
	}
	if got := tickUsage(cpuTicks{}, agg); got != 15 {
		t.Errorf("usage = %v, want 15", got)
	}
}

func TestParseProcStatOutOfOrderCores(t *testing.T) {
	// The kernel emits cpuN in order, but the parser indexes by N so a
	// per-core percentage can never be attributed to the wrong core.
	in := "cpu 4 0 0 6 0\ncpu1 3 0 0 1 0\ncpu0 1 0 0 5 0\n"
	_, cores, ok := parseProcStat(strings.NewReader(in))
	if !ok || len(cores) != 2 {
		t.Fatalf("cores = %+v ok=%v", cores, ok)
	}
	if cores[0].total != 6 || cores[1].total != 4 {
		t.Fatalf("cores mis-indexed: %+v", cores)
	}
}

func TestParseMeminfo(t *testing.T) {
	in := `MemTotal:       16307180 kB
MemFree:          230228 kB
MemAvailable:    8000000 kB
Buffers:          104120 kB
SwapTotal:       2097148 kB
SwapFree:        1000000 kB
HugePages_Total:       0
`
	got := parseMeminfo(strings.NewReader(in))
	if got["MemTotal"] != 16307180*1024 {
		t.Errorf("MemTotal = %d", got["MemTotal"])
	}
	if got["MemAvailable"] != 8000000*1024 {
		t.Errorf("MemAvailable = %d", got["MemAvailable"])
	}
	// A unit-less value must not be scaled.
	if got["HugePages_Total"] != 0 {
		t.Errorf("HugePages_Total = %d", got["HugePages_Total"])
	}
}

func TestParseNetDev(t *testing.T) {
	in := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets
    lo:  123456    1000    0    0    0     0          0         0   123456    1000
  eth0: 9876543   20000    0    0    0     0          0         0  1234567    5000
`
	got := parseNetDev(strings.NewReader(in))
	want := []netCounter{
		{name: "lo", rxBytes: 123456, txBytes: 123456},
		{name: "eth0", rxBytes: 9876543, txBytes: 1234567},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseDiskstats(t *testing.T) {
	in := `   8       0 sda 100 0 2048 50 200 0 4096 90 0 0 0
 259       0 nvme0n1 1 0 8 1 2 0 16 1 0 0 0
`
	got := parseDiskstats(strings.NewReader(in))
	if len(got) != 2 {
		t.Fatalf("got %d entries", len(got))
	}
	// Sectors are always 512 bytes in diskstats regardless of hardware.
	if got[0].device != "sda" || got[0].readBytes != 2048*512 || got[0].writeBytes != 4096*512 {
		t.Fatalf("sda = %+v", got[0])
	}
}

func TestParseLoadavg(t *testing.T) {
	got, ok := parseLoadavg("0.52 0.61 0.70 2/1234 5678\n")
	if !ok || !reflect.DeepEqual(got, []float64{0.52, 0.61, 0.70}) {
		t.Fatalf("got %v ok=%v", got, ok)
	}
	if _, ok := parseLoadavg("garbage"); ok {
		t.Fatal("garbage parsed as a load average")
	}
}

func TestParseCPUModel(t *testing.T) {
	in := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) CPU\n"
	if got := parseCPUModel(strings.NewReader(in)); got != "Intel(R) Xeon(R) CPU" {
		t.Fatalf("got %q", got)
	}
}

func TestParseMountinfo(t *testing.T) {
	in := `25 30 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
26 25 0:22 / /proc rw,nosuid - proc proc rw
27 25 0:23 / /sys rw,nosuid - sysfs sysfs rw
28 25 0:24 / /run rw,nosuid - tmpfs tmpfs rw
29 25 259:1 / /boot/efi rw,relatime - vfat /dev/nvme0n1p1 rw
31 25 259:3 / /mnt/my\040disk rw - xfs /dev/sdb1 rw
32 25 259:2 / /var/lib/docker rw - ext4 /dev/nvme0n1p2 rw
`
	got := parseMountinfo(strings.NewReader(in))
	want := []mountLine{
		{device: "/dev/nvme0n1p2", point: "/", fstype: "ext4"},
		{device: "/dev/nvme0n1p1", point: "/boot/efi", fstype: "vfat"},
		{device: "/dev/sdb1", point: "/mnt/my disk", fstype: "xfs"},
		{device: "/dev/nvme0n1p2", point: "/var/lib/docker", fstype: "ext4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestDeviceKey(t *testing.T) {
	for in, want := range map[string]string{
		"/dev/nvme0n1p2": "nvme0n1p2",
		"/dev/sda1":      "sda1",
		"sda":            "sda",
		"":               ".",
	} {
		if got := deviceKey(in); got != want {
			t.Errorf("deviceKey(%q) = %q, want %q", in, got, want)
		}
	}
}
