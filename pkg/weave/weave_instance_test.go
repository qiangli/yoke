package weave

// A weave run IS a conversation, so the run record carries its UUID and the
// FROZEN family configuration. These tests cover the half that made finding 6
// real: the run produces an identity, records it, and stamps it onto the child
// — which is what sprintLeaseIdentity has been reading from an environment
// nobody wrote.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

func instanceRun(t *testing.T) (*weaveItem, *weaveAgentLaunch) {
	t.Helper()
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv(fleet.InstanceDirEnv, t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_INSTANCE", "")
	it := &weaveItem{ID: 37, Title: "families and UUID instances", Owner: "claude-a", Workspace: "/tmp/ws-37"}
	l := &weaveAgentLaunch{Nick: "claude:opus5.5", Tool: "claude", ToolName: "claude", ModelName: "opus5.5"}
	return it, l
}

// A run with no recorded instance opens a FRESH context and records the UUID
// plus the frozen family id.
func TestWeaveRunRecordsAFreshInstance(t *testing.T) {
	it, l := instanceRun(t)

	ctx, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37")
	if err != nil {
		t.Fatal(err)
	}
	if it.Instance == "" || it.Instance != ctx.Instance.UUID {
		t.Fatalf("the run did not record its UUID: %q vs %q", it.Instance, ctx.Instance.UUID)
	}
	if _, err := fleet.ParseInstanceUUID(it.Instance); err != nil {
		t.Errorf("the recorded instance is not a UUID: %v", err)
	}
	if it.InstanceFamily == "" || it.InstanceFamily != ctx.Instance.FamilyID {
		t.Errorf("the run did not record the FROZEN family: %q", it.InstanceFamily)
	}
	if it.SessionClaim == "" {
		t.Error("the run recorded no session digest; its worker's child commands would each look like a new session")
	}
	// The digest is one-way: a raw session identifier must never be stored.
	if !strings.HasPrefix(it.SessionClaim, "sha256:") {
		t.Errorf("session digest is not hashed: %q", it.SessionClaim)
	}
}

// ACCEPTANCE: resume preserves the UUID. `weave start --resume` continues the
// same conversation, not a new one that happens to work the same issue.
func TestWeaveResumePreservesTheRunInstance(t *testing.T) {
	it, l := instanceRun(t)

	first, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37")
	if err != nil {
		t.Fatal(err)
	}
	// A resume: the item comes back off disk with its recorded identity.
	again, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37")
	if err != nil {
		t.Fatalf("a resume of the same run was refused: %v", err)
	}
	if again.Instance.UUID != first.Instance.UUID {
		t.Fatalf("the resume minted a new conversation: %s -> %s", first.Instance.UUID, again.Instance.UUID)
	}
	if !again.Resumed {
		t.Error("the resume did not report itself as one")
	}
}

// Two concurrent runs of ONE agent are two conversations. Reusing an idle
// context here would give the second run the first one's mailbox.
func TestTwoRunsOfOneAgentAreTwoConversations(t *testing.T) {
	a, l := instanceRun(t)
	b := &weaveItem{ID: 38, Title: "another issue", Owner: "claude-b", Workspace: "/tmp/ws-38"}

	ca, err := weaveOpenRunInstance(a, l, "agent/weave-issue-37")
	if err != nil {
		t.Fatal(err)
	}
	cb, err := weaveOpenRunInstance(b, l, "agent/weave-issue-38")
	if err != nil {
		t.Fatal(err)
	}
	if ca.Instance.UUID == cb.Instance.UUID {
		t.Fatal("two runs share one conversation")
	}
	if ca.Instance.MailAddress() == cb.Instance.MailAddress() {
		t.Fatal("two runs share one mailbox")
	}
	if a.SessionClaim == b.SessionClaim {
		t.Error("two runs minted the same session digest; neither could own its own context")
	}
}

// The session digest is a pure function of the run, so a resume that lost the
// stored value still computes the same one and keeps its ownership.
func TestRunSessionClaimIsStableAcrossRecomputation(t *testing.T) {
	first := weaveRunSessionClaim(37, "agent/weave-issue-37")
	if first != weaveRunSessionClaim(37, "agent/weave-issue-37") {
		t.Fatal("the run digest is not stable")
	}
	if first == weaveRunSessionClaim(38, "agent/weave-issue-37") {
		t.Error("two runs collide on one digest")
	}
	if first == weaveRunSessionClaim(37, "agent/other-branch") {
		t.Error("the branch does not participate in the digest")
	}
}

// ACCEPTANCE: external identity works without a repeated --as. The child env
// is stamped from the RECORDED run identity, and it is what the existing
// sprint-lease reader resolves.
func TestWeaveChildEnvStampsTheRecordedIdentity(t *testing.T) {
	it, l := instanceRun(t)
	if _, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37"); err != nil {
		t.Fatal(err)
	}

	env := weaveInstanceEnv([]string{"PATH=/bin"}, it)
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got[agentlaunch.InstanceUUIDEnv] != it.Instance {
		t.Errorf("the child was not stamped with the run's UUID: %q", got[agentlaunch.InstanceUUIDEnv])
	}
	if got[agentlaunch.InstanceFamilyEnv] != it.InstanceFamily {
		t.Errorf("the child was not stamped with the frozen family: %q", got[agentlaunch.InstanceFamilyEnv])
	}
	if got[agentlaunch.SessionClaimEnv] != it.SessionClaim {
		t.Errorf("the child was not stamped with the session digest: %q", got[agentlaunch.SessionClaimEnv])
	}

	// THE POINT OF THE WHOLE WIRING: the sprint lease reader, which has been
	// reading these names all along, now resolves a real instance.
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "BASHY_") {
			t.Setenv(k, v)
		}
	}
	instance, session := sprintLeaseIdentity()
	if instance != it.Instance {
		t.Errorf("sprintLeaseIdentity() instance = %q, want the run's %q", instance, it.Instance)
	}
	if session != it.SessionClaim {
		t.Errorf("sprintLeaseIdentity() session = %q, want %q", session, it.SessionClaim)
	}

	// And a lease taken under it records the holder rather than falling into
	// the legacy "no instance" path forever.
	lease := &weaveStoryLease{Holder: "claude-a"}
	stampSprintLeaseInstance(lease, instance, session)
	if lease.Instance != it.Instance {
		t.Errorf("the lease recorded instance %q", lease.Instance)
	}
	// A competing session on that same seat is refused.
	if err := sprintLeaseAccepts(321, lease, true, instance, "sha256:someone-else"); err == nil {
		t.Error("a competing session took a held conductor seat")
	}
	// The owning session is accepted.
	if err := sprintLeaseAccepts(321, lease, true, instance, session); err != nil {
		t.Errorf("the owning session was refused its own seat: %v", err)
	}
}

