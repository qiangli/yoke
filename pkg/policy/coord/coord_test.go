// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/role"
)

func agentA() principal.Ref { return principal.Ref{Name: "claude-a", Episode: "ep-aaa", Host: "h"} }
func agentB() principal.Ref { return principal.Ref{Name: "codex-b", Episode: "ep-bbb", Host: "h"} }

func TestNamedResourceLeaseLifecycle(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireResource(dir, "do1", agentA(), "leaf replay", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Resource != "do1" || first.Mode != ModeLease {
		t.Fatalf("claim = %#v", first)
	}
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentB(), "project work", false); err != nil {
		t.Fatalf("named and project claims conflicted: %v", err)
	}
	if _, err := AcquireResource(dir, "do1", agentB(), "other work", false); err == nil {
		t.Fatal("second holder acquired do1")
	} else {
		var conflict *Conflict
		if !errors.As(err, &conflict) || conflict.Claim.Holder.Name != agentA().Name {
			t.Fatalf("conflict = %T %v", err, err)
		}
		for _, want := range []string{"claude-a", "leaf replay", "since", "do1", "on h"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("conflict %q lacks %q", err, want)
			}
		}
	}
	refreshed, err := AcquireResource(dir, "do1", agentA(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.AcquiredAt.Equal(first.AcquiredAt) || refreshed.Intent != first.Intent {
		t.Fatalf("refresh changed tenure: first=%#v refreshed=%#v", first, refreshed)
	}
	if err := ReleaseResource(dir, "do1", agentB()); err == nil {
		t.Fatal("another holder released do1")
	}
	if err := ReleaseResource(dir, "do1", agentA()); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireResource(dir, "do1", agentB(), "next", false); err != nil {
		t.Fatalf("released resource was not reclaimable: %v", err)
	}
}

func TestNamedResourceLivenessIsHonest(t *testing.T) {
	dir := t.TempDir()
	c, err := AcquireResource(dir, "do1", agentA(), "test", false)
	if err != nil {
		t.Fatal(err)
	}
	c.AcquiredAt = time.Now().Add(-TTL - 2*time.Minute)
	c.Heartbeat = time.Now().Add(-TTL - time.Minute)
	if err := writeClaim(resourceClaimPath(dir, "do1"), c); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireResource(dir, "do1", agentB(), "reclaim", false); err != nil {
		t.Fatalf("lapsed lease was not reclaimable: %v", err)
	}

	c, err = AcquireResource(dir, "do2", agentA(), "unknown", false)
	if err != nil {
		t.Fatal(err)
	}
	c.Heartbeat = time.Time{}
	if err := writeClaim(resourceClaimPath(dir, "do2"), c); err != nil {
		t.Fatal(err)
	}
	if got := c.Liveness(time.Now()); got != role.LivenessUnknown {
		t.Fatalf("liveness = %q, want unknown", got)
	}
	if _, err := AcquireResource(dir, "do2", agentB(), "must refuse", false); err == nil {
		t.Fatal("unknown lease was treated as free")
	}
}

func TestNamedResourceWaitRetriesWholeAcquisition(t *testing.T) {
	dir := t.TempDir()
	if _, err := AcquireResource(dir, "do1", agentA(), "first", false); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = ReleaseResource(dir, "do1", agentA())
	}()
	start := time.Now()
	if _, err := AcquireResourceWithin(dir, "do1", agentB(), "second", false, time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("bounded wait did not wait for the lease")
	}
}

// Alone, a claim is silent and free. Friction that fires when you are the only agent
// on the machine is friction nobody accepts — and a rule nobody accepts is a rule
// nobody follows.
func TestUncontestedClaimIsSilent(t *testing.T) {
	dir := t.TempDir()
	c, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "refactor", false)
	if err != nil {
		t.Fatalf("an uncontested claim was refused: %v", err)
	}
	if c.Project != "bashy" {
		t.Fatalf("project = %q", c.Project)
	}
	// Re-acquiring is a heartbeat, not a conflict with oneself.
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "", false); err != nil {
		t.Fatalf("an agent collided with itself: %v", err)
	}
}

