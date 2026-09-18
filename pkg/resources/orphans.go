package resources

import (
	"fmt"
	"sort"
	"strings"
)

// The residue a leaked gate leaves behind.
//
// When a timed-out gate's process group is not reaped, `go test` and the
// pkg.test binary it forked are reparented to init and keep running — Sprint 191
// saw two interp.test processes survive 12 and 39 hours at ~400% CPU each,
// materially distorting resource alerts until a human killed them. The gate seam
// that leaked them is fixed in pkg/gate, but a diagnostic still has to SURFACE
// any that predate the fix or escape it, because the alerting layer otherwise
// attributes their CPU to nothing and reports pressure with no owner to blame.

// isTestBinaryName reports whether name looks like a Go test binary — the
// `pkg.test` shape `go test` compiles and executes. Process names are the short
// comm/BSD name (truncated to ~15 bytes), so this matches the suffix the
// convention guarantees rather than a full path.
func isTestBinaryName(name string) bool {
	return strings.HasSuffix(name, ".test")
}

// OrphanTestProcesses returns the observed processes that look like Go test
// binaries reparented to init (PPID 1) and attributed to no live workload.
//
// It is deliberately conservative, mirroring the attribution rule: a test
// process still owned by a VERIFIED live run is not an orphan and is never
// reported, so a legitimate in-flight gate is left untouched. Only a test binary
// that has both lost its launcher (reparented to init) AND belongs to no live
// workload qualifies — exactly the shape a leaked, timed-out gate leaves.
func OrphanTestProcesses(procs []ProcessObservation) []ProcessObservation {
	var out []ProcessObservation
	for _, p := range procs {
		if p.PPID != 1 {
			continue
		}
		if !isTestBinaryName(p.Name) {
			continue
		}
		if p.Attribution == "verified" {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity.PID < out[j].Identity.PID })
	return out
}

// orphanTestProcessWarnings renders one operator-facing line per orphan, for the
// existing resource-diagnostics warnings channel. It only warns — killing a
// process it does not own is not its job; naming it so a human (or a higher
// layer) can is.
func orphanTestProcessWarnings(procs []ProcessObservation) []string {
	orphans := OrphanTestProcesses(procs)
	if len(orphans) == 0 {
		return nil
	}
	out := make([]string, 0, len(orphans))
	for _, p := range orphans {
		cpu := "cpu unknown"
		if p.CPU.Value != nil && !p.CPU.Status.Stale {
			cpu = "~" + trimFloat(*p.CPU.Value) + "% cpu"
		}
		out = append(out, fmt.Sprintf(
			"orphan test process %s (pid %d, %s) reparented to init and owned by no live workload; a timed-out gate likely leaked it — kill it to clear resource alerts",
			p.Name, p.Identity.PID, cpu))
	}
	return out
}