// A run with no instance recorded stamps nothing. Legacy runs must keep
// working, and a placeholder would later compare equal to another placeholder
// and make two runs look like one session.
func TestWeaveInstanceEnvIsSilentForALegacyRun(t *testing.T) {
	base := []string{"PATH=/bin"}
	if got := weaveInstanceEnv(base, &weaveItem{ID: 5}); len(got) != 1 {
		t.Errorf("a legacy run was stamped: %v", got)
	}
	if got := weaveInstanceEnv(base, nil); len(got) != 1 {
		t.Errorf("a nil item was stamped: %v", got)
	}
}

// Retiring a run's context archives its real mail and releases the label; a
// run with no instance is a no-op rather than an error.
func TestWeaveRetireRunInstanceArchivesAndReleases(t *testing.T) {
	it, l := instanceRun(t)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	if _, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37"); err != nil {
		t.Fatal(err)
	}

	archive, err := weaveRetireRunInstance(it)
	if err != nil {
		t.Fatalf("retiring the run's context failed: %v", err)
	}
	if strings.TrimSpace(archive) == "" {
		t.Fatal("retirement reported no archive")
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("the archive was not written: %v", err)
	}
	// Retired, so it no longer counts against the cap and its label is free.
	store := fleet.NewInstanceStore(os.Getenv(fleet.InstanceDirEnv))
	rec, err := store.Get(it.Instance)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Active() {
		t.Error("the context is still active after retirement")
	}
	// The record itself stays: archived, never deleted.
	if rec.UUID != it.Instance {
		t.Error("retirement lost the record")
	}

	if got, err := weaveRetireRunInstance(&weaveItem{ID: 9}); err != nil || got != "" {
		t.Errorf("retiring a legacy run with no context = %q, %v; want a silent no-op", got, err)
	}
}

// A launch that resolves to NO family must not fail the run. A raw
// `weave start claude` and a named agent this host's catalog does not have are
// both ordinary; the first wiring of this made them a hard `weave start`
// failure ("launch resolves to no family configuration: 007"), which turned an
// identity feature into an outage for every pre-instance run.
func TestRunWithNoResolvableFamilyStartsAnyway(t *testing.T) {
	it, _ := instanceRun(t)
	for _, l := range []*weaveAgentLaunch{
		{Nick: "007", Tool: "claude", ToolName: "claude", ModelName: ""},    // named, not in the catalog
		{Nick: "claude", Tool: "claude", ToolName: "claude", ModelName: ""}, // raw, no model
	} {
		if _, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37"); err != nil {
			t.Fatalf("a run with no resolvable family was refused (%s): %v", l.Nick, err)
		}
		if it.Instance != "" {
			t.Errorf("%s recorded an instance it could not have: %q", l.Nick, it.Instance)
		}
	}
}

