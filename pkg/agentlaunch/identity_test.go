package agentlaunch

// The acceptance criteria of Story #1268 that belong to the LAUNCH side:
// fresh vs resume, the one-live-owner refusal across repositories, reuse
// before minting, cap exhaustion, the immutable-binding refusal, and composite
// selection preserving identity.
//
// Every test here drives OpenContext, not the primitives underneath it. The
// primitives already had their own tests and still nothing produced an
// identity — a passing fleet suite was exactly compatible with a launcher that
// minted nothing.

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/room"
)

// identityHost isolates the HOST-GLOBAL stores a context lifecycle touches:
// the room (ownership claims) and the instance store (records). Both are one
// per host on purpose, so a test that forgot either would be writing into the
// developer's real fleet.
func identityHost(t *testing.T) *fleet.InstanceStore {
	t.Helper()
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv(fleet.InstanceDirEnv, dir)
	// The test process itself runs under an injected agent identity. Clear the
	// instance variables so a leaked BASHY_INSTANCE cannot make an "open a
	// fresh context" case silently resume the developer's own conversation.
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv(principal.InstanceEnv, "")
	return fleet.NewInstanceStore(dir)
}

// rawLaunch is an unnamed tool:model launch — its own single family.
func rawLaunch(binding string) Launch {
	tool, model, _ := strings.Cut(binding, ":")
	return Launch{Nick: binding, Tool: tool, ToolName: tool, ModelName: model}
}

func namedLaunch(nick, tool, model string) Launch {
	return Launch{Nick: nick, Tool: tool, ToolName: tool, ModelName: model}
}

// catalogOf builds a throwaway catalog with the given agents.
func catalogOf(t *testing.T, agents ...fleet.Agent) CatalogFunc {
	t.Helper()
	c := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	for _, a := range agents {
		if err := c.SaveAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	return func() *fleet.Catalog { return c }
}

func req(session string, store *fleet.InstanceStore) ContextRequest {
	return ContextRequest{Store: store, Session: session, OwnerPID: os.Getpid(), Cwd: "/tmp/repo-a"}
}

// deadPID is a pid that has certainly exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child to reap: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return pid
}

