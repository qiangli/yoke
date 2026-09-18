// host_cleanup.go — offline cleanup of orphaned host-side VM state.
//
// Distinct from cleanup.go (which removes session-labeled containers
// inside a running VM). This file deals with the HYPERVISOR layer:
// vfkit/gvproxy processes that outlive their machine config, and
// stale unix sockets in the podman tmpdir.
//
// Why: ycode podman provisions a vfkit + gvproxy per machine. When
// init or shutdown goes sideways (kernel panic in the VM, ycode
// crash mid-init, the user runs `ycode podman machine reset` while
// vfkit is still alive), those processes can outlive the machine
// config — leaving zombies that hold memory, lock the disk image,
// and listen on sockets nothing reads. We hit exactly this scenario
// during cloudbox+k3s build testing today: TWO vfkit instances
// against the same disk image, neither responsive, both consuming
// RAM until manually SIGKILLed.
//
// Cleanup detects these orphans by cross-referencing process args
// against on-disk state:
//   - vfkit args carry `--device virtio-blk,path=<disk.raw>`. If the
//     disk image is missing, the machine has been removed but vfkit
//     didn't notice.
//   - gvproxy args carry `-pid-file <path>`. If the pid-file path is
//     gone, gvproxy was paired with a now-deleted machine.
//   - tmpdir `*.sock` files NOT referenced by any running vfkit /
//     gvproxy are stale; safe to remove.
//
// All operations are OFFLINE — no podman socket required. Suitable
// for the preflight auto-cleanup path, where the machine may not be
// running at all.

package engine

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// OrphanedProcess names a vfkit/gvproxy process the cleanup would
// SIGKILL.
type OrphanedProcess struct {
	PID     int    `json:"pid"`
	Command string `json:"command"` // "vfkit" or "gvproxy"
	Reason  string `json:"reason"`  // human-readable
}

// StaleSocket names a podman tmpdir socket file with no live owner.
type StaleSocket struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// HostCleanupReport summarizes a Cleanup pass for CLI output + the
// auto-cleanup retry path.
type HostCleanupReport struct {
	OrphanedProcesses []OrphanedProcess `json:"orphaned_processes"`
	StaleSockets      []StaleSocket     `json:"stale_sockets"`
	DryRun            bool              `json:"dry_run"`
}

// AnythingCleaned is the convenience the auto-cleanup retry path
// reads to decide whether to re-run preflight.
func (r HostCleanupReport) AnythingCleaned() bool {
	return len(r.OrphanedProcesses) > 0 || len(r.StaleSockets) > 0
}

// HostCleanupOptions tunes CleanupHost behavior. DryRun = true reports
// what would be removed without doing it.
type HostCleanupOptions struct {
	DryRun bool
}

// CleanupHost runs the offline cleanup. Idempotent — a second
// invocation immediately after the first is a no-op. Errors are
// log-warned but don't abort; partial progress beats no progress.
func CleanupHost(opts HostCleanupOptions) (HostCleanupReport, error) {
	report := HostCleanupReport{DryRun: opts.DryRun}

	procs, err := listVfkitGvproxyProcesses()
	if err != nil {
		return report, fmt.Errorf("enumerate processes: %w", err)
	}

	for _, p := range procs {
		orphan, isOrphan := classifyHostProcess(p)
		if !isOrphan {
			continue
		}
		report.OrphanedProcesses = append(report.OrphanedProcesses, orphan)
		if !opts.DryRun {
			if err := killProcess(p.PID); err != nil {
				slog.Warn("host_cleanup: kill failed", "pid", p.PID, "err", err)
			}
		}
	}

	socks, err := findStaleSockets(procs)
	if err != nil {
		slog.Warn("host_cleanup: socket scan failed", "err", err)
	}
	for _, s := range socks {
		report.StaleSockets = append(report.StaleSockets, s)
		if !opts.DryRun {
			if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("host_cleanup: rm socket failed", "path", s.Path, "err", err)
			}
		}
	}

	return report, nil
}

// hostProcess: one row from `ps`. Local type since cleanup.go's
// existing types are container-shaped.
type hostProcess struct {
	PID     int
	Command string // basename(argv[0])
	Args    string // argv joined by spaces
}

