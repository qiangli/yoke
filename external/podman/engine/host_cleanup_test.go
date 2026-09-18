package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realVfkitArgs is a sample of the exact `ps -e -o command` output we
// captured during today's incident — the 4 GB ycode-default vfkit
// that survived its machine state being wiped. Used to catch a parser
// regression against the actual format we'll see in production.
const realVfkitArgs = `/Users/you/Library/Caches/ycode/bin/vfkit --cpus 2 --memory 4096 --bootloader efi,variable-store=/Users/you/.local/share/containers/podman/machine/applehv/efi-bl-ycode-default,create --device virtio-blk,path=/Users/you/.local/share/containers/podman/machine/applehv/ycode-default-arm64.raw --device virtio-rng --device virtio-vsock,port=1025,socketURL=/var/folders/vg/.../podman/ycode-default.sock,listen --device virtio-serial,logFilePath=/var/folders/vg/.../podman/ycode-default.log --device virtio-net,unixSocketPath=/var/folders/vg/.../podman/ycode-default-gvproxy.sock,mac=5a:94:ef:e4:0c:ee --restful-uri tcp://localhost:62126`

const realGvproxyArgs = `/Users/you/Library/Caches/ycode/bin/gvproxy -mtu 1500 -ssh-port 62114 -listen-vfkit unixgram:///var/folders/vg/.../podman/ycode-default-gvproxy.sock -forward-sock /var/folders/vg/.../podman/ycode-default-api.sock -forward-dest /run/user/501/podman/podman.sock -forward-user core -forward-identity /Users/you/.local/share/containers/podman/machine/machine -pid-file /var/folders/vg/.../podman/gvproxy.pid -log-file /var/folders/vg/.../podman/gvproxy.log`

func TestExtractVfkitDiskPath(t *testing.T) {
	got := extractVfkitDiskPath(realVfkitArgs)
	want := "/Users/you/.local/share/containers/podman/machine/applehv/ycode-default-arm64.raw"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if extractVfkitDiskPath("no marker here") != "" {
		t.Errorf("missing marker should return empty")
	}
}

func TestExtractGvproxyPidFile(t *testing.T) {
	got := extractGvproxyPidFile(realGvproxyArgs)
	want := "/var/folders/vg/.../podman/gvproxy.pid"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestExtractSocketPaths covers the variety of socket-bearing tokens
// in real vfkit args: bare path, key=path, scheme://path, and the
// comma-separated form inside virtio-* device specs.
func TestExtractSocketPaths(t *testing.T) {
	args := `--device virtio-vsock,port=1025,socketURL=/path/to/x.sock,listen ` +
		`--device virtio-net,unixSocketPath=/path/to/y.sock,mac=ab:cd ` +
		`-listen-vfkit unixgram:///path/to/z.sock ` +
		`-forward-sock /path/to/w.sock`
	got := extractSocketPaths(args)
	wantContains := []string{
		"/path/to/x.sock",
		"/path/to/y.sock",
		"/path/to/z.sock",
		"/path/to/w.sock",
	}
	for _, w := range wantContains {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in extracted paths %v", w, got)
		}
	}
}

// TestClassifyVfkitOrphan: a vfkit pointing at a disk path that exists
// is NOT an orphan; one pointing at a missing path IS.
func TestClassifyVfkitOrphan(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, "live.raw")
	if err := os.WriteFile(livePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(tmp, "missing.raw")

	live := hostProcess{
		PID: 100, Command: "vfkit",
		Args: "vfkit --device virtio-blk,path=" + livePath,
	}
	orphan, isOrphan := classifyHostProcess(live)
	if isOrphan {
		t.Errorf("live vfkit flagged as orphan: %+v", orphan)
	}

	dead := hostProcess{
		PID: 101, Command: "vfkit",
		Args: "vfkit --device virtio-blk,path=" + missingPath,
	}
	orphan, isOrphan = classifyHostProcess(dead)
	if !isOrphan {
		t.Errorf("vfkit referencing missing disk not flagged as orphan")
	}
	if !strings.Contains(orphan.Reason, missingPath) {
		t.Errorf("reason should name the missing disk: %q", orphan.Reason)
	}
}