// ACCEPTANCE: two fresh parallel contexts in one family get distinct UUIDs and
// distinct mailboxes.
func TestTwoFreshContextsInOneFamilyAreDistinct(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")

	a, err := OpenContext(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		r.MailFrom = 10
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenContext(l, func() ContextRequest {
		r := req("session-b", store)
		r.Fresh = true
		r.MailFrom = 20
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}

	if a.Instance.UUID == b.Instance.UUID {
		t.Fatalf("two fresh contexts share a UUID: %s", a.Instance.UUID)
	}
	if a.Instance.MailAddress() == b.Instance.MailAddress() {
		t.Fatalf("two fresh contexts share a mailbox: %s", a.Instance.MailAddress())
	}
	if a.Instance.FamilyID != b.Instance.FamilyID {
		t.Errorf("same family produced two configuration ids: %s vs %s", a.Instance.FamilyID, b.Instance.FamilyID)
	}
	// Distinct LABELS too, or a human cannot address the two of them.
	if a.Instance.Label == b.Instance.Label {
		t.Errorf("both contexts hold the label %q", a.Instance.Label)
	}
	if a.Resumed || b.Resumed {
		t.Error("a fresh open reported itself as a resume")
	}
}

// ACCEPTANCE: restart/resume preserves the UUID, the mailbox watermark and the
// pending mail below it.
func TestResumePreservesTheUUIDAndMailbox(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")

	first, err := OpenContext(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		r.MailFrom = 42
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}

	// A restart: the harness comes back and names the context it was having.
	again, err := OpenContext(l, func() ContextRequest {
		r := req("session-a", store)
		r.Resume = first.Instance.UUID
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	if again.Instance.UUID != first.Instance.UUID {
		t.Fatalf("resume minted a new UUID: %s -> %s", first.Instance.UUID, again.Instance.UUID)
	}
	if again.Instance.MailFrom != 42 {
		t.Errorf("resume moved the mailbox watermark to %d; pending mail below it is lost", again.Instance.MailFrom)
	}
	if !again.Resumed {
		t.Error("a resume did not report itself as one")
	}
}

// ACCEPTANCE: a competing owning session is refused ACROSS REPOSITORIES.
// Both callers share the host-global room and instance store and differ only
// in session digest and working directory, which is exactly #1245.
func TestCompetingOwningSessionIsRefusedAcrossRepos(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")

	held, err := OpenContext(l, func() ContextRequest {
		r := req("session-in-repo-a", store)
		r.Fresh = true
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}

	_, err = OpenContext(l, ContextRequest{
		Store:    store,
		Resume:   held.Instance.UUID,
		Session:  "session-in-repo-b",
		OwnerPID: os.Getpid(),
		Cwd:      "/tmp/repo-b",
	})
	var live *room.ErrLive
	if !errors.As(err, &live) {
		t.Fatalf("a second live session drove the same context; err = %v", err)
	}
	// The refusal has to say WHERE the incumbent is, or the operator's only
	// next move is to guess which checkout to go look in.
	if !strings.Contains(err.Error(), "repo-a") {
		t.Errorf("refusal does not locate the incumbent: %v", err)
	}
}

// The same session's per-turn CHILD command keeps ownership even though its
// own pid has exited. This is the stable-OwnerPID rule; judging liveness by
// the child's pid made the incumbent look dead between two turns.
func TestSameSessionChildCommandKeepsOwnership(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")

	first, err := OpenContext(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}

	again, err := OpenContext(l, ContextRequest{
		Store:    store,
		Resume:   first.Instance.UUID,
		Session:  "session-a",
		OwnerPID: os.Getpid(),
		Cwd:      "/tmp/repo-a",
	})
	if err != nil {
		t.Fatalf("a child command of the owning session was refused: %v", err)
	}
	if again.Instance.UUID != first.Instance.UUID {
		t.Error("a child command of the owning session landed on another context")
	}
	owner, ok := principal.InstanceOwner(first.Instance)
	if !ok || owner.OwnerPID != os.Getpid() {
		t.Errorf("ownership did not stay anchored to the harness: %+v", owner)
	}
}

// A context whose owner has EXITED is reused rather than contested: the
// ordinary relaunch must not burn a cap slot per start.
func TestIdleContextIsReusedBeforeMintingANewOne(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")

	first, err := OpenContext(l, ContextRequest{
		Store: store, Session: "session-gone", OwnerPID: deadPID(t),
		Cwd: "/tmp/repo-a", Fresh: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	next, err := OpenContext(l, req("session-new", store))
	if err != nil {
		t.Fatal(err)
	}
	if next.Instance.UUID != first.Instance.UUID {
		t.Fatalf("an idle context was abandoned and a new one minted: %s -> %s",
			first.Instance.UUID, next.Instance.UUID)
	}
	if !next.Reused {
		t.Error("the reuse was not reported as one")
	}
	all, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("store holds %d records; reuse should not have minted", len(all))
	}
}

// ACCEPTANCE: active cap exhaustion is ACTIONABLE — it names the family, the
// count, who is holding the slots and how to get one back.
func TestCapExhaustionIsActionable(t *testing.T) {
	store := identityHost(t).WithCap(1)
	l := rawLaunch("claude:opus5.5")

	if _, err := OpenContext(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		return r
	}()); err != nil {
		t.Fatal(err)
	}
	_, err := OpenContext(l, func() ContextRequest {
		r := req("session-b", store)
		r.Fresh = true
		return r
	}())
	var capped *fleet.CapError
	if !errors.As(err, &capped) {
		t.Fatalf("the cap did not bite: %v", err)
	}
	for _, want := range []string{"1/1", "retire", fleet.InstanceCapEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cap refusal omits %q: %v", want, err)
		}
	}
}

// ACCEPTANCE: a binding change requires a handoff. Continuing a live context
// after its family's configuration moved is refused as a mutation.
func TestResumeRefusesAChangedBinding(t *testing.T) {
	store := identityHost(t)
	l := namedLaunch("esme", "claude", "opus5.5")

	catalog := catalogOf(t, fleet.Agent{Name: "esme", Tool: "claude", Model: "opus5.5"})
	first, err := openContextWithCatalog(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		return r
	}(), catalog)
	if err != nil {
		t.Fatal(err)
	}

	// The operator re-points the agent at another model. Same NAME, different
	// configuration: the live conversation may not follow it.
	moved := catalogOf(t, fleet.Agent{Name: "esme", Tool: "claude", Model: "sonnet5"})
	_, err = openContextWithCatalog(l, ContextRequest{
		Store: store, Resume: first.Instance.UUID, Session: "session-a",
		OwnerPID: os.Getpid(), Cwd: "/tmp/repo-a",
	}, moved)
	if !errors.Is(err, fleet.ErrBindingImmutable) {
		t.Fatalf("a live context followed a reconfiguration: %v", err)
	}
	if !strings.Contains(err.Error(), first.Instance.UUID) {
		t.Errorf("the refusal does not name the context: %v", err)
	}

	// And the same session on the NEW configuration gets a different context,
	// so the handoff has somewhere to land.
	fresh, err := openContextWithCatalog(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		return r
	}(), moved)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Instance.UUID == first.Instance.UUID {
		t.Error("the new configuration reused the old context's UUID")
	}
	if fresh.Instance.FamilyID == first.Instance.FamilyID {
		t.Error("two configurations computed one family id")
	}
}

// A binding OUTSIDE the frozen set is a reconfiguration and is refused at
// launch.
func TestLaunchRefusesABindingOutsideTheFrozenSet(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")

	r := req("session-a", store)
	r.Fresh = true
	r.Binding = "claude:sonnet5"
	if _, err := OpenContext(l, r); !errors.Is(err, fleet.ErrBindingImmutable) {
		t.Fatalf("a launch selected a model outside its family: %v", err)
	}
}

// ACCEPTANCE: selecting inside a PREDEFINED COMPOSITE preserves identity.
func TestCompositeSelectionPreservesIdentity(t *testing.T) {
	store := identityHost(t)
	catalog := catalogOf(t,
		fleet.Agent{Name: "cheap", Tool: "glm", Model: "4.9"},
		fleet.Agent{Name: "dear", Tool: "codex", Model: "gpt6-sol"},
		fleet.Agent{Name: "ladder", Tool: "glm", Model: "4.9",
			BandSource: "cascade", Base: "cheap", Escalation: []string{"dear"}},
	)
	l := namedLaunch("ladder", "glm", "4.9")

	base, err := openContextWithCatalog(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		return r
	}(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !base.Family.Composite() {
		t.Fatalf("a cascade did not resolve as a composite: %+v", base.Family)
	}
	if base.Binding != "glm:4.9" {
		t.Errorf("composite started at %q, want the declared base", base.Binding)
	}

	// Escalating to the dear rung is a SELECTION, not a reconfiguration.
	escalated, err := openContextWithCatalog(l, ContextRequest{
		Store: store, Resume: base.Instance.UUID, Session: "session-a",
		OwnerPID: os.Getpid(), Cwd: "/tmp/repo-a", Binding: "codex:gpt6-sol",
	}, catalog)
	if err != nil {
		t.Fatalf("escalating inside a predefined composite was refused: %v", err)
	}
	if escalated.Instance.UUID != base.Instance.UUID {
		t.Error("escalation inside the frozen set changed the UUID")
	}
	if escalated.Binding != "codex:gpt6-sol" {
		t.Errorf("escalation selected %q", escalated.Binding)
	}
	// A model NOT on the ladder is still refused.
	_, err = openContextWithCatalog(l, ContextRequest{
		Store: store, Resume: base.Instance.UUID, Session: "session-a",
		OwnerPID: os.Getpid(), Cwd: "/tmp/repo-a", Binding: "claude:opus5.5",
	}, catalog)
	if !errors.Is(err, fleet.ErrBindingImmutable) {
		t.Errorf("a model outside the ladder was selected: %v", err)
	}
}

// ACCEPTANCE: retiring and reusing a label does not expose the old mail. The
// label comes back; the mailbox does not come with it.
func TestReusedLabelInheritsNoMailboxIdentity(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")

	first, err := OpenContext(l, func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		r.Label = "Esme-2"
		r.MailFrom = 5
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}

	mail := []string{`{"seq":6,"body":"for the first Esme-2"}`}
	archive, err := RetireContext(store, first.Instance.UUID,
		func(fleet.Instance) ([]string, error) { return mail, nil }, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(archive) == "" {
		t.Fatal("retirement reported no archive")
	}

	// The label is free, so a LATER context may hold it.
	next, err := OpenContext(l, func() ContextRequest {
		r := req("session-b", store)
		r.Fresh = true
		r.Label = "Esme-2"
		r.MailFrom = 99
		return r
	}())
	if err != nil {
		t.Fatalf("the released label was not reusable: %v", err)
	}
	if next.Instance.UUID == first.Instance.UUID {
		t.Fatal("label reuse resurrected the retired UUID")
	}
	if next.Instance.MailAddress() == first.Instance.MailAddress() {
		t.Fatal("the reused label inherited the retired mailbox address")
	}
	if next.Instance.MailArchive != "" {
		t.Errorf("a fresh context points at an archive: %s", next.Instance.MailArchive)
	}
	// The old mail is ARCHIVED, not deleted, and still keyed on the old UUID.
	kept, err := store.ArchivedMail(first.Instance.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || !strings.Contains(kept[0], "for the first Esme-2") {
		t.Errorf("the retired context's mail was not preserved: %v", kept)
	}
	if later, err := store.ArchivedMail(next.Instance.UUID); err != nil || len(later) != 0 {
		t.Errorf("the new context can read %d archived records: %v", len(later), err)
	}
}

// Retiring without a real mail source is refused rather than writing an empty
// archive that claims to have preserved something.
func TestRetireRefusesWithoutAMailSource(t *testing.T) {
	store := identityHost(t)
	c, err := OpenContext(rawLaunch("claude:opus5.5"), func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RetireContext(store, c.Instance.UUID, nil, "session-a"); !errors.Is(err, fleet.ErrMailSourceRequired) {
		t.Fatalf("an empty archive was accepted: %v", err)
	}
}

// ACCEPTANCE: external identity works without a repeated --as. The stamped
// environment is what pkg/weave's sprintLeaseIdentity and
// principal.SelfInstanceUUID already read, and it REPLACES an inherited one.
func TestContextEnvStampsTheIdentityConsumersAlreadyRead(t *testing.T) {
	store := identityHost(t)
	c, err := OpenContext(rawLaunch("claude:opus5.5"), func() ContextRequest {
		r := req("session-a", store)
		r.Fresh = true
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}

	base := []string{
		"PATH=/bin",
		InstanceUUIDEnv + "=11111111-2222-3333-4444-555555555555",
		InstanceEnv + "=11111111-2222-3333-4444-555555555555",
		"BASHY_PRINCIPAL=dhnt:agent/esme",
	}
	env := SessionEnv(ContextEnv(base, c), "session-a")

	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Fatalf("%s appears twice; the child would read whichever the consumer scans first", k)
		}
		got[k] = v
	}
	if got[InstanceUUIDEnv] != c.Instance.UUID || got[InstanceEnv] != c.Instance.UUID {
		t.Errorf("the inherited UUID survived: %s / %s", got[InstanceUUIDEnv], got[InstanceEnv])
	}
	if got[InstanceFamilyEnv] != c.Instance.FamilyID {
		t.Errorf("the FROZEN family id was not stamped: %q", got[InstanceFamilyEnv])
	}
	if got[SessionClaimEnv] != "session-a" {
		t.Errorf("the session digest was not stamped: %q", got[SessionClaimEnv])
	}
	// The legacy principal is UNTOUCHED: bus.ResolveAuthoredActor resolves a
	// board name from it and a UUID resolves to no registered agent. #1109.
	if got["BASHY_PRINCIPAL"] != "dhnt:agent/esme" {
		t.Errorf("BASHY_PRINCIPAL was rewritten to %q; legacy agents lose the ability to author", got["BASHY_PRINCIPAL"])
	}
	if got["PATH"] != "/bin" {
		t.Error("an unrelated variable was dropped")
	}

	// And the stamped environment is what the existing reader resolves.
	t.Setenv(principal.InstanceEnv, got[InstanceEnv])
	if id, ok := principal.SelfInstanceUUID(); !ok || id != c.Instance.UUID {
		t.Errorf("principal.SelfInstanceUUID() = %q, %v; want the stamped UUID", id, ok)
	}
	if _, err := principal.SelfInstance(store); err != nil {
		t.Errorf("the stamped identity does not resolve to its record: %v", err)
	}
}

// A context request with no session digest is refused. An empty digest
// compares equal to every other empty digest, so accepting one would make all
// unattributed sessions a single session.
func TestOpenContextRequiresASessionDigest(t *testing.T) {
	store := identityHost(t)
	if _, err := OpenContext(rawLaunch("claude:opus5.5"), ContextRequest{Store: store}); !errors.Is(err, room.ErrNoSessionClaim) {
		t.Fatalf("a context was opened with no session digest: %v", err)
	}
}

// A named launch the catalog does not have is refused rather than silently
// downgraded to a raw binding — freezing a configuration nobody declared.
func TestUnknownNamedLaunchHasNoFamily(t *testing.T) {
	store := identityHost(t)
	r := req("session-a", store)
	r.Fresh = true
	_, err := openContextWithCatalog(namedLaunch("ghost", "claude", "opus5.5"), r, catalogOf(t))
	if !errors.Is(err, ErrFamilyUnknown) {
		t.Fatalf("an undeclared agent got a family: %v", err)
	}
}