// listVfkitGvproxyProcesses runs `ps` and filters to vfkit + gvproxy
// processes. macOS + Linux compatible (both ship `ps -e -o pid,command`).
func listVfkitGvproxyProcesses() ([]hostProcess, error) {
	out, err := exec.Command("ps", "-e", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var procs []hostProcess
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Format: "  PID  /path/to/cmd args..."
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		args := strings.TrimSpace(fields[1])
		argv := strings.Fields(args)
		if len(argv) == 0 {
			continue
		}
		base := filepath.Base(argv[0])
		if base != "vfkit" && base != "gvproxy" {
			continue
		}
		procs = append(procs, hostProcess{PID: pid, Command: base, Args: args})
	}
	return procs, nil
}

// classifyHostProcess decides whether a vfkit/gvproxy is orphaned. Pure
// function over the process record so unit tests can drive every
// branch without ps shenanigans.
func classifyHostProcess(p hostProcess) (OrphanedProcess, bool) {
	switch p.Command {
	case "vfkit":
		if path := extractVfkitDiskPath(p.Args); path != "" {
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				return OrphanedProcess{
					PID:     p.PID,
					Command: "vfkit",
					Reason:  fmt.Sprintf("disk image missing: %s", path),
				}, true
			}
		}
	case "gvproxy":
		// gvproxy's pid-file lives in the same tmpdir as its sockets;
		// if it's gone, the machine config was wiped from under us.
		if path := extractGvproxyPidFile(p.Args); path != "" {
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				return OrphanedProcess{
					PID:     p.PID,
					Command: "gvproxy",
					Reason:  fmt.Sprintf("pid-file missing: %s", path),
				}, true
			}
		}
	}
	return OrphanedProcess{}, false
}

// extractVfkitDiskPath pulls the virtio-blk disk image path from
// vfkit's args. Format: `--device virtio-blk,path=<file>...`.
func extractVfkitDiskPath(args string) string {
	const marker = "virtio-blk,path="
	i := strings.Index(args, marker)
	if i < 0 {
		return ""
	}
	rest := args[i+len(marker):]
	if end := strings.IndexAny(rest, ", \t"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// extractGvproxyPidFile pulls the pid-file path from gvproxy's args.
// Format: `-pid-file <path>` (space-separated).
func extractGvproxyPidFile(args string) string {
	const marker = "-pid-file "
	i := strings.Index(args, marker)
	if i < 0 {
		return ""
	}
	rest := args[i+len(marker):]
	if end := strings.IndexAny(rest, " \t"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// findStaleSockets returns *.sock files in podman tmpdir that no
// running vfkit or gvproxy references.
//
// Conservative: when in doubt, leave the socket alone. False
// negatives (orphan stays one more run) are cheap; false positives
// (delete a live socket) break the active machine.
func findStaleSockets(active []hostProcess) ([]StaleSocket, error) {
	dir := filepath.Join(os.TempDir(), "podman")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	referenced := map[string]bool{}
	for _, p := range active {
		for _, sp := range extractSocketPaths(p.Args) {
			referenced[sp] = true
		}
	}
	var stale []StaleSocket
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if referenced[full] {
			continue
		}
		stale = append(stale, StaleSocket{Path: full, Reason: "no listener (owner process gone)"})
	}
	return stale, nil
}

// extractSocketPaths returns every path-like token in args that ends
// in ".sock" — captures vfkit's `socketURL=...`, gvproxy's
// `-forward-sock`, etc. Strips a leading "key=" and known schemes so
// `socketURL=unixgram:///path/x.sock` resolves to `/path/x.sock`.
//
// vfkit device specs are comma-separated WITHIN a single argv token
// (e.g. `virtio-vsock,port=1025,socketURL=/path/x.sock,listen`), so
// we split by commas FIRST, then strip "key=" / scheme from each
// piece. Doing the key= strip on the whole token would only catch
// the first `=` and leave later sub-fields with their `socketURL=`
// prefix attached.
func extractSocketPaths(args string) []string {
	var paths []string
	for _, tok := range strings.Fields(args) {
		for _, sub := range strings.Split(tok, ",") {
			// Strip a leading "key=" if present.
			if eq := strings.IndexByte(sub, '='); eq >= 0 {
				sub = sub[eq+1:]
			}
			// Strip scheme prefixes (`unixgram://`, `unix://`).
			for _, scheme := range []string{"unixgram://", "unix://"} {
				if strings.HasPrefix(sub, scheme) {
					sub = sub[len(scheme):]
					break
				}
			}
			if strings.HasSuffix(sub, ".sock") {
				paths = append(paths, sub)
			}
		}
	}
	return paths
}
