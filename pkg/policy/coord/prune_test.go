// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

func agentC() principal.Ref { return principal.Ref{Name: "codex-c", Episode: "ep-ccc", Host: "h"} }

// backdate moves a stored claim's tenure past the TTL so it reads lapsed.
func backdate(t *testing.T, path string, c *Claim) {
	t.Helper()
	c.AcquiredAt = time.Now().Add(-TTL - 2*time.Minute)
	c.Heartbeat = time.Now().Add(-TTL - time.Minute)
	if err := writeClaim(path, c); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRemovesOnlyLapsed(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	if _, err := Acquire(dir, []string{"/w/live"}, agentA(), "live work", false); err != nil {
		t.Fatal(err)
	}
	old, err := Acquire(dir, []string{"/w/old"}, agentB(), "dead work", false)
	if err != nil {
		t.Fatal(err)
	}
	backdate(t, claimPath(dir, agentB()), old)

	res, err := AcquireResource(dir, "do-old", agentC(), "dead hold", false)
	if err != nil {
		t.Fatal(err)
	}
	backdate(t, resourceClaimPath(dir, "do-old"), res)

	// Attached with a stale heartbeat on disk: the kernel lock is the
	// liveness signal, so prune must still keep it.
	att, lock, err := AcquireAttached(dir, "do-live", agentA(), "child work", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	att.Heartbeat = time.Now().Add(-TTL - time.Minute)
	if err := writeClaim(resourceClaimPath(dir, "do-live"), att); err != nil {
		t.Fatal(err)
	}

	// Unknown (no heartbeat recorded) is evidence-free: look, don't seize.
	unk, err := AcquireResource(dir, "do-unknown", agentB(), "no signal", false)
	if err != nil {
		t.Fatal(err)
	}
	unk.Heartbeat = time.Time{}
	if err := writeClaim(resourceClaimPath(dir, "do-unknown"), unk); err != nil {
		t.Fatal(err)
	}

	dry, err := Prune(dir, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry) != 2 {
		t.Fatalf("dry-run pruned %d, want 2 (lapsed project + lapsed resource)", len(dry))
	}
	for _, p := range []string{claimPath(dir, agentB()), resourceClaimPath(dir, "do-old")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry-run removed %s: %v", p, err)
		}
	}

	pruned, err := Prune(dir, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 2 {
		t.Fatalf("pruned %d, want 2", len(pruned))
	}
	for _, p := range []string{claimPath(dir, agentB()), resourceClaimPath(dir, "do-old")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still present after prune", p)
		}
	}
	for _, p := range []string{
		claimPath(dir, agentA()),
		resourceClaimPath(dir, "do-live"),
		resourceClaimPath(dir, "do-unknown"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing after prune: %v", p, err)
		}
	}
	rest, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 3 {
		t.Fatalf("claims remaining = %d, want 3 (live, attached, unknown)", len(rest))
	}
}

func TestClaimPruneCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)

	old, err := Acquire(dir, []string{"/w/old"}, agentB(), "dead work", false)
	if err != nil {
		t.Fatal(err)
	}
	backdate(t, claimPath(dir, agentB()), old)

	run := func(args ...string) string {
		t.Helper()
		cmd := NewClaimCmd(func() []string { return []string{"/w/old"} })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("claim %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}

	if out := run("prune", "--dry-run"); !strings.Contains(out, "would prune") ||
		!strings.Contains(out, "codex-b") || !strings.Contains(out, "age ") {
		t.Fatalf("dry-run output = %q", out)
	}
	if _, err := os.Stat(claimPath(dir, agentB())); err != nil {
		t.Fatalf("dry-run removed the claim: %v", err)
	}
	if out := run("prune"); !strings.Contains(out, "pruned") ||
		!strings.Contains(out, "codex-b") || !strings.Contains(out, "age ") {
		t.Fatalf("prune output = %q", out)
	}
	if _, err := os.Stat(claimPath(dir, agentB())); !os.IsNotExist(err) {
		t.Fatal("lapsed claim still present after prune")
	}
	out := run("prune", "--json")
	var got []Claim
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 0 {
		t.Fatalf("json output = %q, claims = %d, err = %v", out, len(got), err)
	}
	if out := run("prune"); !strings.Contains(out, "nothing lapsed") {
		t.Fatalf("empty prune output = %q", out)
	}
}
