package resources

import (
	"strings"
	"testing"
	"time"
)

func proc(pid, ppid int, name, attribution string, cpu *float64) ProcessObservation {
	p := ProcessObservation{
		Identity:    ProcessIdentity{PID: pid, StartID: "test"},
		PPID:        ppid,
		Name:        name,
		Attribution: attribution,
	}
	if cpu != nil {
		p.CPU = observedValue(*cpu, "test", time.Now(), HostObservationTTL)
	}
	return p
}

func floatPtr(v float64) *float64 { return &v }

func TestOrphanTestProcessesSelectsLeakedTestBinaries(t *testing.T) {
	cpu := floatPtr(400)
	procs := []ProcessObservation{
		// The leaked interp.test: reparented to init, no live owner.
		proc(1001, 1, "interp.test", "unattributed", cpu),
		// A test binary still owned by a live run — NOT an orphan.
		proc(1002, 1, "webconsole.test", "verified", cpu),
		// A test binary with a real parent — not reparented, not an orphan.
		proc(1003, 900, "gate.test", "unattributed", cpu),
		// A reparented non-test process — out of scope.
		proc(1004, 1, "sleep", "unattributed", nil),
	}
	got := OrphanTestProcesses(procs)
	if len(got) != 1 {
		t.Fatalf("want exactly one orphan, got %d: %+v", len(got), got)
	}
	if got[0].Identity.PID != 1001 {
		t.Fatalf("want the leaked interp.test (pid 1001), got pid %d", got[0].Identity.PID)
	}

	warnings := orphanTestProcessWarnings(procs)
	if len(warnings) != 1 {
		t.Fatalf("want one warning, got %d", len(warnings))
	}
	if !strings.Contains(warnings[0], "interp.test") || !strings.Contains(warnings[0], "1001") {
		t.Fatalf("warning must name the process and pid: %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "400% cpu") {
		t.Fatalf("warning must report cpu when known: %q", warnings[0])
	}
}

func TestOrphanTestProcessesEmptyWhenNoneLeaked(t *testing.T) {
	procs := []ProcessObservation{
		proc(1002, 1, "webconsole.test", "verified", nil),
		proc(1003, 900, "gate.test", "unattributed", nil),
	}
	if got := OrphanTestProcesses(procs); len(got) != 0 {
		t.Fatalf("want no orphans, got %+v", got)
	}
	if got := orphanTestProcessWarnings(procs); got != nil {
		t.Fatalf("want no warnings, got %v", got)
	}
}