// Cap exhaustion does not block a weave run either — instances accumulate
// until their records are deleted, so an identity ceiling must not become a
// fleet outage. See weaveOpenRunInstance.
func TestCapExhaustionDoesNotBlockARun(t *testing.T) {
	it, l := instanceRun(t)
	t.Setenv(fleet.InstanceCapEnv, "1")

	if _, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37"); err != nil {
		t.Fatal(err)
	}
	next := &weaveItem{ID: 38, Title: "second run", Owner: "claude-b", Workspace: "/tmp/ws-38"}
	if _, err := weaveOpenRunInstance(next, l, "agent/weave-issue-38"); err != nil {
		t.Fatalf("the cap blocked a weave run: %v", err)
	}
	if next.Instance != "" {
		t.Errorf("a capped run recorded an instance: %q", next.Instance)
	}
}

// The two refusals that DO block: a competing owning session, and continuing a
// recorded conversation under a changed configuration.
func TestOwnershipAndReconfigurationBlockARun(t *testing.T) {
	if !weaveInstanceBlocksRun(&room.ErrLive{ID: "instance:x", PID: 1}) {
		t.Error("a competing owning session did not block the run")
	}
	if !weaveInstanceBlocksRun(fmt.Errorf("wrapped: %w", fleet.ErrBindingImmutable)) {
		t.Error("a reconfiguration did not block the run")
	}
	if weaveInstanceBlocksRun(fmt.Errorf("%w: 007", agentlaunch.ErrFamilyUnknown)) {
		t.Error("an unresolvable family blocked the run")
	}
	if weaveInstanceBlocksRun(&fleet.CapError{Cap: 1}) {
		t.Error("cap exhaustion blocked the run")
	}
}

// A recorded context whose record is GONE (store wiped, or retired by a
// `weave reset` in another clone — the store is host-global) must not leave its
// stale UUID on the run. Stamping it would hand the worker an identity with no
// record behind it, failing every principal.SelfInstance lookup. The run
// continues as the new conversation it now is.
func TestUnresumableRecordedContextBecomesAFreshOne(t *testing.T) {
	it, l := instanceRun(t)
	if _, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37"); err != nil {
		t.Fatal(err)
	}
	stale := it.Instance

	// The record disappears underneath the run.
	t.Setenv(fleet.InstanceDirEnv, t.TempDir())

	ctx, err := weaveOpenRunInstance(it, l, "agent/weave-issue-37")
	if err != nil {
		t.Fatalf("a run with an unresumable recorded context was refused: %v", err)
	}
	if it.Instance == stale {
		t.Fatalf("the run kept a UUID with no record: %s", stale)
	}
	if it.Instance == "" || it.Instance != ctx.Instance.UUID {
		t.Fatalf("the run did not record its new conversation: %q", it.Instance)
	}
	// The stamped identity must resolve to a real record.
	store := fleet.NewInstanceStore(os.Getenv(fleet.InstanceDirEnv))
	if _, err := store.Resume(it.Instance); err != nil {
		t.Errorf("the stamped identity does not resolve: %v", err)
	}
}

// When no identity can be taken at all, the run records NOTHING rather than a
// half-written one — honestly unidentified beats wrongly identified.
func TestUnidentifiableRunClearsStaleFields(t *testing.T) {
	it, _ := instanceRun(t)
	it.Instance = "11111111-2222-3333-4444-555555555555"
	it.InstanceFamily = "single:claude:opus5.5@dead"
	it.InstanceLabel = "Ghost"

	// A launch with no resolvable family: nothing can be opened, fresh or not.
	bare := &weaveAgentLaunch{Nick: "claude", Tool: "claude", ToolName: "claude"}
	if _, err := weaveOpenRunInstance(it, bare, "agent/weave-issue-37"); err != nil {
		t.Fatal(err)
	}
	if it.Instance != "" || it.InstanceFamily != "" || it.InstanceLabel != "" {
		t.Errorf("stale identity survived: %q / %q / %q", it.Instance, it.InstanceFamily, it.InstanceLabel)
	}
	if got := weaveInstanceEnv([]string{"PATH=/bin"}, it); len(got) != 1 {
		t.Errorf("an unidentified run was stamped: %v", got)
	}
}
