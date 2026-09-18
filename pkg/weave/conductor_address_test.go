package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
)

// A conductor address exists only while somebody is ACCOUNTABLE for the sprint.
//
// The distinction against the steward seat is deliberate and worth pinning:
// there is exactly one steward seat per host and it is a standing address
// whether or not it is claimed, because "nobody is stewarding this host" is
// itself a message worth delivering. Sprints come and go by the dozen, so an
// address per backlog item would bury the ones that mean something — and an
// address for an unleased sprint would accept mail on behalf of no one.
func TestConductorRoles_OnlyLiveLeasesGetAnAddress(t *testing.T) {
	now := time.Now()
	q := &weaveQueue{Stories: []*weaveStory{
		{ID: 1, Lease: &weaveStoryLease{Holder: "codex-gpt5.6-sol", At: now}},
		{ID: 2}, // no lease at all
		{ID: 3, Lease: &weaveStoryLease{Holder: "", At: now}}, // lease with no holder
		{ID: 4, Lease: &weaveStoryLease{ // EXPIRED
			Holder: "claude-opus5", At: now.Add(-2 * SprintLeaseTTL)}},
		// UNPROVED, not live. A heartbeat materially ahead of us cannot be
		// aged: subtracting it is negative, so a bare `> TTL` test calls it
		// fresh on every future call and this address accepts mail forever on
		// behalf of a conductor who may be long gone.
		{ID: 5, Lease: &weaveStoryLease{
			Holder: "codex-gpt5.6-sol", At: now.Add(time.Hour)}},
	}}

	got := rolesFromQueue(q, now)
	if len(got) != 1 {
		t.Fatalf("got %d addresses, want exactly the one live lease: %+v", len(got), got)
	}
	if got[0].Label != "conductor:1" {
		t.Errorf("label = %q, want the name a person types", got[0].Label)
	}
	if got[0].Topic != sprintTopic(1) {
		t.Errorf("topic = %q, want the sprint's stable address", got[0].Topic)
	}
	if got[0].Holder != "codex-gpt5.6-sol" {
		t.Errorf("holder = %q, want the live lease holder", got[0].Holder)
	}
}

// The address is the SPRINT, never the agent conducting it. A lease changes
// hands; sent to the holder's name, mail would follow the agent rather than the
// responsibility and a handover would silently lose it.
func TestConductorRoles_AddressIsTheSprintNotItsHolder(t *testing.T) {
	now := time.Now()
	first := rolesFromQueue(&weaveQueue{Stories: []*weaveStory{
		{ID: 7, Lease: &weaveStoryLease{Holder: "codex-gpt5.6-sol", At: now}},
	}}, now)
	second := rolesFromQueue(&weaveQueue{Stories: []*weaveStory{
		{ID: 7, Lease: &weaveStoryLease{Holder: "claude-opus5", At: now}},
	}}, now)

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected one address each, got %d and %d", len(first), len(second))
	}
	if first[0].Topic != second[0].Topic {
		t.Fatalf("the address changed with the holder (%q -> %q) — mail would not survive a handover",
			first[0].Topic, second[0].Topic)
	}
}

// No queue, or no sprints, is not an error: resolving an address must never be
// the thing that reports a broken queue.
func TestConductorRoles_EmptyIsNotAnError(t *testing.T) {
	if got := rolesFromQueue(nil, time.Now()); got != nil {
		t.Errorf("nil queue produced %+v", got)
	}
	if got := rolesFromQueue(&weaveQueue{}, time.Now()); got != nil {
		t.Errorf("empty queue produced %+v", got)
	}
}

// Role discovery is part of every unified-inbox snapshot. Looking for a role on
// a host with no sprint board must create neither the board nor the
// repository's weave queue tag (or even the top-level weave state directory).
func TestConductorRoles_AbsentBoardIsSideEffectFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Chdir(repo)
	root, err := weaveRepoRoot(repo)
	if err != nil {
		t.Fatal(err)
	}

	tag, _ := weaveQueueNames(root)
	wantAbsent := filepath.Join(weaveStateRoot(home), tag)
	board, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := conductorRoles(); got != nil {
			t.Fatalf("conductorRoles() = %+v, want no roles", got)
		}
		if bus.HostRoles != nil {
			_ = bus.HostRoles()
		}
		_ = bus.RoleLabelFor("conductor.999")
		_ = bus.AddressedToRole("conductor:999")
	}

	if _, err := os.Stat(board); !os.IsNotExist(err) {
		t.Fatalf("read-only role lookup created the sprint board %s (stat err %v)", board, err)
	}
	if _, err := os.Stat(wantAbsent); !os.IsNotExist(err) {
		t.Fatalf("read-only role lookup created queue tag %s (stat err %v)", wantAbsent, err)
	}
	if _, err := os.Stat(weaveStateRoot(home)); !os.IsNotExist(err) {
		t.Fatalf("read-only role lookup created weave root %s (stat err %v)", weaveStateRoot(home), err)
	}
}

// THE REGRESSION. A sprint board is USER-GLOBAL; the per-repo weave queue is
// not the same store. Reading the wrong one resolved zero addresses on every
// host and said nothing, so `whois conductor:<n>` answered "names nothing on
// this host" over a live lease. This test deliberately writes a live lease into
// BOTH stores with different sprint ids: only the board's may be answered.
func TestConductorRoles_ResolvesFromTheUserGlobalSprintBoard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Chdir(repo)
	root, err := weaveRepoRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	queueDir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(queueDir, &weaveQueue{Stories: []*weaveStory{{
		ID: 41, Lease: &weaveStoryLease{Holder: "not-the-board", At: time.Now()},
	}}}); err != nil {
		t.Fatal(err)
	}
	board, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(board, &weaveQueue{Stories: []*weaveStory{{
		ID: 9, Lease: &weaveStoryLease{Holder: "codex", At: time.Now()},
	}}}); err != nil {
		t.Fatal(err)
	}

	got := conductorRoles()
	if len(got) != 1 || got[0].Label != "conductor:9" || got[0].Holder != "codex" {
		t.Fatalf("conductorRoles() = %+v, want the live lease from the sprint board", got)
	}
}

// A sprint spans repos, so its conductor address may not depend on which
// checkout the reader happens to stand in — including standing in no repo at
// all, which is where a plain `bashy inbox` is usually run from.
func TestConductorRoles_ResolveOutsideAnyRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	board, err := sprintStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(board, &weaveQueue{Stories: []*weaveStory{{
		ID: 12, Lease: &weaveStoryLease{Holder: "trestle", At: time.Now()},
	}}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	got := conductorRoles()
	if len(got) != 1 || got[0].Label != "conductor:12" {
		t.Fatalf("conductorRoles() = %+v, want the board role from outside a repo", got)
	}
}
