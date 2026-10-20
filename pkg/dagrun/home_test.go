package dagrun

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWorkspaceConcurrentWritersIsolated is the S481.4 slice: two owners of
// one workspace get distinct working copies, concurrent claims never share a
// path, and a claim on an existing owner's path fails with
// ErrOwnershipConflict. Everything lives under a caller-given t.TempDir home.
func TestWorkspaceConcurrentWritersIsolated(t *testing.T) {
	home := t.TempDir()
	wc := NewWorkingCopies(home)
	const ws = "ws-01jb6zexample0000000000"

	pa, err := wc.Claim(ws, "agent-a", "yoke")
	if err != nil {
		t.Fatalf("claim a: %v", err)
	}
	pb, err := wc.Claim(ws, "agent-b", "yoke")
	if err != nil {
		t.Fatalf("claim b: %v", err)
	}
	if pa == pb {
		t.Fatalf("two owners of one workspace share path %q", pa)
	}
	if base := filepath.Base(pa); base != "yoke-agent-a" {
		t.Errorf("claim a = %q, want base yoke-agent-a", pa)
	}
	if base := filepath.Base(pb); base != "yoke-agent-b" {
		t.Errorf("claim b = %q, want base yoke-agent-b", pb)
	}

	// The same owner reclaiming its own path is idempotent.
	if again, err := wc.Claim(ws, "agent-a", "yoke"); err != nil || again != pa {
		t.Fatalf("reclaim = %q, %v; want %q, nil", again, err, pa)
	}

	// Concurrent claims by distinct owners of one workspace: every claim
	// succeeds and no two owners share a path (run under -race).
	const n = 16
	paths := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = wc.Claim(ws, fmt.Sprintf("agent-%02d", i), "yoke")
		}(i)
	}
	wg.Wait()
	seen := make(map[string]bool, n)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("concurrent claim %d: %v", i, errs[i])
		}
		if seen[paths[i]] {
			t.Fatalf("concurrent claims share path %q", paths[i])
		}
		seen[paths[i]] = true
	}

	// Two claims folding to the same name (repo "y-z" + owner "x" and
	// repo "y" + owner "z-x"): exactly one wins, the other gets
	// ErrOwnershipConflict.
	type claimRes struct {
		path string
		err  error
	}
	pair := [2][2]string{{"x", "y-z"}, {"z-x", "y"}} // {owner, repo}
	res := make([]claimRes, 2)
	var wg2 sync.WaitGroup
	for i := range pair {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			res[i].path, res[i].err = wc.Claim(ws, pair[i][0], pair[i][1])
		}(i)
	}
	wg2.Wait()
	wins := 0
	for i, r := range res {
		switch {
		case r.err == nil:
			wins++
			if filepath.Base(r.path) != "y-z-x" {
				t.Errorf("colliding claim %d = %q, want base y-z-x", i, r.path)
			}
		case errors.Is(r.err, ErrOwnershipConflict):
		default:
			t.Errorf("colliding claim %d err = %v, want ErrOwnershipConflict", i, r.err)
		}
	}
	if wins != 1 {
		t.Fatalf("colliding claims had %d winners, want exactly 1", wins)
	}

	// Ownership persists in the registry for a fresh instance over the
	// same home: agent-a's path is closed to other owners.
	fresh := NewWorkingCopies(home)
	if _, err := fresh.Claim(ws, "a", "yoke-agent"); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("claim on agent-a's path by another owner err = %v, want ErrOwnershipConflict", err)
	}

	// Only the owner may release.
	if err := fresh.Release("agent-a", pb); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("release of agent-b's path by agent-a err = %v, want ErrOwnershipConflict", err)
	}
	if err := fresh.Release("agent-b", filepath.Join(home, "never-claimed")); err == nil {
		t.Fatal("release of an unregistered path succeeded")
	}

	// Release frees the path for the next owner; the tree itself is
	// weave's to manage, not ours.
	if err := fresh.Release("agent-a", pa); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got, err := fresh.Claim(ws, "a", "yoke-agent"); err != nil || got != pa {
		t.Fatalf("claim after release = %q, %v; want %q, nil", got, err, pa)
	}

	if _, err := os.Stat(filepath.Join(home, workingCopiesFile)); err != nil {
		t.Fatalf("registry file: %v", err)
	}
	if leftover, _ := filepath.Glob(filepath.Join(home, workingCopiesFile+".tmp*")); len(leftover) > 0 {
		t.Fatalf("registry temp files left behind: %v", leftover)
	}
}