// TestClassifyGvproxyOrphan: same logic, gvproxy + pid-file.
func TestClassifyGvproxyOrphan(t *testing.T) {
	tmp := t.TempDir()
	livePid := filepath.Join(tmp, "gvproxy.pid")
	if err := os.WriteFile(livePid, []byte("123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingPid := filepath.Join(tmp, "missing.pid")

	live := hostProcess{
		PID: 200, Command: "gvproxy",
		Args: "gvproxy -pid-file " + livePid + " -log-file /tmp/log",
	}
	if _, isOrphan := classifyHostProcess(live); isOrphan {
		t.Error("live gvproxy flagged as orphan")
	}

	dead := hostProcess{
		PID: 201, Command: "gvproxy",
		Args: "gvproxy -pid-file " + missingPid,
	}
	orphan, isOrphan := classifyHostProcess(dead)
	if !isOrphan {
		t.Error("gvproxy referencing missing pid-file not flagged as orphan")
	}
	if !strings.Contains(orphan.Reason, missingPid) {
		t.Errorf("reason should name the missing pid-file: %q", orphan.Reason)
	}
}

// TestClassifyNonVfkitGvproxyIgnored covers the safety property: we
// don't accidentally classify random processes (the user's editor,
// their shell) as orphans. classifyHostProcess only acts on commands
// "vfkit" or "gvproxy".
func TestClassifyNonVfkitGvproxyIgnored(t *testing.T) {
	random := hostProcess{
		PID: 999, Command: "code",
		Args: "code --some-flag --device virtio-blk,path=/missing/forever",
	}
	if _, isOrphan := classifyHostProcess(random); isOrphan {
		t.Error("non-vfkit process should never be classified as orphan")
	}
}

// TestFindStaleSockets sets up a fake tmpdir with two sockets — one
// referenced by an "active" process arg list, one not. Asserts only
// the unreferenced one shows up in the stale set.
func TestFindStaleSockets(t *testing.T) {
	// Use the real tmpdir + podman subdir because findStaleSockets
	// hard-codes os.TempDir(). To isolate the test, create the
	// expected directory and prefix our socket names.
	dir := filepath.Join(os.TempDir(), "podman")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "host-cleanup-test-live.sock")
	dead := filepath.Join(dir, "host-cleanup-test-dead.sock")
	for _, s := range []string{live, dead} {
		if err := os.WriteFile(s, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(s) })
	}

	// "live" socket is referenced by an active vfkit's arg list.
	active := []hostProcess{{
		PID: 333, Command: "vfkit",
		Args: "vfkit --device virtio-vsock,socketURL=" + live + ",listen",
	}}
	stale, err := findStaleSockets(active)
	if err != nil {
		t.Fatal(err)
	}

	// The test only cares that DEAD shows up and LIVE doesn't.
	gotLive, gotDead := false, false
	for _, s := range stale {
		if s.Path == live {
			gotLive = true
		}
		if s.Path == dead {
			gotDead = true
		}
	}
	if gotLive {
		t.Errorf("live socket %q was marked stale", live)
	}
	if !gotDead {
		t.Errorf("dead socket %q not marked stale (stale set = %v)", dead, stale)
	}
}

// TestAnythingCleaned pins the convenience used by the preflight
// auto-cleanup retry path: empty report → false (no point retrying),
// any orphan → true.
func TestAnythingCleaned(t *testing.T) {
	empty := HostCleanupReport{}
	if empty.AnythingCleaned() {
		t.Error("empty report should not report AnythingCleaned")
	}
	withProc := HostCleanupReport{OrphanedProcesses: []OrphanedProcess{{PID: 1}}}
	if !withProc.AnythingCleaned() {
		t.Error("report with orphans should report AnythingCleaned")
	}
	withSock := HostCleanupReport{StaleSockets: []StaleSocket{{Path: "/x"}}}
	if !withSock.AnythingCleaned() {
		t.Error("report with stale sockets should report AnythingCleaned")
	}
}