// THE COLLISION THAT ACTUALLY HAPPENED, and the reason this package exists.
//
// A holds the project. B is sitting in a DIFFERENT repo — sh — which is part of the
// same project, and attempts a write. It must be REFUSED, because that is exactly
// the shape of the real failure: the trap regression lived in sh, the gate that
// would have caught it lived in bashy, and the pin that carried it lived in the
// umbrella. A claim keyed on one .git root would have prevented NOTHING.
func TestConflictAcrossRepos(t *testing.T) {
	dir := t.TempDir()
	// A claims the whole project: bashy + its sibling repos.
	if _, err := Acquire(dir, []string{"/w/bashy", "/w/sh", "/w/coreutils"}, agentA(), "running the gate", false); err != nil {
		t.Fatal(err)
	}

	// B, standing in sh, tries to work.
	_, err := Acquire(dir, []string{"/w/sh"}, agentB(), "quick fix", false)
	if err == nil {
		t.Fatal("a second agent claimed a DIFFERENT repo of the same project — this is the exact collision that shipped an untested regression")
	}
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("wrong error type: %v", err)
	}
	// The refusal IS the documentation: it must name who, and what to do instead.
	msg := conflict.Error()
	for _, want := range []string{"claude-a", "running the gate", "claim list", "claim request", "weave add", "BASHY_CLAIM_FORCE"} {
		if !contains(msg, want) {
			t.Errorf("the refusal does not mention %q — an agent that read no docs learns the rule HERE:\n%s", want, msg)
		}
	}
}

// A SUBDIRECTORY of a claimed repo is inside the claim. An agent editing
// <repo>/internal is working in <repo>.
func TestConflictFromSubdirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, []string{"/w/bashy/internal/agentos"}, agentB(), "", false); err == nil {
		t.Fatal("an agent in a subdirectory of a claimed repo was not blocked")
	}
}

// An UNRELATED project must not be blocked. Over-blocking is how a safety mechanism
// gets disabled by the people it protects.
func TestUnrelatedProjectIsFree(t *testing.T) {
	dir := t.TempDir()
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, []string{"/w/somethingelse"}, agentB(), "", false); err != nil {
		t.Fatalf("an unrelated project was blocked: %v", err)
	}
}

// A STALE claim is reclaimable WITHOUT --force. A crashed agent must not block a
// project until a human intervenes: the lease is a heartbeat precisely because an
// LLM session dies without cleaning up.
func TestStaleClaimIsReclaimable(t *testing.T) {
	dir := t.TempDir()
	c, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	// It was live a moment ago...
	if !c.Live(time.Now()) {
		t.Fatal("a fresh claim is not live")
	}
	// ...and is stale once the heartbeat lapses.
	if c.Live(time.Now().Add(TTL + time.Minute)) {
		t.Fatal("a claim outlived its TTL without a heartbeat")
	}
	if c.Conflicts([]string{"/w/bashy"}, agentB(), time.Now().Add(TTL+time.Minute)) {
		t.Fatal("a STALE claim still blocked another agent — a crashed session would block the project forever")
	}
}

// --force overrides, because a human must always be able to say "I know, do it
// anyway". The override is recorded; it is not silent.
func TestForceOverrides(t *testing.T) {
	dir := t.TempDir()
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentB(), "", true); err != nil {
		t.Fatalf("--force did not override a live claim: %v", err)
	}
}

// Release frees the project immediately, rather than making the next agent wait out
// the TTL.
func TestRelease(t *testing.T) {
	dir := t.TempDir()
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "", false); err != nil {
		t.Fatal(err)
	}
	if err := Release(dir, agentA()); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentB(), "", false); err != nil {
		t.Fatalf("the project was still blocked after release: %v", err)
	}
	// Releasing a claim that is not held is a no-op, not an error: an agent being
	// killed must be able to call it without knowing whether it ever claimed.
	if err := Release(dir, agentA()); err != nil {
		t.Fatalf("releasing an unheld claim errored: %v", err)
	}
}

// The SAME logical agent may run many processes — a shell, a subagent, a hook. None
// of them may be told it is colliding with itself.
func TestSameAgentDifferentProcessDoesNotCollide(t *testing.T) {
	dir := t.TempDir()
	a := agentA()
	if _, err := Acquire(dir, []string{"/w/bashy"}, a, "", false); err != nil {
		t.Fatal(err)
	}
	// A child process: same episode, different PID.
	child := principal.Ref{Name: a.Name, Episode: a.Episode, Host: a.Host}
	if _, err := Acquire(dir, []string{"/w/bashy"}, child, "", false); err != nil {
		t.Fatalf("an agent collided with its own child process: %v", err)
	}
}

// List answers the question nothing in this codebase could answer: who is working
// right now, and where?
func TestListSeesEveryone(t *testing.T) {
	dir := t.TempDir()
	if _, err := Acquire(dir, []string{"/w/bashy"}, agentA(), "gate", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, []string{"/w/other"}, agentB(), "docs", false); err != nil {
		t.Fatal(err)
	}
	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List saw %d agents, want 2 — the host-wide registry is the whole point", len(got))
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