// TestHomeAffinityRefusesOtherExecutor: the first placement binds a home to
// its executor; the same executor may return, any other gets
// ErrHomeElsewhere naming the bound host, and the record never moves.
func TestHomeAffinityRefusesOtherExecutor(t *testing.T) {
	const ws = "ws-01jb6zexample0000000000"
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rec := &HomeRecord{
		WorkspaceID: ws,
		Key:         HomeKey("dks-01", "default", ws),
		CreatedAt:   now,
	}

	if err := Affinity(rec, "exec-a"); err != nil {
		t.Fatalf("first placement: %v", err)
	}
	if rec.ExecutorHost != "exec-a" {
		t.Fatalf("first placement bound %q, want exec-a", rec.ExecutorHost)
	}
	if err := Affinity(rec, "exec-a"); err != nil {
		t.Fatalf("same executor refused: %v", err)
	}

	err := Affinity(rec, "exec-b")
	if !errors.Is(err, ErrHomeElsewhere) {
		t.Fatalf("other executor err = %v, want ErrHomeElsewhere", err)
	}
	if !strings.Contains(err.Error(), "exec-a") {
		t.Fatalf("error does not name the bound executor: %v", err)
	}
	if rec.ExecutorHost != "exec-a" {
		t.Fatalf("refused placement moved the record to %q", rec.ExecutorHost)
	}

	// An empty candidate never binds an unbound home.
	unbound := &HomeRecord{WorkspaceID: ws, Key: rec.Key, CreatedAt: now}
	if err := Affinity(unbound, ""); err == nil {
		t.Fatal("empty candidate accepted")
	}
	if unbound.ExecutorHost != "" {
		t.Fatalf("empty candidate bound %q", unbound.ExecutorHost)
	}
}

// TestHomeKeyMatchesVknodeScheme checks HomeKey against the golden value of
// the outpost vknode hostPathVolumeName formula: the NUL-joined scope
// (clusterID, namespace, path), sha1, first 8 bytes as hex, behind a
// readable prefix. The empty-clusterID form omits its segment, keeping the
// legacy unscoped names byte-identical.
func TestHomeKeyMatchesVknodeScheme(t *testing.T) {
	const (
		clusterID = "dks-01"
		ns        = "default"
		ws        = "ws-01jb6zexample0000000000"
	)

	key := clusterID + "\x00" + ns + "\x00" + ws
	sum := sha1.Sum([]byte(key))
	want := "bashy-home-" + hex.EncodeToString(sum[:8])
	if got := HomeKey(clusterID, ns, ws); got != want {
		t.Fatalf("HomeKey = %q, want golden %q", got, want)
	}

	legacy := sha1.Sum([]byte(ns + "\x00" + ws))
	want = "bashy-home-" + hex.EncodeToString(legacy[:8])
	if got := HomeKey("", ns, ws); got != want {
		t.Fatalf("HomeKey(no cluster) = %q, want golden %q", got, want)
	}

	got := HomeKey(clusterID, ns, ws)
	if got != HomeKey(clusterID, ns, ws) {
		t.Fatal("HomeKey is not deterministic")
	}
	if !labelValueRE.MatchString(got) {
		t.Fatalf("HomeKey %q is not label/path safe", got)
	}
	for _, other := range []string{
		HomeKey("dks-02", ns, ws),
		HomeKey(clusterID, "other-ns", ws),
		HomeKey(clusterID, ns, "ws-01jb6zother0000000000000"),
	} {
		if other == got {
			t.Fatalf("HomeKey collides across scopes: %q", got)
		}
	}
}

// TestHomePathMatchesLinuxRenderHostPath pins HomePath to the HostPath
// render.go mounts as the workspace home, so the two can never drift.
func TestHomePathMatchesLinuxRenderHostPath(t *testing.T) {
	in := renderCases()["one-shot"]
	m := mustGenerate(t, in)
	objs, err := Render(m, linuxClass())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var workload Object
	for _, o := range objs {
		if o["kind"] == "Job" {
			workload = o
		}
	}
	if workload == nil {
		t.Fatal("no Job rendered")
	}
	vols := dig(t, workload, "spec", "template", "spec", "volumes").([]any)
	hp := dig(t, vols[0], "hostPath", "path").(string)
	if want := HomePath(in.WorkspaceID); hp != want {
		t.Fatalf("rendered HostPath = %q, want HomePath %q", hp, want)
	}
	if !strings.HasPrefix(hp, HomeHostRoot+"/") {
		t.Fatalf("HostPath %q is not under %s", hp, HomeHostRoot)
	}
}

// TestWorkingCopiesStorageUnavailable: a missing or unwritable home is an
// explicit error and nothing is written anywhere else.
func TestWorkingCopiesStorageUnavailable(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "no-such-home")
	wc := NewWorkingCopies(missing)
	if _, err := wc.Claim("ws-01jb6zexample0000000000", "agent-a", "yoke"); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("missing home err = %v, want ErrStorageUnavailable", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("claim created %q: nothing may be written elsewhere", missing)
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 0 {
		t.Fatalf("claim against missing home wrote %d entries into the parent", len(entries))
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not make a directory unwritable")
	}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if _, err := NewWorkingCopies(ro).Claim("ws-01jb6zexample0000000000", "agent-a", "yoke"); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("unwritable home err = %v, want ErrStorageUnavailable", err)
	}
}
